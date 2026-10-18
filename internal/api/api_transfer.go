package api

import (
	"errors"
	"fmt"
	"fst/internal/feature"
	"fst/internal/node"
	"fst/internal/store"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

func (a *API) transfers(r *http.Request) (any, error) {
	q := r.URL.Query()
	page, size := intQ(r, "page", 1), intQ(r, "limit", 20)
	list, total, err := a.n.Store.Transfers(store.TransferFilter{
		PeerID: q.Get("peer_id"), Direction: q.Get("direction"), Status: q.Get("status"),
		Keyword: q.Get("keyword"), Page: page, Size: size,
	})
	if list == nil {
		list = []store.Transfer{}
	}
	return map[string]any{"records": list, "total": total, "current": page, "size": size}, err
}

// send 展开并提交文件/文件夹发送；超过 1000 个文件时返回多个批次，并回报被跳过的特殊文件。
//
// 参数:
//   - r: *http.Request — JSON 体 {peer_id, paths}
//
// 返回:
//   - any — {batch_id, batch_ids, files, dirs, skipped}；无跳过项时 skipped 为空数组
//   - error — 对方离线、路径不可读或提交失败时返回
func (a *API) send(r *http.Request) (any, error) {
	var in struct {
		PeerID string   `json:"peer_id"`
		Paths  []string `json:"paths"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	report, err := a.t.SendReport(in.PeerID, in.Paths)
	if err != nil {
		return nil, err
	}
	res := map[string]any{
		"batch_ids": report.BatchIDs, "ids": report.IDs, "files": report.Files,
		"dirs": report.Dirs, "skipped": report.Skipped,
	}
	if len(report.BatchIDs) > 0 {
		res["batch_id"] = report.BatchIDs[0]
	}
	return res, nil
}

// retryBatch 继续一个发送批次：只重发未完成的文件。
//
// 参数:
//   - r: *http.Request — 路径参数 {id} 为原批次 ID
//
// 返回:
//   - any — {batch_id} 新批次 ID
//   - error — 批次不属于本机发送、没有可重试文件或对方离线时返回
func (a *API) retryBatch(r *http.Request) (any, error) {
	b, err := a.t.RetryBatch(r.PathValue("id"))
	return map[string]string{"batch_id": b}, err
}

// decide accepts/rejects a pending incoming offer; {id} is the batch id from the transfer:offer event.
func (a *API) decide(accept bool) func(*http.Request) (any, error) {
	return func(r *http.Request) (any, error) { return nil, a.t.Decide(r.PathValue("id"), accept) }
}

func (a *API) cancel(r *http.Request) (any, error) { return nil, a.t.Cancel(r.PathValue("id")) }

func (a *API) retry(r *http.Request) (any, error) {
	b, err := a.t.Retry(r.PathValue("id"))
	return map[string]string{"batch_id": b}, err
}

func (a *API) reveal(r *http.Request) (any, error) {
	t, ok := a.n.Store.Transfer(r.PathValue("id"))
	if !ok || t.LocalPath == "" {
		return nil, errors.New("文件不存在")
	}
	a.h.Reveal(t.LocalPath)
	return nil, nil
}

func (a *API) pickFiles(*http.Request) (any, error) {
	paths, err := a.h.PickFiles()
	if paths == nil {
		paths = []string{}
	}
	return paths, err
}

func (a *API) pickDir(*http.Request) (any, error) { return a.h.PickDir() }

// clearTransfers 清理**勾选**的传输日志；未勾选的记录保持不动，磁盘文件也不动。
//
// 参数:
//   - r: *http.Request — JSON {"ids":["..."]}；ids 必填，空数组直接拒绝（避免误清全表）
//
// 返回:
//   - any — {"deleted": n}，n 为实际删除的记录数（运行中/被聊天引用的记录会被跳过）
//   - error — 未勾选、请求体非法或数据库失败
func (a *API) clearTransfers(r *http.Request) (any, error) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			return nil, err
		}
	}
	if len(in.IDs) == 0 {
		return nil, errors.New("请先勾选要清理的传输记录")
	}
	n, err := a.n.Store.DeleteTransfers(in.IDs)
	if err != nil {
		return nil, fmt.Errorf("清理传输日志失败: %w", err)
	}
	return map[string]any{"deleted": n}, nil
}

// pickFolder 弹出原生目录选择框，供「发送文件夹」使用（只返回路径，文件字节不经过 JS）。
//
// 返回:
//   - any — 选中的目录路径字符串；用户取消时为空串
//   - error — 对话框创建失败时返回
func (a *API) pickFolder(*http.Request) (any, error) { return a.h.PickFolder() }

type settings struct {
	DownloadDir string `json:"download_dir"`
	Concurrency int    `json:"concurrency"`
	AutoAccept  bool   `json:"auto_accept"`
	Autostart   bool   `json:"autostart"`
	// SingleInstance and CloseAction: read at startup / on window close (see main.go).
	SingleInstance bool   `json:"single_instance"`
	CloseAction    string `json:"close_action"` // ask | tray | quit
	// RateLimitKbps caps the total transfer bandwidth per process (0 = unlimited).
	RateLimitKbps int `json:"rate_limit_kbps"`
	// Prehash computes whole-file SHA-256 before FILE_OFFER so receivers can skip files they already have.
	Prehash bool `json:"prehash"`
	// WanEnabled, Relays and Group live in config.yaml (not the database) and take effect after a restart
	// (the libp2p host is built once). ConfigPath is read-only: where that file is.
	WanEnabled   bool     `json:"wan_enabled"`
	Group        string   `json:"group"`
	Relays       []string `json:"relays"`
	ConfigPath   string   `json:"config_path"`
	ProxyEnable  bool     `json:"proxy_enable"`
	ProxyMode    string   `json:"proxy_mode"`
	ProxyExit    string   `json:"proxy_exit"`
	ProxyRoutes  []string `json:"proxy_routes"`
	ProxyDomains []string `json:"proxy_domains"`
	// ProxyToken/IPv6/Socks* are the hardening options written to config.yaml (restart to apply).
	ProxyToken    string   `json:"proxy_token"`
	ProxyIPv6     bool     `json:"proxy_ipv6"`
	SocksEnable   bool     `json:"socks_enable"`
	SocksUser     string   `json:"socks_user"`
	SocksPass     string   `json:"socks_pass"`
	SocksAllow    []string `json:"socks_allow"`
	SocksListen   string   `json:"socks_listen"`
	SocksMaxConn  int      `json:"socks_max_conn"`
	SocksMaxPerIP int      `json:"socks_max_per_ip"`
	// RelayGroup* is read-only: the group name/expiry/error the relay reported in its HELLO reply.
	RelayGroupName    string `json:"relay_group_name"`
	RelayGroupExpires int64  `json:"relay_group_expires"`
	RelayGroupError   string `json:"relay_group_error"`
}

func (a *API) getSettings(*http.Request) (any, error) {
	c, _ := strconv.Atoi(a.n.Store.Setting("concurrency", "3"))
	wan, relays, group, path := a.h.NetConfig()
	proxyEnable, proxyMode, proxyExit, proxyRoutes, proxyDomains := a.h.ProxyConfig()
	if proxyMode == "" {
		proxyMode = "split"
	}
	if relays == nil {
		relays = []string{}
	}
	if proxyRoutes == nil {
		proxyRoutes = []string{}
	}
	if proxyDomains == nil {
		proxyDomains = []string{}
	}
	sec := a.h.ProxySecurity()
	if sec.SocksAllow == nil {
		sec.SocksAllow = []string{}
	}
	rgName, rgExp, rgErr := a.n.RelayGroup()
	kbps, _ := strconv.Atoi(a.n.Store.Setting("rate_limit_kbps", "0"))
	st := settings{
		DownloadDir: a.t.DownloadDir(), Concurrency: c,
		AutoAccept: a.n.Store.Setting("auto_accept", "0") == "1", Autostart: a.h.Autostart(),
		SingleInstance:    a.n.Store.Setting("single_instance", "1") == "1",
		CloseAction:       a.n.Store.Setting("close_action", "ask"),
		RateLimitKbps:     kbps,
		Prehash:           a.n.Store.Setting("prehash", "0") == "1",
		WanEnabled:        wan,
		Group:             group,
		Relays:            relays,
		ConfigPath:        path,
		ProxyEnable:       proxyEnable,
		ProxyMode:         proxyMode,
		ProxyExit:         proxyExit,
		ProxyRoutes:       proxyRoutes,
		ProxyDomains:      proxyDomains,
		ProxyToken:        sec.Token,
		ProxyIPv6:         sec.IPv6,
		SocksEnable:       sec.SocksEnable,
		SocksUser:         sec.SocksUser,
		SocksPass:         sec.SocksPass,
		SocksAllow:        sec.SocksAllow,
		SocksListen:       sec.SocksListen,
		SocksMaxConn:      sec.SocksMaxConn,
		SocksMaxPerIP:     sec.SocksMaxPerIP,
		RelayGroupName:    rgName,
		RelayGroupExpires: rgExp,
		RelayGroupError:   rgErr,
	}
	// 高级网络功能门禁：锁定态不向界面暴露中继、分组、代理与配置文件路径。
	if !feature.WANUnlocked(a.n.Store) {
		maskWAN(&st)
	}
	return st, nil
}

// maskWAN 把锁定态不可暴露的跨网络/代理字段原地清零（含中继分组状态与配置文件路径）。
//
// 参数:
//   - st: *settings — 待掩码的设置快照，原地修改
func maskWAN(st *settings) {
	st.WanEnabled, st.Relays, st.Group, st.ConfigPath = false, []string{}, "", ""
	st.ProxyEnable, st.ProxyMode, st.ProxyExit = false, "", ""
	st.ProxyRoutes, st.ProxyDomains = []string{}, []string{}
	st.ProxyToken, st.ProxyIPv6, st.SocksEnable = "", false, false
	st.SocksUser, st.SocksPass, st.SocksAllow = "", "", []string{}
	st.SocksListen, st.SocksMaxConn, st.SocksMaxPerIP = "", 0, 0
	st.RelayGroupName, st.RelayGroupExpires, st.RelayGroupError = "", 0, ""
}

func (a *API) putSettings(r *http.Request) (any, error) {
	var in settings
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	unlocked := feature.WANUnlocked(a.n.Store)
	// 高级网络功能门禁：锁定态拒绝任何跨网络/代理字段，其他设置照常保存。
	if !unlocked && (in.WanEnabled || len(in.Relays) > 0 || in.Group != "" ||
		in.ProxyEnable || in.ProxyExit != "" || len(in.ProxyRoutes) > 0 || len(in.ProxyDomains) > 0 ||
		in.ProxyToken != "" || in.ProxyIPv6 || in.SocksEnable || in.SocksUser != "" || in.SocksPass != "" ||
		len(in.SocksAllow) > 0 || in.SocksListen != "" || in.SocksMaxConn != 0 || in.SocksMaxPerIP != 0) {
		return nil, errors.New("当前版本不支持跨网络配置")
	}
	if in.Concurrency < 1 || in.Concurrency > 8 {
		return nil, errors.New("并发数需在 1 到 8 之间")
	}
	if in.RateLimitKbps < 0 || in.RateLimitKbps > 1<<20 {
		return nil, errors.New("限速需在 0 到 1048576 KB/s 之间（0 表示不限速）")
	}
	if in.DownloadDir == "" {
		return nil, errors.New("下载目录不能为空")
	}
	if in.CloseAction != "ask" && in.CloseAction != "tray" && in.CloseAction != "quit" {
		return nil, errors.New("关闭行为取值无效")
	}
	if in.Relays == nil {
		in.Relays = []string{}
	}
	if _, err := node.ParseRelays(in.Relays); err != nil {
		return nil, err
	}
	in.Group = strings.TrimSpace(in.Group)
	if len([]rune(in.Group)) > 200 {
		return nil, errors.New("分组密钥不能超过 200 个字符")
	}
	if in.ProxyMode == "" {
		in.ProxyMode = "split"
	}
	if in.ProxyMode != "split" && in.ProxyMode != "global" {
		return nil, errors.New("代理模式无效")
	}
	if in.ProxyEnable {
		if _, err := node.ParseRelays([]string{in.ProxyExit}); err != nil {
			return nil, fmt.Errorf("代理出口地址无效: %w", err)
		}
		if !in.WanEnabled {
			return nil, errors.New("开启代理前必须开启跨网络连接")
		}
	}
	if in.ProxyRoutes == nil {
		in.ProxyRoutes = []string{}
	}
	if in.ProxyDomains == nil {
		in.ProxyDomains = []string{}
	}
	for _, route := range in.ProxyRoutes {
		p, err := netip.ParsePrefix(strings.TrimSpace(route))
		if err != nil {
			return nil, fmt.Errorf("无效的 CIDR: %s", route)
		}
		if p.Addr().Is6() && !p.Addr().Is4In6() && !in.ProxyIPv6 {
			return nil, fmt.Errorf("IPv6 CIDR 需要先开启代理 IPv6: %s", route)
		}
	}
	var domains []string
	for _, d := range in.ProxyDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		d = strings.TrimPrefix(d, "*.")
		d = strings.TrimPrefix(d, ".")
		d = strings.TrimSuffix(d, ".")
		if d == "" || strings.ContainsAny(d, " /\\") {
			return nil, fmt.Errorf("无效的域名规则: %s", d)
		}
		domains = append(domains, d)
	}
	in.ProxyDomains = domains
	if len(in.ProxyRoutes) == 0 && len(in.ProxyDomains) == 0 && in.ProxyEnable && in.ProxyMode == "split" {
		return nil, errors.New("分流模式至少需要一个 CIDR 或域名")
	}
	// SOCKS5 入站加固：来源必须是回环/私网/ULA；开放局域网来源必须同时设置密码。
	in.SocksUser = strings.TrimSpace(in.SocksUser)
	in.SocksListen = strings.TrimSpace(in.SocksListen)
	if in.SocksAllow == nil {
		in.SocksAllow = []string{}
	}
	for _, cidr := range in.SocksAllow {
		p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			return nil, fmt.Errorf("无效的 SOCKS5 来源 CIDR: %s", cidr)
		}
		a := p.Addr().Unmap()
		if !a.IsLoopback() && !a.IsPrivate() {
			return nil, fmt.Errorf("SOCKS5 来源只允许回环/私网/ULA: %s", cidr)
		}
	}
	// 关闭 SOCKS5 时页面不展示这些参数，也不再强制认证/上限，保留原值以便再次开启。
	if in.SocksEnable {
		if in.SocksUser != "" && in.SocksPass == "" {
			return nil, errors.New("SOCKS5 用户名不能为空密码")
		}
		if in.SocksMaxConn < 0 || in.SocksMaxConn > 1024 || in.SocksMaxPerIP < 0 || in.SocksMaxPerIP > 256 {
			return nil, errors.New("SOCKS5 并发上限超出范围（全局 0-1024，单 IP 0-256）")
		}
		if len(in.SocksAllow) > 0 && in.SocksUser == "" {
			return nil, errors.New("允许局域网来源时必须设置 SOCKS5 用户名密码")
		}
	}
	if in.ProxyToken != "" && len([]rune(in.ProxyToken)) > 200 {
		return nil, errors.New("出口令牌不能超过 200 个字符")
	}
	s := a.n.Store
	b := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	for k, v := range map[string]string{"download_dir": in.DownloadDir, "concurrency": strconv.Itoa(in.Concurrency), "auto_accept": b(in.AutoAccept),
		"single_instance": b(in.SingleInstance), "close_action": in.CloseAction,
		"rate_limit_kbps": strconv.Itoa(in.RateLimitKbps), "prehash": b(in.Prehash)} {
		if err := s.SetSetting(k, v); err != nil {
			return nil, err
		}
	}
	// 跨网络/代理写 config.yaml：锁定态整体跳过，保留文件里已有的 relays/group/proxy 原值。
	if unlocked {
		// only touch config.yaml when the cross-network part actually changed
		wan, relays, group, _ := a.h.NetConfig()
		proxyEnable, proxyMode, proxyExit, proxyRoutes, proxyDomains := a.h.ProxyConfig()
		if in.WanEnabled != wan || in.Group != group || !slices.Equal(in.Relays, relays) ||
			in.ProxyEnable != proxyEnable || in.ProxyMode != proxyMode || in.ProxyExit != proxyExit ||
			!slices.Equal(in.ProxyRoutes, proxyRoutes) || !slices.Equal(in.ProxyDomains, proxyDomains) {
			if err := a.h.SaveNetConfig(in.WanEnabled, in.Relays, in.Group, in.ProxyEnable, in.ProxyMode, in.ProxyExit, in.ProxyRoutes, in.ProxyDomains); err != nil {
				return nil, fmt.Errorf("写入 config.yaml 失败: %w", err)
			}
		}
		// 分组密钥是唯一支持运行时生效的跨网络项；其余 relay/proxy 配置仍需重启。
		if a.n.GroupKey() != in.Group {
			if err := a.n.SetGroup(in.Group); err != nil {
				return nil, fmt.Errorf("应用分组密钥失败: %w", err)
			}
		}
		// 代理安全/IPv6/SOCKS5 入站同样写在 config.yaml，重启生效。
		sec := a.h.ProxySecurity()
		want := ProxySecurity{
			Token: in.ProxyToken, IPv6: in.ProxyIPv6, SocksEnable: in.SocksEnable,
			SocksUser: in.SocksUser, SocksPass: in.SocksPass, SocksAllow: in.SocksAllow,
			SocksListen: in.SocksListen, SocksMaxConn: in.SocksMaxConn, SocksMaxPerIP: in.SocksMaxPerIP,
		}
		if want.SocksAllow == nil {
			want.SocksAllow = []string{}
		}
		if sec.SocksAllow == nil {
			sec.SocksAllow = []string{}
		}
		if want.Token != sec.Token || want.IPv6 != sec.IPv6 || want.SocksEnable != sec.SocksEnable ||
			want.SocksUser != sec.SocksUser || want.SocksPass != sec.SocksPass || want.SocksListen != sec.SocksListen ||
			want.SocksMaxConn != sec.SocksMaxConn || want.SocksMaxPerIP != sec.SocksMaxPerIP ||
			!slices.Equal(want.SocksAllow, sec.SocksAllow) {
			if err := a.h.SaveProxySecurity(want); err != nil {
				return nil, fmt.Errorf("写入 config.yaml 失败: %w", err)
			}
		}
		_, _, _, in.ConfigPath = a.h.NetConfig()
	}
	if in.Autostart != a.h.Autostart() {
		if err := a.h.SetAutostart(in.Autostart); err != nil {
			return nil, err
		}
	}
	// 锁定态不在 PUT 响应里回填跨网络/代理字段，与 GET 保持同一掩码。
	if !unlocked {
		maskWAN(&in)
	}
	return in, nil
}
