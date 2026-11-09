// Command fstcored 是 FST 移动端的 Go 核心守护进程。
//
// 为什么是独立进程而不是 gomobile 静态库：
//
//	Android 端的 Go 核心如果以 AAR 形式在进程内调用，必须用 gomobile 生成
//	JNI 胶水，而 gomobile 依赖 cgo + NDK 的 clang。该链路在 Windows 上不稳定
//	（cgo 探针会挂死），且一旦失败整条发布链就断了。改成「纯 Go 静态可执行文件
//	+ 本机回环 HTTP」后：不需要 cgo、不需要 NDK，协议与桌面端完全一致，
//	崩溃隔离（核心挂了 UI 还能提示重启），并且同一份核心可以直接给 iOS 用。
//
// 通信约定：
//   - 只监听 127.0.0.1 的随机端口，启动后在 stdout 打印 "LISTEN <addr>"。
//   - 每个请求必须带 X-FST-Token 头，值来自环境变量 FST_TOKEN（Android 每次
//     启动随机生成并只交给本应用）。
//   - 响应体一律是 JSON；列表类直接转发 core 层的 JSON 字符串，避免二次编解码。
//   - 事件用 SSE（text/event-stream）下发，格式 event: <name> / data: <json>。
//
// 由 Android 侧的 FstProcess 拉起与关闭，正常退出路径见 handleShutdown。
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"fst/phone/core"
)

// tokenHeader 是 Android → 守护进程的鉴权头。
const tokenHeader = "X-FST-Token"

func main() {
	dataDir := os.Getenv("FST_DATA_DIR")
	downloadDir := os.Getenv("FST_DOWNLOAD_DIR")
	deviceName := os.Getenv("FST_DEVICE_NAME")
	lanIP := os.Getenv("FST_LAN_IP")
	token := os.Getenv("FST_TOKEN")

	if dataDir == "" || token == "" {
		fmt.Fprintln(os.Stderr, "缺少必需环境变量 FST_DATA_DIR / FST_TOKEN")
		os.Exit(2)
	}

	c := core.New()
	// FST_PORT 仅用于同机多实例联调（Windows 允许重复绑定同一端口，不隔离就分不清
	// 连接进了哪个进程）；真机不设置，永远用 8787。
	var opts []core.Option
	if v := os.Getenv("FST_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			opts = append(opts, core.WithPort(p))
		}
	}
	if err := c.Start(dataDir, downloadDir, deviceName, lanIP, opts...); err != nil {
		fmt.Fprintf(os.Stderr, "核心启动失败: %v\n", err)
		os.Exit(1)
	}
	defer c.Stop()

	d := &daemon{core: c, token: token, hub: newHub()}
	// core 的事件回调来自任意协程，hub.broadcast 内部加锁，可直接转发。
	c.SetListener(listenerFunc(func(name, payload string) {
		d.hub.broadcast(name, payload)
	}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "监听失败: %v\n", err)
		os.Exit(1)
	}
	// Android 侧读取这一行拿到端口；必须立即 flush，否则 bufio 会攒着。
	fmt.Printf("LISTEN %s\n", ln.Addr().String())
	os.Stdout.Sync()

	srv := &http.Server{Handler: d.routes(), ReadHeaderTimeout: 10 * time.Second}

	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		<-ch
		d.hub.close()
		_ = srv.Close()
	}()

	slog.Info("fstcored 已启动", "addr", ln.Addr().String())
	// 现场诊断：Android 11+ 上这里必然失败（NETLINK_ROUTE 被禁用），属于预期。
	// 输出到 stderr 后 App 的「核心日志」面板能直接看到，省去 logcat。
	if ifs, err := net.Interfaces(); err != nil {
		slog.Warn("网卡枚举失败，发现由系统 NsdManager 负责", "err", err)
	} else {
		slog.Info("网卡枚举成功", "count", len(ifs))
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "服务异常退出: %v\n", err)
		os.Exit(1)
	}
}

// listenerFunc 把函数适配成 core.Listener。
type listenerFunc func(name, payload string)

