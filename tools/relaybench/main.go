// relaybench benchmarks registry-mirrors-relay: it starts a fake upstream on the local machine (TLS,
// with content generated from a pattern in memory), then starts relay as a child process and measures
// idle memory, single-core throughput, manifest cache hit latency, and how much memory each stream
// uses with many slow clients. Linux only (it reads /proc). Results are printed as Markdown.
//
//	go build -o relaybench ./tools/relaybench
//	./relaybench -relay ./registry-mirrors-relay [-streams 1000] [-slow-rate 32768]
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	relayBin   = flag.String("relay", "./registry-mirrors-relay", "relay executable")
	streams    = flag.Int("streams", 1000, "number of concurrent slow client streams")
	slowRate   = flag.Int("slow-rate", 32<<10, "read speed of each slow client (bytes/second)")
	blobSize   = flag.Int64("blob", 1<<30, "size of the test layer (bytes)")
	fastConns  = flag.Int("fast", 4, "number of concurrent clients in the throughput test")
	fastSecs   = flag.Int("fast-seconds", 15, "duration of the throughput test (seconds)")
	procs      = flag.Int("gomaxprocs", 1, "relay GOMAXPROCS (1 for measuring single-core throughput)")
	extraYAML  = flag.String("extra", "", "YAML appended to the end of the relay configuration (for comparing settings)")
	label      = flag.String("label", "Default configuration", "name of this set of results")
	memLimit   = flag.String("gomemlimit", "", "relay GOMEMLIMIT (simulates a container memory limit); empty leaves it unset")
	slowSettle = flag.Int("settle", 15, "seconds to wait after all slow clients have started before sampling memory")
)

const repo = "bench/app"

func main() {
	flag.Parse()
	log.SetFlags(0)
	dir, err := os.MkdirTemp("", "relaybench")
	check(err)
	defer os.RemoveAll(dir)

	ca, caPEM, leaf := certs()
	_ = ca
	caFile := filepath.Join(dir, "ca.pem")
	check(os.WriteFile(caFile, caPEM, 0o600))

	up := newUpstream(*blobSize)
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	upSrv := &http.Server{Handler: up, TLSConfig: &tls.Config{Certificates: []tls.Certificate{leaf}, NextProtos: []string{"h2", "http/1.1"}},
		ConnState: func(_ net.Conn, st http.ConnState) {
			switch st {
			case http.StateNew:
				up.conns.Add(1)
			case http.StateClosed, http.StateHijacked:
				up.conns.Add(-1)
			}
		}}
	go func() { _ = upSrv.ServeTLS(upLn, "", "") }()
	defer upSrv.Close()

	relayLn, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	relayAddr := relayLn.Addr().String()
	_ = relayLn.Close()

	cfg := fmt.Sprintf(`data_dir: %q
listen: {http: %q}
entrances:
  - hosts: ["*"]
    upstream: bench.test
upstreams:
  bench.test:
    registry: %q
    tls_ca_file: %q
    allow_private_network: true
    token_realm_hosts: ['none\.invalid']
admin: {store: {driver: none}}
log: {level: warn, access: {output: "off"}}
`, dir, relayAddr, upLn.Addr().String(), caFile)
	cfg += *extraYAML
	cfgFile := filepath.Join(dir, "config.yaml")
	check(os.WriteFile(cfgFile, []byte(cfg), 0o600))

	cmd := exec.Command(*relayBin, "serve", "-config", cfgFile)
	cmd.Env = append(os.Environ(), "GOMAXPROCS="+strconv.Itoa(*procs))
	if *memLimit != "" {
		cmd.Env = append(cmd.Env, "GOMEMLIMIT="+*memLimit)
	}
	stderr, _ := os.Create(filepath.Join(dir, "relay.log"))
	cmd.Stdout, cmd.Stderr = stderr, stderr
	check(cmd.Start())
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	base := "http://" + relayAddr
	waitReady(base)
	time.Sleep(2 * time.Second)

	var rows [][2]string
	add := func(k, v string) { rows = append(rows, [2]string{k, v}); log.Printf("%-28s %s", k, v) }
	add("relay GOMAXPROCS", strconv.Itoa(*procs))
	if *memLimit != "" {
		add("relay GOMEMLIMIT", *memLimit)
	}
	idle := rss(pid)
	add("Idle RSS", mib(idle))

	// manifest: pull once so the cache is ready, then measure hit latency
	c := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	mget(c, base+"/v2/"+repo+"/manifests/v1")
	byDigest := latency(c, base+"/v2/"+repo+"/manifests/"+up.manifestDigest, 5000)
	byTag := latency(c, base+"/v2/"+repo+"/manifests/v1", 5000)
	add("Manifest hit by digest p50/p99", byDigest)
	add("Manifest hit by tag p50/p99", byTag)
	add("Manifest GETs received by upstream", strconv.Itoa(int(up.manifestGets.Load())))

	// Single-core throughput: several clients download a large layer at full speed at once
	cpu0 := cpuSeconds(pid)
	bytes, secs := throughput(base+"/v2/"+repo+"/blobs/"+up.blobDigest, *fastConns, time.Duration(*fastSecs)*time.Second)
	cpu1 := cpuSeconds(pid)
	gbps := float64(bytes) * 8 / secs / 1e9
	add("Throughput", fmt.Sprintf("%.2f Gbit/s (%d clients, %.0f s, relay CPU %.1f s)", gbps, *fastConns, secs, cpu1-cpu0))
	add("RSS after throughput test", mib(rss(pid)))

	// Slow clients: many concurrent clients, each very slow, to see how much memory each stream uses
	ctx, cancel := context.WithCancel(context.Background())
	var started, failed atomic.Int64
	var wg sync.WaitGroup
	slowClient := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 0, MaxIdleConnsPerHost: *streams, ReadBufferSize: 4 << 10}}
	for i := 0; i < *streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := slowRead(ctx, slowClient, base+"/v2/"+repo+"/blobs/"+up.blobDigest, *slowRate, &started); err != nil && ctx.Err() == nil {
				failed.Add(1)
			}
		}()
		if i%50 == 49 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	deadline := time.Now().Add(60 * time.Second)
	for started.Load() < int64(*streams) && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(time.Duration(*slowSettle) * time.Second)
	var peak int64
	for i := 0; i < 10; i++ {
		peak = max(peak, rss(pid))
		time.Sleep(500 * time.Millisecond)
	}
	add("Slow clients", fmt.Sprintf("%d streams started, %d failed, %d KiB/s each", started.Load(), failed.Load(), *slowRate>>10))
	add(fmt.Sprintf("RSS with %d slow streams", *streams), mib(peak))
	if started.Load() > 0 {
		add("Per stream", fmt.Sprintf("%.0f KiB", float64(peak-idle)/float64(started.Load())/1024))
	}
	add("Upstream TCP connections (during slow streams)", strconv.Itoa(int(up.conns.Load())))
	cancel()
	wg.Wait()

	fmt.Printf("\n### %s\n\n| Item | Result |\n| --- | --- |\n", *label)
	for _, r := range rows {
		fmt.Printf("| %s | %s |\n", r[0], r[1])
	}
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func mib(b int64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }

