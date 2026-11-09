// Package core 是 FST 移动端的 Go 核心封装。
//
// 设计目标：把桌面版已经验证过的 node / chat / transfer 三个模块原样复用，
// 只在边界上做一层「移动友好」的适配，从而保证手机与电脑说的是同一套协议
// （/fst/msg、/fst/file、mDNS 双服务名），可以互相发现、聊天、传文件。
//
// 本包被 phone/daemon 使用：守护进程把它暴露成本机回环 HTTP/SSE，Android 的
// Compose 外壳通过 HTTP 调用。之所以不直接用 gomobile 做进程内绑定，是因为
// gomobile 依赖 cgo + NDK clang，构建链在本仓库的 Windows 环境上不可靠；详见
// phone/README.md。
//
// 三个必须说明的边界约定：
//
//  1. 跨语言只传 JSON 字符串与基础类型。列表类数据统一序列化成 JSON，由
//     Kotlin 用 kotlinx.serialization 解析，避免每次跨语言都做结构体映射。
//  2. 文件仍以「本地路径」为接口。Android 的 SAF 是 URI 不是路径，所以由
//     Kotlin 侧负责把用户选中的内容复制进应用私有目录，再把真实路径交给本包；
//     接收方向由本包写入 Start 时指定的下载目录，Kotlin 再导出到用户可见位置。
//  3. 事件通过 Listener 回调 + 事件环形缓冲双通道投递。守护进程用回调实时推
//     SSE；回调注册晚于事件发生时可调用 DrainEventsJSON 补拉。
//
// 该包不包含：TUN 代理（Android 需要 VpnService + socket protect，成本与收益
// 不匹配）、跨网络中继（WAN）、群聊（GossipSub）。这些能力在桌面端保留，
// 移动端首版不启用。
package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"fst/internal/chat"
	"fst/internal/node"
	"fst/internal/proto"
	"fst/internal/store"
	"fst/internal/transfer"

	"github.com/libp2p/go-libp2p/core/peer"
)

// Version 是移动端核心的版本号，随协议版本一起向上层展示。
const Version = "0.1.0"

// DefaultPort 是移动端使用的 libp2p 监听端口，与桌面端一致（TCP+QUIC 同号）。
// 端口被占用时 node 会自动回落到随机端口，并通过 SRV 记录广播实际端口。
const DefaultPort = 8787

// maxPendingEvents 是事件环形缓冲的容量。超过后丢弃最旧的事件。
const maxPendingEvents = 512

// Listener 是 Go → Android 的事件回调。Android 侧实现该接口并在 Start 前注册。
//
// 注意：OnEvent 会在 Go 的任意协程上被调用，实现方必须自己切回主线程再更新 UI。
type Listener interface {
	// OnEvent 接收一个 UI 事件。
	//
	// 参数:
	//   - name: string — 与桌面端完全一致的事件名，例如 peer:online、chat:message、transfer:progress
	//   - payload: string — 该事件的 JSON 载荷（对象或数组，取决于事件类型）
	OnEvent(name string, payload string)
}

// event 是事件环形缓冲里的一条记录。
type event struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload"`
}

// Core 是移动端唯一的核心对象。生命周期由 Android 的前台服务持有。
//
// 并发约定：所有导出方法都可从任意协程调用；内部用 mu 保护生命周期字段，
// 网络与磁盘操作在锁外执行，避免阻塞 UI 线程（gomobile 调用是同步的）。
type Core struct {
	mu       sync.Mutex
	db       *store.Store
	node     *node.Node
	chat     *chat.Service
	transfer *transfer.Service
	listener Listener
	pending  []event
	dataDir  string
}

// New 创建一个未启动的 Core。
//
// 参数:
//
//	无
//
// 返回:
//   - *Core — 需要再调用 Start 才会加入网络
func New() *Core { return &Core{} }

// startConfig 汇总 Start 的可选参数。
type startConfig struct {
	port int
}

// Option 是 Start 的可选参数，零个参数时使用移动端默认配置。
type Option func(*startConfig)

