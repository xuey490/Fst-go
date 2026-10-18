package proxy

import (
	"net/netip"
	"testing"

	M "github.com/xjasonlyu/tun2socks/v2/metadata"
)

func TestTunDialerDestFakeIP(t *testing.T) {
	tab := newNameTable([]string{"example.com"}, false)
	ip := tab.assign("example.com")
	d := &tunDialer{names: tab}
	addr, err := d.dest(&M.Metadata{DstIP: ip, DstPort: 443})
	if err != nil || addr != "example.com:443" {
		t.Fatalf("fake-IP dest: %q %v", addr, err)
	}
	if _, err := d.dest(&M.Metadata{DstIP: fakeIP(99), DstPort: 443}); err == nil {
		t.Fatal("unknown fake-IP must be rejected")
	}
	addr, err = d.dest(&M.Metadata{DstIP: netip.MustParseAddr("203.0.113.9"), DstPort: 443})
	if err != nil || addr != "203.0.113.9:443" {
		t.Fatalf("public dest: %q %v", addr, err)
	}
}

// 分流+域名模式：物理网卡的 DNS（任意 :53）也必须被接管，否则真实 A 记录会与 fake-IP 竞争。
func TestSplitWithDomainsHijacksAnyResolver(t *testing.T) {
	tab := newNameTable([]string{"example.com"}, false)
	if tab.hijack(netip.MustParseAddr("8.8.8.8"), 53) {
		t.Fatal("split mode without domain rules must not hijack public DNS")
	}
	tab.hijackAny = true
	for _, dst := range []string{fakeDNS, "8.8.8.8", "192.168.1.1"} {
		if !tab.hijack(netip.MustParseAddr(dst), 53) {
			t.Fatalf("split+domains must hijack %s:53", dst)
		}
	}
	if tab.hijack(netip.MustParseAddr("8.8.8.8"), 1234) {
		t.Fatal("only port 53 is hijacked")
	}
	var nilTable *nameTable
	if nilTable.hijack(netip.MustParseAddr(fakeDNS), 53) {
		t.Fatal("nil table must not hijack")
	}
}

// Global mode must hijack DNS addressed to ANY resolver so real answers
// (especially AAAA) can never race with fake-IPs. Split mode keeps
// intercepting only the fakeDNS address.
func TestDialUDPHijacksAnyDNSInGlobalMode(t *testing.T) {
	d := &tunDialer{client: &Client{}, names: newNameTable(nil, true)}
	for _, dst := range []string{"198.18.0.2", "8.8.8.8", "114.114.114.114", "192.168.1.1"} {
		pc, err := d.DialUDP(&M.Metadata{DstIP: netip.MustParseAddr(dst), DstPort: 53})
		if err != nil {
			t.Fatalf("global mode must hijack %s:53: %v", dst, err)
		}
		pc.Close()
	}

	d = &tunDialer{client: &Client{}, names: newNameTable(nil, false)}
	pc, err := d.DialUDP(&M.Metadata{DstIP: netip.MustParseAddr(fakeDNS), DstPort: 53})
	if err != nil {
		t.Fatalf("split mode must still hijack fakeDNS: %v", err)
	}
	pc.Close()
}
