package proxy

import (
	"container/list"
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// upstream talks to a public resolver over a bypassed route, so split+domain
// DNS hijacking cannot recurse into the TUN.
var upstream = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, "udp", "8.8.8.8:53")
		if err != nil {
			return d.DialContext(ctx, "udp", "1.1.1.1:53")
		}
		return c, nil
	},
}

const (
	fakeDNS = "198.18.0.2"
	fakeMin = 10 // 198.18.0.10 onwards; 198.18.0.1 is the TUN, .2 is DNS
	// fakeDNS6/fakePrefix6 是 IPv6 的 fake 段：与 v4 的 198.18.0.0/15 分开，避免族判断歧义。
	fakeDNS6    = "fdfe:2::2"
	fakePrefix6 = "fdfe:2::/64"
	fake6Min    = 10
)

// exitResolver 是「经出口解析」的函数：split+域名模式下本地上游 DNS 已被吸进 TUN，只能让出口去查。
//
// 参数:
//   - ctx: context.Context — 取消/超时（实现内部另有 2 秒上限）
//   - host: string — 待解析的域名（不带结尾点）
//   - family: string — "ip6" 查 AAAA，其余查 A
//
// 返回:
//   - []netip.Addr — 解析到的对应地址族地址；空表示无记录
//   - error — 出口不可用、超时或应答格式错误
type exitResolver func(ctx context.Context, host, family string) ([]netip.Addr, error)

// nameTableCap 是 fake-IP 表的默认容量：长期运行时按 LRU 淘汰，不依赖重启重置。
const nameTableCap = 8192

// nameEntry 是 LRU 队列里的一条 fake-IP 映射。
type nameEntry struct {
	host string
	ip   netip.Addr
}

type nameTable struct {
	mu     sync.Mutex
	rules  []string
	all    bool // global mode: fake-IP every A query; real resolving happens on the exit
	byIP   map[netip.Addr]string
	byName map[string]*list.Element // host（v4）或 "6:"+host（v6）-> LRU 元素；front 最新
	order  *list.List
	cap    int // 容量上限，测试可调小
	next   uint32
	next6  uint32
	// ipv6 为 true 时 AAAA 也分配 fake 地址；关闭时维持 NODATA（旧行为）。
	ipv6 bool
	// hijackAny: split 且有域名规则时为 true，任意目标 :53 的 UDP 都被接管（否则只有 198.18.0.2 被劫持）。
	hijackAny bool
	// resolveExit: 非 nil 时未命中规则的域名改由出口解析，绝不回落到已被吸进 TUN 的本地 upstream。
	resolveExit exitResolver
}

func newNameTable(rules []string, all bool) *nameTable {
	var clean []string
	for _, r := range rules {
		r = normalizeDomain(r)
		if r != "" {
			clean = append(clean, r)
		}
	}
	return &nameTable{
		rules:  clean,
		all:    all,
		byIP:   map[netip.Addr]string{},
		byName: map[string]*list.Element{},
		order:  list.New(),
		cap:    nameTableCap,
		next:   fakeMin,
		next6:  fake6Min,
	}
}

func normalizeDomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "*.")
	s = strings.TrimPrefix(s, ".")
	s = strings.TrimSuffix(s, ".")
	if s == "" || strings.ContainsAny(s, " /\\") {
		return ""
	}
	return s
}

func matchDomain(host string, rules []string) bool {
	host = normalizeDomain(host)
	if host == "" {
		return false
	}
	for _, r := range rules {
		if host == r || strings.HasSuffix(host, "."+r) {
			return true
		}
	}
	return false
}

// hijack 判断一个 UDP 目标是否应交给本地 fakeDNS 处理。
//
// 参数:
//   - dst: netip.Addr — 目标 IPv4
//   - port: uint16 — 目标端口
//
// 返回:
//   - bool — 端口 53 且（全局模式 / 分流+域名模式 / 目标就是 fakeDNS）时为 true
//
// 允许 nil 接收者，方便 tunDialer 在未持有解析表时直接调用。
func (t *nameTable) hijack(dst netip.Addr, port uint16) bool {
	return t != nil && port == 53 && (t.all || t.hijackAny || dst.String() == fakeDNS)
}

func (t *nameTable) lookup(ip netip.Addr) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.byIP[ip]
	return s, ok
}

