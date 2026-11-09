package node

import (
	"testing"

	ma "github.com/multiformats/go-multiaddr"
)

// TestRewriteIP4 验证对外广播地址的改写规则。
//
// 功能：AnnounceIP 生效后，回环 IPv4 必须被换成本机局域网 IPv4，端口与传输
// 协议（TCP / QUIC）保持不变，IPv6 回环则被丢弃。
//
// 参数:
//   - t: *testing.T — Go 测试上下文
//
// 返回:
//
//	无；断言失败时通过 t.Fatalf 终止
func TestRewriteIP4(t *testing.T) {
	in := []ma.Multiaddr{
		ma.StringCast("/ip4/127.0.0.1/tcp/8787"),
		ma.StringCast("/ip4/127.0.0.1/udp/8787/quic-v1"),
		ma.StringCast("/ip6/::1/tcp/8787"),
	}
	got := rewriteIP4(in, "192.168.1.13")
	want := map[string]bool{
		"/ip4/192.168.1.13/tcp/8787":         true,
		"/ip4/192.168.1.13/udp/8787/quic-v1": true,
	}
	if len(got) != len(want) {
		t.Fatalf("rewriteIP4 = %v, want %d 条", got, len(want))
	}
	for _, a := range got {
		if !want[a.String()] {
			t.Fatalf("rewriteIP4 出现意外地址 %s（全部结果 %v）", a, got)
		}
	}
}

// TestRewriteIP4KeepsOriginal 验证没有可用 IPv4 时不把地址清空。
//
// 功能：只监听 IPv6 的宿主上，改写后应当保留原始地址，避免节点对外失联。
//
// 参数:
//   - t: *testing.T — Go 测试上下文
//
// 返回:
//
//	无
func TestRewriteIP4KeepsOriginal(t *testing.T) {
	in := []ma.Multiaddr{ma.StringCast("/ip6/::1/tcp/8787")}
	if got := rewriteIP4(in, "192.168.1.13"); len(got) != 1 {
		t.Fatalf("rewriteIP4(仅 IPv6) = %v, want 原样保留 1 条", got)
	}
}

// TestDialableAddrs 验证拨号前的地址过滤。
//
// 功能：回环/未指定/链路本地地址会被剔除；当全部地址都不可路由时原样返回，
// 以便同机多实例联调仍能走回环。
//
// 参数:
//   - t: *testing.T — Go 测试上下文
//
// 返回:
//
//	无
func TestDialableAddrs(t *testing.T) {
	mixed := []ma.Multiaddr{
		ma.StringCast("/ip4/127.0.0.1/tcp/8787"),
		ma.StringCast("/ip4/169.254.3.4/tcp/8787"),
		ma.StringCast("/ip4/192.168.1.13/tcp/8787"),
	}
	got := dialableAddrs(mixed)
	if len(got) != 1 || got[0].String() != "/ip4/192.168.1.13/tcp/8787" {
		t.Fatalf("dialableAddrs(mixed) = %v, want 仅 192.168.1.13", got)
	}

	onlyLoopback := []ma.Multiaddr{ma.StringCast("/ip4/127.0.0.1/tcp/8787")}
	if got := dialableAddrs(onlyLoopback); len(got) != 1 {
		t.Fatalf("dialableAddrs(仅回环) = %v, want 原样保留", got)
	}
}
