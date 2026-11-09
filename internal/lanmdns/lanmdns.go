// Package lanmdns 实现 FST 的局域网自动发现。
//
// 与直接使用 libp2p 内置 mdns 的差别（P0 改造点）：
//
//  1. 服务类型符合 DNS-SD 规范。历史上 FST 把裸标签 "fst" 传给 zeroconf，
//     实际广播的是 "fst.local."，这种裸标签不是合法服务类型，iOS 的
//     NWBrowser 与 Android 的 NsdManager 都无法浏览，NSBonjourServices 也
//     无法登记。本包改为广播标准类型 "_fst._tcp.local."。
//  2. 同时保留旧标签。老版本桌面端仍只认 "fst.local."，因此默认「双注册 +
//     双浏览」：既让新客户端（含移动端）能发现，也不打断已在运行的旧装机。
//  3. 记录去重。同一台设备会在多个网卡/多个服务名上被看到，本包按 PeerID
//     做短 TTL 去重，避免上层反复收到同一次发现。
//  4. 发现代理（Bridge）。移动端受系统 NsdManager 限制只能注册 "_fst._tcp"，
//     而早期桌面端只认 "fst.local."，两者天然互相不可见；另外部分手机的
//     mdnsd 收不到组播查询，永远不做应答。为此本包允许把「已经发现或已经
//     连上的对端」由本机代为补播到另一套服务名上：只要网络里有一台新客户端，
//     全场设备就能互相看见，不必逐台升级。
//
// 载荷格式与 libp2p 保持一致：TXT 记录为一组 "dnsaddr=<multiaddr>"，
// 接收方用 peer.AddrInfosFromP2pAddrs 还原 PeerID 与地址，因此与旧版本、
// 与标准 libp2p 节点天然互通。
package lanmdns

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/zeroconf/v2"
	ma "github.com/multiformats/go-multiaddr"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	// StandardService 是标准 DNS-SD 服务类型（不含域名后缀）。
	// 实际广播名称为 "_fst._tcp.local."，移动端 NsdManager/NWBrowser 用它浏览。
	StandardService = "_fst._tcp"
	// LegacyService 是历史版本使用的裸标签，广播名称为 "fst.local."。
	// 仅为兼容旧装机而保留，新代码不要把它当作服务类型使用。
	LegacyService = "fst"

	// domain 是 mDNS 固定域名。
	domain = "local"
	// dnsaddrPrefix 是 TXT 记录前缀，与 libp2p mdns 一致。
	dnsaddrPrefix = "dnsaddr="
	// dedupeTTL 是同一 PeerID 的重复发现抑制窗口。
	dedupeTTL = 20 * time.Second
	// entryBuffer 是每个浏览协程的条目缓冲，防止突发包丢事件。
	entryBuffer = 256
	// InstancePrefix 是服务实例名的固定前缀，后面紧跟完整 PeerID。
	// 它同时是 TXT 缺失时的兜底信息来源（见 handle）。
	InstancePrefix = "FST-"
	// bridgeTTL 是代播记录的闲置过期时间。超过这个时间既没有新发现、也没有
	// 连接保持，就认为对端已离网，撤销代播，避免对端列表里堆积幽灵设备。
	bridgeTTL = 20 * time.Minute
	// bridgeSweep 是过期巡检周期。
	bridgeSweep = time.Minute
	// bridgeSuffix 是本机代播实例名的后缀（"FST-<PeerID>-br"）。
	//
	// 为什么必须区分：代播记录会被本机自己的浏览协程收到，如果和真身同名，
	// 就会把「自己发出去的记录」当成「对端在自播」，进而把自己的记录撤掉，
	// 形成「创建→撤销→再创建」的自毁循环（现场表现为记录时有时无、日志刷屏）。
	// 加后缀后，浏览侧靠 isOwnBridgeRecord 认领「自己发的」并忽略，
	// 别人发的代播记录照收——否则中转机牵的线会被全部丢掉。
	bridgeSuffix = "-br"
)

// Notifee 是发现回调。实现方通常就是 *node.Node。
type Notifee interface {
	// HandlePeerFound 在局域网发现一个对端时被调用，可能在多个协程并发调用。
	HandlePeerFound(peer.AddrInfo)
}

// Config 是 New 的参数。
type Config struct {
	// Host 提供本机 PeerID 与网卡地址，用于生成广播内容。必填。
	Host host.Host
	// Notifee 接收发现结果。必填。
	Notifee Notifee
	// Port 写入 SRV 记录，通常等于 libp2p 监听端口（8787）。0 表示 8787。
	Port int
	// Standard 覆盖标准服务类型，空则用 StandardService。测试用。
	Standard string
	// Legacy 覆盖旧标签，空则用 LegacyService；设为 "-" 表示停用旧标签
	// （例如只想要新行为的测试或移动端专用构建）。
	Legacy string
	// Interfaces 限定参与组播的网卡，nil 表示全部（zeroconf 默认）。
	Interfaces []net.Interface
}

// Service 是一个「双注册 + 双浏览」的局域网发现实例。
// 它实现 node 所需的 Start/Close 语义，可安全重复 Close。
type Service struct {
	cfg      Config
	services []string // 实际注册并浏览的服务类型（标准在前）
	// legacyName 是历史裸标签（默认 "fst"），关闭旧标签时为空。
	// 代播只往它上面补：手机自己就会注册标准名，抢注标准名会让
	// 系统 mdnsd 判定重名并把手机自己的实例改成 "FST-xxx (2)"。
	legacyName string

	instance string
	txt      []string
	ips      []string
	hostName string

	// ipv6OK 表示选中的网卡是否能正常设置 IPv6 组播出口。
	// 不能时浏览侧只走 IPv4：zeroconf 的 v6 发送循环没有开关，会对每块网卡
	// 反复 setsockopt 并打印 WARN，而 FST 的发现只依赖 A 记录 + TXT。
	ipv6OK bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	servers []*zeroconf.Server

	// closed 标记 Close 已执行，防止 Close 之后再注册代播记录。
	closed bool

	mu   sync.Mutex
	seen map[peer.ID]time.Time
	// bridges 是本机代播的对端记录，键为对端 PeerID。
	bridges map[peer.ID]*bridgeRec
	// advertised 记录「某对端自己会在哪些服务类型上广播」，键为 PeerID。
	// 它与 bridges 分开保存：连接断开时代播记录会撤销，但「对方用什么标签」
	// 这一事实要留着，重连后才知道该往哪个标签补。
	advertised map[peer.ID]map[string]time.Time
}