// assign 返回 host 对应的 IPv4 fake-IP：命中即刷新 LRU 位置，新域名分配下一个序号并按容量淘汰最旧条目。
//
// 参数:
//   - host: string — 域名（内部会 normalizeDomain，忽略大小写与结尾点）
//
// 返回:
//   - netip.Addr — 该域名稳定使用的 198.18.0.x 地址
func (t *nameTable) assign(host string) netip.Addr {
	return t.assignFor(host, false)
}

// assign6 返回 host 对应的 IPv6 fake 地址（fdfe:2::/64）。
//
// 参数:
//   - host: string — 域名（与 v4 共用 LRU，但使用独立键）
//
// 返回:
//   - netip.Addr — 该域名稳定使用的 IPv6 地址
func (t *nameTable) assign6(host string) netip.Addr {
	return t.assignFor(host, true)
}

// assignFor 是 v4/v6 共用的分配逻辑（同一个 LRU 与互斥，容量上限共享）。
//
// 参数:
//   - host: string — 域名
//   - v6: bool — 分配 IPv6 fake 地址
//
// 返回:
//   - netip.Addr — 稳定映射的 fake 地址
func (t *nameTable) assignFor(host string, v6 bool) netip.Addr {
	host = normalizeDomain(host)
	key := host
	if v6 {
		key = "6:" + host
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, ok := t.byName[key]; ok {
		t.order.MoveToFront(el) // 命中刷新，热点域名不会被淘汰
		return el.Value.(*nameEntry).ip
	}
	// 序号回绕：清空全表，避免新 IP 复用旧映射导致串名
	if (!v6 && t.next > 0x1ffff) || (v6 && t.next6 > 0xfffe) {
		t.clearLocked()
	}
	var ip netip.Addr
	if v6 {
		ip = fakeIP6(t.next6)
		t.next6++
	} else {
		ip = fakeIP(t.next)
		t.next++
	}
	t.byName[key] = t.order.PushFront(&nameEntry{host: host, ip: ip})
	t.byIP[ip] = host
	for t.cap > 0 && t.order.Len() > t.cap {
		t.evictLocked(t.order.Back())
	}
	return ip
}

// evictLocked 淘汰 LRU 队尾的一条映射（正反向同时删除，之后查旧 fake-IP 会安全失败）。
//
// 参数:
//   - el: *list.Element — 队尾元素；nil 时什么都不做
//
// 返回:
//   - 无
//
// 调用方必须持有 t.mu。
func (t *nameTable) evictLocked(el *list.Element) {
	if el == nil {
		return
	}
	e := el.Value.(*nameEntry)
	t.order.Remove(el)
	delete(t.byName, e.key())
	delete(t.byIP, e.ip)
}

// key 返回该条目在 byName 中的键（v6 带 "6:" 前缀）。
//
// 返回:
//   - string — 映射键
func (e *nameEntry) key() string {
	if e.ip.Is6() {
		return "6:" + e.host
	}
	return e.host
}

// clearLocked 清空 fake-IP 的正反向映射与 LRU 队列，并把序号复位到 fakeMin。
//
// 返回:
//   - 无
//
// 调用方必须持有 t.mu。
func (t *nameTable) clearLocked() {
	t.order.Init()
	clear(t.byName)
	clear(t.byIP)
	t.next = fakeMin
	t.next6 = fake6Min
}

func fakeIP(n uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], 0xC6120000+n) // 198.18.0.0 + n
	return netip.AddrFrom4(b)
}

// fakeIP6 返回 fdfe:2::n（n ≥ fake6Min）的 IPv6 fake 地址。
//
// 参数:
//   - n: uint32 — 分配序号
//
// 返回:
//   - netip.Addr — IPv6 地址
func fakeIP6(n uint32) netip.Addr {
	var b [16]byte
	b[0], b[1], b[3] = 0xfd, 0xfe, 0x02 // fdfe:0002::/64
	binary.BigEndian.PutUint32(b[12:], n)
	return netip.AddrFrom16(b)
}

