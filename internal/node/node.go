// Package node owns the libp2p host: identity, LAN discovery, connection/presence tracking
// and the request/response dispatcher for /fst/msg.
package node

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fst/internal/proto"
	"fst/internal/store"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	"github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

type Config struct {
	DataDir     string
	Port        int // 0 = random free port
	Store       *store.Store
	Emit        func(name string, data any)
	DisableMDNS bool // tests
	// WAN (enabled by the advanced networking gate): advertise non-LAN addresses and enable AutoNATv2,
	// circuit-relay client/service and DCUtR hole punching. Needs a reachable relay (Relays) when both
	// sides are behind NAT.
	WAN bool
	// Relays are static relay multiaddrs (with /p2p/<id>) for AutoRelay; there are no built-in public relays.
	Relays []string
	// ForcePublic skips AutoNAT and declares this node publicly reachable, which is what starts the relay
	// service at once. For servers known to have a public IP (cmd/fst-relay); needs WAN.
	ForcePublic bool
	// Directory makes this node (a relay) answer HELLO with the PeerIDs of the other FST peers connected to it,
	// so terminals that share the relay find each other without any manual adding. Only cmd/fst-relay sets it.
	Directory bool
	// Group is the relay-group key of this node. A terminal sends it only to configured relays, which then
	// hand out a directory (and arrival notices) restricted to terminals of the same group; a relay serves
	// only terminals that present its own key. Empty keeps the pre-group behaviour (everyone sees everyone).
	Group string
	// Groups is the relay-side group table (name/key/expiry). When set it supersedes Group on a relay;
	// terminals still only carry a single key. Empty keeps the legacy single-group behaviour.
	Groups []GroupSpec
	// GroupMaxTerminals is a soft cap per group on a relay: over it, no more directory entries or arrival
	// notices are sent for that group (connections are never dropped). 0 = unlimited.
	GroupMaxTerminals int
	// GroupFile is the relay-side group table path; when set the relay polls it and hot-swaps the table
	// without restarting. Only used with Directory=true.
	GroupFile string
	// GroupReload is the poll interval for GroupFile; <=0 means 10s, values below 2s are raised to 2s.
	GroupReload time.Duration
}

// GroupSpec 是中继组表的一行。
type GroupSpec struct {
	Name      string // 组名（终端不可见，用于目录隔离与出口分组）
	Key       string // 终端出示的密钥
	ExpiresAt int64  // unix ms；0 表示永不过期
}

// Handler answers one inbound /fst/msg request. Returning nil replies with nothing.
type Handler func(ctx context.Context, from peer.ID, in *proto.Msg) *proto.Msg

// RemoteError 表示对端明确回了 ERROR（例如协议错误/拒绝），重试不会改变结果。
type RemoteError struct{ Reason string }

// Error 实现 error，返回对端给出的原因。
//
// 返回:
//   - string — 中文/英文原因
func (e *RemoteError) Error() string { return e.Reason }

type Node struct {
	Host  host.Host
	Store *store.Store

	emit      func(string, any)
	wan       bool
	directory bool                      // relay: hand out the online-peer directory in HELLO replies
	group     string                    // my relay-group key (terminal: sent to relays; relay: the group it serves)
	relays    map[peer.ID]peer.AddrInfo // configured relays (WAN only): the only senders of a directory we trust
	dht       *dht.IpfsDHT              // nil unless WAN: PeerID -> addresses lookup
	reach     string                    // AutoNATv2 verdict: unknown | public | private
	closed    atomic.Bool               // set first in Close: shutdown-time disconnects must not emit
	mdns      mdns.Service
	dialSlots chan struct{} // cap 8: directory / mDNS dials share this so a join cannot open 200 circuits at once
	mu        sync.Mutex
	online    map[peer.ID]bool
	groups    map[peer.ID]string             // relay: group name each online terminal presented in HELLO
	groupErr  map[peer.ID]string             // relay: why the last HELLO key was rejected
	groupSpec map[string]GroupSpec           // relay: key -> spec（含过期时间）
	byName    map[string]GroupSpec           // relay: name -> spec（查过期用）
	groupMax  int                            // relay: 每组在线终端软上限；0 = 不限
	// 终端：中继最近一次回包里的分组状态（设置页展示「已过期/密钥无效」）。
	relayGroupName  string
	relayGroupExp   int64
	relayGroupError string
	busy      map[string]bool                // connect/hello in flight, keyed by "hello:<id>" / "dial:<id>"
	handlers  map[string]Handler
	ctx       context.Context
	cancel    context.CancelFunc
	// onPeerOnline 在终端从离线变为在线时调用（chat 用来冲刷离线队列）；必须非阻塞。
	onPeerOnline func(peer.ID)
	// onGroupChange 在终端分组密钥/中继分组状态变化时调用，参数为当前应订阅的分组密钥（空=退订）。
	onGroupChange func(string)
	groupFile     string        // 中继：组表文件路径（热加载用）
	groupReload   time.Duration // 中继：组表轮询间隔
	groupStamp    string        // 中继：上次成功解析时的 mtime+size
}

type Self struct {
	PeerID       string   `json:"peer_id"`
	Name         string   `json:"name"`
	Platform     string   `json:"platform"`
	Version      string   `json:"version"`
	Port         int      `json:"port"`
	Addrs        []string `json:"addrs"`
	WAN          bool     `json:"wan"`
	Reachability string   `json:"reachability"`
	// Avatar is the local avatar as base64 (PNG/JPEG, <= 16KB base64); empty means none.
	Avatar string `json:"avatar"`
}