// OnEvent 实现 core.Listener，把事件转给 SSE hub。
//
// 参数:
//   - name: string — 事件名
//   - payload: string — JSON 载荷
func (f listenerFunc) OnEvent(name, payload string) { f(name, payload) }

// daemon 持有核心实例与鉴权令牌。
type daemon struct {
	core  *core.Core
	token string
	hub   *hub
}

// routes 装配全部本地 IPC 路由。
//
// 返回:
//   - http.Handler — 已套上鉴权中间件的 mux
func (d *daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", d.handleHealth)
	mux.HandleFunc("GET /self", d.handleSelf)
	mux.HandleFunc("GET /peers", d.handlePeers)
	mux.HandleFunc("GET /conversations", d.handleConversations)
	mux.HandleFunc("GET /messages", d.handleMessages)
	mux.HandleFunc("GET /transfers", d.handleTransfers)
	mux.HandleFunc("GET /settings", d.handleSettings)
	mux.HandleFunc("POST /settings", d.handleSaveSettings)
	mux.HandleFunc("POST /avatar", d.handleAvatar)
	mux.HandleFunc("POST /trust", d.handleTrust)
	mux.HandleFunc("POST /peers/remove", d.handleRemovePeer)
	mux.HandleFunc("POST /transfers/clear", d.handleClearTransfers)
	mux.HandleFunc("GET /events", d.hub.serve)
	mux.HandleFunc("POST /send", d.handleSend)
	mux.HandleFunc("POST /sendfiles", d.handleSendFiles)
	mux.HandleFunc("POST /accept", d.handleDecide(true))
	mux.HandleFunc("POST /reject", d.handleDecide(false))
	mux.HandleFunc("POST /markread", d.handleMarkRead)
	mux.HandleFunc("POST /discovered", d.handleDiscovered)
	mux.HandleFunc("POST /lanip", d.handleLANIP)
	mux.HandleFunc("POST /shutdown", d.handleShutdown)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(tokenHeader) != d.token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// handleHealth 返回就绪状态，Android 用它等待核心可用。
func (d *daemon) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeRaw(w, `{"ok":true}`)
}

// handleSelf 返回本机信息。
func (d *daemon) handleSelf(w http.ResponseWriter, _ *http.Request) {
	writeRaw(w, d.core.SelfJSON())
}

// handlePeers 返回设备列表。
func (d *daemon) handlePeers(w http.ResponseWriter, _ *http.Request) {
	writeRaw(w, d.core.PeersJSON())
}

// handleConversations 返回会话列表。
func (d *daemon) handleConversations(w http.ResponseWriter, _ *http.Request) {
	writeRaw(w, d.core.ConversationsJSON())
}

// handleMessages 返回某个会话的历史消息。
//
// 查询参数: peer（必填）、before（毫秒，0 表示最新）、limit（默认 50）。
func (d *daemon) handleMessages(w http.ResponseWriter, r *http.Request) {
	peer := r.URL.Query().Get("peer")
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	writeRaw(w, d.core.MessagesJSON(peer, before, limit))
}

// handleTransfers 返回传输列表。
//
// 查询参数: status（状态过滤，可空）、limit（默认 100）。
func (d *daemon) handleTransfers(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	writeRaw(w, d.core.TransfersJSON(status, limit))
}

// handleSettings 返回可配置项的当前值（昵称、头像、并发、限速、开关、版本）。
func (d *daemon) handleSettings(w http.ResponseWriter, _ *http.Request) {
	writeRaw(w, d.core.SettingsJSON())
}

// settingsReq 是 POST /settings 的请求体，字段与 SettingsJSON 的读侧一致。
type settingsReq struct {
	Name          string `json:"name"`
	Concurrency   int    `json:"concurrency"`
	RateLimitKbps int    `json:"rate_limit_kbps"`
	Prehash       bool   `json:"prehash"`
	AutoAccept    bool   `json:"auto_accept"`
}

// handleSaveSettings 保存昵称、并发、限速与开关。
//
// 说明：并发/限速/预校验/自动接收由 transfer 服务在每次使用时重新读取，
// 保存后立即生效；昵称会立即向已连接对端重发 HELLO。
func (d *daemon) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var in settingsReq
	if !decode(w, r, &in) {
		return
	}
	err := d.core.SaveSettings(in.Name, in.Concurrency, in.RateLimitKbps, in.Prehash, in.AutoAccept)
	writeResult(w, map[string]any{"ok": err == nil}, err)
}