// WithPort 指定 libp2p 的 TCP/UDP 监听端口；0 或负数表示沿用 DefaultPort。
//
// 参数:
//   - port: int — 期望端口，被占用时 node 会自动回落到随机端口
//
// 返回:
//   - Option — 传给 Start 的可选参数
//
// 用途：同机双实例联调（`phone/scripts/e2e-ipc.sh`）。真机永远用默认 8787。
func WithPort(port int) Option {
	return func(c *startConfig) { c.port = port }
}

// Start 打开数据库、创建 libp2p 节点、启动文件传输服务。
//
// 参数:
//   - dataDir: string — 应用私有数据目录（数据库、身份密钥、日志），例如 Context.getFilesDir()
//   - downloadDir: string — 接收文件的落盘目录，必须可写；空则沿用上次设置或默认值
//   - deviceName: string — 本机显示名，空则保留上次设置
//   - lanIP: string — 本机 Wi-Fi/以太网 IPv4，用于对外广播；空表示按系统枚举结果广播
//   - opts: ...Option — 可选参数，见 WithPort
//
// 返回:
//   - error — 目录不可写、数据库打不开、端口与身份密钥初始化失败时返回；
//     重复调用是幂等的，已启动时直接返回 nil
//
// 为什么需要 lanIP：Android 11+ 禁止应用 UID 使用 NETLINK_ROUTE，Go 的
// net.Interfaces() 必然失败，libp2p 展开监听地址后只剩 127.0.0.1。若把它
// 广播出去，对端（桌面端或另一台手机）学到的就是回环地址，之后按该地址
// 拨号只会连到它自己——这正是「能发出去、收不到」与「手机之间互相发现却
// 连不上」的根因。地址由 Kotlin 侧从 ConnectivityManager 取，不经 netlink。
//
// 副作用：占用 8787 端口（TCP/UDP），开始写数据库。
func (c *Core) Start(dataDir, downloadDir, deviceName, lanIP string, opts ...Option) error {
	c.mu.Lock()
	if c.node != nil {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	if dataDir == "" {
		return errors.New("dataDir 不能为空")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	initLog(dataDir)

	db, err := store.Open(dataDir)
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	if downloadDir != "" {
		if err := os.MkdirAll(downloadDir, 0o700); err != nil {
			db.Close()
			return fmt.Errorf("创建下载目录失败: %w", err)
		}
		if err := db.SetSetting("download_dir", downloadDir); err != nil {
			db.Close()
			return fmt.Errorf("保存下载目录失败: %w", err)
		}
	}
	// 移动端默认手动接收：陌生设备不能未经确认就往手机里写文件。
	if db.Setting("auto_accept", "") == "" {
		_ = db.SetSetting("auto_accept", "0")
	}

	sc := startConfig{port: DefaultPort}
	for _, o := range opts {
		if o != nil {
			o(&sc)
		}
	}
	if sc.port <= 0 {
		sc.port = DefaultPort
	}

	n, err := node.New(node.Config{
		DataDir: dataDir,
		Port:    sc.port,
		Store:   db,
		Emit:    c.emit,
		WAN:     false,
		// 本机真实 IPv4，由 Kotlin 从 ConnectivityManager 取得（见 Core.Start 说明）。
		AnnounceIP: lanIP,
		// 移动端不用 Go 侧 mDNS：Android 11+ 禁止应用使用 NETLINK_ROUTE，Go 的
		// net.Interfaces() 必然失败（netlinkrib: permission denied），libp2p 会
		// 退化成只有回环地址；系统 mdnsd 还常驻占用 5353，第三方绑定会 EADDRINUSE。
		// 因此发现改为 Android 的 NsdManager（见 FstRepository/Nsd），找到的对端
		// 通过 AddPeer 注入。桌面端仍走 lanmdns。
		DisableMDNS: true,
	})
	if err != nil {
		db.Close()
		return fmt.Errorf("启动节点失败: %w", err)
	}
	chatSvc := chat.New(n)
	transferSvc := transfer.New(n)

	c.mu.Lock()
	c.db, c.node, c.chat, c.transfer, c.dataDir = db, n, chatSvc, transferSvc, dataDir
	c.mu.Unlock()

	if deviceName != "" {
		if err := n.SetName(deviceName); err != nil {
			slog.Warn("设置设备名失败", "err", err)
		}
	}
	slog.Info("移动端核心已启动", "peer", n.Host.ID(), "announce_ip", n.AnnounceIP(), "addrs", n.Host.Addrs())
	return nil
}

// SetLANIP 更新对外广播的本机 IPv4；核心未启动时返回错误。
//
// 用途：手机切换 Wi-Fi、从飞行模式恢复、DHCP 续租换地址后，本机 IP 会变，
// 而节点已经在运行。此时不需要重启核心，改广播地址即可——下一次 HELLO /
// identify 就会把新地址告诉对端。
//
// 参数:
//   - ip: string — 形如 "192.168.1.13" 的 IPv4；空字符串表示交还给系统枚举
//
// 返回:
//   - error — 核心未启动或 ip 非法时返回
//
// 副作用：立即影响 /self 返回值与对端学到的地址。
func (c *Core) SetLANIP(ip string) error {
	c.mu.Lock()
	n := c.node
	c.mu.Unlock()
	if n == nil {
		return errors.New("核心尚未启动")
	}
	if err := n.SetAnnounceIP(ip); err != nil {
		return err
	}
	slog.Info("对外广播地址已更新", "announce_ip", ip, "addrs", n.Host.Addrs())
	return nil
}

// Stop 关闭节点并释放数据库；可重复调用。
//
// 参数:
//
//	无
//
// 返回:
//
//	无
//
// 副作用：断开所有连接；未完成的传输会被标记为中断，下次启动可断点续传。
func (c *Core) Stop() {
	c.mu.Lock()
	n, db := c.node, c.db
	c.node, c.db, c.chat, c.transfer = nil, nil, nil, nil
	c.mu.Unlock()
	if n != nil {
		n.Close()
	}
	if db != nil {
		db.Close()
	}
}

// SetListener 注册事件回调，传 nil 表示取消。
//
// 参数:
//   - l: Listener — 实现方；注册会立即替换旧回调
//
// 返回:
//
//	无
func (c *Core) SetListener(l Listener) {
	c.mu.Lock()
	c.listener = l
	c.mu.Unlock()
}

// SelfJSON 返回本机信息。
//
// 参数:
//
//	无
//
// 返回:
//   - string — JSON: {"peer_id","name","version","port","lan_ip","addrs":[],"download_dir","avatar"}；
//     未启动时返回空对象 "{}"
//
// 说明：avatar 是本机头像的 base64（≤16KB），供设置页回显；对端头像随 HELLO
// 同步，出现在 PeersJSON 的 avatar 字段里。
func (c *Core) SelfJSON() string {
	c.mu.Lock()
	n, t := c.node, c.transfer
	c.mu.Unlock()
	if n == nil {
		return "{}"
	}
	addrs := make([]string, 0, len(n.Host.Addrs()))
	for _, a := range n.Host.Addrs() {
		addrs = append(addrs, a.String())
	}
	dl := ""
	if t != nil {
		dl = t.DownloadDir()
	}
	return toJSON(map[string]any{
		"peer_id":      n.Host.ID().String(),
		"name":         n.Name(),
		"version":      Version,
		"port":         n.ListenPort(),
		"lan_ip":       n.AnnounceIP(),
		"addrs":        addrs,
		"download_dir": dl,
		"avatar":       n.Avatar(),
	})
}

// AddPeer 接收 Android 侧 DNS-SD 解析到的对端地址并尝试连接。
//
// Android 11+ 的应用进程既不能枚举网卡（NETLINK_ROUTE 被禁用），系统 mdnsd 又
// 常驻占用 5353，Go 侧 mDNS 在手机上不可用。因此发现由系统 NsdManager 负责，
// 它解析出的 TXT 记录（dnsaddr=...）通过本方法回灌给 libp2p——传输、加密与
// 分帧仍然全部由 Go 侧完成，协议与桌面端保持一致。
//
// 参数:
//   - addr: string — 带 PeerID 的完整 multiaddr，例如
//     /ip4/192.168.1.15/tcp/8787/p2p/12D3KooW...
//
// 返回:
//   - error — 地址格式非法或核心未启动时返回；连接本身在后台异步进行，
//     无法连接不会在这里报错（对端可能刚离开）
func (c *Core) AddPeer(addr string) error {
	c.mu.Lock()
	n := c.node
	c.mu.Unlock()
	if n == nil {
		return errors.New("核心尚未启动")
	}
	info, err := peer.AddrInfoFromString(addr)
	if err != nil {
		return fmt.Errorf("地址格式错误: %w", err)
	}
	if info.ID == n.Host.ID() {
		return nil
	}
	n.HandlePeerFound(*info)
	return nil
}

// PeersJSON 返回已发现的设备列表。
//
// 参数:
//
//	无
//
// 返回:
//   - string — JSON 数组，元素字段与桌面端的 PeerView 一致：
//     peer_id、name、platform、online、relayed、ip、addrs、avatar、last_seen、trusted
func (c *Core) PeersJSON() string {
	c.mu.Lock()
	n := c.node
	c.mu.Unlock()
	if n == nil {
		return "[]"
	}
	return toJSON(n.Peers())
}

// ConversationsJSON 返回会话列表（最近一条消息 + 未读数），用于首页列表。
//
// 参数:
//
//	无
//
// 返回:
//   - string — JSON 数组，字段同 store.Conversation
func (c *Core) ConversationsJSON() string {
	c.mu.Lock()
	db := c.db
	c.mu.Unlock()
	if db == nil {
		return "[]"
	}
	list, err := db.Conversations()
	if err != nil || list == nil {
		return "[]"
	}
	return toJSON(list)
}

// MessagesJSON 返回与某个对端的历史消息（按时间倒序分页）。
//
// 参数:
//   - peerID: string — 对方 PeerID
//   - before: int64 — 只取该时间戳（毫秒）之前的消息；0 表示从最新开始
//   - limit: int — 条数上限，<=0 时用 50
//
// 返回:
//   - string — JSON 数组，字段同 store.Message；参数非法时返回 "[]"
func (c *Core) MessagesJSON(peerID string, before int64, limit int) string {
	c.mu.Lock()
	db := c.db
	c.mu.Unlock()
	if db == nil || peerID == "" {
		return "[]"
	}
	if limit <= 0 {
		limit = 50
	}
	list, err := db.Messages(peerID, before, limit)
	if err != nil {
		return "[]"
	}
	if list == nil {
		// nil 切片会被编码成 null，Kotlin 侧解析 List<T> 会直接抛异常。
		return "[]"
	}
	return toJSON(list)
}

// MarkRead 把某个对端发来的消息标记为已读。
//
// 参数:
//   - peerID: string — 对方 PeerID
//
// 返回:
//   - error — 数据库写入失败时返回
func (c *Core) MarkRead(peerID string) error {
	c.mu.Lock()
	db := c.db
	c.mu.Unlock()
	if db == nil {
		return errors.New("核心尚未启动")
	}
	return db.MarkRead(peerID)
}

// SendText 向对端发送一条文本消息；对方离线时进入离线队列自动重试。
//
// 参数:
//   - peerID: string — 对方 PeerID
//   - text: string — 消息内容，不能为空
//
// 返回:
//   - string — 本地消息 ID（用于气泡回显与撤回）
//   - error — 参数非法或落库失败时返回；网络失败不算错误（消息会排队重试）
func (c *Core) SendText(peerID, text string) (string, error) {
	c.mu.Lock()
	svc := c.chat
	c.mu.Unlock()
	if svc == nil {
		return "", errors.New("核心尚未启动")
	}
	if peerID == "" || text == "" {
		return "", errors.New("peerID 与内容不能为空")
	}
	m, err := svc.Send(peerID, text)
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

// SendFiles 向对端发起文件发送请求（对方接受后才开始传输）。
//
// 参数:
//   - peerID: string — 对方 PeerID
//   - paths: []string — 本地文件或目录的绝对路径；目录会被递归展开
//
// 返回:
//   - string — 首个批次 ID；后续可用 AcceptBatch/TransfersJSON 跟踪
//   - error — 路径不可读、没有可发送内容时返回
//
// 说明：Android 的 SAF URI 需要调用方先复制到应用私有目录，再传真实路径。
func (c *Core) SendFiles(peerID string, paths []string) (string, error) {
	c.mu.Lock()
	svc := c.transfer
	c.mu.Unlock()
	if svc == nil {
		return "", errors.New("核心尚未启动")
	}
	if peerID == "" || len(paths) == 0 {
		return "", errors.New("peerID 与文件列表不能为空")
	}
	return svc.Send(peerID, paths)
}

// AcceptBatch 接受一个待确认的文件批次，开始接收。
//
// 参数:
//   - batch: string — 批次 ID（来自 transfer:offer 事件）
//
// 返回:
//   - error — 批次不存在或已处理时返回
func (c *Core) AcceptBatch(batch string) error {
	c.mu.Lock()
	svc := c.transfer
	c.mu.Unlock()
	if svc == nil {
		return errors.New("核心尚未启动")
	}
	return svc.Decide(batch, true)
}

// RejectBatch 拒绝一个待确认的文件批次。
//
// 参数:
//   - batch: string — 批次 ID
//
// 返回:
//   - error — 批次不存在或已处理时返回
func (c *Core) RejectBatch(batch string) error {
	c.mu.Lock()
	svc := c.transfer
	c.mu.Unlock()
	if svc == nil {
		return errors.New("核心尚未启动")
	}
	return svc.Decide(batch, false)
}

// TransfersJSON 返回传输记录列表。
//
// 参数:
//   - status: string — 状态过滤（pending/receiving/sending/done/failed/canceled），空表示全部
//   - limit: int — 条数上限，<=0 时用 100
//
// 返回:
//   - string — JSON: {"total":N,"items":[Transfer...]}；未启动时返回 {"total":0,"items":[]}
func (c *Core) TransfersJSON(status string, limit int) string {
	c.mu.Lock()
	db := c.db
	c.mu.Unlock()
	if db == nil {
		return `{"total":0,"items":[]}`
	}
	if limit <= 0 {
		limit = 100
	}
	items, total, err := db.Transfers(store.TransferFilter{Status: status, Size: limit})
	if err != nil {
		return `{"total":0,"items":[]}`
	}
	if items == nil {
		return `{"total":0,"items":[]}`
	}
	return toJSON(map[string]any{"total": total, "items": items})
}

// ---------- 设置 ----------

// SettingsJSON 返回移动端可配置项的当前快照。
//
// 参数:
//
//	无
//
// 返回:
//   - string — JSON：
//     {"name","avatar","download_dir","data_dir","concurrency","rate_limit_kbps",
//     "prehash","auto_accept","version","protocol_version"}；核心未启动时返回 "{}"
//
// 用途：设置页一次性拿到全部开关的当前值，避免前端猜默认值。并发/限速/
// 预校验/自动接收都存在 settings 表里，transfer 服务每次使用时重新读取，
// 因此保存后立即生效，不需要重启核心。
func (c *Core) SettingsJSON() string {
	c.mu.Lock()
	n, t, db, dataDir := c.node, c.transfer, c.db, c.dataDir
	c.mu.Unlock()
	if n == nil || db == nil {
		return "{}"
	}
	conc, _ := strconv.Atoi(db.Setting("concurrency", "3"))
	kbps, _ := strconv.Atoi(db.Setting("rate_limit_kbps", "0"))
	dl := ""
	if t != nil {
		dl = t.DownloadDir()
	}
	return toJSON(map[string]any{
		"name":             n.Name(),
		"avatar":           n.Avatar(),
		"download_dir":     dl,
		"data_dir":         dataDir,
		"concurrency":      conc,
		"rate_limit_kbps":  kbps,
		"prehash":          db.Setting("prehash", "0") == "1",
		"auto_accept":      db.Setting("auto_accept", "0") == "1",
		"version":          Version,
		"protocol_version": proto.Version,
	})
}

// SaveSettings 保存移动端设置项。
//
// 参数:
//   - name: string — 本机显示名；空串表示不改（要求 1–40 字符，去首尾空白）
//   - concurrency: int — 并行传输文件数，1–8
//   - rateLimitKbps: int — 全局限速 KB/s，0 表示不限速（上限 1048576）
//   - prehash: bool — 发送前是否先算整文件 SHA-256（接收端可秒跳过已有文件）
//   - autoAccept: bool — 是否对所有设备跳过接收确认弹窗
//
// 返回:
//   - error — 数值越界、昵称非法或写库失败时返回
//
// 说明：自动接收属于「降低安全性换便利」的开关，默认关闭；只想信任单台设备
// 时应改用 SetTrusted，而不是打开它。
func (c *Core) SaveSettings(name string, concurrency int, rateLimitKbps int, prehash, autoAccept bool) error {
	c.mu.Lock()
	n, db := c.node, c.db
	c.mu.Unlock()
	if n == nil || db == nil {
		return errors.New("核心尚未启动")
	}
	if concurrency < 1 || concurrency > 8 {
		return errors.New("并发数需在 1 到 8 之间")
	}
	if rateLimitKbps < 0 || rateLimitKbps > 1<<20 {
		return errors.New("限速需在 0 到 1048576 KB/s 之间（0 表示不限速）")
	}
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	for k, v := range map[string]string{
		"concurrency":     strconv.Itoa(concurrency),
		"rate_limit_kbps": strconv.Itoa(rateLimitKbps),
		"prehash":         b(prehash),
		"auto_accept":     b(autoAccept),
	} {
		if err := db.SetSetting(k, v); err != nil {
			return err
		}
	}
	if name = strings.TrimSpace(name); name != "" && name != n.Name() {
		if err := n.SetName(name); err != nil {
			return err
		}
	}
	return nil
}

// SetAvatar 设置本机头像，空串表示清除。
//
// 参数:
//   - b64: string — base64 编码的 PNG/JPEG，长度 ≤ 16KB（约 12KB 图片）
//
// 返回:
//   - error — 编码无效、超长或写库失败时返回
//
// 说明：图片压缩在 Android 侧完成（见 FstRepository.compressAvatar），
// 这里只做长度与编码校验，避免把大图塞进每一轮 HELLO。
func (c *Core) SetAvatar(b64 string) error {
	c.mu.Lock()
	n := c.node
	c.mu.Unlock()
	if n == nil {
		return errors.New("核心尚未启动")
	}
	return n.SetAvatar(b64)
}

// SetTrusted 设置某台设备的信任状态。受信设备的文件请求会被自动接收。
//
// 参数:
//   - peerID: string — 对方完整 PeerID
//   - trusted: bool — true 信任，false 取消信任
//
// 返回:
//   - error — PeerID 为空或写库失败时返回
//
// 副作用：向界面广播 peer:updated，使设备列表上的信任标记立即更新。
func (c *Core) SetTrusted(peerID string, trusted bool) error {
	c.mu.Lock()
	n := c.node
	c.mu.Unlock()
	if n == nil {
		return errors.New("核心尚未启动")
	}
	if peerID == "" {
		return errors.New("缺少设备 ID")
	}
	// 未发现过的 PeerID 直接写库会「成功」但没有任何记录，界面看起来是开关失灵；
	// 这里先确认设备存在，让错误指向真正的原因。
	if _, ok := n.Store.Peer(peerID); !ok {
		return errors.New("设备不存在")
	}
	if err := n.Store.SetTrusted(peerID, trusted); err != nil {
		return err
	}
	if v, ok := n.PeerView(peerID); ok {
		n.Emit("peer:updated", v)
	}
	return nil
}

// RemovePeer 从本机设备表里删除一台离线设备。
//
// 参数:
//   - peerID: string — 对方完整 PeerID
//
// 返回:
//   - error — PeerID 非法、设备在线或设备不存在时返回
//
// 用途：同一台电脑重装后会换身份（PeerID），旧记录会以同名离线设备堆在列表里；
// 在线设备不允许删除，避免误删正在传输的对端。
func (c *Core) RemovePeer(peerID string) error {
	c.mu.Lock()
	n, db := c.node, c.db
	c.mu.Unlock()
	if n == nil || db == nil {
		return errors.New("核心尚未启动")
	}
	pid, err := peer.Decode(peerID)
	if err != nil {
		return errors.New("设备 ID 无效")
	}
	if n.IsOnline(pid) {
		return errors.New("只能移除离线设备")
	}
	if _, ok := db.Peer(peerID); !ok {
		return errors.New("设备不存在")
	}
	return db.DeletePeer(peerID)
}

// ClearTransfers 清理已结束的传输记录（文件本身不动）。
//
// 参数:
//
//	无
//
// 返回:
//   - int64 — 实际删除的记录数
//   - error — SQL 执行失败时返回
//
// 说明：只删 done/failed/cancelled/rejected 且未被聊天附件引用的行，
// 运行中的传输与聊天里的图片/文件卡片不受影响（见 store.DeleteTransfers）。
func (c *Core) ClearTransfers() (int64, error) {
	c.mu.Lock()
	db := c.db
	c.mu.Unlock()
	if db == nil {
		return 0, errors.New("核心尚未启动")
	}
	return db.DeleteTransfers(nil)
}

// DrainEventsJSON 取走并清空事件缓冲。
//
// 参数:
//
// 返回:
//   - string — JSON 数组 [{"name":"...","payload":...}]；缓冲为空时返回 "[]"
//
// 用途：Android 从后台回到前台、或回调注册晚于事件发生时补拉，避免漏掉
// 新消息、文件请求等一次性事件。
func (c *Core) DrainEventsJSON() string {
	c.mu.Lock()
	list := c.pending
	c.pending = nil
	c.mu.Unlock()
	if len(list) == 0 {
		return "[]"
	}
	return toJSON(list)
}

// emit 是注入给 node/chat/transfer 的事件出口。
//
// 参数:
//   - name: string — 事件名
//   - data: any — 事件载荷，通常是 store 里的结构体或 map
//
// 返回:
//
//	无
//
// 副作用：写入环形缓冲，并在有回调时同步投递给 Android。
func (c *Core) emit(name string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		payload = []byte(`null`)
	}
	c.mu.Lock()
	c.pending = append(c.pending, event{Name: name, Payload: payload})
	if len(c.pending) > maxPendingEvents {
		c.pending = c.pending[len(c.pending)-maxPendingEvents:]
	}
	l := c.listener
	c.mu.Unlock()
	if l != nil {
		l.OnEvent(name, string(payload))
	}
}

// toJSON 把任意值序列化成字符串，失败时返回该类型的空表示。
//
// 参数:
//   - v: any — 待序列化值
//
// 返回:
//   - string — JSON 文本；失败时为 "null"
func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// initLog 把 slog 输出写入 <dataDir>/fst.log，便于用 adb 拉取排查。
//
// 参数:
//   - dir: string — 数据目录
//
// 返回:
//
//	无；打开失败时保持默认输出，不影响启动
//
// 副作用：日志同时写文件与 stderr。移动端的 stderr 会被 FstProcess 采集到
// App 的「核心日志」面板里，只写文件会让现场排查变成黑盒（真实故障时用户
// 看不到任何 slog 输出）。
func initLog(dir string) {
	f, err := os.OpenFile(filepath.Join(dir, "fst.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(f, os.Stderr),
		&slog.HandlerOptions{Level: slog.LevelInfo})))
}
