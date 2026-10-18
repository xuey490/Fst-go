// Package api exposes /api/fst/* to the Vue front end using the template's
// {code, message, data} envelope, so its axios layer and useTable work unchanged.
// Business errors are returned as HTTP 200 + code 400 (the template shows `message`).
package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"

	"fst/internal/chat"
	"fst/internal/node"
	fstproxy "fst/internal/proxy"
	"fst/internal/room"
	"fst/internal/transfer"

	"github.com/libp2p/go-libp2p/core/peer"
)

// Hooks are the desktop-shell capabilities the API needs (native dialogs, tray, autostart).
type Hooks struct {
	PickFiles    func() ([]string, error)
	PickDir      func() (string, error) // 选择下载目录
	PickFolder   func() (string, error) // 选择要发送的文件夹
	Reveal       func(path string)
	Quit         func()
	HideWindow   func()
	Autostart    func() bool
	SetAutostart func(bool) error
	// NetConfig / SaveNetConfig read and write the cross-network part of config.yaml
	// (enable, relays, relay group, file path).
	NetConfig     func() (enable bool, relays []string, group, path string)
	ProxyConfig   func() (enable bool, mode, exit string, routes, domains []string)
	SaveNetConfig func(enable bool, relays []string, group string, proxyEnable bool, proxyMode, proxyExit string, proxyRoutes, proxyDomains []string) error
	// ProxySecurity reads/writes the hardening half of config.yaml (exit token, IPv6, SOCKS5 inbound).
	ProxySecurity     func() ProxySecurity
	SaveProxySecurity func(ProxySecurity) error
}

// ProxySecurity 是 config.yaml 里新增的代理安全/IPv6 选项（保存后重启生效）。
type ProxySecurity struct {
	Token         string   `json:"token"`
	IPv6          bool     `json:"ipv6"`
	SocksEnable   bool     `json:"socks_enable"`
	SocksUser     string   `json:"socks_user"`
	SocksPass     string   `json:"socks_pass"`
	SocksAllow    []string `json:"socks_allow"`
	SocksListen   string   `json:"socks_listen"`
	SocksMaxConn  int      `json:"socks_max_conn"`
	SocksMaxPerIP int      `json:"socks_max_per_ip"`
}

type API struct {
	n *node.Node
	c *chat.Service
	t *transfer.Service
	r *room.Service
	h Hooks
}

func New(n *node.Node, c *chat.Service, t *transfer.Service, r *room.Service, h Hooks) http.Handler {
	a := &API{n: n, c: c, t: t, r: r, h: h}
	m := http.NewServeMux()
	m.HandleFunc("GET /api/fst/self", wrap(a.getSelf))
	m.HandleFunc("PUT /api/fst/self", wrap(a.putSelf))
	m.HandleFunc("GET /api/fst/peers", wrap(a.peers))
	m.HandleFunc("PUT /api/fst/peers/{id}/trusted", wrap(a.trust))
	m.HandleFunc("DELETE /api/fst/peers/{id}", wrap(a.deletePeer))
	m.HandleFunc("POST /api/fst/peers/connect", wrap(a.connect))
	m.HandleFunc("GET /api/fst/conversations", wrap(a.conversations))
	m.HandleFunc("GET /api/fst/messages", wrap(a.messages))
	m.HandleFunc("POST /api/fst/messages", wrap(a.sendMessage))
	m.HandleFunc("POST /api/fst/messages/{id}/recall", wrap(a.recallMessage))
	m.HandleFunc("POST /api/fst/messages/{id}/retry", wrap(a.retryMessage))
	m.HandleFunc("POST /api/fst/messages/read", wrap(a.markRead))
	m.HandleFunc("GET /api/fst/rooms", wrap(a.rooms))
	m.HandleFunc("GET /api/fst/rooms/{id}/messages", wrap(a.roomMessages))
	m.HandleFunc("POST /api/fst/rooms/{id}/messages", wrap(a.sendRoomMessage))
	m.HandleFunc("PUT /api/fst/self/avatar", wrap(a.putAvatar))
	m.HandleFunc("GET /api/fst/media/{id}", a.media)
	m.HandleFunc("GET /api/fst/transfers", wrap(a.transfers))
	m.HandleFunc("POST /api/fst/transfers", wrap(a.send))
	m.HandleFunc("POST /api/fst/transfers/{id}/accept", wrap(a.decide(true)))
	m.HandleFunc("POST /api/fst/transfers/{id}/reject", wrap(a.decide(false)))
	m.HandleFunc("POST /api/fst/transfers/{id}/cancel", wrap(a.cancel))
	m.HandleFunc("POST /api/fst/transfers/{id}/retry", wrap(a.retry))
	m.HandleFunc("POST /api/fst/transfers/batch/{id}/retry", wrap(a.retryBatch))
	m.HandleFunc("POST /api/fst/transfers/clear", wrap(a.clearTransfers))
	m.HandleFunc("POST /api/fst/transfers/{id}/reveal", wrap(a.reveal))
	m.HandleFunc("POST /api/fst/pick-files", wrap(a.pickFiles))
	m.HandleFunc("POST /api/fst/pick-dir", wrap(a.pickDir))
	m.HandleFunc("POST /api/fst/pick-folder", wrap(a.pickFolder))
	m.HandleFunc("GET /api/fst/settings", wrap(a.getSettings))
	m.HandleFunc("PUT /api/fst/settings", wrap(a.putSettings))
	m.HandleFunc("GET /api/fst/proxy/status", wrap(a.proxyStatus))
	m.HandleFunc("GET /api/fst/features", wrap(a.features))
	m.HandleFunc("POST /api/fst/features", wrap(a.setFeature))
	m.HandleFunc("POST /api/fst/app/quit", wrap(func(*http.Request) (any, error) { a.h.Quit(); return nil, nil }))
	m.HandleFunc("POST /api/fst/app/open-download-dir", wrap(func(*http.Request) (any, error) { a.h.Reveal(a.t.DownloadDir()); return nil, nil }))
	m.HandleFunc("POST /api/fst/app/hide", wrap(func(*http.Request) (any, error) { a.h.HideWindow(); return nil, nil }))
	return m
}

type envelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

func wrap(f func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := f(r)
		env := envelope{Code: 200, Message: "ok", Data: data}
		if err != nil {
			env = envelope{Code: 400, Message: err.Error()}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(env)
	}
}

func decode(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return errors.New("请求参数无效")
	}
	return nil
}

func intQ(r *http.Request, k string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(k)); err == nil {
		return v
	}
	return def
}

func (a *API) selfInfo() map[string]any {
	s := a.n.Self()
	return map[string]any{
		"peer_id": s.PeerID, "name": s.Name, "platform": s.Platform, "version": s.Version,
		"port": s.Port, "addrs": s.Addrs, "download_dir": a.t.DownloadDir(),
		"wan": s.WAN, "reachability": s.Reachability, "avatar": s.Avatar,
	}
}

// maxAvatarRaw 是头像源文件的上限（base64 后会 ≤ 16KB，HELLO 才不会被拖大）。
const maxAvatarRaw = 128 << 10

// putAvatar 用本机图片文件设置头像（Go 读文件，字节不经过 JS）。
//
// 参数:
//   - r: *http.Request — JSON 体 {path}；path 为空表示清除头像
//
// 返回:
//   - any — 更新后的本机信息（含 avatar）
//   - error — 文件不存在、超过 128KB、非 PNG/JPEG 或写库失败时返回
func (a *API) putAvatar(r *http.Request) (any, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	path := strings.TrimSpace(in.Path)
	if path == "" {
		if err := a.n.SetAvatar(""); err != nil {
			return nil, err
		}
		return a.selfInfo(), nil
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return nil, errors.New("无法读取图片文件")
	}
	if st.Size() > maxAvatarRaw {
		return nil, errors.New("头像文件不能超过 128KB（建议 64×64 PNG）")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("无法读取图片文件")
	}
	if ct := http.DetectContentType(b); ct != "image/png" && ct != "image/jpeg" {
		return nil, errors.New("仅支持 PNG 或 JPEG 图片")
	}
	if err := a.n.SetAvatar(base64.StdEncoding.EncodeToString(b)); err != nil {
		return nil, err
	}
	return a.selfInfo(), nil
}

func (a *API) getSelf(*http.Request) (any, error) { return a.selfInfo(), nil }

func (a *API) putSelf(r *http.Request) (any, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if err := a.n.SetName(in.Name); err != nil {
		return nil, err
	}
	return a.selfInfo(), nil
}

func (a *API) peers(*http.Request) (any, error) { return a.n.Peers(), nil }

func (a *API) trust(r *http.Request) (any, error) {
	var in struct {
		Trusted bool `json:"trusted"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	if err := a.n.Store.SetTrusted(id, in.Trusted); err != nil {
		return nil, err
	}
	v, _ := a.n.PeerView(id)
	a.n.Emit("peer:updated", v)
	return v, nil
}

func (a *API) deletePeer(r *http.Request) (any, error) {
	id := r.PathValue("id")
	if id == "" {
		return nil, errors.New("缺少设备 ID")
	}
	pid, err := peer.Decode(id)
	if err != nil {
		return nil, errors.New("设备 ID 无效")
	}
	if a.n.IsOnline(pid) {
		return nil, errors.New("只能移除离线设备")
	}
	if _, ok := a.n.Store.Peer(id); !ok {
		return nil, errors.New("设备不存在")
	}
	return nil, a.n.Store.DeletePeer(id)
}

// proxyStatus exposes the live proxy state (startup result + connectivity probe)
// so the settings page can show why the proxy is not working, not just that it is on.
func (a *API) proxyStatus(*http.Request) (any, error) { return fstproxy.Current(), nil }

func (a *API) connect(r *http.Request) (any, error) {
	var in struct {
		Addr string `json:"addr"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, a.n.ConnectString(r.Context(), in.Addr)
}
