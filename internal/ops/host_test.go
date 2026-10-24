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
		"[fe80::1%eth0]:80":  false, // 带 zone 的 IPv6
		"mirror.example.com": false,
		"192.0.2.10.nip.io":  false, // 域名，可以被 DNS 重绑定
	} {
		if got := DirectHost(host); got != want {
			t.Errorf("%s：期望 %v，实际 %v", host, want, got)
		}
	}
}