type PeerView struct {
	store.Peer
	IP     string   `json:"ip"`
	Addrs  []string `json:"addrs"`
	Online bool     `json:"online"`
	// Relayed: online, but only through a circuit relay (no direct connection yet; DCUtR may upgrade it).
	Relayed bool `json:"relayed"`
	// IsRelay: this peer is one of the servers listed in Relays (the "中继设备" tab), not a terminal.
	IsRelay bool `json:"is_relay"`
}

func New(cfg Config) (*Node, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	priv, err := loadOrCreateKey(filepath.Join(cfg.DataDir, "identity.key"))
	if err != nil {
		return nil, err
	}
	emit := cfg.Emit
	if emit == nil {
		emit = func(string, any) {}
	}
	n := &Node{
		Store: cfg.Store, emit: emit,
		online: map[peer.ID]bool{}, groups: map[peer.ID]string{}, groupErr: map[peer.ID]string{},
		busy: map[string]bool{}, handlers: map[string]Handler{},
		dialSlots: make(chan struct{}, 8),
		group:     strings.TrimSpace(cfg.Group),
	}
	n.ctx, n.cancel = context.WithCancel(context.Background())
	n.reach = "unknown"
	n.directory = cfg.Directory
	n.groupMax = cfg.GroupMaxTerminals
	n.groupFile = cfg.GroupFile
	n.groupReload = cfg.GroupReload
	n.initGroups(cfg.Groups)
	relays, err := ParseRelays(cfg.Relays)
	if err != nil {
		return nil, err
	}
	if cfg.WAN {
		n.relays = map[peer.ID]peer.AddrInfo{}
		for _, r := range relays {
			n.relays[r.ID] = r
		}
	}

	// desktop: trim at 200. a relay holding hundreds of terminals must not share that cap.
	low, high := 50, 200
	if cfg.Directory {
		low, high = 2000, 4000
	}
	cm, err := connmgr.NewConnManager(low, high)
	if err != nil {
		return nil, err
	}
	newHost := func(port int) (host.Host, error) {
		rm, rerr := newResourceManager(cfg.Directory)
		if rerr != nil {
			return nil, rerr
		}
		opts := []libp2p.Option{
			libp2p.Identity(priv),
			libp2p.ListenAddrStrings(
				fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port),
				fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", port),
				fmt.Sprintf("/ip6/::/tcp/%d", port),
				fmt.Sprintf("/ip6/::/udp/%d/quic-v1", port),
			),
			libp2p.ConnectionManager(cm),
			libp2p.ResourceManager(rm),
		}
		if cfg.WAN {
			opts = append(opts, libp2p.EnableAutoNATv2(), libp2p.EnableHolePunching())
			// only the dedicated relay (Directory) forwards; a home PC with WAN on must not become an open relay.
			if cfg.Directory {
				opts = append(opts, libp2p.EnableRelayService(relay.WithResources(fstRelayResources())))
			}
			if cfg.ForcePublic {
				opts = append(opts, libp2p.ForceReachabilityPublic())
			}
			if len(relays) > 0 {
				opts = append(opts, libp2p.EnableAutoRelayWithStaticRelays(relays))
				// AutoRelay only reserves a relay slot once reachability is known to be private, and AutoNAT never
				// reaches a verdict for a node that advertises no public address (a home PC behind NAT): it stayed
				// 'unknown' forever, so nobody could dial us through the relay (NO_RESERVATION). A user who configured
				// relays wants to use them, so declare private. ponytail: the UI then always shows 'LAN only';
				// upgrade path is to drive this from an AutoNAT probe against the relay instead.
				if !cfg.ForcePublic {
					opts = append(opts, libp2p.ForceReachabilityPrivate())
				}
			}
		} else {
			opts = append(opts, libp2p.AddrsFactory(lanOnly), libp2p.DisableRelay())
		}
		h, err := libp2p.New(opts...)
		if err != nil {
			return nil, err
		}
		return h, nil
	}
	h, err := newHost(cfg.Port)
	if err != nil && cfg.Port != 0 {
		slog.Warn("fixed port unavailable, falling back to a random one", "port", cfg.Port, "err", err)
		h, err = newHost(0)
	}
	if err != nil {
		return nil, fmt.Errorf("libp2p: %w", err)
	}
	n.Host = h
	n.wan = cfg.WAN
	if cfg.WAN {
		go n.watchReachability()
		if len(relays) > 0 {
			go n.refreshRelays()
		}
		// A private DHT just for FST ("/fst" protocol prefix) so we never join the public IPFS one.
		// Relays are servers (and bootstrap seeds); terminals are clients so lookup traffic stays on the relay.
		// PeerID add falls back to `<relay>/p2p-circuit` when FindPeer misses a client-mode node.
		// ponytail: accept-all filters because the default ones drop private/loopback addresses; fine for a
		// closed deployment, revisit if the DHT is ever exposed to untrusted peers.
		mode := dht.ModeClient
		if cfg.Directory {
			mode = dht.ModeServer
		}
		d, err := dht.New(h, dht.Mode(mode), dht.ProtocolPrefix("/fst"), dht.DisableValues(), dht.DisableProviders(),
			dht.QueryFilter(func(any, peer.AddrInfo) bool { return true }),
			dht.RoutingTableFilter(func(any, peer.ID) bool { return true }),
			dht.BootstrapPeers(relays...))
		if err != nil {
			h.Close()
			return nil, fmt.Errorf("dht: %w", err)
		}
		n.dht = d
		_ = d.Bootstrap(n.ctx)
		for id := range n.relays {
			h.ConnManager().Protect(id, "fst-relay")
		}
	}
	h.SetStreamHandler(proto.MsgProtocol, n.serveMsg)
	h.Network().Notify(&network.NotifyBundle{
		ConnectedF:    func(_ network.Network, c network.Conn) { go n.onConnected(c.RemotePeer()) },
		DisconnectedF: func(_ network.Network, c network.Conn) { n.onDisconnected(c.RemotePeer()) },
	})
	n.Handle(proto.THello, n.onHello)

	if !cfg.DisableMDNS {
		n.mdns = mdns.NewMdnsService(h, proto.MDNSService, n)
		if err := n.mdns.Start(); err != nil {
			slog.Warn("mdns start failed (use manual connect)", "err", err)
		}
	}
	if n.directory && n.groupFile != "" {
		go n.watchGroupFile()
	}
	return n, nil
}

