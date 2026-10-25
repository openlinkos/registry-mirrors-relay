// Package testutil is for tests: a test CA, a dialer that maps host names to local ports, and fake
// registry, token service and CDN servers.
package testutil

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// CA is a certificate authority for tests.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	Pool *x509.CertPool
}

// NewCA generates a CA that is valid only for this test run.
func NewCA(t testing.TB) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "registry-mirrors-relay test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &CA{cert: cert, key: key, Pool: pool}
}

// Leaf issues a server certificate for these names.
func (ca *CA) Leaf(t testing.TB, names ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     dnsNames(names),
		IPAddresses:  ipAddrs(names),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func dnsNames(names []string) []string {
	var out []string
	for _, n := range names {
		if net.ParseIP(n) == nil {
			out = append(out, n)
		}
	}
	return out
}

func ipAddrs(names []string) []net.IP {
	var out []net.IP
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

// LeafFiles issues a server certificate, writes the certificate and private key (PEM) into dir, and
// returns the paths of the two files.
func (ca *CA) LeafFiles(t testing.TB, dir string, names ...string) (certFile, keyFile string) {
	t.Helper()
	c := ca.Leaf(t, names...)
	keyDER, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// PEM returns the CA certificate (in PEM format), for settings that need a file (such as tls_ca_file).
func (ca *CA) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// Net maps "hostname:443" to local test servers in place of DNS.
type Net struct {
	CA    *CA
	mu    sync.Mutex
	hosts map[string]string
}

// NewNet creates a test network with a CA.
func NewNet(t testing.TB) *Net {
	return &Net{CA: NewCA(t), hosts: map[string]string{}}
}

// Serve starts handler over TLS and points port 443 of each name at it. It is closed automatically
// when the test ends.
func (n *Net) Serve(t testing.TB, h http.Handler, names ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{n.CA.Leaf(t, names...)}, NextProtos: []string{"h2", "http/1.1"}}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	n.mu.Lock()
	for _, name := range names {
		n.hosts[name+":443"] = srv.Listener.Addr().String()
	}
	n.mu.Unlock()
	return srv
}

// DialContext dials according to the mapping table; addresses not in the table return an error, so
// tests never actually reach the internet.
func (n *Net) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	n.mu.Lock()
	target, ok := n.hosts[addr]
	n.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%s is not in the test network", addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, target)
}
