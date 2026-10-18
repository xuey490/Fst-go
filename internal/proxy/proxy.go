// Package proxy carries outbound TCP/UDP over libp2p streams.
// It is independent from the TUN frontend: tests and future SOCKS clients can
// use the same Client directly.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"fst/internal/proto"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	// One slot per proxied connection for its whole lifetime. A single browser
	// page can hold well over a hundred, and several terminals share the exit.
	maxExitCircuits = 1024
	dialTimeout     = 10 * time.Second
)

// ExitOptions 是出口鉴权的三层开关，任一为 nil/空即不参与校验（旧行为）。
type ExitOptions struct {
	// Allow 是 PeerID 白名单（中继 -exit-allow）；nil 表示不校验。
	Allow func(peer.ID) bool
	// Token 校验终端携带的出口令牌（中继 -exit-token）；nil 表示不校验。
	Token func(string) bool
	// GroupOf 返回终端分组名（由中继的组表决定）；nil 表示不区分分组。
	GroupOf func(peer.ID) string
	// ExitGroup 非空时只有该分组的终端可用出口；同组终端免令牌。
	ExitGroup string
	// SlotsPerGroup 是每组并发上限，0 表示只用全局槽位。
	SlotsPerGroup int
}

// Exit installs the proxy protocol on a dedicated fst-relay.
type Exit struct {
	h    host.Host
	opt  ExitOptions
	slots chan struct{}
	// groupMu/groupSlots: 每组独立并发槽位（按需创建，释放后删除空表）。
	groupMu    sync.Mutex
	groupSlots map[string]chan struct{}
	// dial/listenPacket 可注入，测试不碰真实网络。
	dial         func(context.Context, string, string) (net.Conn, error)
	listenPacket func(network string) (net.PacketConn, error)
}

// ServeExit 在宿主上安装代理出口协议并返回句柄（Close 时摘除）。
//
// 参数:
//   - h: host.Host — 中继的 libp2p 宿主（监听 8787，不另开端口）
//   - opt: ExitOptions — 白名单/令牌/分组与按组槽位；零值等于全部放行（旧行为）
//
// 返回:
//   - *Exit — 出口句柄；Close 摘除 stream handler
func ServeExit(h host.Host, opt ExitOptions) *Exit {
	x := &Exit{
		h: h, opt: opt, slots: make(chan struct{}, maxExitCircuits),
		groupSlots: map[string]chan struct{}{},
		dial:       (&net.Dialer{}).DialContext,
		listenPacket: func(network string) (net.PacketConn, error) {
			return net.ListenPacket(network, "")
		},
	}
	if opt.ExitGroup != "" && opt.GroupOf == nil {
		// fail-closed：配置了出口分组却没有分组来源时，没人能通过校验。
		slog.Error("proxy exit group configured without a resolver; all sessions will be rejected", "group", opt.ExitGroup)
	}
	h.SetStreamHandler(proto.ProxyProtocol, x.handle)
	return x
}

// Close 摘除出口的 stream handler（已建立的连接不受影响）。
func (x *Exit) Close() { x.h.RemoveStreamHandler(proto.ProxyProtocol) }

// exitUnavailable 把「对端没有注册出口协议」翻译成可操作的中文提示。
// 中继没有以 -exit 启动时不会安装 /fst/proxy/1.0.0，libp2p 只会报 protocols not supported，
// 直接抛给用户很难看出是服务端漏参数。
//
// 参数:
//   - err: error — NewStream 返回的原始错误
//
// 返回:
//   - error — 命中协议缺失时返回带 -exit 提示的错误；其它错误原样包装
func exitUnavailable(err error) error {
	if strings.Contains(err.Error(), "protocols not supported") {
		return errors.New("出口中继未开启代理：服务端 fst-relay 需要带 -exit 启动，并在 -exit-allow 中列入本机 PeerID")
	}
	return fmt.Errorf("打开代理流失败: %w", err)
}

// groupExempt 判断该终端是否因同组而免令牌。
//
// 参数:
//   - group: string — 终端分组名（空表示未分组）
//
// 返回:
//   - bool — 配置了出口分组且终端属于该组时为 true
func (x *Exit) groupExempt(group string) bool {
	return x.opt.ExitGroup != "" && group == x.opt.ExitGroup
}