func (n *Node) Close() {
	n.closed.Store(true)
	n.cancel()
	if n.mdns != nil {
		n.mdns.Close()
	}
	if n.dht != nil {
		n.dht.Close()
	}
	n.Host.Close()
}

func (n *Node) Context() context.Context { return n.ctx }

func (n *Node) Emit(name string, data any) { n.emit(name, data) }

// Handle registers the handler for an inbound /fst/msg type.
func (n *Node) Handle(typ string, h Handler) { n.handlers[typ] = h }

// IsRelay 返回当前节点是否以中继角色运行（cmd/fst-relay）。
//
// 返回:
//   - bool — true 表示中继节点（不参与群聊房间）
func (n *Node) IsRelay() bool { return n.directory }

// IsLANAddr 判断 multiaddr 是否是私网/回环地址（供群聊校验直连邻居）。
//
// 参数:
//   - addr: multiaddr.Multiaddr — 待判断地址
//
// 返回:
//   - bool — 命中私网或回环时为 true
func IsLANAddr(addr ma.Multiaddr) bool { return isLAN(addr) }

// SetOnPeerOnline 注册终端上线回调（可传 nil 取消）。
//
// 参数:
//   - fn: func(peer.ID) — 仅在离线→在线转换时调用；实现必须自行起 goroutine，不能阻塞 HELLO
//
// 返回:
//   - 无
func (n *Node) SetOnPeerOnline(fn func(peer.ID)) {
	n.mu.Lock()
	n.onPeerOnline = fn
	n.mu.Unlock()
}

// SetOnGroupChange 注册终端分组状态回调（群聊房间用它退订过期/被撤销的组）。
//
// 参数:
//   - fn: func(string) — 参数为当前应订阅的分组密钥；空串表示未分组/已失效
//
// 返回:
//   - 无
func (n *Node) SetOnGroupChange(fn func(string)) {
	n.mu.Lock()
	n.onGroupChange = fn
	n.mu.Unlock()
}

// GroupKey 返回终端当前使用的分组密钥。
//
// 返回:
//   - string — 分组密钥；未分组或中继节点为空
func (n *Node) GroupKey() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.directory {
		return ""
	}
	return n.group
}

// SetGroup 运行时更新终端分组密钥，并立即向已配置中继重发 HELLO。
//
// 参数:
//   - key: string — 新分组密钥（去空白；最长 256 字节；空串表示退出分组）
//
// 返回:
//   - error — 中继节点调用、密钥过长或节点已关闭时返回
//
// 副作用：更新内存分组、清空旧的中继分组状态、异步重发 HELLO，并通知群聊房间切换订阅。
func (n *Node) SetGroup(key string) error {
	key = strings.TrimSpace(key)
	if len([]rune(key)) > 200 {
		return errors.New("分组密钥过长")
	}
	if n.directory {
		return errors.New("中继不支持运行时修改分组")
	}
	if n.ctx.Err() != nil {
		return errors.New("节点已关闭")
	}
	n.mu.Lock()
	n.group = key
	n.relayGroupName, n.relayGroupExp, n.relayGroupError = "", 0, ""
	n.mu.Unlock()
	for pid := range n.relays {
		go n.hello(pid)
	}
	n.notifyGroupChange(key)
	return nil
}

// notifyGroupChange 调用已注册的分组状态回调（不持锁执行）。
//
// 参数:
//   - key: string — 当前有效分组密钥；空串表示失效
//
// 返回:
//   - 无
func (n *Node) notifyGroupChange(key string) {
	n.mu.Lock()
	fn := n.onGroupChange
	n.mu.Unlock()
	if fn != nil {
		fn(key)
	}
}

func (n *Node) Name() string {
	def, _ := os.Hostname()
	if def == "" {
		def = "FST"
	}
	return n.Store.Setting("nickname", def)
}

// SetName changes the nickname and announces it to every connected peer.
func (n *Node) SetName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 40 {
		return errors.New("昵称不能为空且不超过 40 个字符")
	}
	if err := n.Store.SetSetting("nickname", name); err != nil {
		return err
	}
	for _, pid := range n.Host.Network().Peers() {
		go n.hello(pid)
	}
	return nil
}