func (t *nameTable) answer(query []byte) []byte {
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		return nil
	}
	qs, err := p.AllQuestions()
	if err != nil || len(qs) == 0 {
		return nil
	}
	q := qs[0]
	name := strings.TrimSuffix(q.Name.String(), ".")
	hdr.Response = true
	hdr.Authoritative = true
	hdr.RecursionAvailable = true
	var a4 [][4]byte
	var a6 [][16]byte
	switch q.Type {
	case dnsmessage.TypeA:
		if t.all || matchDomain(name, t.rules) {
			a4 = append(a4, t.assign(name).As4())
		} else if t.resolveExit != nil {
			// 分流+域名模式：本地 upstream 也走 TUN，只能让出口解析，失败回 SERVFAIL（不回落，避免环回）
			ips, err := t.resolveExit(context.Background(), name, "ip4")
			if err != nil {
				slog.Info("split dns resolve via exit failed", "name", name, "err", err)
				hdr.RCode = dnsmessage.RCodeServerFailure
			} else {
				for _, ip := range ips {
					if ip.Is4() {
						a4 = append(a4, ip.As4())
					}
				}
			}
		} else {
			ips, err := upstream.LookupIP(context.Background(), "ip4", name)
			if err != nil {
				hdr.RCode = dnsmessage.RCodeNameError
			} else {
				for _, ip := range ips {
					if v4 := ip.To4(); v4 != nil {
						var a [4]byte
						copy(a[:], v4)
						a4 = append(a4, a)
					}
				}
			}
		}
	case dnsmessage.TypeAAAA:
		if !t.ipv6 {
			// 关闭 IPv6 时代维持 NODATA：让客户端继续使用 IPv4。
			break
		}
		if t.all || matchDomain(name, t.rules) {
			a6 = append(a6, t.assign6(name).As16())
		} else if t.resolveExit != nil {
			ips, err := t.resolveExit(context.Background(), name, "ip6")
			if err != nil {
				slog.Info("split dns resolve via exit failed", "name", name, "family", "ip6", "err", err)
				hdr.RCode = dnsmessage.RCodeServerFailure
			} else {
				for _, ip := range ips {
					if ip.Is6() && !ip.Is4In6() {
						a6 = append(a6, ip.As16())
					}
				}
			}
		} else {
			ips, err := upstream.LookupIP(context.Background(), "ip6", name)
			if err != nil {
				hdr.RCode = dnsmessage.RCodeNameError
			} else {
				for _, ip := range ips {
					if v6 := ip.To16(); v6 != nil && ip.To4() == nil {
						var a [16]byte
						copy(a[:], v6)
						a6 = append(a6, a)
					}
				}
			}
		}
	default:
		hdr.RCode = dnsmessage.RCodeNotImplemented
	}
	b := dnsmessage.NewBuilder(nil, hdr)
	b.EnableCompression()
	_ = b.StartQuestions()
	_ = b.Question(q)
	_ = b.StartAnswers()
	for _, a := range a4 {
		_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: q.Class, TTL: 30}, dnsmessage.AResource{A: a})
	}
	for _, a := range a6 {
		_ = b.AAAAResource(dnsmessage.ResourceHeader{Name: q.Name, Class: q.Class, TTL: 30}, dnsmessage.AAAAResource{AAAA: a})
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	slog.Debug("dns query answered", "name", name, "type", q.Type, "answers", len(a4)+len(a6), "fake", t.all || matchDomain(name, t.rules))
	return out
}

type dnsPacketConn struct {
	table  *nameTable
	ch     chan []byte
	addr   net.Addr
	closed chan struct{}
}

func (t *nameTable) dnsConn(from net.Addr) net.PacketConn {
	return &dnsPacketConn{table: t, ch: make(chan []byte, 4), addr: from, closed: make(chan struct{})}
}

func (c *dnsPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case <-c.closed:
		return 0, nil, net.ErrClosed
	case ans := <-c.ch:
		return copy(p, ans), c.addr, nil
	}
}

func (c *dnsPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	ans := c.table.answer(p)
	if ans == nil {
		return len(p), nil
	}
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	case c.ch <- ans:
		return len(p), nil
	default:
		return len(p), nil
	}
}

func (c *dnsPacketConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}
func (c *dnsPacketConn) LocalAddr() net.Addr              { return &net.UDPAddr{IP: net.ParseIP(fakeDNS), Port: 53} }
func (c *dnsPacketConn) SetDeadline(time.Time) error      { return nil }
func (c *dnsPacketConn) SetReadDeadline(time.Time) error  { return nil }
func (c *dnsPacketConn) SetWriteDeadline(time.Time) error { return nil }

func looksLikeHost(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 253 || net.ParseIP(s) != nil {
		return false
	}
	return strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
}