// bridgeRec 保存本机为某个对端代播的记录与状态。
//
// 代播的意义见包注释第 4 点：移动端只能注册标准服务名，旧桌面端只认历史
// 裸标签，两者必须由第三方（任意一台新客户端）把记录翻译过去。
type bridgeRec struct {
	// port 是写进 SRV 的端口（对端 libp2p 监听端口）。
	port int
	// ips 是写进 A 记录的地址。
	ips []string
	// txt 是写进 TXT 的 dnsaddr 记录。
	txt []string
	// host 是 SRV 目标主机名（"FST-<PeerID>"），与实例名保持一致。
	host string
	// servers 是按服务类型索引的代播服务器。
	servers map[string]*zeroconf.Server
	// last 是最近一次被刷新（发现或连接）的时间。
	last time.Time
}

// New 创建发现实例，不启动任何协程；调用 Start 才开始注册与浏览。
//
// 参数:
//   - cfg: Config — 必需 Host/Notifee，可选 Port/服务名/网卡
//
// 返回:
//   - *Service — 就绪实例；Host 或 Notifee 为空时返回 nil（调用方需判空）
func New(cfg Config) *Service {
	if cfg.Host == nil || cfg.Notifee == nil {
		return nil
	}
	if cfg.Port == 0 {
		cfg.Port = 8787
	}
	standard := cfg.Standard
	if standard == "" {
		standard = StandardService
	}
	legacy := cfg.Legacy
	if legacy == "" {
		legacy = LegacyService
	}
	services := []string{standard}
	if legacy != "-" && legacy != standard {
		services = append(services, legacy)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		cfg:      cfg,
		services: services,
		legacyName: func() string {
			if legacy == "-" || legacy == standard {
				return ""
			}
			return legacy
		}(),
		// 实例名带完整 PeerID（而不是前 8 位）：TXT 记录被中间件/ROM 吞掉时，
		// 对端仍能用实例名还原 PeerID，配合 SRV+A 记录直接拨号（见 handle）。
		// "FST-" + 52 位 base58 共 56 字节，在 DNS-SD 单个标签 63 字节上限内。
		instance:   InstancePrefix + cfg.Host.ID().String(),
		ctx:        ctx,
		cancel:     cancel,
		ipv6OK:     true,
		seen:       make(map[peer.ID]time.Time),
		bridges:    make(map[peer.ID]*bridgeRec),
		advertised: make(map[peer.ID]map[string]time.Time),
	}
}

// Start 生成广播载荷、注册全部服务类型并开始浏览。
//
// 参数:
//
//	无
//
// 返回:
//   - error — 一个服务类型都没能注册成功时返回错误；部分成功只记录日志并返回 nil
//
// 副作用：占用 UDP 5353 组播；失败不 panic，上层可降级为手动连接。
func (s *Service) Start() error {
	if s == nil {
		return nil
	}
	// 启动即打印一次网卡决策：现场排障时，「这行在不在」就能区分
	// 「跑的是旧版 exe」与「新版选错了网卡」，不必再猜。
	if len(s.cfg.Interfaces) > 0 {
		s.ipv6OK = hasIPv6Multicast(s.cfg.Interfaces)
		slog.Info("mdns interfaces",
			"selected", describeInterfaces(s.cfg.Interfaces),
			"ipv6_multicast", s.ipv6OK,
			"hint", "若此处选错网卡，可用环境变量 FST_MDNS_IFACE=<网卡名或序号> 指定",
		)
	}
	if err := s.buildPayload(); err != nil {
		return err
	}
	registered := 0
	for _, name := range s.services {
		srv, err := zeroconf.RegisterProxy(
			s.instance, name, domain, s.cfg.Port, s.hostName, s.ips, s.txt, s.cfg.Interfaces,
		)
		if err != nil {
			slog.Warn("mdns register failed", "service", name, "err", err)
			continue
		}
		s.servers = append(s.servers, srv)
		registered++
		slog.Info("mdns registered", "service", name+"."+domain+".", "instance", s.instance)
	}
	if registered == 0 {
		return fmt.Errorf("mdns: no service could be registered")
	}
	for _, name := range s.services {
		s.browse(name)
	}
	s.wg.Add(1)
	go s.sweepBridges()
	return nil
}

// Close 停止浏览并撤销全部注册；可重复调用。
//
// 参数:
//
//	无
//
// 返回:
//   - error — 恒为 nil（保持 io.Closer 兼容）
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	for _, rec := range s.bridges {
		for _, srv := range rec.servers {
			srv.Shutdown()
		}
	}
	s.bridges = make(map[peer.ID]*bridgeRec)
	s.mu.Unlock()
	s.cancel()
	for _, srv := range s.servers {
		srv.Shutdown()
	}
	s.servers = nil
	s.wg.Wait()
	return nil
}

// Services 返回实际启用的服务类型列表（标准在前，可能含旧标签），用于日志与自检。
//
// 参数:
//
//	无
//
// 返回:
//   - []string — 例如 ["_fst._tcp", "fst"]
func (s *Service) Services() []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s.services))
	copy(out, s.services)
	return out
}

// buildPayload 生成 TXT（dnsaddr 记录）与 A/AAAA 地址。
//
// 参数:
//
//	无
//
// 返回:
//   - error — 拿不到任何监听地址或没有任何可用 IP 时返回
func (s *Service) buildPayload() error {
	listen, err := s.cfg.Host.Network().InterfaceListenAddresses()
	if err != nil {
		return fmt.Errorf("mdns: listen addresses: %w", err)
	}
	addrs, err := peer.AddrInfoToP2pAddrs(&peer.AddrInfo{ID: s.cfg.Host.ID(), Addrs: listen})
	if err != nil {
		return fmt.Errorf("mdns: build p2p addrs: %w", err)
	}
	// 限定网卡时只广播这些网卡上的地址：多网卡机器（WSL/Hyper-V/无 DHCP 的
	// 备用网卡）会给出 172.19.x、169.254.x 之类地址，写进 TXT 只会让对端
	// 拨到不可达的地址，还会让 Windows 的组播出口选择反复失败。
	// 只广播首选网卡上的地址（LANInterfaces 已按默认路由排序）。多网卡机器
	// 上把 vEthernet/Docker 网段写进 TXT，对端拨号只会超时；mDNS 本身是
	// 链路本地协议，跨网段广播也没有意义。
	allowed := ipSetOf(s.cfg.Interfaces[:min(1, len(s.cfg.Interfaces))])
	for _, addr := range addrs {
		if !suitable(addr) {
			continue
		}
		if allowed != nil && !allowed[firstIP(addr)] {
			continue
		}
		s.txt = append(s.txt, dnsaddrPrefix+addr.String())
	}
	if len(s.txt) == 0 {
		return fmt.Errorf("mdns: no advertisement-suitable address")
	}
	s.ips = pickIPs(addrs)
	if len(s.ips) == 0 {
		return fmt.Errorf("mdns: no IP for A/AAAA record")
	}
	// 只传主机名，不带 ".local"：zeroconf 在 RegisterProxy 里判断
	// HasSuffix(host, domain)，若已经带 ".local" 就不再补结尾的点，
	// 生成的 SRV 目标不是 FQDN，miekg/dns 打包会失败并导致整条广播发不出去。
	s.hostName = s.instance
	return nil
}

