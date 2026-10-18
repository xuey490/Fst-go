package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// queryA 组装一条 A 记录查询报文。
//
// 参数:
//   - t: *testing.T — 测试上下文
//   - host: string — 待查询域名
//
// 返回:
//   - []byte — 可直接交给 nameTable.answer 的 DNS 报文
func queryA(t *testing.T, host string) []byte {
	t.Helper()
	name, err := dnsmessage.NewName(host + ".")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// answersA 解析应答报文的返回码与 A 记录。
//
// 参数:
//   - t: *testing.T — 测试上下文
//   - raw: []byte — nameTable.answer 的输出
//
// 返回:
//   - dnsmessage.Header — 含 RCode 的应答头
//   - []netip.Addr — 应答中的 IPv4 地址
func answersA(t *testing.T, raw []byte) (dnsmessage.Header, []netip.Addr) {
	t.Helper()
	var p dnsmessage.Parser
	hdr, err := p.Start(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.AllQuestions(); err != nil {
		t.Fatal(err)
	}
	var out []netip.Addr
	for {
		h, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Type != dnsmessage.TypeA {
			if err := p.SkipAnswer(); err != nil {
				t.Fatal(err)
			}
			continue
		}
		r, err := p.AResource()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, netip.AddrFrom4(r.A))
	}
	return hdr, out
}

func TestMatchDomain(t *testing.T) {
	rules := newNameTable([]string{"google.com", "*.youtube.com"}, false).rules
	if !matchDomain("google.com", rules) || !matchDomain("www.google.com", rules) {
		t.Fatal("google.com should match")
	}
	if !matchDomain("youtube.com", rules) || !matchDomain("m.youtube.com", rules) {
		t.Fatal("youtube.com should match after *. strip")
	}
	if matchDomain("notgoogle.com", rules) || matchDomain("google.com.evil.test", rules) {
		t.Fatal("suffix must be dotted")
	}
}

func TestFakeIPAssignAndDNS(t *testing.T) {
	tab := newNameTable([]string{"example.com"}, false)
	a := tab.assign("www.example.com")
	b := tab.assign("www.example.com")
	if a != b || a.String() != "198.18.0.10" {
		t.Fatalf("stable fake-IP: %s %s", a, b)
	}
	host, ok := tab.lookup(a)
	if !ok || host != "www.example.com" {
		t.Fatalf("lookup: %s %v", host, ok)
	}

	name, err := dnsmessage.NewName("www.example.com.")
	if err != nil {
		t.Fatal(err)
	}
	q := dnsmessage.Message{Header: dnsmessage.Header{RecursionDesired: true}, Questions: []dnsmessage.Question{{
		Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET,
	}}}
	raw, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	ans := tab.answer(raw)
	var p dnsmessage.Parser
	if _, err := p.Start(ans); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AllQuestions(); err != nil {
		t.Fatal(err)
	}
	rrs, err := p.AllAnswers()
	if err != nil || len(rrs) != 1 {
		t.Fatalf("answers: %v %v", rrs, err)
	}
	body, ok := rrs[0].Body.(*dnsmessage.AResource)
	if !ok || netip.AddrFrom4(body.A) != a {
		t.Fatalf("A record: %#v", rrs[0].Body)
	}

	q.Questions[0].Type = dnsmessage.TypeAAAA
	raw, _ = q.Pack()
	ans = tab.answer(raw)
	p = dnsmessage.Parser{}
	hdr, err := p.Start(ans)
	if err != nil || hdr.RCode != 0 {
		t.Fatalf("AAAA NODATA: %+v %v", hdr, err)
	}
	if _, err := p.AllQuestions(); err != nil {
		t.Fatal(err)
	}
	if rrs, err := p.AllAnswers(); err != nil || len(rrs) != 0 {
		t.Fatalf("AAAA should have no answers: %v %v", rrs, err)
	}
}

// Global mode must fake-IP every A query without touching the (polluted)
// local upstream resolver.
func TestGlobalFakeIPAllDomains(t *testing.T) {
	tab := newNameTable(nil, true)
	for _, name := range []string{"example.com", "random.org", "a.b.c.deep.example.net"} {
		name, err := dnsmessage.NewName(name + ".")
		if err != nil {
			t.Fatal(err)
		}
		q := dnsmessage.Message{Questions: []dnsmessage.Question{{
			Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET,
		}}}
		raw, _ := q.Pack()
		ans := tab.answer(raw)
		var p dnsmessage.Parser
		if _, err := p.Start(ans); err != nil {
			t.Fatalf("answer %s: %v", name, err)
		}
		if _, err := p.AllQuestions(); err != nil {
			t.Fatalf("questions %s: %v", name, err)
		}
		rrs, err := p.AllAnswers()
		if err != nil || len(rrs) != 1 {
			t.Fatalf("answers for %s: %v %v", name, rrs, err)
		}
		body, ok := rrs[0].Body.(*dnsmessage.AResource)
		if !ok {
			t.Fatalf("expected A record for %s", name)
		}
		ip := netip.AddrFrom4(body.A)
		if ip.String() == "" || !strings.HasPrefix(ip.String(), "198.18.") {
			t.Fatalf("expected fake-IP for %s, got %s", name, ip)
		}
		if host, ok := tab.lookup(ip); !ok || host != strings.TrimSuffix(name.String(), ".") {
			t.Fatalf("reverse lookup %s -> %q %v", ip, host, ok)
		}
	}
}

// TestFakeIPLRUEviction 验证容量上限、命中刷新与序号回绕清表。
func TestFakeIPLRUEviction(t *testing.T) {
	tab := newNameTable(nil, false)
	tab.cap = 2
	a := tab.assign("a.example")
	b := tab.assign("b.example")
	if tab.assign("a.example") != a {
		t.Fatal("a must keep its fake-IP")
	}
	c := tab.assign("c.example")
	if _, ok := tab.lookup(b); ok {
		t.Fatal("least recently used b must be evicted")
	}
	if _, ok := tab.lookup(a); !ok {
		t.Fatal("refreshed a must stay")
	}
	if _, ok := tab.lookup(c); !ok {
		t.Fatal("new c must stay")
	}
	if tab.order.Len() != 2 {
		t.Fatalf("LRU len = %d, want 2", tab.order.Len())
	}

	old := tab.assign("d.example")
	tab.mu.Lock()
	tab.next = 0x20000 // 超过可分配序号，触发回绕
	tab.mu.Unlock()
	if got := tab.assign("e.example"); got != fakeIP(fakeMin) {
		t.Fatalf("wrap should restart at fakeMin: %v", got)
	}
	if _, ok := tab.lookup(old); ok {
		t.Fatal("wrap must drop old mappings to avoid name mix-ups")
	}
	if tab.order.Len() != 1 {
		t.Fatalf("after wrap LRU len = %d, want 1", tab.order.Len())
	}
}

// TestFakeIPConcurrent 验证并发分配/查询不会 panic，且在容量内结果稳定。
func TestFakeIPConcurrent(t *testing.T) {
	tab := newNameTable(nil, false)
	tab.cap = 64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				ip := tab.assign(fmt.Sprintf("h%d.example", j%32))
				if host, ok := tab.lookup(ip); !ok || host == "" {
					t.Errorf("lookup %v failed", ip)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestLooksLikeHost(t *testing.T) {
	if !looksLikeHost("example.com") || looksLikeHost("203.0.113.9") || looksLikeHost("") {
		t.Fatal("looksLikeHost")
	}
}

// 分流+域名模式：命中规则的域名仍走 fake-IP；未命中的交给注入的出口解析器，失败回 SERVFAIL（不改用本地上游）。
func TestSplitDNSUnmatchedResolvedViaExit(t *testing.T) {
	tab := newNameTable([]string{"example.com"}, false)
	calls := 0
	tab.resolveExit = func(_ context.Context, host, family string) ([]netip.Addr, error) {
		calls++
		if host != "other.org" {
			t.Fatalf("resolver called for %q", host)
		}
		if family != "ip4" {
			t.Fatalf("family = %q, want ip4", family)
		}
		return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
	}
	hdr, ips := answersA(t, tab.answer(queryA(t, "other.org")))
	if hdr.RCode != dnsmessage.RCodeSuccess || len(ips) != 1 || ips[0].String() != "203.0.113.7" {
		t.Fatalf("via exit: %+v %v", hdr, ips)
	}
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls)
	}
	// 命中规则：fake-IP，且不消耗出口解析
	_, ips = answersA(t, tab.answer(queryA(t, "www.example.com")))
	if len(ips) != 1 || !strings.HasPrefix(ips[0].String(), "198.18.") {
		t.Fatalf("rule domain should get a fake-IP: %v", ips)
	}
	if calls != 1 {
		t.Fatalf("rule domain must not hit the exit resolver (calls=%d)", calls)
	}

	tab.resolveExit = func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, errors.New("出口不可用")
	}
	hdr, ips = answersA(t, tab.answer(queryA(t, "down.org")))
	if hdr.RCode != dnsmessage.RCodeServerFailure || len(ips) != 0 {
		t.Fatalf("failed exit resolve must be SERVFAIL: %+v %v", hdr, ips)
	}
}

