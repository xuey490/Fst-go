// Package bootstrap 装配 FST 与界面无关的核心运行时：配置、节点、聊天、传输、
// 群聊、代理与 /api/fst handler。桌面模式（Wails）与服务模式（fst serve）共用
// 同一个 Assemble，只注入不同的 Emitter 与桌面 Hooks，保证两种形态行为一致。
package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"fst/internal/api"
	"fst/internal/chat"
	"fst/internal/feature"
	"fst/internal/node"
	fstproxy "fst/internal/proxy"
	"fst/internal/room"
	"fst/internal/store"
	"fst/internal/transfer"

	ma "github.com/multiformats/go-multiaddr"
)

// Emitter 是 Go → UI 的单向事件推送抽象。桌面模式注入 Wails Event.Emit 包装，
// 服务模式注入 SSE hub 广播；事件名与载荷在两种模式下完全一致。
type Emitter func(name string, data any)

// Options 是 Assemble 的注入参数。DB/DataDir/Port 来自 OpenInstance，两种模式相同；
// GUI 只需要填桌面专属钩子（对话框/托盘/自启等），服务模式留零值即可（自动补中性实现）。
type Options struct {
	DB      *store.Store // 已打开的数据库（OpenInstance 的返回值）
	DataDir string       // 数据目录（身份密钥、TUN 设备名等）
	Port    int          // libp2p 监听端口（DefaultPort 或 FST_PORT 覆盖值）
	Emit    Emitter      // Go → UI 事件推送；nil 时丢弃所有事件
	GUI     api.Hooks    // 桌面专属钩子；NetConfig/ProxySecurity 等配置钩子由本包内部填
}

// Result 是装配完成后的运行时句柄。API 交给 HTTP 服务方（Wails 资产中间件或
// serve mux）；Shutdown 按依赖逆序关闭 socks/TUN/群聊/节点，但不关 DB（归调用方所有）。
type Result struct {
	Node     *node.Node
	Chat     *chat.Service
	Transfer *transfer.Service
	Room     *room.Service // 群聊启动失败时为 nil（节点其余功能不受影响）
	API      http.Handler
	Relays   []string // 校验后的有效中继列表
	WAN      bool     // 高级网络功能是否已解锁
	Shutdown func()   // 幂等；关闭 socks、TUN、room、node（不含 DB）
}

