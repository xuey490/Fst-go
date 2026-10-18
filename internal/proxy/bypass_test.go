package proxy

import (
	"net"
	"net/netip"
	"testing"
)

func TestGlobalBypass(t *testing.T) {
	// global mode hijacks DNS: no public/system DNS bypass, LAN + exit only.
	b := globalBypass(net.ParseIP("203.0.113.9"), true)
	need := []string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "203.0.113.9/32",
	}
	have := map[string]bool{}
	for _, p := range b {
		have[p.String()] = true
	}
	for _, s := range need {
		if !have[s] {
			t.Fatalf("missing bypass %s in %v", s, have)
		}
	}
	if have["8.8.8.8/32"] || have["1.1.1.1/32"] {
		t.Fatalf("DNS must go through the TUN in global mode: %v", have)
	}
	if prefixContainsIP(b, net.ParseIP("1.2.3.4")) {
		t.Fatal("prefixContainsIP")
	}
}

// TestTunRoutePrefixes 验证分流+域名模式把 DNS 服务器 /32 装进 TUN，且绝不把出口自身装进去（防路由环路）。
func TestTunRoutePrefixes(t *testing.T) {
	user := mustPrefix("93.184.216.0/24")
	p := routePlan{mode: "split", prefixes: []netip.Prefix{user}, exit: net.ParseIP("203.0.113.9"), hijackDNS: true}
	have := map[string]bool{}
	for _, x := range tunRoutePrefixes(p) {
		have[x.String()] = true
	}
	if !have[user.String()] || !have["8.8.8.8/32"] || !have["1.1.1.1/32"] {
		t.Fatalf("split+domains routes missing user CIDR or DNS /32: %v", have)
	}
	if have["203.0.113.9/32"] {
		t.Fatalf("exit must not be routed into the TUN: %v", have)
	}
	for _, x := range dnsHijackPrefixes(net.ParseIP("8.8.8.8")) {
		if x.String() == "8.8.8.8/32" {
			t.Fatal("a DNS server that is also the exit must be excluded")
		}
	}

	// 没有域名规则的分流：行为不变，不接管系统 DNS
	plain := routePlan{mode: "split", prefixes: []netip.Prefix{user}, exit: net.ParseIP("203.0.113.9")}
	for _, x := range tunRoutePrefixes(plain) {
		if x.String() == "8.8.8.8/32" {
			t.Fatal("split without domains must not add DNS routes")
		}
	}

	// 全局模式仍只装两半默认路由（DNS 由 TUN 接口自身劫持）
	g := tunRoutePrefixes(routePlan{mode: "global", hijackDNS: true})
	if len(g) != 2 || g[0].String() != "0.0.0.0/1" || g[1].String() != "128.0.0.0/1" {
		t.Fatalf("global routes = %v", g)
	}
}