func TestDNSPacketConn(t *testing.T) {
	tab := newNameTable([]string{"example.com"}, false)
	pc := tab.dnsConn(&net.UDPAddr{IP: net.IPv4(198, 18, 0, 1), Port: 12345})
	defer pc.Close()
	name, _ := dnsmessage.NewName("example.com.")
	q := dnsmessage.Message{Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	raw, _ := q.Pack()
	if _, err := pc.WriteTo(raw, nil); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, _, err := pc.ReadFrom(buf)
	if err != nil || n == 0 {
		t.Fatalf("ReadFrom: %d %v", n, err)
	}
}

// queryAAAA 组装一条 AAAA 记录查询报文。
func queryAAAA(t *testing.T, host string) []byte {
	t.Helper()
	name, err := dnsmessage.NewName(host + ".")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}},
	}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// answersAAAA 解析应答中的 AAAA 记录。
func answersAAAA(t *testing.T, raw []byte) (dnsmessage.Header, []netip.Addr) {
	t.Helper()
	var p dnsmessage.Parser
	hdr, err := p.Start(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.AllQuestions(); err != nil {
		t.Fatal(err)
	}
	var out []netip.Addr
	for {
		h, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Type != dnsmessage.TypeAAAA {
			if err := p.SkipAnswer(); err != nil {
				t.Fatal(err)
			}
			continue
		}
		r, err := p.AAAAResource()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, netip.AddrFrom16(r.AAAA))
	}
	return hdr, out
}