// maxAvatarB64 是本机头像的 base64 长度上限（HELLO 每轮都会携带，不能过大）。
const maxAvatarB64 = 16 << 10

// Avatar 返回本机头像的 base64 编码，未设置时为空串。
//
// 返回:
//   - string — base64（无 data: 前缀）
func (n *Node) Avatar() string { return n.Store.Setting("avatar", "") }

// SetAvatar 保存本机头像并立即向已连接对端重新广播 HELLO。
//
// 参数:
//   - b64: string — base64 编码的 PNG/JPEG（长度 ≤ maxAvatarB64）；空串表示清除头像
//
// 返回:
//   - error — 编码无效、超长或写库失败时返回
//
// 副作用：写入 settings 表；每个已连接对端异步重发一次 HELLO。
func (n *Node) SetAvatar(b64 string) error {
	b64 = strings.TrimSpace(b64)
	if len(b64) > maxAvatarB64 {
		return errors.New("头像过大：base64 需不超过 16KB（约 12KB 图片）")
	}
	if b64 != "" {
		if _, err := base64.StdEncoding.DecodeString(b64); err != nil {
			return errors.New("头像编码无效")
		}
	}
	if err := n.Store.SetSetting("avatar", b64); err != nil {
		return err
	}
	for _, pid := range n.Host.Network().Peers() {
		go n.hello(pid)
	}
	return nil
}

func (n *Node) Self() Self {
	n.mu.Lock()
	reach := n.reach
	n.mu.Unlock()
	s := Self{
		PeerID: n.Host.ID().String(), Name: n.Name(), Platform: runtime.GOOS,
		Version: proto.Version, WAN: n.wan, Reachability: reach, Avatar: n.Avatar(),
	}
	for _, a := range n.Host.Addrs() {
		s.Addrs = append(s.Addrs, a.String()+"/p2p/"+n.Host.ID().String())
		if s.Port == 0 {
			if v, err := a.ValueForProtocol(ma.P_TCP); err == nil {
				fmt.Sscan(v, &s.Port)
			}
		}
	}
	return s
}

func (n *Node) IsOnline(pid peer.ID) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.online[pid]
}

func (n *Node) PeerName(pid string) string {
	if p, ok := n.Store.Peer(pid); ok && p.Name != "" {
		return p.Name
	}
	return pid
}

// Peers merges persisted peers with live connection state.
func (n *Node) Peers() []PeerView {
	list, _ := n.Store.Peers()
	out := make([]PeerView, 0, len(list))
	for _, p := range list {
		out = append(out, n.view(p))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Online && !out[j].Online })
	return out
}

func (n *Node) PeerView(pid string) (PeerView, bool) {
	p, ok := n.Store.Peer(pid)
	if !ok {
		return PeerView{}, false
	}
	return n.view(p), true
}

func (n *Node) view(p store.Peer) PeerView {
	v := PeerView{Peer: p, Addrs: []string{}}
	if id, err := peer.Decode(p.PeerID); err == nil {
		v.Online = n.IsOnline(id)
		v.Relayed = v.Online && n.relayedOnly(id)
		_, v.IsRelay = n.relays[id]
		for _, a := range n.Host.Peerstore().Addrs(id) {
			v.Addrs = append(v.Addrs, a.String())
			// display the first non-loopback address, but let an IPv4 one win over IPv6
			if ip, err := manet.ToIP(a); err == nil && !ip.IsLoopback() && (v.IP == "" || (ip.To4() != nil && net.ParseIP(v.IP).To4() == nil)) {
				v.IP = ip.String()
			}
		}
	}
	return v
}

// ConnectString dials a full multiaddr such as "/ip4/192.168.1.5/tcp/8787/p2p/<PeerID>" (manual fallback when
// multicast is blocked). libp2p cannot dial without the PeerID, so the peer's own address (shown in its
// settings page) is what gets pasted here.
func (n *Node) ConnectString(ctx context.Context, s string) error {
	if id, err := peer.Decode(strings.TrimSpace(s)); err == nil { // a bare PeerID: look the address up in the DHT
		return n.connectByID(ctx, id)
	}
	addr, err := ma.NewMultiaddr(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("地址无效，需形如 /ip4/192.168.1.5/tcp/8787/p2p/<PeerID>: %w", err)
	}
	info, err := peer.AddrInfoFromP2pAddr(addr)
	if err != nil {
		return errors.New("地址缺少 /p2p/<PeerID> 部分，请粘贴对方设置页中显示的完整地址")
	}
	if info.ID == n.Host.ID() {
		return errors.New("不能连接自己")
	}
	return n.Host.Connect(ctx, *info)
}

// connectByID finds pid via the DHT, then (if that misses) through a configured relay's circuit.
func (n *Node) connectByID(ctx context.Context, pid peer.ID) error {
	if pid == n.Host.ID() {
		return errors.New("不能连接自己")
	}
	if n.dht != nil {
		if info, err := n.dht.FindPeer(ctx, pid); err == nil && len(info.Addrs) > 0 {
			if err := n.Host.Connect(ctx, info); err == nil {
				return nil
			}
		}
	}
	if err := n.dialViaRelays(ctx, pid); err == nil {
		return nil
	}
	if n.dht == nil && len(n.relays) == 0 {
		return errors.New("按 PeerID 添加需要开启“允许跨网络连接”并配置中继/引导节点")
	}
	return errors.New("没有找到该设备（对方需开启跨网络并连上同一批中继）")
}