// Assemble 完成与界面无关的全部启动装配：加载 config.yaml、功能门禁、创建节点、
// 聊天/传输/群聊服务、按配置启动 SOCKS5 入站与 TUN 代理，并产出 /api/fst handler。
// 桌面与服务两种模式共用，装配失败（节点起不来）返回 error。
//
// 参数:
//   - o: Options — 注入的 DB/目录/端口/Emitter 与桌面 Hooks
//
// 返回:
//   - *Result — 运行时句柄；Shutdown 必须由调用方在进程退出前调用一次
//   - error — node.New 失败时返回；其余子系统失败只降级并记录日志
func Assemble(o Options) (*Result, error) {
	emit := o.Emit
	if emit == nil {
		emit = func(string, any) {}
	}
	db, dataDir, port := o.DB, o.DataDir, o.Port

	// cross-network settings live in config.yaml; the first run migrates the old database values into it
	cfgPaths := ConfigPaths()
	ncfg, cfgPath, err := LoadNetConfig(cfgPaths)
	if err != nil {
		slog.Warn("config.yaml is invalid, cross-network settings ignored", "path", cfgPath, "err", err)
	} else if cfgPath == "" {
		if lc, ok := LegacyNetConfig(db.Setting); ok {
			ncfg = lc
			if cfgPath, err = SaveNetConfig("", ncfg, cfgPaths); err != nil {
				slog.Warn("could not write config.yaml", "err", err)
			}
		}
	}
	// a typo in a hand-edited file must not stop the app from starting: drop bad relay addresses with a warning
	var goodRelays []string
	for _, r := range ncfg.Relays {
		if _, err := node.ParseRelays([]string{r}); err != nil {
			slog.Warn("ignoring invalid relay in config.yaml", "relay", r, "err", err)
			continue
		}
		goodRelays = append(goodRelays, r)
	}
	var cfgMu sync.Mutex

	// 高级网络功能门禁：默认关闭，解锁入口见 docs/开发文档.md。
	unlocked := feature.WANUnlocked(db)
	if unlocked {
		slog.Info("相关功能已经完全解锁")
	}
	wanRelays, wanGroup := goodRelays, ncfg.Group
	if !unlocked {
		// 锁定态不把中继与分组密钥交给节点，避免进入 HELLO。
		wanRelays, wanGroup = nil, ""
	}
	n, err := node.New(node.Config{DataDir: dataDir, Port: port, Store: db, Emit: emit, WAN: unlocked, Relays: wanRelays, Group: wanGroup})
	if err != nil {
		return nil, err
	}
	chatSvc := chat.New(n)
	transferSvc := transfer.New(n)
	roomSvc, err := room.New(n)
	if err != nil {
		slog.Warn("group chat disabled", "err", err)
	}
	var proxyTUN *fstproxy.TUN
	var socks *fstproxy.SOCKS5
	publishProxy := func(s *fstproxy.Status) {
		fstproxy.SetStatus(s)
		emit("proxy:status", s)
	}
	if unlocked {
		publishProxy(&fstproxy.Status{Enabled: ncfg.Proxy.Enable, Mode: ncfg.Proxy.Mode})
	} else {
		// 锁定态不把代理模式等配置回填给界面/事件。
		publishProxy(&fstproxy.Status{})
	}
	if unlocked && ncfg.Proxy.Enable {
		exit := ncfg.Proxy.Exit
		if exit == "" && len(goodRelays) > 0 {
			exit = goodRelays[0]
		}
		client, err := fstproxy.NewClient(n.Host, exit)
		if err != nil {
			publishProxy(&fstproxy.Status{Enabled: true, Error: err.Error()})
		} else {
			client.SetToken(ncfg.Proxy.Token)
			// Local SOCKS5 inbound works without admin rights or wintun.dll;
			// browsers/system proxy can use it even if the TUN fails below.
			// Fixed port so the user can keep it in browser settings; fall
			// back to an auto port when occupied.
			if ncfg.Proxy.SocksEnabled() {
				socksOpt, socksListen := socksOptions(ncfg.Proxy)
				if socksListen == "" {
					socksListen = "127.0.0.1:20300"
				}
				socks, err = fstproxy.StartSOCKS5(client, socksListen, socksOpt)
				if err != nil {
					slog.Warn("SOCKS5 inbound failed; falling back to loopback without auth", "err", err)
					socks, err = fstproxy.StartSOCKS5(client, "", fstproxy.SocksOptions{})
				}
				if err != nil {
					slog.Warn("local SOCKS5 inbound failed", "err", err)
				}
			} else {
				slog.Info("SOCKS5 inbound disabled by config")
			}
			mode := ncfg.Proxy.Mode
			if mode == "" {
				mode = "split"
			}
			st := &fstproxy.Status{Enabled: true, Mode: mode, ExitPeer: client.ExitPeer().String()}
			if socks != nil {
				st.SocksAddr = socks.Addr()
			}
			proxyTUN, err = fstproxy.StartTUN(client, fstproxy.TUNConfig{
				Mode: mode, Routes: ncfg.Proxy.Routes, Domains: ncfg.Proxy.Domains, ExitIPv4: client.ExitIPv4(),
				IPv6: ncfg.Proxy.IPv6, ExitIPv6: exitIPv6FromMultiaddr(exit),
			})
			if err != nil {
				st.Error = err.Error()
				publishProxy(st)
				slog.Error("network proxy could not start; FST will continue without it", "err", err)
			} else {
				st.Running = true
				publishProxy(st)
				slog.Info("network proxy started", "mode", mode, "exit", client.ExitPeer(), "socks", st.SocksAddr)
				// Cached answers include fake-IPs from the previous run that
				// the fresh nameTable no longer knows; drop them up front.
				go fstproxy.FlushSystemDNSCache()
				// Connectivity self-check through the exit; result only affects UI state.
				go func() {
					pctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					pc, perr := client.DialContext(pctx, "tcp4", "1.1.1.1:80")
					if perr == nil {
						_ = pc.Close()
						st.Probe = "ok"
					} else {
						st.Probe = perr.Error()
					}
					publishProxy(st)
				}()
			}
		}
	}
	slog.Info("node started", "peer", n.Host.ID(), "addrs", n.Host.Addrs())

	hooks := o.GUI
	hooks.NetConfig = func() (enable bool, relays []string, group, path string) {
		if !unlocked {
			return false, nil, "", ""
		}
		cfgMu.Lock()
		defer cfgMu.Unlock()
		return ncfg.EnableRelay, ncfg.Relays, ncfg.Group, cfgPath
	}
	hooks.ProxyConfig = func() (enable bool, mode, exit string, routes, domains []string) {
		if !unlocked {
			return false, "", "", nil, nil
		}
		cfgMu.Lock()
		defer cfgMu.Unlock()
		return ncfg.Proxy.Enable, ncfg.Proxy.Mode, ncfg.Proxy.Exit, ncfg.Proxy.Routes, ncfg.Proxy.Domains
	}
	hooks.SaveNetConfig = func(enable bool, relays []string, group string, proxyEnable bool, proxyMode, proxyExit string, proxyRoutes, proxyDomains []string) error {
		if !unlocked {
			return errors.New("当前版本不支持跨网络配置")
		}
		cfgMu.Lock()
		defer cfgMu.Unlock()
		// 从当前配置复制，保留同一张 proxy 卡片里的令牌/IPv6/SOCKS5 加固项。
		next := ncfg
		next.EnableRelay = enable
		next.Relays = relays
		next.Group = group
		next.Proxy.Enable = proxyEnable
		next.Proxy.Mode = proxyMode
		next.Proxy.Exit = proxyExit
		next.Proxy.Routes = proxyRoutes
		next.Proxy.Domains = proxyDomains
		p, err := SaveNetConfig(cfgPath, next, cfgPaths)
		if err != nil {
			return err
		}
		ncfg, cfgPath = next, p
		ncfg.Relays = CleanRelays(ncfg.Relays)
		return nil
	}
	hooks.ProxySecurity = func() api.ProxySecurity {
		if !unlocked {
			return api.ProxySecurity{}
		}
		cfgMu.Lock()
		defer cfgMu.Unlock()
		p := ncfg.Proxy
		if p.SocksAllow == nil {
			p.SocksAllow = []string{}
		}
		return api.ProxySecurity{
			Token: p.Token, IPv6: p.IPv6, SocksEnable: p.SocksEnabled(),
			SocksUser: p.SocksUser, SocksPass: p.SocksPass, SocksAllow: p.SocksAllow,
			SocksListen: p.SocksListen, SocksMaxConn: p.SocksMaxConn, SocksMaxPerIP: p.SocksMaxPerIP,
		}
	}
	hooks.SaveProxySecurity = func(in api.ProxySecurity) error {
		if !unlocked {
			return errors.New("当前版本不支持跨网络配置")
		}
		cfgMu.Lock()
		defer cfgMu.Unlock()
		next := ncfg
		next.Proxy.Token = strings.TrimSpace(in.Token)
		next.Proxy.IPv6 = in.IPv6
		enabled := in.SocksEnable
		next.Proxy.SocksEnable = &enabled
		next.Proxy.SocksUser = strings.TrimSpace(in.SocksUser)
		next.Proxy.SocksPass = in.SocksPass
		next.Proxy.SocksAllow = in.SocksAllow
		next.Proxy.SocksListen = strings.TrimSpace(in.SocksListen)
		next.Proxy.SocksMaxConn = in.SocksMaxConn
		next.Proxy.SocksMaxPerIP = in.SocksMaxPerIP
		p, err := SaveNetConfig(cfgPath, next, cfgPaths)
		if err != nil {
			return err
		}
		ncfg, cfgPath = next, p
		return nil
	}
	handler := api.New(n, chatSvc, transferSvc, roomSvc, fillGUIHooks(hooks))

	res := &Result{
		Node: n, Chat: chatSvc, Transfer: transferSvc, Room: roomSvc,
		API: handler, Relays: goodRelays, WAN: unlocked,
	}
	var once sync.Once
	res.Shutdown = func() {
		once.Do(func() {
			if socks != nil {
				socks.Close()
			}
			if proxyTUN != nil {
				proxyTUN.Close()
			}
			if roomSvc != nil {
				roomSvc.Close()
			}
			n.Close()
		})
	}
	return res, nil
}