// rss reads VmRSS from /proc/<pid>/status.
func rss(pid int) int64 {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	check(err)
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "VmRSS:"); ok {
			kb, _ := strconv.ParseInt(strings.Fields(v)[0], 10, 64)
			return kb << 10
		}
	}
	return 0
}

// cpuSeconds reads utime+stime from /proc/<pid>/stat (assuming 100 clock ticks per second).
func cpuSeconds(pid int) float64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	check(err)
	f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+2:]))
	ut, _ := strconv.ParseFloat(f[11], 64)
	st, _ := strconv.ParseFloat(f[12], 64)
	return (ut + st) / 100
}

func waitReady(base string) {
	for i := 0; i < 100; i++ {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/healthz", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Fatal("relay did not become ready")
}

func mget(c *http.Client, url string) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	resp, err := c.Do(req)
	check(err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("%s returned %d", url, resp.StatusCode)
	}
}

func latency(c *http.Client, url string, n int) string {
	d := make([]time.Duration, n)
	for i := range d {
		t := time.Now()
		mget(c, url)
		d[i] = time.Since(t)
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return fmt.Sprintf("%.2f ms / %.2f ms", float64(d[n/2].Microseconds())/1000, float64(d[n*99/100].Microseconds())/1000)
}

func throughput(url string, conns int, dur time.Duration) (int64, float64) {
	var total atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &http.Client{}
			buf := make([]byte, 256<<10)
			for ctx.Err() == nil {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				resp, err := c.Do(req)
				if err != nil {
					return
				}
				for {
					n, err := resp.Body.Read(buf)
					total.Add(int64(n))
					if err != nil {
						break
					}
				}
				_ = resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	return total.Load(), time.Since(start).Seconds()
}

func slowRead(ctx context.Context, c *http.Client, url string, rate int, started *atomic.Int64) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	started.Add(1)
	chunk := make([]byte, 8<<10)
	per := time.Duration(float64(time.Second) * float64(len(chunk)) / float64(rate))
	for {
		t := time.Now()
		if _, err := io.ReadFull(resp.Body, chunk); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		if d := per - time.Since(t); d > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(d):
			}
		}
	}
}

// upstream is the fake upstream: one manifest and one large layer generated from a pattern (using no
// memory).
type upstream struct {
	manifest       []byte
	manifestDigest string
	blobDigest     string
	size           int64
	manifestGets   atomic.Int64
	conns          atomic.Int64
}

type pattern struct{}

func (pattern) ReadAt(p []byte, off int64) (int, error) {
	for i := range p {
		x := off + int64(i)
		p[i] = byte(x*131 + x>>11)
	}
	return len(p), nil
}

func newUpstream(size int64) *upstream {
	// relay does not verify layer content; the benchmark only cares about relaying
	blobDigest := "sha256:" + strings.Repeat("b", 64)
	m := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:%s","size":2},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":%q,"size":%d}]}`, strings.Repeat("c", 64), blobDigest, size))
	sum := sha256.Sum256(m)
	return &upstream{manifest: m, manifestDigest: "sha256:" + hex.EncodeToString(sum[:]), blobDigest: blobDigest, size: size}
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v2/":
		w.WriteHeader(http.StatusOK)
	case strings.Contains(r.URL.Path, "/manifests/"):
		if r.Method == http.MethodGet {
			u.manifestGets.Add(1)
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", u.manifestDigest)
		w.Header().Set("Content-Length", strconv.Itoa(len(u.manifest)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(u.manifest)
		}
	case strings.Contains(r.URL.Path, "/blobs/"):
		http.ServeContent(w, r, "", time.Unix(0, 0), io.NewSectionReader(pattern{}, 0, u.size))
	default:
		http.NotFound(w, r)
	}
}

// certs generates a CA and a server certificate for 127.0.0.1.
func certs() (*x509.Certificate, []byte, tls.Certificate) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relaybench CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	check(err)
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:   time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	check(err)
	return ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