// EnsureConnected dials pid if needed, using peerstore addrs or a configured relay circuit.
func (n *Node) EnsureConnected(ctx context.Context, pid peer.ID) error {
	if n.Host.Network().Connectedness(pid) == network.Connected {
		return nil
	}
	if addrs := n.Host.Peerstore().Addrs(pid); len(addrs) > 0 {
		if err := n.Host.Connect(ctx, peer.AddrInfo{ID: pid, Addrs: addrs}); err == nil {
			return nil
		}
	}
	return n.connectByID(ctx, pid)
}

func (n *Node) onConnected(pid peer.ID) {
	if n.directory {
		n.Host.ConnManager().Protect(pid, "fst")
	}
	n.dropCircuitIfDirect(pid)
	n.hello(pid)
}

func (n *Node) onDisconnected(pid peer.ID) {
	if n.closed.Load() {
		return
	}
	if n.Host.Network().Connectedness(pid) == network.Connected {
		return // another connection (e.g. TCP+QUIC duplicate) is still up
	}
	if n.directory {
		n.Host.ConnManager().Unprotect(pid, "fst")
	}
	n.mu.Lock()
	was := n.online[pid]
	delete(n.online, pid)
	delete(n.groups, pid)
	n.mu.Unlock()
	if was {
		n.emit("peer:offline", map[string]any{"peer_id": pid.String(), "last_seen": store.Now()})
	}
}

// dropCircuitIfDirect closes leftover /p2p-circuit connections once DCUtR (or LAN) has a direct path,
// so the relay stops forwarding bytes that no longer need it.
func (n *Node) dropCircuitIfDirect(pid peer.ID) {
	conns := n.Host.Network().ConnsToPeer(pid)
	var circuit []network.Conn
	direct := false
	for _, c := range conns {
		if _, err := c.RemoteMultiaddr().ValueForProtocol(ma.P_CIRCUIT); err == nil {
			circuit = append(circuit, c)
		} else {
			direct = true
		}
	}
	if !direct || len(circuit) == 0 {
		return
	}
	for _, c := range circuit {
		_ = c.Close()
	}
	if v, ok := n.PeerView(pid.String()); ok {
		n.emit("peer:updated", v)
	}
}

func (n *Node) markOnline(pid peer.ID) {
	n.mu.Lock()
	was := n.online[pid]
	n.online[pid] = true
	hook := n.onPeerOnline
	n.mu.Unlock()
	if v, ok := n.PeerView(pid.String()); ok {
		if was {
			n.emit("peer:updated", v)
		} else {
			n.emit("peer:online", v)
		}
	}
	if !was && hook != nil {
		hook(pid)
	}
}

// buildHello assembles a HELLO for typ (or another type that piggybacks the same fields) addressed to `to`.
//
// 参数:
//   - typ: string — 消息类型，通常为 proto.THello
//   - to: peer.ID — 收件 PeerID（广播给多个终端时传空串）；仅当它是本机配置的中继时，HELLO 才携带分组密钥
//
// 返回:
//   - *proto.Msg — 已填好昵称/平台/版本（终端另有最多 50 个局域网地址）的 HELLO
func (n *Node) buildHello(typ string, to peer.ID) *proto.Msg {
	m := &proto.Msg{Type: typ, TS: proto.Now(), Name: n.Name(), Platform: runtime.GOOS, Version: proto.Version}
	if a := n.Avatar(); a != "" {
		m.Avatar = a
	}
	// 分组密钥只发给配置里写明的中继：普通对端（含局域网内陌生节点）拿到它就能冒充同组。
	n.mu.Lock()
	grp := n.group
	n.mu.Unlock()
	if !n.directory && grp != "" {
		if _, ok := n.relays[to]; ok {
			m.Group = grp
		}
	}
	// 中继在回包里带上分组状态，终端设置页据此显示「已过期 / 密钥无效」。
	if n.directory && to != "" {
		m.GroupName, m.GroupExpires, m.GroupError = n.groupInfo(to)
	}
	for _, pid := range n.Host.Network().Peers() {
		if n.directory {
			break // a relay hands out PeerIDs via Directory; its own LAN neighbours mean nothing to remote clients
		}
		for _, a := range n.Host.Peerstore().Addrs(pid) {
			if isLAN(a) {
				m.Peers = append(m.Peers, a.String()+"/p2p/"+pid.String())
				break // one address per peer is enough for introductions
			}
		}
		if len(m.Peers) >= 50 {
			break
		}
	}
	return m
}

// hello exchanges HELLO with pid (once at a time per peer); success means pid speaks FST and is online.
func (n *Node) hello(pid peer.ID) {
	n.mu.Lock()
	key := "hello:" + pid.String()
	if n.busy[key] {
		n.mu.Unlock()
		return
	}
	n.busy[key] = true
	n.mu.Unlock()
	defer func() { n.mu.Lock(); delete(n.busy, key); n.mu.Unlock() }()

	for attempt := 0; attempt < 3 && n.ctx.Err() == nil; attempt++ {
		ctx, cancel := context.WithTimeout(n.ctx, 10*time.Second)
		reply, err := n.Request(ctx, pid, n.buildHello(proto.THello, pid), 10*time.Second)
		cancel()
		if err == nil && reply.Type == proto.THello {
			n.learn(pid, reply)
			return
		}
		select {
		case <-time.After(time.Second):
		case <-n.ctx.Done():
			return
		}
	}
}