// acquireGroup 尝试占用一个分组槽位；0 表示不限组。
//
// 参数:
//   - group: string — 分组名
//
// 返回:
//   - func() — 释放函数；获取失败时为 nil
//   - bool — 是否获取成功
func (x *Exit) acquireGroup(group string) (func(), bool) {
	if x.opt.SlotsPerGroup <= 0 || group == "" {
		return func() {}, true
	}
	x.groupMu.Lock()
	ch := x.groupSlots[group]
	if ch == nil {
		ch = make(chan struct{}, x.opt.SlotsPerGroup)
		x.groupSlots[group] = ch
	}
	x.groupMu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() {
			<-ch
			x.groupMu.Lock()
			if len(ch) == 0 && x.groupSlots[group] == ch {
				delete(x.groupSlots, group)
			}
			x.groupMu.Unlock()
		}, true
	default:
		return nil, false
	}
}

func (x *Exit) handle(s network.Stream) {
	from := s.Conn().RemotePeer()
	if x.opt.Allow != nil && !x.opt.Allow(from) {
		// Tell the client why, instead of a bare reset (older clients just read
		// the frame and surface the reason the same way as other failures).
		slog.Warn("proxy exit denied peer", "peer", from, "reason", "allowlist")
		writeProxyError(s, "本机未列入出口白名单（中继 -exit-allow）")
		return
	}
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(dialTimeout))

	var req proto.ProxyReq
	if err := proto.ReadFrame(s, &req); err != nil {
		_ = s.Reset()
		return
	}
	group := ""
	if x.opt.GroupOf != nil {
		group = x.opt.GroupOf(from)
	}
	if x.opt.ExitGroup != "" && group != x.opt.ExitGroup {
		slog.Warn("proxy exit denied peer", "peer", from, "reason", "group", "group", group)
		writeProxyError(s, "本中继出口仅对指定分组开放")
		return
	}
	if x.opt.Token != nil && !x.groupExempt(group) && !x.opt.Token(req.Token) {
		slog.Warn("proxy exit denied peer", "peer", from, "reason", "token")
		writeProxyError(s, "出口令牌无效或已过期")
		return
	}
	select {
	case x.slots <- struct{}{}:
		defer func() { <-x.slots }()
	default:
		slog.Warn("proxy exit busy")
		writeProxyError(s, "出口繁忙，请稍后重试")
		return
	}
	release, ok := x.acquireGroup(group)
	if !ok {
		slog.Warn("proxy exit group busy", "group", group)
		writeProxyError(s, "该分组出口繁忙，请稍后重试")
		return
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	host, port, err := resolveTarget(ctx, req)
	if err != nil {
		slog.Info("proxy exit target rejected", "peer", from, "req", req.Net+" "+req.Addr, "err", err)
		writeProxyError(s, err.Error())
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(port))
	ip := net.ParseIP(host)
	udpNet := "udp4"
	tcpNet := "tcp4"
	if ip == nil || ip.To4() == nil {
		udpNet, tcpNet = "udp6", "tcp6"
	}
	if req.Net == "udp" {
		ua, err := net.ResolveUDPAddr(udpNet, target)
		if err != nil {
			slog.Warn("proxy exit udp resolve failed", "target", target, "err", err)
			writeProxyError(s, "出口解析 UDP 目标失败")
			return
		}
		pc, err := x.listenPacket(udpNet)
		if err != nil {
			slog.Warn("proxy exit udp listen failed", "err", err)
			writeProxyError(s, "出口连接目标失败")
			return
		}
		defer pc.Close()
		if err := proto.WriteFrame(s, proto.ProxyResp{OK: true}); err != nil {
			return
		}
		_ = s.SetDeadline(time.Time{})
		pipeUDP(s, pc, ua)
		return
	}
	dst, err := x.dial(ctx, tcpNet, target)
	if err != nil {
		slog.Warn("proxy exit dial failed", "peer", from, "target", target, "err", err)
		writeProxyError(s, "出口连接目标失败")
		return
	}
	defer dst.Close()
	if err := proto.WriteFrame(s, proto.ProxyResp{OK: true}); err != nil {
		return
	}
	_ = s.SetDeadline(time.Time{})
	pipe(s, dst)
}

func writeProxyError(s network.Stream, reason string) {
	_ = proto.WriteFrame(s, proto.ProxyResp{Error: reason})
}

// forbiddenIP 判断出口目标是否落在禁止范围（环回/私网/链路本地/ULA/组播/未指定）。
//
// 参数:
//   - ip: net.IP — 已解析的目标地址（IPv4 或 IPv6）
//
// 返回:
//   - bool — 禁止代理时为 true；IPv4-mapped IPv6 会先归一成 IPv4 再判断
func forbiddenIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// targetFamily 归一化请求里的地址族。
//
// 参数:
//   - req: proto.ProxyReq — Family 可空或 "ip4"/"ip6"
//
// 返回:
//   - string — 空串表示双栈优先，否则 "ip4"/"ip6"
//   - error — Family 取值非法时返回
func targetFamily(req proto.ProxyReq) (string, error) {
	switch f := strings.ToLower(strings.TrimSpace(req.Family)); f {
	case "", "4", "ip4":
		if f == "" {
			return "", nil
		}
		return "ip4", nil
	case "6", "ip6":
		return "ip6", nil
	default:
		return "", errors.New("目标地址族无效")
	}
}