// browse 为单个服务类型启动一个浏览协程。
//
// 参数:
//   - name: string — 服务类型（"_fst._tcp" 或 "fst"）
func (s *Service) browse(name string) {
	entries := make(chan *zeroconf.ServiceEntry, entryBuffer)
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		for e := range entries {
			s.handle(e, name)
		}
	}()
	go func() {
		defer s.wg.Done()
		// 未指定网卡时交给 zeroconf 自行枚举；指定了就必须传下去，否则浏览侧
		// 仍会调用 net.Interfaces()，在受限环境（Android 11+）里必然失败。
		var opts []zeroconf.ClientOption
		if len(s.cfg.Interfaces) > 0 {
			opts = append(opts, zeroconf.SelectIfaces(s.cfg.Interfaces))
			if !s.ipv6OK {
				// 浏览侧可以只走 IPv4，直接消掉 v6 的 setsockopt 告警。
				opts = append(opts, zeroconf.SelectIPTraffic(zeroconf.IPv4))
			}
		}
		if err := zeroconf.Browse(s.ctx, name, domain, entries, opts...); err != nil && s.ctx.Err() == nil {
			slog.Warn("mdns browse stopped", "service", name, "err", err)
		}
	}()
}

// handle 解析一条服务条目、记录对端自播状态、按需代播并回调 Notifee。
//
// 参数:
//   - e: *zeroconf.ServiceEntry — 浏览到的条目；TXT 缺失时退回实例名+SRV+A 还原
//   - svc: string — 该条目是从哪个服务类型浏览到的（用于判断是否需要代播）
func (s *Service) handle(e *zeroconf.ServiceEntry, svc string) {
	if e == nil {
		return
	}
	// 代播记录（实例名带 -br）要分两种：
	//   1) 本机代播出去的、被自己浏览回来的 —— 必须整条忽略，否则会把
	//      「本机代播」记成「对端自播」，触发撤销注册的自毁循环；
	//   2) 别的机器替第三方代播的 —— 必须收下并派发，否则当两台设备之间
	//      组播不通、只能靠中转机牵线时（现场 192.168.1.24 与手机
	//      192.168.1.13），被代播的那台永远发现不了对端。
	// 现场证据：192.168.1.24 的 mdnsview 只看到
	//   service=fst instance=FST-<手机 PeerID>-br ipv4=192.168.1.13
	// 没有手机的自播记录；2026-11-09 之前这里无条件 return，于是这条唯一的
	// 线索被丢掉，App 里就永远只有开发机。
	bridged := isBridgeInstance(e.Instance)
	if bridged && isOwnBridgeRecord(e.Instance, s.cfg.Host.ID()) {
		return
	}
	infos := infosFromTXT(e)
	if len(infos) == 0 {
		// 兜底：部分 mdnsd 实现与 AP 代理不转发 TXT 记录（Android 上实测常见），
		// 这时只能靠实例名里的 PeerID + SRV 端口 + A 记录还原地址。不做这一步的
		// 表现是「服务明明看得见，却永远连不上」，且没有任何报错。
		if info, ok := fallbackInfo(e); ok {
			infos = []peer.AddrInfo{info}
		}
	}
	for _, info := range infos {
		if info.ID == s.cfg.Host.ID() {
			continue
		}
		if bridged {
			// 第三方代播记录只用于「发现」，不参与代播决策：
			// 它既不能证明对端自播过某个服务名，也不该再去代播，
			// 否则多台中转机会互相代播形成回环。
			if !s.markSeen(info.ID) {
				continue
			}
			slog.Info("mdns discovered", "peer", info.ID, "service", svc+"."+domain+".",
				"addrs", addrsOf(info), "via", "bridge")
			go s.cfg.Notifee.HandlePeerFound(info)
			continue
		}
		// 记下「对端自己已经在 svc 上广播过」，然后把它补播到本机还在用的
		// 其它服务名上：移动端只注册标准名、旧桌面端只注册裸标签，缺的那一侧
		// 由本机补上，两代客户端才能互相看见。
		s.bridge(info, svc)
		if !s.markSeen(info.ID) {
			continue
		}
		// 发现结果必须留痕：现场「这台 PC 到底看不看得见手机」无法从界面上
		// 判断，而一行 INFO 就能把「收到了组播」与「收到了但解析失败」分开。
		slog.Info("mdns discovered", "peer", info.ID, "service", svc+"."+domain+".", "addrs", addrsOf(info))
		go s.cfg.Notifee.HandlePeerFound(info)
	}
}

// infosFromTXT 解析 TXT 里的 dnsaddr= 记录。
//
// 参数:
//   - e: *zeroconf.ServiceEntry — 一条发现结果
//
// 返回:
//   - []peer.AddrInfo — 解析出的对端；没有可用 TXT 时为空
func infosFromTXT(e *zeroconf.ServiceEntry) []peer.AddrInfo {
	addrs := make([]ma.Multiaddr, 0, len(e.Text))
	for _, t := range e.Text {
		if !strings.HasPrefix(t, dnsaddrPrefix) {
			continue
		}
		addr, err := ma.NewMultiaddr(strings.TrimPrefix(t, dnsaddrPrefix))
		if err != nil {
			continue
		}
		addrs = append(addrs, addr)
	}
	if len(addrs) == 0 {
		return nil
	}
	infos, err := peer.AddrInfosFromP2pAddrs(addrs...)
	if err != nil {
		return nil
	}
	return infos
}

// fallbackInfo 在 TXT 缺失时，用「实例名里的 PeerID + SRV 端口 + A 记录」还原地址。
//
// 参数:
//   - e: *zeroconf.ServiceEntry — 一条发现结果
//
// 返回:
//   - peer.AddrInfo — 可直接拨号的对端
//   - bool — false 表示实例名里没有合法 PeerID，或没有可用的 IPv4 地址
func fallbackInfo(e *zeroconf.ServiceEntry) (peer.AddrInfo, bool) {
	if e.Port <= 0 {
		return peer.AddrInfo{}, false
	}
	id, err := peerIDFromInstance(e.Instance)
	if err != nil {
		return peer.AddrInfo{}, false
	}
	addrs := make([]ma.Multiaddr, 0, len(e.AddrIPv4))
	for _, ip := range e.AddrIPv4 {
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
			continue
		}
		if a, err := ma.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/%d", ip.String(), e.Port)); err == nil {
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		return peer.AddrInfo{}, false
	}
	return peer.AddrInfo{ID: id, Addrs: addrs}, true
}