// learn records a peer's HELLO, flips it online and follows introductions.
//
// 参数:
//   - pid: peer.ID — 发来 HELLO 的对端
//   - h: *proto.Msg — 对端的 HELLO（昵称、平台、分组密钥、局域网地址、目录）
//
// 返回:
//   - 无（副作用：落库设备档案、更新在线状态与分组、按需后台拨号）
func (n *Node) learn(pid peer.ID, h *proto.Msg) {
	n.Store.UpsertPeer(store.Peer{
		PeerID: pid.String(), Name: h.Name, Platform: h.Platform,
		LastAddrs: n.addrString(pid), Avatar: h.Avatar,
	})
	if n.directory {
		n.assignGroup(pid, h.Group)
	} else if h.GroupName != "" || h.GroupError != "" {
		// 中继在 HELLO 回包里说明了分组状态；终端只保留最近一次结果供设置页展示。
		n.mu.Lock()
		changed := n.relayGroupName != h.GroupName || n.relayGroupExp != h.GroupExpires ||
			n.relayGroupError != h.GroupError
		n.relayGroupName, n.relayGroupExp, n.relayGroupError = h.GroupName, h.GroupExpires, h.GroupError
		n.mu.Unlock()
		if changed {
			// 只有中继接受时房间才继续存在；密钥无效/过期/撤销都退订。
			key := ""
			if h.GroupName != "" && h.GroupError == "" {
				key = n.GroupKey()
			}
			n.notifyGroupChange(key)
		}
	}
	n.markOnline(pid)
	if len(h.Directory) > 0 && n.wan {
		n.followDirectory(pid, h.Directory)
	}
	for _, s := range h.Peers {
		a, err := ma.NewMultiaddr(s)
		if err != nil || !isLAN(a) {
			continue // never dial non-LAN addresses supplied by a peer
		}
		info, err := peer.AddrInfoFromP2pAddr(a)
		if err != nil || info.ID == n.Host.ID() {
			continue
		}
		n.dial(*info)
	}
}

// onHello answers one inbound HELLO and, on a relay, attaches the directory plus arrival notices.
//
// 参数:
//   - from: peer.ID — 发来 HELLO 的对端（连接层已认证，不可伪造）
//   - in: *proto.Msg — 对端的 HELLO，中继从中读取分组密钥
//
// 返回:
//   - *proto.Msg — 本机 HELLO 回复；中继会带上目录（同组时才非空）
func (n *Node) onHello(_ context.Context, from peer.ID, in *proto.Msg) *proto.Msg {
	was := n.IsOnline(from)
	if n.directory {
		// 目录要在同一个响应里按组过滤，因此分组必须先于 directoryFor 落定（learn 是异步的）
		if errText := n.assignGroup(from, in.Group); errText != "" {
			slog.Warn("relay rejected group key", "peer", from, "reason", errText)
		}
	}
	go n.learn(from, in)
	r := n.buildHello(proto.THello, from)
	if n.directory {
		r.Directory = n.directoryFor(from)
		if !was {
			go n.announceArrival(from)
		}
	}
	return r
}

// directoryFor lists the FST peers currently online here, except the asker.
// ponytail: capped at 200 and only group-filtered; page it or move to a rendezvous protocol if a relay
// ever serves more than a few hundred terminals per group.
//
// 参数:
//   - except: peer.ID — 提问的终端（不会出现在结果里）
//
// 返回:
//   - []string — 同组在线终端的 PeerID；提问方不在本中继分组内时返回 nil（不下发目录）
//
// 说明：配置了 GroupMaxTerminals 时，每组最多下发该数量的终端（软上限，不踢连接）。
func (n *Node) directoryFor(except peer.ID) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.sameGroupLocked(except) {
		return nil
	}
	group := n.groups[except]
	var out []string
	for id := range n.online {
		if id == except || !n.sameGroupLocked(id) {
			continue
		}
		if group != "" && n.groups[id] != group {
			continue // 多组中继：只介绍同组终端
		}
		if n.groupMax > 0 && group != "" && len(out) >= n.groupMax {
			slog.Warn("relay group over soft cap; directory truncated", "group", group, "cap", n.groupMax)
			break
		}
		out = append(out, id.String())
	}
	return out
}

// followDirectory dials the peers a configured relay told us about, through that relay. A directory from
// anyone else is ignored, so a random peer cannot stuff our device list.
func (n *Node) followDirectory(relay peer.ID, ids []string) {
	ri, ok := n.relays[relay]
	if !ok {
		return
	}
	addrs := circuitAddrs(ri)
	if len(addrs) == 0 {
		return
	}
	for _, s := range ids {
		id, err := peer.Decode(s)
		if err != nil || id == n.Host.ID() {
			continue
		}
		n.Host.Peerstore().AddAddrs(id, addrs, peerstore.AddressTTL)
		n.dial(peer.AddrInfo{ID: id, Addrs: addrs})
	}
}