// fillGUIHooks 为未注入的桌面专属钩子补上中性实现，避免服务模式（serve）里
// 调用 nil 函数 panic；配置类钩子（NetConfig 等）不在此列，由 Assemble 统一填。
//
// 参数:
//   - h: api.Hooks — 调用方注入的钩子（零值字段视为服务模式）
//
// 返回:
//   - api.Hooks — 所有函数字段均非 nil 的钩子
func fillGUIHooks(h api.Hooks) api.Hooks {
	const msg = "此操作需要在桌面客户端中执行"
	if h.PickFiles == nil {
		h.PickFiles = func() ([]string, error) { return nil, errors.New(msg) }
	}
	if h.PickDir == nil {
		h.PickDir = func() (string, error) { return "", errors.New(msg) }
	}
	if h.PickFolder == nil {
		h.PickFolder = func() (string, error) { return "", errors.New(msg) }
	}
	if h.Reveal == nil {
		h.Reveal = func(string) {}
	}
	if h.Quit == nil {
		h.Quit = func() {}
	}
	if h.HideWindow == nil {
		h.HideWindow = func() {}
	}
	if h.Autostart == nil {
		h.Autostart = func() bool { return false }
	}
	if h.SetAutostart == nil {
		h.SetAutostart = func(bool) error { return errors.New(msg) }
	}
	return h
}