// isOwnBridgeRecord 判断一条代播记录是不是本机自己发出去的。
//
// 代播记录（"FST-<PeerID>-br"）有两种来源：本机替别人播的，和别的机器替
// 第三方播的。前者会被本机自己的浏览协程收到，必须忽略；后者恰恰是
// 「两台设备之间组播不通、只能靠中转机牵线」时唯一可用的线索，必须收下。
// 区分方式就是实例名里的 PeerID 是不是本机。
//
// 参数:
//   - instance: string — DNS-SD 实例名
//   - self: peer.ID — 本机 PeerID
//
// 返回:
//   - bool — true 表示本机自己代播、应当忽略；PeerID 解析失败按 false 处理
//     （宁可多收一条记录，也不要把真实发现丢掉）
func isOwnBridgeRecord(instance string, self peer.ID) bool {
	if self == "" || !isBridgeInstance(instance) {
		return false
	}
	id, err := peerIDFromInstance(instance)
	if err != nil {
		return false
	}
	return id == self
}

// isBridgeInstance 判断一条记录是不是代播记录（实例名带 -br 后缀）。
//
// 参数:
//   - instance: string — DNS-SD 实例名；mdnsd 重名时会追加 " (2)" 后缀
//
// 返回:
//   - bool — true 表示是代播记录；它可能是本机发的，也可能是别的机器发的，
//     用 isOwnBridgeRecord 进一步区分
func isBridgeInstance(instance string) bool {
	name := instance
	if i := strings.LastIndex(name, " ("); i > 0 && strings.HasSuffix(name, ")") {
		name = name[:i]
	}
	return strings.HasSuffix(name, bridgeSuffix)
}

// peerIDFromInstance 从 "FST-<PeerID>" 取出 PeerID。
//
// 参数:
//   - instance: string — DNS-SD 实例名；mdnsd 在重名时会追加 " (2)" 后缀
//
// 返回:
//   - peer.ID — 解析出的 PeerID
//   - error — 缺少前缀或不是合法 PeerID 时返回
func peerIDFromInstance(instance string) (peer.ID, error) {
	name := strings.TrimPrefix(instance, InstancePrefix)
	if name == instance {
		return "", fmt.Errorf("lanmdns: 实例名 %q 缺少 %s 前缀", instance, InstancePrefix)
	}
	if i := strings.LastIndex(name, " ("); i > 0 && strings.HasSuffix(name, ")") {
		name = name[:i]
	}
	// 代播记录带 -br 后缀（见 bridgeSuffix），解析 PeerID 时必须先剥掉。
	name = strings.TrimSuffix(name, bridgeSuffix)
	return peer.Decode(name)
}

// markSeen 记录一次发现，返回是否应当继续派发（TTL 内重复则丢弃）。
//
// 参数:
//   - id: peer.ID — 被发现的 PeerID
//
// 返回:
//   - bool — true 表示首次在窗口内出现，需要回调；false 表示重复
func (s *Service) markSeen(id peer.ID) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.seen[id]; ok && now.Sub(t) < dedupeTTL {
		return false
	}
	s.seen[id] = now
	if len(s.seen) > 256 {
		for k, t := range s.seen {
			if now.Sub(t) >= dedupeTTL {
				delete(s.seen, k)
			}
		}
	}
	return true
}

// Bridge 让本机代替某台对端，在「对端自己没广播的服务类型」上补播 DNS-SD 记录。
//
// 为什么需要它（现场实测的三个组合）：
//   - 手机端受 NsdManager 限制只能注册 "_fst._tcp"，旧桌面端只浏览
//     "fst.local."，于是旧桌面端永远看不到手机；
//   - 旧桌面端只注册 "fst.local."，手机只浏览 "_fst._tcp"，于是手机也
//     看不到那台旧桌面端（「手机只发现一台 PC」就是这个原因）；
//   - 部分手机的 mdnsd 收不到组播查询、从不做应答，桌面端就算服务名对得上
//     也发现不了它；只要它主动连上过本机，本机就能把它的地址补播出去。
//
// 参数:
//   - info: peer.AddrInfo — 对端 PeerID 与已知地址；地址里至少要有一个可
//     广播的 IPv4/IPv6（回环、链路本地、中继地址会被丢弃）
//
// 返回: 无
//
// 副作用：向 5353 追加代理记录；同一对端重复调用幂等（只刷新时间戳）。
// 失败只记日志，不影响发现主流程。
func (s *Service) Bridge(info peer.AddrInfo) {
	s.bridge(info, "")
}

// bridge 是 Bridge 的实际实现。
//
// 参数:
//   - info: peer.AddrInfo — 对端
//   - svc: string — 对端自己广播所用的服务类型；空表示只知道它连上了本机
func (s *Service) bridge(info peer.AddrInfo, svc string) {
	if s == nil || info.ID == "" || len(info.Addrs) == 0 {
		return
	}
	if info.ID == s.cfg.Host.ID() {
		return
	}
	port, ips, txt, host, ok := bridgeParts(info)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if svc != "" {
		s.noteServiceLocked(info.ID, svc)
	}
	rec := s.bridges[info.ID]
	if rec == nil {
		rec = &bridgeRec{servers: make(map[string]*zeroconf.Server)}
		s.bridges[info.ID] = rec
	}
	// 地址取并集而不是覆盖：同一个对端，mDNS 看到的 TXT 里通常只有一条地址，
	// 而连接建立后 peerstore 里还有 quic/中继等更多地址。若直接覆盖，两个来源
	// 会互相「改回」对方，表现为同一条记录被反复重建（抓包里记录抖动）。
	mergedIPs := mergeUnique(rec.ips, ips)
	mergedTXT := mergeUnique(rec.txt, txt)
	// 只有「端口/主机名/A 记录」变了才重建记录；TXT 变多不算。
	// 连接建立后 peerstore 会补上 quic/中继等地址，TXT 因此从 1 条变成多条，
	// 若把它也算作变化，同一条记录会在几十毫秒内被撤销重播一次（现场日志里
	// 同一个对端连续两条 "mdns bridged peer" 就是这个原因），客户端会看到
	// 记录抖动，而首条 dnsaddr 本来就够拨号用了。
	changed := rec.port != port || rec.host != host || !equalStrings(rec.ips, mergedIPs)
	rec.port, rec.ips, rec.txt, rec.host = port, mergedIPs, mergedTXT, host
	rec.last = time.Now()
	if changed {
		// 地址变了（换网、端口回落），旧记录必须撤销，否则对端会按旧地址拨号。
		for svc, srv := range rec.servers {
			srv.Shutdown()
			delete(rec.servers, svc)
		}
	}
	// 对端自己开始广播某个服务名后，撤销本机在同一个服务名上的代播记录：
	// 两条同名记录会让客户端把同一台设备列两次。
	for name, srv := range rec.servers {
		if s.advertisesLocked(info.ID, name) {
			srv.Shutdown()
			delete(rec.servers, name)
		}
	}
	for _, target := range s.bridgeTargetsLocked(info.ID) {
		if _, ok := rec.servers[target]; ok {
			continue
		}
		srv, err := zeroconf.RegisterProxy(
			rec.host, target, domain, rec.port, rec.host, rec.ips, rec.txt, s.cfg.Interfaces,
		)
		if err != nil {
			slog.Debug("mdns bridge register failed", "service", target, "peer", info.ID, "err", err)
			continue
		}
		rec.servers[target] = srv
		slog.Info("mdns bridged peer", "peer", info.ID, "service", target+"."+domain+".", "ip", rec.ips)
	}
}