// announceArrival tells already-online terminals about a newcomer (one PeerID), so they do not have to
// wait for the 5-minute directory poll. Only terminals in the relay's group are notified.
// ponytail: 8 at a time, same cap as dialSlots; page if a join storm of thousands of already-online
// peers becomes the bottleneck.
//
// 参数:
//   - newbie: peer.ID — 刚上线的终端
//
// 返回:
//   - 无（副作用：向同组在线终端逐个发 HELLO，携带 newbie 的 PeerID）
func (n *Node) announceArrival(newbie peer.ID) {
	n.mu.Lock()
	if !n.sameGroupLocked(newbie) { // 异组终端上线时不通知任何人，它就只是占用一条中继连接
		n.mu.Unlock()
		return
	}
	group := n.groups[newbie]
	var ids []peer.ID
	for id := range n.online {
		if id == newbie || !n.sameGroupLocked(id) {
			continue
		}
		if group != "" && n.groups[id] != group {
			continue
		}
		if n.groupMax > 0 && group != "" && len(ids) >= n.groupMax {
			slog.Warn("relay group over soft cap; arrival notice skipped", "group", group, "cap", n.groupMax)
			break
		}
		ids = append(ids, id)
	}
	n.mu.Unlock()
	msg := n.buildHello(proto.THello, "")
	msg.Directory = []string{newbie.String()}
	slots := make(chan struct{}, 8)
	for _, id := range ids {
		slots <- struct{}{}
		go func(id peer.ID) {
			defer func() { <-slots }()
			ctx, cancel := context.WithTimeout(n.ctx, 8*time.Second)
			defer cancel()
			_, _ = n.Request(ctx, id, msg, 8*time.Second)
		}(id)
	}
}

func circuitAddrs(ri peer.AddrInfo) []ma.Multiaddr {
	hop := ma.StringCast("/p2p/" + ri.ID.String() + "/p2p-circuit")
	var addrs []ma.Multiaddr
	for _, a := range ri.Addrs {
		addrs = append(addrs, a.Encapsulate(hop))
	}
	return addrs
}

func (n *Node) dialViaRelays(ctx context.Context, pid peer.ID) error {
	var last error = errors.New("no relays")
	for _, ri := range n.relays {
		addrs := circuitAddrs(ri)
		if len(addrs) == 0 {
			continue
		}
		last = n.Host.Connect(ctx, peer.AddrInfo{ID: pid, Addrs: addrs})
		if last == nil {
			return nil
		}
	}
	return last
}

// refreshRelays re-asks every configured relay for its directory (and redials a relay that dropped).
// Arrival is pushed by the relay; this is the backup for missed reservations and relay restarts.
func (n *Node) refreshRelays() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-t.C:
			for id, ri := range n.relays {
				if n.Host.Network().Connectedness(id) == network.Connected {
					go n.hello(id)
				} else {
					n.dial(ri)
				}
			}
		}
	}
}

func (n *Node) addrString(pid peer.ID) string {
	var s []string
	for _, a := range n.Host.Peerstore().Addrs(pid) {
		s = append(s, a.String())
	}
	return strings.Join(s, ",")
}

// HandlePeerFound is the mDNS callback; it may fire repeatedly for the same peer.
func (n *Node) HandlePeerFound(pi peer.AddrInfo) {
	if pi.ID == n.Host.ID() {
		return
	}
	n.Host.Peerstore().AddAddrs(pi.ID, pi.Addrs, peerstore.AddressTTL)
	n.dial(pi)
}

// dial connects in the background unless already connected or dialing.
func (n *Node) dial(pi peer.AddrInfo) {
	if n.Host.Network().Connectedness(pi.ID) == network.Connected {
		return
	}
	key := "dial:" + pi.ID.String()
	n.mu.Lock()
	if n.busy[key] {
		n.mu.Unlock()
		return
	}
	n.busy[key] = true
	n.mu.Unlock()
	go func() {
		defer func() { n.mu.Lock(); delete(n.busy, key); n.mu.Unlock() }()
		select {
		case n.dialSlots <- struct{}{}:
			defer func() { <-n.dialSlots }()
		case <-n.ctx.Done():
			return
		}
		ctx, cancel := context.WithTimeout(n.ctx, 15*time.Second)
		defer cancel()
		if err := n.Host.Connect(ctx, pi); err != nil {
			slog.Debug("dial failed", "peer", pi.ID, "err", err)
		}
	}()
}

// fstRelayResources caps how many reservations and concurrent circuits a dedicated relay will take,
// but does not impose the default 2 min / 128KB per-circuit limit (that made file transfer unusable).
func fstRelayResources() relay.Resources {
	r := relay.DefaultResources()
	r.Limit = nil
	r.MaxReservations = 4096
	r.MaxCircuits = 128
	r.MaxReservationsPerPeer = 1
	r.MaxReservationsPerIP = 32
	r.MaxReservationsPerASN = 256
	return r
}

// Request sends one message and waits for the single reply.
func (n *Node) Request(ctx context.Context, pid peer.ID, m *proto.Msg, timeout time.Duration) (*proto.Msg, error) {
	// allow relay connections that are time/data limited (public relays): fine for chat-sized messages
	s, err := n.Host.NewStream(network.WithAllowLimitedConn(ctx, "fst"), pid, proto.MsgProtocol)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(timeout))
	// closing on ctx cancel lets callers abort a long wait (e.g. an unanswered offer)
	stop := context.AfterFunc(ctx, func() { s.Reset() })
	defer stop()
	if err := proto.WriteFrame(s, m); err != nil {
		return nil, err
	}
	_ = s.CloseWrite()
	var reply proto.Msg
	if err := proto.ReadFrame(s, &reply); err != nil {
		return nil, err
	}
	if reply.Type == proto.TError {
		return nil, &RemoteError{Reason: reply.Reason}
	}
	return &reply, nil
}