// handleAvatar 设置本机头像（base64 PNG/JPEG，≤16KB）。
//
// 图片压缩在 Android 侧完成：Go 只校验长度与编码，避免大图进入每轮 HELLO。
func (d *daemon) handleAvatar(w http.ResponseWriter, r *http.Request) {
	var in struct {
		B64 string `json:"b64"`
	}
	if !decode(w, r, &in) {
		return
	}
	writeResult(w, map[string]any{"ok": true}, d.core.SetAvatar(in.B64))
}

// handleTrust 切换某台设备的信任状态（受信设备的文件会被自动接收）。
func (d *daemon) handleTrust(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Peer    string `json:"peer"`
		Trusted bool   `json:"trusted"`
	}
	if !decode(w, r, &in) {
		return
	}
	writeResult(w, map[string]any{"ok": true}, d.core.SetTrusted(in.Peer, in.Trusted))
}

// handleRemovePeer 移除一台离线设备记录（清理同名重装留下的旧身份）。
func (d *daemon) handleRemovePeer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Peer string `json:"peer"`
	}
	if !decode(w, r, &in) {
		return
	}
	writeResult(w, map[string]any{"ok": true}, d.core.RemovePeer(in.Peer))
}

// handleClearTransfers 清理已结束的传输记录，返回删除条数。
func (d *daemon) handleClearTransfers(w http.ResponseWriter, _ *http.Request) {
	n, err := d.core.ClearTransfers()
	writeResult(w, map[string]any{"deleted": n}, err)
}

// sendReq 是 /send 与 /sendfiles 的请求体。
type sendReq struct {
	Peer  string   `json:"peer"`
	Text  string   `json:"text"`
	Paths []string `json:"paths"`
}

// handleSend 发送一条文本消息。
func (d *daemon) handleSend(w http.ResponseWriter, r *http.Request) {
	var in sendReq
	if !decode(w, r, &in) {
		return
	}
	id, err := d.core.SendText(in.Peer, in.Text)
	writeResult(w, map[string]any{"id": id}, err)
}

// handleSendFiles 发起文件传输请求（路径必须是应用可读的真实路径）。
func (d *daemon) handleSendFiles(w http.ResponseWriter, r *http.Request) {
	var in sendReq
	if !decode(w, r, &in) {
		return
	}
	batch, err := d.core.SendFiles(in.Peer, in.Paths)
	writeResult(w, map[string]any{"batch_id": batch}, err)
}

// handleDecide 生成接受/拒绝文件批次的处理器。
//
// 参数:
//   - accept: bool — true 接收，false 拒绝
//
// 返回:
//   - http.HandlerFunc — 读取 {"batch":"..."} 并调用核心
func (d *daemon) handleDecide(accept bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Batch string `json:"batch"`
		}
		if !decode(w, r, &in) {
			return
		}
		var err error
		if accept {
			err = d.core.AcceptBatch(in.Batch)
		} else {
			err = d.core.RejectBatch(in.Batch)
		}
		writeResult(w, map[string]any{"ok": err == nil}, err)
	}
}

// handleMarkRead 把某个会话标记为已读。
func (d *daemon) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Peer string `json:"peer"`
	}
	if !decode(w, r, &in) {
		return
	}
	writeResult(w, map[string]any{"ok": true}, d.core.MarkRead(in.Peer))
}

// handleDiscovered 接收 Android 系统 DNS-SD（NsdManager）解析出的对端地址。
//
// 请求体: {"addr":"/ip4/192.168.1.15/tcp/8787/p2p/12D3KooW..."}
//
// 之所以由 Android 侧做发现：Android 11+ 禁止应用使用 NETLINK_ROUTE，
// Go 的 net.Interfaces() 必然失败，zeroconf 无法枚举网卡；系统 mdnsd 又常驻
// 占用 5353。libp2p 的传输/加密/分帧仍在 Go 侧，这里只是把「地址」送进来。
func (d *daemon) handleDiscovered(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Addr string `json:"addr"`
	}
	if !decode(w, r, &in) {
		return
	}
	writeResult(w, map[string]any{"ok": true}, d.core.AddPeer(in.Addr))
}