// addrsOf 把对端地址转成字符串切片，只用于日志。
//
// 参数:
//   - info: peer.AddrInfo — 对端信息
//
// 返回:
//   - []string — multiaddr 字符串
func addrsOf(info peer.AddrInfo) []string {
	out := make([]string, 0, len(info.Addrs))
	for _, a := range info.Addrs {
		out = append(out, a.String())
	}
	return out
}

// noteServiceLocked 记录「对端自己会在 svc 上广播」。调用方需持有 s.mu。
//
// 参数:
//   - id: peer.ID — 对端
//   - svc: string — 服务类型
func (s *Service) noteServiceLocked(id peer.ID, svc string) {
	m := s.advertised[id]
	if m == nil {
		m = make(map[string]time.Time)
		s.advertised[id] = m
	}
	m[svc] = time.Now()
}

// advertisesLocked 判断对端是否自己广播过 svc。调用方需持有 s.mu。
//
// 参数:
//   - id: peer.ID — 对端
//   - svc: string — 服务类型
//
// 返回:
//   - bool — true 表示对端自己会广播该服务名，本机无需代播
func (s *Service) advertisesLocked(id peer.ID, svc string) bool {
	_, ok := s.advertised[id][svc]
	return ok
}

// bridgeTargetsLocked 计算需要代播的服务类型：本机注册的、但对方自己不广播的。
//
// 参数:
//   - id: peer.ID — 对端
//
// 返回:
//   - []string — 目标服务类型列表（可能为空）
func (s *Service) bridgeTargetsLocked(id peer.ID) []string {
	out := make([]string, 0, len(s.services))
	for _, name := range s.services {
		if s.advertisesLocked(id, name) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// Unbridge 撤销某台对端的全部代播记录（对端断开或离网时调用）。
//
// 参数:
//   - id: peer.ID — 对端 PeerID
//
// 返回: 无
func (s *Service) Unbridge(id peer.ID) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.bridges[id]
	if rec == nil {
		return
	}
	for _, srv := range rec.servers {
		srv.Shutdown()
	}
	// 只撤销记录，保留「对端用什么标签」的认知，重连后才能继续往对的标签补播。
	delete(s.bridges, id)
}

// sweepBridges 周期性清理闲置过久的代播记录，避免对端列表里堆积幽灵设备。
//
// 参数:
//
//	无
//
// 返回: 无（随 Service 上下文退出）
func (s *Service) sweepBridges() {
	defer s.wg.Done()
	t := time.NewTicker(bridgeSweep)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-t.C:
			s.mu.Lock()
			for id, rec := range s.bridges {
				if now.Sub(rec.last) < bridgeTTL {
					continue
				}
				for _, srv := range rec.servers {
					srv.Shutdown()
				}
				delete(s.bridges, id)
			}
			for id, m := range s.advertised {
				for svc, at := range m {
					if now.Sub(at) >= bridgeTTL {
						delete(m, svc)
					}
				}
				if len(m) == 0 {
					delete(s.advertised, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

// bridgeParts 从对端地址里抽出代播所需的 SRV 端口、A 记录、TXT 与主机名。
//
// 参数:
//   - info: peer.AddrInfo — 对端 PeerID 与地址
//
// 返回:
//   - port: int — SRV 端口（优先 TCP，其次 QUIC 的 UDP 端口）
//   - ips: []string — A/AAAA 记录内容
//   - txt: []string — "dnsaddr=<multiaddr>" 列表
//   - host: string — SRV 目标主机名，"FST-<PeerID>"
//   - ok: bool — false 表示没有任何可广播地址
func bridgeParts(info peer.AddrInfo) (port int, ips, txt []string, host string, ok bool) {
	p2p, err := peer.AddrInfoToP2pAddrs(&info)
	if err != nil {
		return 0, nil, nil, "", false
	}
	good := make([]ma.Multiaddr, 0, len(p2p))
	for _, a := range p2p {
		if suitable(a) {
			good = append(good, a)
		}
	}
	if len(good) == 0 {
		return 0, nil, nil, "", false
	}
	ips = pickIPs(good)
	if len(ips) == 0 {
		return 0, nil, nil, "", false
	}
	for _, a := range good {
		txt = append(txt, dnsaddrPrefix+a.String())
	}
	for _, a := range good {
		if v, err := a.ValueForProtocol(ma.P_TCP); err == nil {
			if p, err := strconv.Atoi(v); err == nil && p > 0 {
				port = p
				break
			}
		}
	}
	if port == 0 {
		for _, a := range good {
			if v, err := a.ValueForProtocol(ma.P_UDP); err == nil {
				if p, err := strconv.Atoi(v); err == nil && p > 0 {
					port = p
					break
				}
			}
		}
	}
	if port == 0 {
		return 0, nil, nil, "", false
	}
	// 排序后再比较：同一台设备的多次广播里地址顺序可能不同，不排序会让 Bridge
	// 误判为「地址变了」而反复重建记录（表现为日志刷屏与 5353 上的记录抖动）。
	sort.Strings(ips)
	sort.Strings(txt)
	return port, ips, txt, InstancePrefix + info.ID.String() + bridgeSuffix, true
}

// mergeUnique 返回 a 与 b 的并集（去重并排序）。
//
// 参数:
//   - a: []string — 已有集合
//   - b: []string — 新增集合
//
// 返回:
//   - []string — 去重排序后的并集；两者都为空时返回 nil
func mergeUnique(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, v := range list {
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// equalStrings 比较两个字符串切片内容是否完全一致。
//
// 参数:
//   - a: []string — 切片一
//   - b: []string — 切片二
//
// 返回:
//   - bool — 长度与逐项内容都相同时为 true
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// LANInterfaces 返回适合跑 mDNS 的网卡列表。
//
// 筛选顺序（任一条不满足即剔除）：
//  1. 已启用（Up）且支持组播（Multicast）；
//  2. 非回环；
//  3. 至少有一个可路由的 IPv4（排除 169.254.x 这类没有 DHCP 时出现的地址）；
//  4. 真实试探 setsockopt(IP_MULTICAST_IF) 能成功——这一步才是关键：
//     有的机器上 IPv4 地址齐全，但系统仍拒绝把它作为组播出口，zeroconf
//     随后会在每发一个报文时打印 "[WARN] mdns: Failed to set multicast
//     interface"，并且报文可能从别的网卡发出，局域网里的手机永远收不到。
//  5. 有物理网卡时剔除虚拟网卡（Hyper-V/Docker/VMnet/隧道），见 isVirtualIface；
//  6. 最后收敛到「默认路由所在的那一块」网卡：zeroconf 是逐网卡设置组播出口
//     再发送的，列表越长越容易被其中一块拖成告警，报文出口也越不确定。
//     真需要多网卡时用 FST_MDNS_IFACE 显式指定。
//
// 若环境变量 FST_MDNS_IFACE 指定了网卡（名字或序号，逗号分隔），则完全按
// 它来——现场机器网卡命名复杂时（"以太网 3"、"WLAN"、Hyper-V 虚拟网卡），
// 自动挑选可能选错，留这个开关可以一次定位。
//
// 返回:
//   - []net.Interface — 可用网卡；一个都没有时返回 nil（调用方回落到库默认行为）
func LANInterfaces() []net.Interface {
	if pinned, ok := pinnedInterfaces(); ok {
		slog.Info("mdns interfaces pinned", "selected", describeInterfaces(pinned))
		return pinned
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		slog.Warn("mdns: list interfaces failed", "err", err)
		return nil
	}
	cands := make([]net.Interface, 0, len(ifaces))
	skipped := make([]string, 0, len(ifaces))
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 {
			continue
		}
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		if !hasRoutableIPv4(&ifc) {
			skipped = append(skipped, ifc.Name+"(无可用 IPv4)")
			continue
		}
		cands = append(cands, ifc)
	}
	// 虚拟网卡（Hyper-V vEthernet、Docker、VMnet、WSL、VPN 隧道）只在本机
	// 没有别的物理网卡时才保留：它们上面的地址不属于用户所在的局域网，
	// 却经常能通过地址筛选与组播探测，最后变成「报文从虚拟网卡发出、
	// 局域网里的手机永远收不到」。
	if physical := dropVirtual(cands); len(physical) > 0 {
		for _, ifc := range cands {
			if isVirtualIface(ifc.Name) {
				skipped = append(skipped, ifc.Name+"(虚拟网卡，已让位给物理网卡)")
			}
		}
		cands = physical
	}
	out := make([]net.Interface, 0, len(cands))
	for _, ifc := range cands {
		if err := probeMulticastIPv4(&ifc); err != nil {
			skipped = append(skipped, ifc.Name+"(组播出口被系统拒绝: "+err.Error()+")")
			continue
		}
		out = append(out, ifc)
	}
	if len(out) == 0 && len(cands) > 0 {
		// 探测本身在个别环境里可能不可用（受限容器、特殊驱动）。全失败时
		// 宁可退回候选列表：mDNS 也许还能靠系统默认出口工作，总比彻底关掉好。
		out = cands
		slog.Warn("mdns: multicast probe failed on all candidates, using them anyway",
			"ifaces", describeInterfaces(cands))
	}
	if len(out) == 0 {
		slog.Warn("mdns: no usable LAN interface, falling back to library default")
		return nil
	}
	// 排序：默认路由所在网卡 > 私网地址 > 其他。
	//
	// 排序不是「好看」：Hyper-V 的 vEthernet (Default Switch) 有 172.19.x 私网
	// 地址，能通过前面所有检查，但局域网里的设备不在那个网段；把它排在前面
	// 会让 buildPayload 把不可达地址写进 TXT，对端拨号必然超时。
	primary := primaryIPv4()
	sort.SliceStable(out, func(i, j int) bool {
		return ifaceRank(&out[i], primary) > ifaceRank(&out[j], primary)
	})
	// 单出口收敛：默认路由网卡可用时，只用它收发组播。
	//
	// 为什么要收敛而不是「能用的都发」：zeroconf 在 Windows 上是逐网卡
	// setsockopt(IP_MULTICAST_IF) 后发送，只要列表里有一块网卡被系统拒绝
	// （Hyper-V 的 vEthernet 就是典型：地址齐全、但 setsockopt 返回
	// WSAEINVAL），每发一个报文就会打印一次
	// "[WARN] mdns: Failed to set multicast interface"。收敛到一块网卡后
	// 既没有这个告警，报文出口也唯一确定，不会出现「从虚拟网卡发出去、
	// 局域网里谁都收不到」。
	//
	// 多网卡（真有两张物理网卡分别接不同局域网）的场景用
	// FST_MDNS_IFACE=网卡1,网卡2 显式指定，走不到这里。
	if len(out) > 1 && ifaceRank(&out[0], primary) >= 3 && !isVirtualIface(out[0].Name) {
		for _, ifc := range out[1:] {
			skipped = append(skipped, ifc.Name+"(非默认路由网卡，组播只走主网卡)")
		}
		slog.Info("mdns egress pinned", "iface", describeInterfaces(out[:1]),
			"hint", "确需多网卡组播时设置 FST_MDNS_IFACE=<网卡名>,<网卡名>")
		out = out[:1]
	}
	if len(skipped) > 0 {
		slog.Info("mdns interfaces skipped", "ifaces", skipped)
	}
	return out
}

// pinnedInterfaces 读取 FST_MDNS_IFACE 指定的网卡。
//
// 返回:
//   - []net.Interface — 命中的网卡（按名字或序号匹配）
//   - bool — true 表示环境变量已设置（即使一个都没匹配上）
func pinnedInterfaces() ([]net.Interface, bool) {
	spec := strings.TrimSpace(os.Getenv("FST_MDNS_IFACE"))
	if spec == "" {
		return nil, false
	}
	all, err := net.Interfaces()
	if err != nil {
		return nil, true
	}
	out := make([]net.Interface, 0, 2)
	for _, want := range strings.Split(spec, ",") {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		for _, ifc := range all {
			if strings.EqualFold(ifc.Name, want) || strconv.Itoa(ifc.Index) == want {
				out = append(out, ifc)
				break
			}
		}
	}
	if len(out) == 0 {
		slog.Warn("mdns: FST_MDNS_IFACE matched no interface", "value", spec)
	}
	return out, true
}

// primaryIPv4 返回系统默认路由所使用的本机 IPv4。
//
// 做法：对一个公网字面量地址建 UDP 连接。UDP 的 connect 不发包，只是让系统
// 选路，LocalAddr 就是出口网卡的地址——这是跨平台识别「主用网卡」的稳妥办法。
// 选 223.5.5.5 是因为它是 IP 字面量，不会触发 DNS 查询，离线也不会卡住。
//
// 返回:
//   - string — 主用网卡的 IPv4 字面量；无法判断时返回空串
func primaryIPv4() string {
	c, err := net.Dial("udp4", "223.5.5.5:53")
	if err != nil {
		return ""
	}
	defer c.Close()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || ua.IP == nil {
		return ""
	}
	if v4 := ua.IP.To4(); v4 != nil {
		return v4.String()
	}
	return ""
}

// ifaceRank 给网卡打排序分值。
//
// 参数:
//   - ifc: *net.Interface — 待评估网卡
//   - primaryIP: string — 默认路由所在的 IPv4，空表示未知
//
// 返回:
//   - int — 3=默认路由网卡，2=其他私网网卡，1=其他可路由网卡，0=无 IPv4
func ifaceRank(ifc *net.Interface, primaryIP string) int {
	addrs, err := ifc.Addrs()
	if err != nil {
		return 0
	}
	best := 0
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil {
			continue
		}
		if primaryIP != "" && ip4.String() == primaryIP {
			return 3
		}
		if ip4.IsPrivate() {
			best = 2
			continue
		}
		if usableMDNSIP(ip4.String()) && best < 1 {
			best = 1
		}
	}
	return best
}

// virtualIfacePrefixes 是虚拟网卡/隧道的名字前缀（小写比较）。
//
// 这些网卡在系统里也是「已启用 + 支持组播 + 有私网 IPv4」，能通过所有地址
// 筛查，但它们承载的是虚拟机交换机、容器网段或隧道，不是用户所在的局域网：
//   - Hyper-V 的 "vEthernet (Default Switch)" 上 setsockopt(IP_MULTICAST_IF)
//     会返回 WSAEINVAL，zeroconf 每发一个报文就打印一次 WARN；
//   - 即使设置成功，报文也只会出现在那个虚拟交换机里，局域网中的手机和
//     其它 PC 一个都收不到。
//
// 只在「本机还有物理网卡」时才剔除它们，见 dropVirtual。
var virtualIfacePrefixes = []string{
	"vethernet", "hyper-v", "virtual", "vmnet", "vmware", "vboxnet", "virtualbox",
	"docker", "veth", "br-", "cni", "flannel", "kube",
	"tun", "tap", "utun", "wg", "ppp", "tailscale", "zerotier", "hamachi",
	"npcap", "loopback", "isatap", "teredo", "wsl",
}

// isVirtualIface 判断网卡名是否属于虚拟网卡/隧道。
//
// 参数:
//   - name: string — 系统网卡名，例如 "vEthernet (Default Switch)"、"WLAN"
//
// 返回:
//   - bool — true 表示这是虚拟网卡，不应作为局域网组播出口
func isVirtualIface(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// dropVirtual 从候选网卡里剔除虚拟网卡。
//
// 参数:
//   - cands: []net.Interface — 候选网卡
//
// 返回:
//   - []net.Interface — 只含物理网卡的列表；全部都是虚拟网卡时返回 nil
//     （调用方据此保留原列表，避免在没有物理网卡的环境里把 mDNS 关掉）
func dropVirtual(cands []net.Interface) []net.Interface {
	out := make([]net.Interface, 0, len(cands))
	for _, ifc := range cands {
		if isVirtualIface(ifc.Name) {
			continue
		}
		out = append(out, ifc)
	}
	if len(out) == len(cands) || len(out) == 0 {
		return nil
	}
	return out
}

// describeInterfaces 把网卡列表压成 "名字#序号=IPv4" 的短字符串，用于日志。
//
// 参数:
//   - ifaces: []net.Interface — 待描述网卡
//
// 返回:
//   - []string — 每个网卡一条描述
func describeInterfaces(ifaces []net.Interface) []string {
	out := make([]string, 0, len(ifaces))
	for i := range ifaces {
		ips := make([]string, 0, 2)
		if addrs, err := ifaces[i].Addrs(); err == nil {
			for _, a := range addrs {
				if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
					ips = append(ips, ipnet.IP.To4().String())
				}
			}
		}
		out = append(out, fmt.Sprintf("%s#%d=%s", ifaces[i].Name, ifaces[i].Index, strings.Join(ips, "/")))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// InterfaceStatus 是一块网卡的 mDNS 可用性快照，供现场诊断使用。
//
// 字段均为「观测值」而非「推断值」：MulticastV4Err/MulticastV6Err 来自真实
// setsockopt 探测，因此能直接回答「这台机器为什么刷 Failed to set
// multicast interface」。
type InterfaceStatus struct {
	// Index 是系统网卡序号；Name 是系统网卡名（可能与设备管理器显示不同）。
	Index int
	Name  string
	// Flags 是标志位短名列表，例如 ["up","mcast"]。
	Flags []string
	// IPv4/IPv6 是网卡上配置的地址字面量。
	IPv4 []string
	IPv6 []string
	// HasRoutableIPv4 表示存在非回环、非链路本地的 IPv4。
	HasRoutableIPv4 bool
	// MulticastV4Err/MulticastV6Err 是设置组播出口的探测结果，nil 表示成功。
	MulticastV4Err error
	MulticastV6Err error
	// Selected 表示该网卡被 LANInterfaces 选中参与 mDNS。
	Selected bool
}

// Diagnose 返回全部网卡的 mDNS 可用性快照，用于现场排障。
//
// 与 LANInterfaces 的区别：它不筛选，逐块网卡都做真实探测，因此可以看到
// 「哪块网卡被系统拒绝作为组播出口」——这正是 zeroconf 打印
// "[WARN] mdns: Failed to set multicast interface" 的直接原因。
//
// 返回:
//   - []InterfaceStatus — 按网卡序号升序排列；枚举失败时返回 nil
func Diagnose() []InterfaceStatus {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	selected := make(map[int]bool)
	for _, ifc := range LANInterfaces() {
		selected[ifc.Index] = true
	}
	out := make([]InterfaceStatus, 0, len(ifaces))
	for _, ifc := range ifaces {
		st := InterfaceStatus{
			Index:          ifc.Index,
			Name:           ifc.Name,
			Flags:          flagNames(ifc),
			MulticastV4Err: probeMulticastIPv4(&ifc),
			MulticastV6Err: probeMulticastIPv6(&ifc),
			Selected:       selected[ifc.Index],
		}
		if addrs, err := ifc.Addrs(); err == nil {
			for _, a := range addrs {
				ipnet, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				if v4 := ipnet.IP.To4(); v4 != nil {
					st.IPv4 = append(st.IPv4, v4.String())
					if usableMDNSIP(v4.String()) {
						st.HasRoutableIPv4 = true
					}
					continue
				}
				st.IPv6 = append(st.IPv6, ipnet.IP.String())
			}
		}
		out = append(out, st)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out
}

// flagNames 把网卡标志位转成短名列表。
//
// 参数:
//   - ifc: net.Interface — 待转换网卡
//
// 返回:
//   - []string — 例如 ["up","mcast","loop"]
func flagNames(ifc net.Interface) []string {
	out := make([]string, 0, 4)
	if ifc.Flags&net.FlagUp != 0 {
		out = append(out, "up")
	}
	if ifc.Flags&net.FlagMulticast != 0 {
		out = append(out, "mcast")
	}
	if ifc.Flags&net.FlagLoopback != 0 {
		out = append(out, "loop")
	}
	if ifc.Flags&net.FlagPointToPoint != 0 {
		out = append(out, "p2p")
	}
	return out
}

// hasIPv6Multicast 判断给定网卡是否都能正常设置 IPv6 组播出口。
//
// 用来决定浏览侧是否关掉 IPv6：zeroconf 的 IPv6 发送循环没有开关，网卡
// 不支持时会对每个报文打印一次 WARN。FST 的发现只依赖 A 记录 + TXT，
// 关掉 IPv6 不影响功能。
//
// 参数:
//   - ifaces: []net.Interface — 参与组播的网卡
//
// 返回:
//   - bool — true 表示全部网卡的 IPv6 组播出口都可用
func hasIPv6Multicast(ifaces []net.Interface) bool {
	if len(ifaces) == 0 {
		return true
	}
	for i := range ifaces {
		if err := probeMulticastIPv6(&ifaces[i]); err != nil {
			return false
		}
	}
	return true
}

// probeMulticastIPv4 试探在指定网卡上设置 IPv4 组播出口是否被系统接受。
//
// 参数:
//   - ifc: *net.Interface — 目标网卡
//
// 返回:
//   - error — nil 表示可以正常设置
func probeMulticastIPv4(ifc *net.Interface) error {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return err
	}
	defer c.Close()
	return ipv4.NewPacketConn(c).SetMulticastInterface(ifc)
}

// probeMulticastIPv6 试探在指定网卡上设置 IPv6 组播出口是否被系统接受。
//
// 参数:
//   - ifc: *net.Interface — 目标网卡
//
// 返回:
//   - error — nil 表示可以正常设置
func probeMulticastIPv6(ifc *net.Interface) error {
	c, err := net.ListenUDP("udp6", &net.UDPAddr{})
	if err != nil {
		return err
	}
	defer c.Close()
	return ipv6.NewPacketConn(c).SetMulticastInterface(ifc)
}

// hasRoutableIPv4 判断网卡上是否有非回环、非链路本地的 IPv4。
//
// 参数:
//   - ifc: *net.Interface — 待判断网卡
//
// 返回:
//   - bool — true 表示该网卡有一个可用的局域网 IPv4
func hasRoutableIPv4(ifc *net.Interface) bool {
	addrs, err := ifc.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if usableMDNSIP(ipnet.IP.String()) {
			return true
		}
	}
	return false
}

// ipSetOf 汇总网卡上的 IP 字面量，用于过滤广播地址。
//
// 参数:
//   - ifaces: []net.Interface — 参与组播的网卡；空表示不限制
//
// 返回:
//   - map[string]bool — IP 集合；ifaces 为空时返回 nil（表示不限制）
func ipSetOf(ifaces []net.Interface) map[string]bool {
	if len(ifaces) == 0 {
		return nil
	}
	out := make(map[string]bool, len(ifaces)*2)
	for i := range ifaces {
		addrs, err := ifaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			// 同一块网卡上可能同时挂着 169.254.x（DHCP 失败时的自动地址），
			// 这类地址写进 A 记录等于让对端拨一个不存在的地址。
			if !usableMDNSIP(ipnet.IP.String()) {
				continue
			}
			out[ipnet.IP.String()] = true
		}
	}
	return out
}

// firstIP 取 multiaddr 的首个 IP 字面量。
//
// 参数:
//   - addr: ma.Multiaddr — 形如 /ip4/192.168.1.5/tcp/8787
//
// 返回:
//   - string — IP 字面量；无法解析时返回空串
func firstIP(addr ma.Multiaddr) string {
	first, _ := ma.SplitFirst(addr)
	if first == nil {
		return ""
	}
	if first.Protocol().Code != ma.P_IP4 && first.Protocol().Code != ma.P_IP6 {
		return ""
	}
	return first.Value()
}

// suitable 判断一个 multiaddr 是否适合放进 mDNS TXT。
// 与 libp2p 的规则一致：必须是 /ip4 或 /ip6，且不含中继/浏览器专用传输。
// 另外剔除回环与未指定地址：把它们广播出去，对端只会连到自己。
//
// 参数:
//   - addr: ma.Multiaddr — 待判断地址
//
// 返回:
//   - bool — true 表示可广播
func suitable(addr ma.Multiaddr) bool {
	if addr == nil {
		return false
	}
	first, _ := ma.SplitFirst(addr)
	if first == nil {
		return false
	}
	switch first.Protocol().Code {
	case ma.P_IP4, ma.P_IP6:
		if !usableMDNSIP(first.Value()) {
			return false
		}
	default:
		return false
	}
	bad := false
	ma.ForEach(addr, func(c ma.Component) bool {
		switch c.Protocol().Code {
		case ma.P_CIRCUIT, ma.P_WEBTRANSPORT, ma.P_WEBRTC, ma.P_WEBRTC_DIRECT,
			ma.P_P2P_WEBRTC_DIRECT, ma.P_WS, ma.P_WSS:
			bad = true
			return false
		}
		return true
	})
	return !bad
}

// pickIPs 从 multiaddr 列表里取第一个可用的 IPv4 与 IPv6 字面量。
//
// 必须跳过回环、未指定地址（0.0.0.0/::）与链路本地地址：libp2p 监听
// 0.0.0.0 时会把回环也展开进地址列表，若直接取第一个 IPv4，A 记录就会写成
// 127.0.0.1，对端拿到的地址是它自己的回环，连接必然失败（移动端尤其常见）。
//
// 参数:
//   - addrs: []ma.Multiaddr — 节点地址
//
// 返回:
//   - []string — IPv4 在前；都没有时为空
func pickIPs(addrs []ma.Multiaddr) []string {
	var v4, v6 string
	for _, addr := range addrs {
		first, _ := ma.SplitFirst(addr)
		if first == nil {
			continue
		}
		switch first.Protocol().Code {
		case ma.P_IP4:
			if v4 == "" && usableMDNSIP(first.Value()) {
				v4 = first.Value()
			}
		case ma.P_IP6:
			if v6 == "" && usableMDNSIP(first.Value()) {
				v6 = first.Value()
			}
		}
	}
	out := make([]string, 0, 2)
	if v4 != "" {
		out = append(out, v4)
	}
	if v6 != "" {
		out = append(out, v6)
	}
	return out
}

// usableMDNSIP 判断一个 IP 字面量是否适合写进 A/AAAA 记录。
//
// 参数:
//   - s: string — IP 字面量（如 "192.168.1.13"）
//
// 返回:
//   - bool — true 表示全局可单播且非回环，可作为对端连接目标
func usableMDNSIP(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return false
	}
	return true
}
