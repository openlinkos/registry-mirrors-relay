package ops

import "testing"

func TestDirectHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost":          true,
		"localhost:8080":     true,
		"x.localhost":        true,
		"127.0.0.1:9090":     true,
		"[::1]:8080":         true,
		"192.0.2.10":         true,
		"192.0.2.10:8080":    true,
		"[2001:db8::10]:443": true,
		"[fe80::1%eth0]:80":  false, // IPv6 with a zone
		"mirror.example.com": false,
		"192.0.2.10.nip.io":  false, // a domain name, open to DNS rebinding
	} {
		if got := DirectHost(host); got != want {
			t.Errorf("%s: got %v, want %v", host, got, want)
		}
	}
}
