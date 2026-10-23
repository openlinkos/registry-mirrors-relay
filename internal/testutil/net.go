// Package testutil 给测试用：测试 CA、把主机名映射到本地端口的拨号器，以及假的 registry、令牌服务和 CDN。
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

// CA 是测试用的证书颁发机构。
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	Pool *x509.CertPool
}

// NewCA 生成一个只在本次测试有效的 CA。
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

// Leaf 签发一张包含这些域名的服务器证书。
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

// LeafFiles 签发一张服务器证书，把证书和私钥（PEM）写进 dir，返回两个文件的路径。
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

// PEM 返回 CA 证书（PEM 格式），给需要文件的配置项（如 tls_ca_file）用。
func (ca *CA) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// Net 把 "主机名:443" 映射到本地的测试服务器，代替 DNS。
type Net struct {
	CA    *CA
	mu    sync.Mutex
	hosts map[string]string
}

// NewNet 创建一个带 CA 的测试网络。
func NewNet(t testing.TB) *Net {
	return &Net{CA: NewCA(t), hosts: map[string]string{}}
}

// Serve 用 TLS 启动 handler，并把每个域名的 443 端口指向它。测试结束时自动关闭。
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

// DialContext 按映射表拨号；表里没有的地址返回错误，测试里不会真的访问外网。
func (n *Net) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	n.mu.Lock()
	target, ok := n.hosts[addr]
	n.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("测试网络里没有 %s", addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, target)
}
