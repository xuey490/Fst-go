package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"

	"github.com/xjasonlyu/tun2socks/v2/core"
	tundevice "github.com/xjasonlyu/tun2socks/v2/core/device/tun"
	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	t2proxy "github.com/xjasonlyu/tun2socks/v2/proxy"
	"github.com/xjasonlyu/tun2socks/v2/tunnel"
	"github.com/xjasonlyu/tun2socks/v2/tunnel/statistic"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

const (
	tunName = "FST-TUN"
	// 1380 leaves headroom for the tunnel (libp2p stream + security + framing)
	// wrapping each IP packet, avoiding mid-path fragmentation black holes.
	tunMTU = 1380
)

type TUNConfig struct {
	Mode     string // split | global
	Routes   []string
	Domains  []string
	ExitIPv4 string // must bypass the TUN in global mode
	// IPv6 打开 IPv6 栈（接口地址 fdfe:1::1/64、fake AAAA、v6 路由）；默认关闭。
	IPv6 bool
	// ExitIPv6 是出口中继的 IPv6 地址（用于全局模式绕行）；出口没有 AAAA 时为空。
	ExitIPv6 string
}

type routePlan struct {
	mode      string
	prefixes  []netip.Prefix
	exit      net.IP
	hijackDNS bool
	// ipv6/prefixes6/exit6 仅在 TUNConfig.IPv6 为 true 时有效。
	ipv6      bool
	prefixes6 []netip.Prefix
	exit6     netip.Addr
}

// TUN owns the virtual interface, netstack and the exact routes it installed.
type TUN struct {
	device interface{ Close() }
	stack  *stack.Stack
	tunnel *tunnel.Tunnel
	routes routeManager
	once   sync.Once
}

func StartTUN(client *Client, cfg TUNConfig) (*TUN, error) {
	if client == nil {
		return nil, errors.New("代理客户端为空")
	}
	prefixes4, prefixes6, exit6, err := validateTUNConfig(cfg)
	if err != nil {
		return nil, err
	}
	dev, err := tundevice.Open(tunName, tunMTU)
	if err != nil {
		return nil, fmt.Errorf("创建 TUN 失败（需要管理员/root 权限；Windows 还需 wintun.dll）: %w", err)
	}
	names := newNameTable(cfg.Domains, cfg.Mode == "global")
	names.ipv6 = cfg.IPv6
	// Global mode hijacks all DNS: every A query gets a fake-IP and the real
	// resolving happens on the exit, so a polluted local resolver can't break us.
	// Split mode only hijacks when domain rules exist (other names recurse via
	// the bypassed upstream resolver).
	plan := routePlan{
		mode: cfg.Mode, prefixes: prefixes4, exit: net.ParseIP(cfg.ExitIPv4),
		hijackDNS: cfg.Mode == "global" || (cfg.Mode == "split" && len(names.rules) > 0),
		ipv6:      cfg.IPv6, prefixes6: prefixes6, exit6: exit6,
	}
	// 分流+域名：DNS 服务器的 /32 会被 tunRoutePrefixes 装进 TUN，于是本地上游解析器也会环回，
	// 未命中规则的域名必须改由出口解析；同时任意 :53 都交给 fakeDNS，真实应答不再与 fake-IP 竞争。
	names.hijackAny = plan.hijackDNS
	if cfg.Mode == "split" && plan.hijackDNS {
		names.resolveExit = newExitResolver(client)
	}
	rm := newRouteManager(dev.Name())
	if err := rm.Setup(plan); err != nil {
		dev.Close()
		return nil, err
	}
	d := &tunDialer{client: client, names: names}
	tr := tunnel.New(d, statistic.DefaultManager)
	tr.ProcessAsync()
	ns, err := core.CreateStack(&core.Config{LinkEndpoint: dev, TransportHandler: tr})
	if err != nil {
		tr.Close()
		rm.Close()
		dev.Close()
		return nil, fmt.Errorf("创建用户态网络栈失败: %w", err)
	}
	return &TUN{device: dev, stack: ns, tunnel: tr, routes: rm}, nil
}

func (t *TUN) Close() error {
	var err error
	t.once.Do(func() {
		if t.stack != nil {
			t.stack.Close()
			t.stack.Wait()
		}
		if t.tunnel != nil {
			t.tunnel.Close()
		}
		if t.routes != nil {
			err = t.routes.Close()
		}
		if t.device != nil {
			t.device.Close()
		}
	})
	return err
}

