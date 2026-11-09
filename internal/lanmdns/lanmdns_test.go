package lanmdns

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/zeroconf/v2"
	ma "github.com/multiformats/go-multiaddr"
)

// TestSuitable 测广播地址筛选：只接受可路由的 IP 字面量，拒绝中继、浏览器传输、
// 回环与链路本地地址（后两者写进 A/AAAA 记录会让对端连到自己或无法路由）。
func TestSuitable(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"/ip4/192.168.1.5/tcp/8787", true},
		{"/ip4/192.168.1.5/udp/8787/quic-v1", true},
		{"/ip6/2001:db8::1/tcp/8787", true},
		{"/ip4/192.168.1.5/tcp/8787/p2p/QmYyQSo1c1Ym7orWxLYvCrM2EmxFTANf8wXmmE7DWjhx5N", true},
		{"/ip6/fe80::1/tcp/8787", false}, // 链路本地：离开本链路即不可达
		{"/ip4/127.0.0.1/tcp/8787", false},
		{"/ip4/0.0.0.0/tcp/8787", false},
		{"/ip6/::1/tcp/8787", false},
		{"/ip4/1.2.3.4/tcp/8787/ws", false},
		{"/ip4/1.2.3.4/tcp/8787/wss", false},
		{"/ip4/1.2.3.4/tcp/8787/p2p-circuit", false},
		{"/dns4/example.com/tcp/8787", false},
	}
	for _, c := range cases {
		addr, err := ma.NewMultiaddr(c.addr)
		if err != nil {
			t.Fatalf("bad fixture %q: %v", c.addr, err)
		}
		if got := suitable(addr); got != c.want {
			t.Errorf("suitable(%s) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// TestPickIPs 测 A/AAAA 记录选取：跳过回环与链路本地，取第一个可路由的
// IPv4 与 IPv6，v4 在前。
func TestPickIPs(t *testing.T) {
	addrs := []ma.Multiaddr{
		ma.StringCast("/ip4/127.0.0.1/tcp/8787"),   // 监听 0.0.0.0 时会被展开进来
		ma.StringCast("/ip6/fe80::2/tcp/8787"),     // 链路本地，跳过
		ma.StringCast("/ip6/2001:db8::2/tcp/8787"), // 可路由，选中
		ma.StringCast("/ip4/10.0.0.9/tcp/8787"),
	}
	got := pickIPs(addrs)
	if len(got) != 2 || got[0] != "10.0.0.9" || got[1] != "2001:db8::2" {
		t.Fatalf("pickIPs = %v, want [10.0.0.9 2001:db8::2]", got)
	}
	if out := pickIPs(nil); len(out) != 0 {
		t.Fatalf("pickIPs(nil) = %v, want empty", out)
	}
	// 只有回环时必须返回空：宁可注册失败，也不要广播 127.0.0.1。
	onlyLoopback := []ma.Multiaddr{ma.StringCast("/ip4/127.0.0.1/tcp/8787")}
	if out := pickIPs(onlyLoopback); len(out) != 0 {
		t.Fatalf("pickIPs(loopback) = %v, want empty", out)
	}
}

// TestMarkSeenDedupe 测同一 PeerID 在 TTL 内只派发一次，过期后可再次派发。
func TestMarkSeenDedupe(t *testing.T) {
	s := &Service{seen: make(map[peer.ID]time.Time)}
	id := peer.ID("peer-a")
	if !s.markSeen(id) {
		t.Fatal("首次发现应返回 true")
	}
	if s.markSeen(id) {
		t.Fatal("TTL 内重复发现应返回 false")
	}
	s.seen[id] = time.Now().Add(-2 * dedupeTTL)
	if !s.markSeen(id) {
		t.Fatal("超过 TTL 后应重新派发")
	}
}

// TestNewRejectsMissingDeps 测 New 在缺少 Host/Notifee 时返回 nil，调用方需判空。
func TestNewRejectsMissingDeps(t *testing.T) {
	if s := New(Config{}); s != nil {
		t.Fatal("缺少 Host/Notifee 时应返回 nil")
	}
}

// testPeerID 是一个合法的 libp2p PeerID（base58btc 形式），供解析测试使用。
const testPeerID = "QmYyQSo1c1Ym7orWxLYvCrM2EmxFTANf8wXmmE7DWjhx5N"

// TestPeerIDFromInstance 验证从 DNS-SD 实例名还原 PeerID。
//
// 功能：实例名形如 "FST-<PeerID>"；mdnsd 重名时会追加 " (2)"，也要能正确剥离。
//
// 参数:
//   - t: *testing.T — Go 测试上下文
//
// 返回:
//
//	无
func TestPeerIDFromInstance(t *testing.T) {
	for _, in := range []string{
		InstancePrefix + testPeerID,
		InstancePrefix + testPeerID + " (2)",
	} {
		got, err := peerIDFromInstance(in)
		if err != nil {
			t.Fatalf("peerIDFromInstance(%q) 报错: %v", in, err)
		}
		if got.String() != testPeerID {
			t.Fatalf("peerIDFromInstance(%q) = %s, want %s", in, got, testPeerID)
		}
	}
	if _, err := peerIDFromInstance("fst-abc"); err == nil {
		t.Fatal("缺少 FST- 前缀时应当报错")
	}
	if _, err := peerIDFromInstance(InstancePrefix + "not-a-peer-id"); err == nil {
		t.Fatal("非法 PeerID 应当报错")
	}
}

// TestFallbackInfo 验证 TXT 缺失时的兜底还原。
//
// 功能：用实例名里的 PeerID + SRV 端口 + A 记录拼出可拨号地址；
// 回环地址必须被跳过，没有任何可用 IPv4 时不投递。
//
// 参数:
//   - t: *testing.T — Go 测试上下文
//
// 返回:
//
//	无
func TestFallbackInfo(t *testing.T) {
	entry := &zeroconf.ServiceEntry{
		ServiceRecord: zeroconf.ServiceRecord{Instance: InstancePrefix + testPeerID, Service: "_fst._tcp"},
		Port:          8787,
		AddrIPv4:      []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("192.168.1.13")},
	}
	info, ok := fallbackInfo(entry)
	if !ok {
		t.Fatal("fallbackInfo 应当成功还原地址")
	}
	if info.ID.String() != testPeerID {
		t.Fatalf("PeerID = %s, want %s", info.ID, testPeerID)
	}
	if len(info.Addrs) != 1 || info.Addrs[0].String() != "/ip4/192.168.1.13/tcp/8787" {
		t.Fatalf("Addrs = %v, want 仅 /ip4/192.168.1.13/tcp/8787", info.Addrs)
	}

	loopbackOnly := &zeroconf.ServiceEntry{
		ServiceRecord: zeroconf.ServiceRecord{Instance: InstancePrefix + testPeerID},
		Port:          8787,
		AddrIPv4:      []net.IP{net.ParseIP("127.0.0.1")},
	}
	if _, ok := fallbackInfo(loopbackOnly); ok {
		t.Fatal("只有回环地址时不应返回可用结果")
	}
}

// TestBridgeParts 测代播载荷提取：端口优先取 TCP，TXT 带完整 p2p 地址，
// A 记录只保留可路由 IP；没有可用地址时必须返回 ok=false（否则会播出一条
// 谁都连不上的幽灵记录）。
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestBridgeParts(t *testing.T) {
	id, err := peer.Decode(testPeerID)
	if err != nil {
		t.Fatalf("bad peer id: %v", err)
	}
	info := peer.AddrInfo{ID: id, Addrs: []ma.Multiaddr{
		ma.StringCast("/ip4/127.0.0.1/tcp/8787"),
		ma.StringCast("/ip4/192.168.1.13/tcp/8787"),
		ma.StringCast("/ip4/192.168.1.13/udp/8787/quic-v1"),
	}}
	port, ips, txt, host, ok := bridgeParts(info)
	if !ok {
		t.Fatal("bridgeParts 应当成功提取载荷")
	}
	if port != 8787 {
		t.Fatalf("port = %d, want 8787", port)
	}
	if len(ips) != 1 || ips[0] != "192.168.1.13" {
		t.Fatalf("ips = %v, want 仅 192.168.1.13", ips)
	}
	if len(txt) != 2 {
		t.Fatalf("txt = %v, want 2 条（回环地址应被剔除）", txt)
	}
	if host != InstancePrefix+testPeerID+bridgeSuffix {
		t.Fatalf("host = %s, want %s（必须带代播后缀，否则本机会把自己的记录当成对端自播）", host, InstancePrefix+testPeerID+bridgeSuffix)
	}

	noAddr := peer.AddrInfo{ID: id, Addrs: []ma.Multiaddr{ma.StringCast("/ip4/127.0.0.1/tcp/8787")}}
	if _, _, _, _, ok := bridgeParts(noAddr); ok {
		t.Fatal("只有回环地址时不应返回可代播载荷")
	}
}

// TestNoteService 测「对端自己会广播某个服务名」的记录：这条认知必须保留，
// 否则重连后本机又会在同一个服务名上代播，出现两条同名记录。
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestNoteService(t *testing.T) {
	id, err := peer.Decode(testPeerID)
	if err != nil {
		t.Fatalf("bad peer id: %v", err)
	}
	s := &Service{
		services:   []string{StandardService, LegacyService},
		legacyName: LegacyService,
		bridges:    map[peer.ID]*bridgeRec{},
		advertised: map[peer.ID]map[string]time.Time{},
	}

	s.noteServiceLocked(id, StandardService)
	if !s.advertisesLocked(id, StandardService) {
		t.Fatal("标准服务名应被记为对端自播")
	}
	if got := s.bridgeTargetsLocked(id); len(got) != 1 || got[0] != LegacyService {
		t.Fatalf("只需补播 %q，实际 %v", LegacyService, got)
	}
	// 断开再重连：advertised 不清空，仍只补播历史标签。
	delete(s.bridges, id)
	if got := s.bridgeTargetsLocked(id); len(got) != 1 || got[0] != LegacyService {
		t.Fatalf("重连后仍只需补播 %q，实际 %v", LegacyService, got)
	}
}

// TestEqualStrings 测代播载荷比较：地址变化必须被识别出来，否则不会重建记录。
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestEqualStrings(t *testing.T) {
	if !equalStrings(nil, nil) {
		t.Fatal("nil 与 nil 应相等")
	}
	if equalStrings([]string{"a"}, []string{"a", "b"}) {
		t.Fatal("长度不同不应相等")
	}
	if equalStrings([]string{"a"}, []string{"b"}) {
		t.Fatal("内容不同不应相等")
	}
	if !equalStrings([]string{"a", "b"}, []string{"a", "b"}) {
		t.Fatal("内容相同应相等")
	}
}

// TestIsBridgeInstance 测代播记录的识别：带 -br 后缀（含 mdnsd 追加的 " (2)"）
// 必须被整条忽略，否则本机会把自己的记录当成「对端在自播」并撤销它，
// 形成创建→撤销→再创建的自毁循环。
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestIsBridgeInstance(t *testing.T) {
	self := InstancePrefix + testPeerID
	if !isBridgeInstance(self + bridgeSuffix) {
		t.Fatal("带 -br 后缀应判定为代播记录")
	}
	if !isBridgeInstance(self + bridgeSuffix + " (2)") {
		t.Fatal("mdnsd 重名后缀不应影响判定")
	}
	if isBridgeInstance(self) {
		t.Fatal("对端真实实例名不应判定为代播记录")
	}
	// 代播记录也要能被还原出 PeerID，用于对端 TXT 缺失时的兜底。
	id, err := peerIDFromInstance(self + bridgeSuffix)
	if err != nil || id.String() != testPeerID {
		t.Fatalf("peerIDFromInstance(代播实例) = %v, %v", id, err)
	}
}

// TestFirstIP 测从 multiaddr 取 IP 字面量：用于把广播地址限制在选定网卡上。
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestFirstIP(t *testing.T) {
	if got := firstIP(ma.StringCast("/ip4/192.168.1.24/tcp/8787/p2p/" + testPeerID)); got != "192.168.1.24" {
		t.Fatalf("firstIP = %q, want 192.168.1.24", got)
	}
	if got := firstIP(ma.StringCast("/dns4/x.local/tcp/8787")); got != "" {
		t.Fatalf("非 IP 字面量应返回空串，实际 %q", got)
	}
}

// TestIPSetOf 测网卡 IP 集合：nil 表示「不限制」，否则必须只含选定网卡上的地址。
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestIPSetOf(t *testing.T) {
	if ipSetOf(nil) != nil {
		t.Fatal("未限定网卡时应返回 nil（不限制）")
	}
	ifaces, err := net.Interfaces()
	if err != nil || len(ifaces) == 0 {
		t.Skip("本机没有网卡，跳过")
	}
	set := ipSetOf(ifaces[:1])
	if set == nil {
		t.Fatal("限定网卡时不应返回 nil")
	}
}

// TestLANInterfaces 测网卡筛选：选出来的网卡不能是回环，且必须能拿到 IPv4。
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestLANInterfaces(t *testing.T) {
	for _, ifc := range LANInterfaces() {
		if ifc.Flags&net.FlagLoopback != 0 {
			t.Fatalf("回环网卡 %q 不应参与 mDNS", ifc.Name)
		}
		if !hasRoutableIPv4(&ifc) {
			t.Fatalf("网卡 %q 没有可用 IPv4 却被选中", ifc.Name)
		}
	}
}

// TestPinnedInterfaces 测 FST_MDNS_IFACE 覆盖逻辑：名字与序号都要能命中，
// 未设置时必须返回 ok=false（表示「没有人工指定」），指定错名字时返回
// ok=true 但列表为空（调用方据此知道「人工指定无效」，不要静默回落）。
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestPinnedInterfaces(t *testing.T) {
	t.Setenv("FST_MDNS_IFACE", "")
	if _, ok := pinnedInterfaces(); ok {
		t.Fatal("未设置环境变量时 ok 应为 false")
	}
	ifaces, err := net.Interfaces()
	if err != nil || len(ifaces) == 0 {
		t.Skip("本机没有网卡，跳过")
	}
	first := ifaces[0]

	t.Setenv("FST_MDNS_IFACE", first.Name)
	got, ok := pinnedInterfaces()
	if !ok || len(got) != 1 || got[0].Index != first.Index {
		t.Fatalf("按名字指定失败: %v %v", got, ok)
	}

	t.Setenv("FST_MDNS_IFACE", strconv.Itoa(first.Index))
	got, ok = pinnedInterfaces()
	if !ok || len(got) != 1 || got[0].Index != first.Index {
		t.Fatalf("按序号指定失败: %v %v", got, ok)
	}

	t.Setenv("FST_MDNS_IFACE", "no-such-iface-xyz")
	got, ok = pinnedInterfaces()
	if !ok || len(got) != 0 {
		t.Fatalf("指定不存在的网卡时应返回 ok=true 且列表为空，实际 %v %v", got, ok)
	}
}

// TestDescribeInterfaces 测网卡描述文本：日志里要能看出名字、序号与 IPv4，
// 否则现场无法判断到底选中了哪块网卡。
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestDescribeInterfaces(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil || len(ifaces) == 0 {
		t.Skip("本机没有网卡，跳过")
	}
	desc := describeInterfaces(ifaces[:1])
	if len(desc) != 1 {
		t.Fatalf("描述条数 = %d, want 1", len(desc))
	}
	if !strings.Contains(desc[0], ifaces[0].Name) {
		t.Fatalf("描述 %q 应包含网卡名 %q", desc[0], ifaces[0].Name)
	}
	if describeInterfaces(nil) != nil {
		t.Fatal("空输入应返回 nil")
	}
}

// TestProbeMulticastIPv4 测探测函数本身：本机局域网网卡应当能设置组播出口，
// 若这里失败说明测试环境异常，而不是代码逻辑问题。
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestProbeMulticastIPv4(t *testing.T) {
	for _, ifc := range LANInterfaces() {
		if err := probeMulticastIPv4(&ifc); err != nil {
			t.Fatalf("网卡 %q 探测失败: %v", ifc.Name, err)
		}
	}
}

// TestPrimaryIPv4Ranking 测「默认路由所在网卡优先」：这是多网卡机器的关键，
// Hyper-V/Docker 网桥（172.19.x）也是私网地址，若排在前面，TXT 里就会写入
// 局域网设备根本不可达的地址。
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestPrimaryIPv4Ranking(t *testing.T) {
	primary := primaryIPv4()
	if primary == "" {
		t.Skip("本机没有默认路由（离线环境），跳过")
	}
	ifaces, err := net.Interfaces()
	if err != nil || len(ifaces) == 0 {
		t.Skip("本机没有网卡，跳过")
	}
	found := false
	for i := range ifaces {
		addrs, _ := ifaces[i].Addrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.String() == primary {
				found = true
				if got := ifaceRank(&ifaces[i], primary); got != 3 {
					t.Fatalf("默认路由网卡 %q 的分值 = %d, want 3", ifaces[i].Name, got)
				}
			}
		}
	}
	if !found {
		t.Skipf("默认路由地址 %s 不在任何网卡上（虚拟网卡场景），跳过", primary)
	}
	// LANInterfaces 的第一项必须就是默认路由网卡，buildPayload 只广播它。
	sel := LANInterfaces()
	if len(sel) > 0 && ifaceRank(&sel[0], primary) != 3 {
		t.Fatalf("排序后首选网卡 = %q，但不是默认路由网卡", sel[0].Name)
	}
}

// TestMergeUnique 测代播载荷合并：mDNS 与 peerstore 给出的地址集合不同，
// 取并集后必须收敛（第二次合并结果不变），否则记录会被反复重建。
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestMergeUnique(t *testing.T) {
	if mergeUnique(nil, nil) != nil {
		t.Fatal("两个空集合应返回 nil")
	}
	got := mergeUnique([]string{"b", "a"}, []string{"a", "c", ""})
	want := []string{"a", "b", "c"}
	if !equalStrings(got, want) {
		t.Fatalf("mergeUnique = %v, want %v", got, want)
	}
	// 幂等：再次合并同样的内容不应产生变化（这是「记录不抖动」的前提）。
	if again := mergeUnique(got, []string{"a", "b", "c"}); !equalStrings(again, got) {
		t.Fatalf("合并应幂等，第二次得到 %v", again)
	}
}

// TestIsVirtualIface 测虚拟网卡识别：现场那台机器的 WLAN 必须保留，
// Hyper-V 的 vEthernet 必须剔除——它既会让 zeroconf 每发一包打印一次
// "Failed to set multicast interface"，报文也只会出现在虚拟交换机里。
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestIsVirtualIface(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"vEthernet (Default Switch)", true},
		{"vEthernet (WSL)", true},
		{"VMware Network Adapter VMnet1", true},
		{"VirtualBox Host-Only Network", true},
		{"docker0", true},
		{"br-3f2a1b", true},
		{"tun0", true},
		{"wg0", true},
		{"WLAN", false},
		{"以太网", false},
		{"Ethernet 2", false},
		{"Wi-Fi", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isVirtualIface(c.name); got != c.want {
			t.Errorf("isVirtualIface(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestDropVirtual 测虚拟网卡剔除：有物理网卡时只留物理网卡；
// 全是虚拟网卡时必须返回 nil（调用方保留原列表），否则会把 mDNS 整个关掉。
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestDropVirtual(t *testing.T) {
	mk := func(names ...string) []net.Interface {
		out := make([]net.Interface, 0, len(names))
		for i, n := range names {
			out = append(out, net.Interface{Index: i + 1, Name: n})
		}
		return out
	}
	got := dropVirtual(mk("vEthernet (Default Switch)", "WLAN"))
	if len(got) != 1 || got[0].Name != "WLAN" {
		t.Fatalf("应只保留 WLAN，实际 %v", got)
	}
	if got := dropVirtual(mk("WLAN")); got != nil {
		t.Fatalf("没有虚拟网卡时应返回 nil，实际 %v", got)
	}
	if got := dropVirtual(mk("vEthernet (Default Switch)")); got != nil {
		t.Fatalf("全是虚拟网卡时应返回 nil（保留原列表），实际 %v", got)
	}
}

// TestLANInterfacesSingleEgress 测单出口收敛：只要选中的网卡里有默认路由
// 那块的物理网卡，最终列表就必须只有它——zeroconf 会逐网卡 setsockopt，
// 多一块网卡就多一份 "Failed to set multicast interface" 告警风险。
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestLANInterfacesSingleEgress(t *testing.T) {
	primary := primaryIPv4()
	if primary == "" {
		t.Skip("本机没有默认路由（离线环境），跳过")
	}
	sel := LANInterfaces()
	if len(sel) <= 1 {
		return
	}
	if ifaceRank(&sel[0], primary) >= 3 && !isVirtualIface(sel[0].Name) {
		t.Fatalf("默认路由网卡可用时应收敛到一块网卡，实际选中 %d 块: %v",
			len(sel), describeInterfaces(sel))
	}
}

// TestIsOwnBridgeRecord 测「代播记录归属」判定。这是现场 192.168.1.24
// 永远看不见手机的直接原因：
//
//	service=fst instance=FST-<手机 PeerID>-br ipv4=192.168.1.13
//
// 这条记录是开发机替手机代播的、唯一的线索。之前的实现看到 -br 就整条丢弃，
// 于是「自己代播的」和「别人代播的」一起被扔掉了。要求：
//   - 自己代播的 -> true（必须忽略，否则会把本机代播当成对端自播，自毁循环）
//   - 别人代播的 -> false（必须收下，这是中转机牵的线）
//   - 真实记录    -> false
//   - 解析失败    -> false（宁可多收一条，也别把真实发现丢掉）
//
// 参数:
//   - t: *testing.T — 测试句柄
//
// 返回:
//
//	无
func TestIsOwnBridgeRecord(t *testing.T) {
	const otherPeerID = "12D3KooWQU69ZRkhQ3kRU8tNFqYfuxMMhTJMpZXnXuMCjy3Fy9Be"
	self, err := peer.Decode(testPeerID)
	if err != nil {
		t.Fatalf("测试用 PeerID 不合法: %v", err)
	}
	other := InstancePrefix + otherPeerID + bridgeSuffix

	if !isOwnBridgeRecord(InstancePrefix+testPeerID+bridgeSuffix, self) {
		t.Fatal("自己代播的记录应判定为「本机的」，需要忽略")
	}
	if !isOwnBridgeRecord(InstancePrefix+testPeerID+bridgeSuffix+" (2)", self) {
		t.Fatal("mdnsd 重名后缀不应影响归属判定")
	}
	if isOwnBridgeRecord(other, self) {
		t.Fatal("别人代播的记录必须收下，否则中转机牵的线全丢")
	}
	if isOwnBridgeRecord(InstancePrefix+testPeerID, self) {
		t.Fatal("真实记录不是代播，别误判")
	}
	if isOwnBridgeRecord("FST-not-a-peer-id"+bridgeSuffix, self) {
		t.Fatal("实例名解析失败时应放行（false）")
	}
	if isOwnBridgeRecord(InstancePrefix+testPeerID+bridgeSuffix, "") {
		t.Fatal("self 为空时应放行（false）")
	}
}