// socksOptions 把 config.yaml 的 SOCKS5 加固项转成代理包选项与监听地址。
//
// 参数:
//   - p: ProxyConfig — config.yaml 的 proxy 段
//
// 返回:
//   - fstproxy.SocksOptions — 用户名/密码、来源 CIDR、并发上限；CIDR 解析失败时记录 Warn 并忽略该项
//   - string — 监听地址；空表示用默认 127.0.0.1:20300。配置了来源白名单但没写地址时绑 0.0.0.0
func socksOptions(p ProxyConfig) (fstproxy.SocksOptions, string) {
	opt := fstproxy.SocksOptions{
		User: p.SocksUser, Pass: p.SocksPass,
		MaxTotal: p.SocksMaxConn, MaxPerIP: p.SocksMaxPerIP,
	}
	for _, text := range p.SocksAllow {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(text))
		if err != nil {
			slog.Warn("invalid SOCKS5 allow CIDR ignored", "cidr", text, "err", err)
			continue
		}
		opt.Allow = append(opt.Allow, prefix)
	}
	listen := strings.TrimSpace(p.SocksListen)
	if listen == "" && len(opt.Allow) > 0 {
		listen = "0.0.0.0:20300"
	}
	return opt, listen
}

// exitIPv6FromMultiaddr 取出中继 multiaddr 里的 /ip6/ 地址（供 TUN 绕行使用）。
//
// 参数:
//   - s: string — 代理出口 multiaddr（可能为空或只含 IPv4/DNS）
//
// 返回:
//   - string — IPv6 字面量；没有 /ip6/ 组件时为空
func exitIPv6FromMultiaddr(s string) string {
	maAddr, err := ma.NewMultiaddr(s)
	if err != nil {
		return ""
	}
	v, err := maAddr.ValueForProtocol(ma.P_IP6)
	if err != nil {
		return ""
	}
	return v
}