// handleLANIP 更新对外广播的本机 IPv4（Wi-Fi 重连、IP 变化时由 Android 侧调用）。
//
// 请求体: {"ip":"192.168.1.13"}；ip 为空表示交还给系统枚举。
//
// 为什么需要动态更新：libp2p 的地址是启动时枚举的，Android 上又枚举不到网卡，
// 只能由 Kotlin 从 ConnectivityManager 取。用户在 App 运行期间换 Wi-Fi 时，
// 不重启核心也能让对端学到新地址。
func (d *daemon) handleLANIP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IP string `json:"ip"`
	}
	if !decode(w, r, &in) {
		return
	}
	writeResult(w, map[string]any{"ok": true}, d.core.SetLANIP(in.IP))
}

// handleShutdown 让进程优雅退出（Android 侧也会直接 kill 作为兜底）。
func (d *daemon) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	writeRaw(w, `{"ok":true}`)
	go func() {
		time.Sleep(80 * time.Millisecond) // 让响应先写回
		d.hub.close()
		os.Exit(0)
	}()
}

// decode 解析 JSON 请求体，失败时已写好响应。
//
// 参数:
//   - w: http.ResponseWriter — 响应
//   - r: *http.Request — 请求
//   - v: any — 目标结构体指针
//
// 返回:
//   - bool — true 表示解析成功，可继续处理
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	defer func() { _ = r.Body.Close() }()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"请求格式错误"}`))
		return false
	}
	return true
}

// writeResult 把结果与错误编码成统一响应。
//
// 参数:
//   - w: http.ResponseWriter — 响应
//   - data: map[string]any — 成功载荷
//   - err: error — 业务错误；非 nil 时返回 200 + error 字段（与桌面 API 信封一致）
func writeResult(w http.ResponseWriter, data map[string]any, err error) {
	body := map[string]any{"data": data}
	if err != nil {
		body["error"] = err.Error()
	}
	b, _ := json.Marshal(body)
	writeRaw(w, string(b))
}

// writeRaw 写出一段已经是 JSON 的响应体。
func writeRaw(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(s))
}

// hub 是 SSE 广播中心：把 core.Listener 的事件分发给所有已连接的客户端。
type hub struct {
	mu      sync.Mutex
	clients map[chan eventMsg]struct{}
	closed  bool
}

// eventMsg 是一条待下发的 SSE 消息。
type eventMsg struct {
	Name string
	Data string
}

// newHub 创建广播中心。
func newHub() *hub { return &hub{clients: make(map[chan eventMsg]struct{})} }

// broadcast 向所有客户端投递；客户端缓冲满时丢弃该条，避免拖慢核心。
//
// 参数:
//   - name: string — 事件名
//   - data: string — JSON 载荷
func (h *hub) broadcast(name, data string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for ch := range h.clients {
		select {
		case ch <- eventMsg{Name: name, Data: data}:
		default:
		}
	}
}

// close 关闭所有连接并拒绝后续广播。
func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for ch := range h.clients {
		close(ch)
		delete(h.clients, ch)
	}
}

// serve 处理 GET /events：长连接推送事件。
func (h *hub) serve(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	ch := make(chan eventMsg, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if _, ok := h.clients[ch]; ok {
			delete(h.clients, ch)
		}
		h.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	// 首帧注释：立即把响应头刷给客户端，避免 Android 侧卡在等待。
	_, _ = w.Write([]byte(": connected\n\n"))
	flusher.Flush()

	bw := bufio.NewWriter(w)
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			// SSE 的多行 data 需要逐行加前缀；这里载荷是单行 JSON。
			fmt.Fprintf(bw, "event: %s\ndata: %s\n\n", msg.Name, strings.ReplaceAll(msg.Data, "\n", ""))
			if err := bw.Flush(); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(bw, ": ping\n\n")
			if err := bw.Flush(); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
