package proxy

import (
	"net"
	"net/netip"
)

// wellKnownDNS is routed around the TUN in global mode so name lookups still work
// while UDP to the rest of the Internet can go through the relay. Split+domain mode
// still bypasses these so the in-process resolver can recurse without looping.
var wellKnownDNS = []string{
	"8.8.8.8/32", "8.8.4.4/32",
	"1.1.1.1/32", "1.0.0.1/32",
	"9.9.9.9/32", "149.112.112.112/32",
	"223.5.5.5/32", "223.6.6.6/32",
	"119.29.29.29/32", "180.76.76.76/32",
}

var lanCIDRs = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"127.0.0.0/8", "169.254.0.0/16", "100.64.0.0/10", "224.0.0.0/4",
}

// lanCIDRs6 是 IPv6 下必须绕开 TUN 的本地范围（ULA/链路本地/组播）。
var lanCIDRs6 = []string{"fc00::/7", "fe80::/10", "ff00::/8"}

// wellKnownDNS6 是 IPv6 分流时需要拉进 TUN 的公共 DNS（/128）。
var wellKnownDNS6 = []string{
	"2001:4860:4860::8888/128", "2001:4860:4860::8844/128",
	"2606:4700:4700::1111/128", "2606:4700:4700::1001/128",
	"2620:fe::fe/128",
}

const fakeNet = "198.18.0.0/15"

func mustPrefix(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func lanPrefixes() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(lanCIDRs))
	for _, s := range lanCIDRs {
		out = append(out, mustPrefix(s))
	}
	return out
}

func wellKnownDNSPrefixes() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(wellKnownDNS))
	for _, s := range wellKnownDNS {
		out = append(out, mustPrefix(s))
	}
	return out
}

// lanPrefixes6 返回 IPv6 下绕开 TUN 的本地前缀。
//
// 返回:
//   - []netip.Prefix — fc00::/7、fe80::/10、ff00::/8
func lanPrefixes6() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(lanCIDRs6))
	for _, s := range lanCIDRs6 {
		out = append(out, mustPrefix(s))
	}
	return out
}