// validateTUNConfig 校验模式/出口/网段并拆分 IPv4 与 IPv6 前缀。
//
// 参数:
//   - cfg: TUNConfig — 模式、用户网段、域名规则、出口地址与 IPv6 开关
//
// 返回:
//   - []netip.Prefix — IPv4 用户网段（已 Masked）
//   - []netip.Prefix — IPv6 用户网段；未开启 IPv6 时为空
//   - netip.Addr — 出口中继 IPv6（未配置时为无效地址）
//   - error — 模式非法、出口缺失、CIDR 非法/包含出口（环路）或分流没有任何规则
func validateTUNConfig(cfg TUNConfig) ([]netip.Prefix, []netip.Prefix, netip.Addr, error) {
	if cfg.Mode != "split" && cfg.Mode != "global" {
		return nil, nil, netip.Addr{}, errors.New("代理模式必须是 split 或 global")
	}
	exit, err := netip.ParseAddr(cfg.ExitIPv4)
	if err != nil || !exit.Is4() {
		return nil, nil, netip.Addr{}, errors.New("代理出口必须包含 IPv4 地址")
	}
	exit6 := netip.Addr{}
	if cfg.IPv6 {
		if a, perr := netip.ParseAddr(cfg.ExitIPv6); perr == nil && a.Is6() && !a.Is4In6() {
			exit6 = a
		}
	}
	var out4, out6 []netip.Prefix
	for _, text := range cfg.Routes {
		p, err := netip.ParsePrefix(text)
		if err != nil {
			return nil, nil, netip.Addr{}, fmt.Errorf("无效的 CIDR: %s", text)
		}
		p = p.Masked()
		if p.Addr().Is4() {
			if p.Contains(exit) {
				return nil, nil, netip.Addr{}, fmt.Errorf("代理网段 %s 包含出口中继，启用后会形成路由环路", p)
			}
			out4 = append(out4, p)
			continue
		}
		if !cfg.IPv6 {
			return nil, nil, netip.Addr{}, fmt.Errorf("IPv6 CIDR 需要先在设置中开启代理 IPv6: %s", text)
		}
		if exit6.IsValid() && p.Contains(exit6) {
			return nil, nil, netip.Addr{}, fmt.Errorf("代理网段 %s 包含出口中继，启用后会形成路由环路", p)
		}
		out6 = append(out6, p)
	}
	var domains int
	for _, d := range cfg.Domains {
		if normalizeDomain(d) != "" {
			domains++
		}
	}
	if cfg.Mode == "split" && len(out4)+len(out6) == 0 && domains == 0 {
		return nil, nil, netip.Addr{}, errors.New("分流模式至少需要一个 CIDR 或域名")
	}
	return out4, out6, exit6, nil
}

type tunDialer struct {
	client *Client
	names  *nameTable
}

var _ t2proxy.Dialer = (*tunDialer)(nil)

// dest 把 netstack 的目标地址还原成「域名:端口」或原始 IP:端口。
//
// 参数:
//   - m: *M.Metadata — tun2socks 的连接元数据
//
// 返回:
//   - string — "host:port"；fake-IP 命中时 host 为域名
//   - error — fake 段内的未知地址（客户端缓存过期）
func (d *tunDialer) dest(m *M.Metadata) (string, error) {
	if d.names != nil {
		if host, ok := d.names.lookup(m.DstIP); ok {
			return net.JoinHostPort(host, strconv.Itoa(int(m.DstPort))), nil
		}
	}
	fake4 := mustPrefix(fakeNet).Contains(m.DstIP)
	fake6 := m.DstIP.Is6() && mustPrefix(fakePrefix6).Contains(m.DstIP)
	if fake4 || fake6 {
		// Stale client cache (e.g. from before a restart) or a raced lookup;
		// the /15 broadcast (NetBIOS noise) is expected and stays quiet.
		if m.DstIP.String() != "198.19.255.255" && m.DstIP.String() != fakeDNS6 {
			slog.Warn("tun unknown fake-IP; client cache is stale", "ip", m.DstIP.String(), "port", m.DstPort)
		}
		return "", errors.New("未知的 fake-IP")
	}
	return m.DestinationAddress(), nil
}

func (d *tunDialer) DialContext(ctx context.Context, m *M.Metadata) (net.Conn, error) {
	if m == nil || m.Network != M.TCP {
		return nil, errors.New("仅支持 TCP")
	}
	addr, err := d.dest(m)
	if err != nil {
		return nil, err
	}
	network := "tcp4"
	if !m.DstIP.Is4() {
		network = "tcp6"
	}
	conn, err := d.client.DialContext(ctx, network, addr)
	if err != nil {
		slog.Info("tun tcp dial failed", "target", addr, "err", err)
	}
	return conn, err
}

func (d *tunDialer) DialUDP(m *M.Metadata) (net.PacketConn, error) {
	if m == nil {
		return nil, errors.New("缺少 UDP 元数据")
	}
	// Global mode hijacks EVERY port-53 datagram into the local fakeDNS, no
	// matter which resolver it was addressed to: forwarded real resolvers would
	// answer with real AAAA/A records that race (and lose) against fake-IPs,
	// leaving clients with half-real addresses (Happy Eyeballs prefers the
	// unroutable IPv6). Split mode hijacks port 53 too once domain rules exist
	// (hijackAny), because the physical NIC's resolvers are routed into the TUN
	// and would otherwise return real A records for rule domains.
	if d.names.hijack(m.DstIP, m.DstPort) {
		return d.names.dnsConn(m.UDPAddr()), nil
	}
	addr, err := d.dest(m)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	pc, err := d.client.DialUDP(ctx, addr)
	if err != nil {
		slog.Info("tun udp dial failed", "target", addr, "err", err)
	}
	return pc, err
}

type routeManager interface {
	Setup(routePlan) error
	Close() error
}