// familyMatch 判断地址是否满足请求的地址族。
//
// 参数:
//   - ip: net.IP — 目标地址
//   - family: string — "" / "ip4" / "ip6"
//
// 返回:
//   - bool — 满足时为 true
func familyMatch(ip net.IP, family string) bool {
	switch family {
	case "ip4":
		return ip.To4() != nil
	case "ip6":
		return ip.To4() == nil
	default:
		return true
	}
}

// resolveTarget 校验请求并在必要时由出口解析域名（DNS 不出中继）。
//
// 参数:
//   - ctx: context.Context — 解析超时/取消
//   - req: proto.ProxyReq — Net/Addr/Port/Family；Addr 可为 IPv4/IPv6 字面量或域名
//
// 返回:
//   - string — 目标 IP（IPv6 为不带方括号的字面量）
//   - int — 目标端口
//   - error — 参数非法、域名解析失败或解析结果全部落在禁止范围
//
// 说明：域名可能同时有 A/AAAA，逐个检查并返回第一个可用地址；私网结果不会被采纳。
func resolveTarget(ctx context.Context, req proto.ProxyReq) (string, int, error) {
	if req.Net != "tcp" && req.Net != "udp" {
		return "", 0, errors.New("仅支持 TCP 或 UDP")
	}
	if req.Port < 1 || req.Port > 65535 {
		return "", 0, errors.New("目标端口无效")
	}
	family, err := targetFamily(req)
	if err != nil {
		return "", 0, err
	}
	if ip := net.ParseIP(strings.TrimSpace(req.Addr)); ip != nil {
		if !familyMatch(ip, family) {
			return "", 0, errors.New("目标地址族与请求不一致")
		}
		if forbiddenIP(ip) {
			return "", 0, errors.New("不允许代理到本机、私网或链路本地地址")
		}
		return ip.String(), req.Port, nil
	}
	if !looksLikeHost(req.Addr) {
		return "", 0, errors.New("目标地址无效")
	}
	network := "ip"
	switch family {
	case "ip4":
		network = "ip4"
	case "ip6":
		network = "ip6"
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, network, req.Addr)
	if err != nil || len(ips) == 0 {
		return "", 0, errors.New("出口无法解析该域名")
	}
	sawForbidden := false
	for _, ip := range ips {
		if !familyMatch(ip, family) {
			continue
		}
		if forbiddenIP(ip) {
			sawForbidden = true
			continue
		}
		return ip.String(), req.Port, nil
	}
	if sawForbidden {
		return "", 0, errors.New("不允许代理到本机、私网或链路本地地址")
	}
	return "", 0, errors.New("出口无法解析该域名")
}

// validatedTarget is the pre-resolve check used by tests.
func validatedTarget(req proto.ProxyReq) (string, error) {
	if req.Net != "tcp" && req.Net != "udp" {
		return "", errors.New("仅支持 TCP 或 UDP")
	}
	if req.Port < 1 || req.Port > 65535 {
		return "", errors.New("目标端口无效")
	}
	family, err := targetFamily(req)
	if err != nil {
		return "", err
	}
	if ip := net.ParseIP(strings.TrimSpace(req.Addr)); ip != nil {
		if !familyMatch(ip, family) {
			return "", errors.New("目标地址族与请求不一致")
		}
		if forbiddenIP(ip) {
			return "", errors.New("不允许代理到本机、私网或链路本地地址")
		}
		return net.JoinHostPort(ip.String(), strconv.Itoa(req.Port)), nil
	}
	if !looksLikeHost(req.Addr) {
		return "", errors.New("目标地址无效")
	}
	return net.JoinHostPort(req.Addr, strconv.Itoa(req.Port)), nil
}

func pipe(stream network.Stream, dst net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(dst, stream)
		if tcp, ok := dst.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(stream, dst)
		_ = stream.CloseWrite()
	}()
	wg.Wait()
}

// Client opens one libp2p stream per TCP connection or UDP session. All streams
// share the existing encrypted connection to the relay (and therefore its port 8787).
type Client struct {
	h     host.Host
	exit  peer.AddrInfo
	token string // 出口令牌；空串表示不携带（旧中继/未启用令牌）
}

// SetToken 设置出口令牌，之后打开的每条代理流都会携带（运行中改配置需重建 Client）。
//
// 参数:
//   - token: string — 中继 -exit-token 配置的令牌；空串表示清除
//
// 返回:
//   - 无
func (c *Client) SetToken(token string) { c.token = strings.TrimSpace(token) }

