package upstream

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// 除了 netip 能直接判断的私网、回环、链路本地、组播，再拦这些地址段。
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // 本网络
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF 协议分配
	netip.MustParsePrefix("192.0.2.0/24"),    // 文档用
	netip.MustParsePrefix("198.18.0.0/15"),   // 测试用
	netip.MustParsePrefix("198.51.100.0/24"), // 文档用
	netip.MustParsePrefix("203.0.113.0/24"),  // 文档用
	netip.MustParsePrefix("240.0.0.0/4"),     // 保留
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64，可能映射到内网 IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001:db8::/32"), // 文档用
}

// Blocked 判断上游连接是否不能连到这个地址。上游只应该在公网上：
// 拦住内网地址，重定向和令牌服务的地址就算被篡改，也碰不到本机和内网的服务。
func Blocked(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() ||
		a.IsMulticast() || !a.IsGlobalUnicast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// guardControl 在 DNS 解析之后、真正建立连接之前检查地址，所以 DNS 重绑定也绕不过去。
func guardControl(allow func(netip.Addr) bool) func(ctx context.Context, network, address string, c syscall.RawConn) error {
	return func(_ context.Context, _ string, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		a, err := netip.ParseAddr(host)
		if err != nil {
			return fmt.Errorf("无法解析的地址 %q", host)
		}
		if allow != nil && allow(a) {
			return nil
		}
		if Blocked(a) {
			return &BlockedError{Addr: a}
		}
		return nil
	}
}

// BlockedError 表示连接被拦下。
type BlockedError struct{ Addr netip.Addr }

func (e *BlockedError) Error() string { return "拒绝连接非公网地址 " + e.Addr.String() }