// TestFakeIPv6AAAA 验证开启 IPv6 时 AAAA 得到 fdfe:2::/64 稳定映射，关闭时维持 NODATA。
func TestFakeIPv6AAAA(t *testing.T) {
	tab := newNameTable([]string{"example.com"}, false)
	tab.ipv6 = true
	ip := tab.assign6("example.com")
	if !mustPrefix(fakePrefix6).Contains(ip) || tab.assign6("example.com") != ip {
		t.Fatalf("fake AAAA not stable: %s", ip)
	}
	hdr, got := answersAAAA(t, tab.answer(queryAAAA(t, "www.example.com")))
	if hdr.RCode != dnsmessage.RCodeSuccess || len(got) != 1 || !mustPrefix(fakePrefix6).Contains(got[0]) {
		t.Fatalf("AAAA answer = %+v %v", hdr, got)
	}
	if got[0] != tab.assign6("www.example.com") {
		t.Fatal("AAAA answer must reuse the assigned address")
	}

	off := newNameTable([]string{"example.com"}, false)
	hdr, got = answersAAAA(t, off.answer(queryAAAA(t, "www.example.com")))
	if hdr.RCode != dnsmessage.RCodeSuccess || len(got) != 0 {
		t.Fatalf("IPv6 disabled must answer NODATA: %+v %v", hdr, got)
	}
}