func NewClient(h host.Host, exitAddr string) (*Client, error) {
	a, err := ma.NewMultiaddr(exitAddr)
	if err != nil {
		return nil, fmt.Errorf("代理出口地址无效: %w", err)
	}
	ai, err := peer.AddrInfoFromP2pAddr(a)
	if err != nil {
		return nil, errors.New("代理出口地址缺少 /p2p/<PeerID>")
	}
	if ai.ID == h.ID() {
		return nil, errors.New("不能把本机作为代理出口")
	}
	return &Client{h: h, exit: *ai}, nil
}

func (c *Client) ExitPeer() peer.ID { return c.exit.ID }

func (c *Client) ExitIPv4() string {
	for _, a := range c.exit.Addrs {
		if s, err := a.ValueForProtocol(ma.P_IP4); err == nil {
			return s
		}
	}
	return ""
}

// open 打开一条代理流并完成请求/应答握手。
//
// 参数:
//   - ctx: context.Context — 拨号与握手超时
//   - networkName: string — tcp/tcp4/tcp6 或 udp/udp4/udp6；带 6 表示要求 IPv6 解析
//   - address: string — "host:port"；host 可为域名或 IPv4/IPv6 字面量
//
// 返回:
//   - network.Stream — 已获出口确认的流（TCP 已连接、UDP 已建立会话）
//   - error — 参数非法、连接中继失败或出口拒绝（含令牌/分组/白名单原因）
func (c *Client) open(ctx context.Context, networkName, address string) (network.Stream, error) {
	kind := "tcp"
	switch networkName {
	case "tcp", "tcp4", "tcp6":
		kind = "tcp"
	case "udp", "udp4", "udp6":
		kind = "udp"
	default:
		return nil, errors.New("仅支持 TCP 或 UDP")
	}
	hostName, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, errors.New("目标端口无效")
	}
	// 字面量地址直接按地址族；域名则由调用方通过 tcp6/udp6 表达「只要 AAAA」。
	family := ""
	if ip := net.ParseIP(hostName); ip != nil {
		if ip.To4() == nil {
			family = "ip6"
		} else {
			family = "ip4"
		}
	} else if strings.HasSuffix(networkName, "6") {
		family = "ip6"
	} else if strings.HasSuffix(networkName, "4") {
		family = "ip4"
	}
	if c.h.Network().Connectedness(c.exit.ID) != network.Connected {
		if err := c.h.Connect(ctx, c.exit); err != nil {
			return nil, fmt.Errorf("连接代理出口失败: %w", err)
		}
	}
	s, err := c.h.NewStream(network.WithAllowLimitedConn(ctx, "fst-proxy"), c.exit.ID, proto.ProxyProtocol)
	if err != nil {
		return nil, exitUnavailable(err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = s.SetDeadline(deadline)
	}
	if err := proto.WriteFrame(s, proto.ProxyReq{Net: kind, Addr: hostName, Port: port, Family: family, Token: c.token}); err != nil {
		_ = s.Reset()
		return nil, err
	}
	var resp proto.ProxyResp
	if err := proto.ReadFrame(s, &resp); err != nil {
		_ = s.Reset()
		return nil, err
	}
	if !resp.OK {
		_ = s.Reset()
		return nil, errors.New(resp.Error)
	}
	_ = s.SetDeadline(time.Time{})
	return s, nil
}

func (c *Client) DialContext(ctx context.Context, networkName, address string) (net.Conn, error) {
	s, err := c.open(ctx, networkName, address)
	if err != nil {
		return nil, err
	}
	return &streamConn{Stream: s}, nil
}

func (c *Client) DialUDP(ctx context.Context, address string) (net.PacketConn, error) {
	s, err := c.open(ctx, "udp", address)
	if err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(address)
	return &streamPacketConn{
		ReadWriteCloser: s,
		remote:          &net.UDPAddr{IP: net.ParseIP(host), Port: 0},
		local:           p2pAddr("udp/" + c.exit.ID.String()),
	}, nil
}

type streamConn struct{ network.Stream }

func (c *streamConn) LocalAddr() net.Addr  { return multiaddrNetAddr(c.Conn().LocalMultiaddr()) }
func (c *streamConn) RemoteAddr() net.Addr { return multiaddrNetAddr(c.Conn().RemoteMultiaddr()) }

type p2pAddr string

func (a p2pAddr) Network() string { return "libp2p" }
func (a p2pAddr) String() string  { return string(a) }

func multiaddrNetAddr(a ma.Multiaddr) net.Addr { return p2pAddr(a.String()) }