// globalBypass6 返回 IPv6 全局模式下必须走原网关的前缀（本地范围 + 出口 /128）。
//
// 参数:
//   - exit: netip.Addr — 出口中继 IPv6；无效时不追加
//
// 返回:
//   - []netip.Prefix — 去重后的前缀
func globalBypass6(exit netip.Addr) []netip.Prefix {
	seen := map[netip.Prefix]bool{}
	var out []netip.Prefix
	add := func(p netip.Prefix) {
		p = p.Masked()
		if !p.Addr().Is6() || p.Addr().Is4In6() || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, p := range lanPrefixes6() {
		add(p)
	}
	if exit.IsValid() {
		add(netip.PrefixFrom(exit, 128))
	}
	return out
}

// dnsHijackPrefixes6 返回 IPv6 分流时要拉进 TUN 的 DNS /128（排除出口自身）。
//
// 参数:
//   - exit: netip.Addr — 出口中继 IPv6；无效表示不排除
//
// 返回:
//   - []netip.Prefix — 公共 DNS + 系统 DNS 的 /128 列表
func dnsHijackPrefixes6(exit netip.Addr) []netip.Prefix {
	seen := map[netip.Prefix]bool{}
	var out []netip.Prefix
	add := func(p netip.Prefix) {
		p = p.Masked()
		if !p.Addr().Is6() || p.Addr().Is4In6() || seen[p] || (exit.IsValid() && p.Contains(exit)) {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, s := range wellKnownDNS6 {
		add(mustPrefix(s))
	}
	for _, ip := range systemDNSv6() {
		add(netip.PrefixFrom(ip, 128))
	}
	return out
}

// globalBypass is more-specific than 0.0.0.0/1, so it wins: the LAN and the
// relay stay on the original gateway. DNS is NOT bypassed: global mode hijacks
// the system resolver to the TUN's fake-IP DNS, so every lookup (including
// browser DoH) resolves on the relay exit instead of a polluted local one.
func globalBypass(exit net.IP, hijackDNS bool) []netip.Prefix {
	var out []netip.Prefix
	seen := map[netip.Prefix]bool{}
	add := func(p netip.Prefix) {
		p = p.Masked()
		if !p.Addr().Is4() || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, p := range lanPrefixes() {
		add(p)
	}
	if !hijackDNS {
		for _, p := range wellKnownDNSPrefixes() {
			add(p)
		}
		for _, ip := range systemDNSv4() {
			add(netip.PrefixFrom(ip, 32))
		}
	}
	if ip, err := netip.ParseAddr(exit.String()); err == nil && ip.Is4() {
		add(netip.PrefixFrom(ip, 32))
	}
	return out
}

// dnsHijackPrefixes 返回分流模式下要拉进 TUN 的 DNS 服务器地址（系统 DNS + 内置公共 DNS）。
//
// 参数:
//   - exit: net.IP — 出口中继 IPv4；包含它的前缀会被跳过，否则隧道自身会绕回 TUN
//
// 返回:
//   - []netip.Prefix — 去重后的 /32 列表；系统 DNS 读不到时只剩内置公共 DNS
//
// 说明：分流模式原本只劫持发往 198.18.0.2 的查询，物理网卡上配置的 DNS 仍在直连解析，
// 真实 A/AAAA 可能与 fake-IP 竞争（Windows 会并行查询所有网卡的 DNS）。把这些 /32
// 具体路由装进 TUN 后，发往它们的 53 端口查询才会进入 netstack 被统一接管。
func dnsHijackPrefixes(exit net.IP) []netip.Prefix {
	var exitAddr netip.Addr
	if v4 := exit.To4(); v4 != nil {
		if a, ok := netip.AddrFromSlice(v4); ok {
			exitAddr = a
		}
	}
	seen := map[netip.Prefix]bool{}
	var out []netip.Prefix
	add := func(p netip.Prefix) {
		p = p.Masked()
		if !p.Addr().Is4() || seen[p] || (exitAddr.IsValid() && p.Contains(exitAddr)) {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, p := range wellKnownDNSPrefixes() {
		add(p)
	}
	for _, ip := range systemDNSv4() {
		add(netip.PrefixFrom(ip, 32))
	}
	return out
}

// tunRoutePrefixes 返回要装到 TUN 接口上的路由前缀（不含绕过 TUN 的路由）。
//
// 参数:
//   - p: routePlan — 模式、用户网段、出口 IPv4 与 DNS 劫持开关
//
// 返回:
//   - []netip.Prefix — global: 0.0.0.0/1 + 128.0.0.0/1；split: 用户网段，DNS 劫持时追加 DNS 服务器 /32
func tunRoutePrefixes(p routePlan) []netip.Prefix {
	if p.mode == "global" {
		return []netip.Prefix{mustPrefix("0.0.0.0/1"), mustPrefix("128.0.0.0/1")}
	}
	out := append([]netip.Prefix{}, p.prefixes...)
	if p.hijackDNS {
		out = append(out, dnsHijackPrefixes(p.exit)...)
	}
	return out
}

// tunRoutePrefixes6 返回要装到 TUN 接口上的 IPv6 前缀。
//
// 参数:
//   - p: routePlan — 模式、用户 v6 网段、出口 v6 与 DNS 劫持开关
//
// 返回:
//   - []netip.Prefix — 未开启 IPv6 时为 nil；global: ::/1 + 8000::/1；split: 用户网段（劫持时加 DNS /128）
func tunRoutePrefixes6(p routePlan) []netip.Prefix {
	if !p.ipv6 {
		return nil
	}
	if p.mode == "global" {
		return []netip.Prefix{mustPrefix("::/1"), mustPrefix("8000::/1")}
	}
	out := append([]netip.Prefix{}, p.prefixes6...)
	if p.hijackDNS {
		out = append(out, dnsHijackPrefixes6(p.exit6)...)
	}
	return out
}

func prefixContainsIP(prefixes []netip.Prefix, ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