func (n *Node) serveMsg(s network.Stream) {
	defer s.Close()
	_ = s.SetReadDeadline(time.Now().Add(30 * time.Second))
	var in proto.Msg
	if err := proto.ReadFrame(s, &in); err != nil {
		s.Reset()
		return
	}
	_ = s.SetDeadline(time.Time{})
	h := n.handlers[in.Type]
	if h == nil {
		_ = proto.WriteFrame(s, &proto.Msg{Type: proto.TError, Reason: "unsupported type " + in.Type})
		return
	}
	// a handler may wait on a human (file offer); bound it, and abort if the requester hangs up
	ctx, cancel := context.WithTimeout(n.ctx, 11*time.Minute)
	defer cancel()
	if reply := h(ctx, s.Conn().RemotePeer(), &in); reply != nil {
		_ = s.SetWriteDeadline(time.Now().Add(30 * time.Second))
		_ = proto.WriteFrame(s, reply)
	}
}

func loadOrCreateKey(path string) (crypto.PrivKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		return crypto.UnmarshalPrivateKey(b)
	}
	priv, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		return nil, err
	}
	b, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return priv, os.WriteFile(path, b, 0o600)
}

// newResourceManager returns libp2p limits tuned for FST. Every proxied
// TCP/UDP connection holds one stream on /fst/proxy/1.0.0, and a browser page
// opens hundreds in a burst; the library defaults cap one peer/protocol at 256
// streams, which surfaced as "cannot reserve outbound stream: resource limit
// exceeded" and half-loaded pages (fixed by a refresh that opens fewer).
// Relays keep unlimited limits.
func newResourceManager(directory bool) (network.ResourceManager, error) {
	if directory {
		return rcmgr.NewResourceManager(rcmgr.NewFixedLimiter(rcmgr.InfiniteLimits))
	}
	limits := rcmgr.DefaultLimits
	limits.SystemBaseLimit.Streams = 65536
	limits.SystemBaseLimit.StreamsOutbound = 65536
	limits.SystemBaseLimit.Memory = 8 << 30
	limits.PeerBaseLimit.Streams = 16384
	limits.PeerBaseLimit.StreamsOutbound = 16384
	limits.PeerBaseLimit.Memory = 4 << 30
	limits.ProtocolBaseLimit.Streams = 32768
	limits.ProtocolBaseLimit.StreamsOutbound = 32768
	limits.ProtocolBaseLimit.Memory = 4 << 30
	limits.ProtocolPeerBaseLimit.Streams = 8192
	limits.ProtocolPeerBaseLimit.StreamsOutbound = 8192
	limits.ProtocolPeerBaseLimit.StreamsInbound = 2048
	limits.ProtocolPeerBaseLimit.Memory = 2 << 30
	return rcmgr.NewResourceManager(rcmgr.NewFixedLimiter(limits.AutoScale()))
}

// ParseRelays validates static relay multiaddrs ("/ip4/1.2.3.4/tcp/4001/p2p/<id>").
func ParseRelays(list []string) ([]peer.AddrInfo, error) {
	var addrs []ma.Multiaddr
	for _, s := range list {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		a, err := ma.NewMultiaddr(s)
		if err != nil {
			return nil, fmt.Errorf("中继地址无效 %q: %w", s, err)
		}
		addrs = append(addrs, a)
	}
	if len(addrs) == 0 {
		return nil, nil
	}
	infos, err := peer.AddrInfosFromP2pAddrs(addrs...)
	if err != nil {
		return nil, fmt.Errorf("中继地址需包含 /p2p/<PeerID>: %w", err)
	}
	return infos, nil
}

// relayedOnly reports whether every live connection to pid goes through a circuit relay.
func (n *Node) relayedOnly(pid peer.ID) bool {
	conns := n.Host.Network().ConnsToPeer(pid)
	for _, c := range conns {
		if _, err := c.RemoteMultiaddr().ValueForProtocol(ma.P_CIRCUIT); err != nil {
			return false
		}
	}
	return len(conns) > 0
}

// watchReachability mirrors AutoNATv2's verdict into Self and tells the UI.
func (n *Node) watchReachability() {
	sub, err := n.Host.EventBus().Subscribe(new(event.EvtLocalReachabilityChanged))
	if err != nil {
		return
	}
	defer sub.Close()
	for {
		select {
		case <-n.ctx.Done():
			return
		case e := <-sub.Out():
			r := "unknown"
			switch e.(event.EvtLocalReachabilityChanged).Reachability {
			case network.ReachabilityPublic:
				r = "public"
			case network.ReachabilityPrivate:
				r = "private"
			}
			n.mu.Lock()
			n.reach = r
			n.mu.Unlock()
			n.emit("node:reachability", r)
		}
	}
}
func isLAN(a ma.Multiaddr) bool {
	return manet.IsPrivateAddr(a) || manet.IsIPLoopback(a)
}

// lanOnly advertises private/loopback addresses only, so VPN/public interfaces never leak into mDNS.
func lanOnly(addrs []ma.Multiaddr) []ma.Multiaddr {
	out := addrs[:0:0]
	for _, a := range addrs {
		if isLAN(a) {
			out = append(out, a)
		}
	}
	return out
}
