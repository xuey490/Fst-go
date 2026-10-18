package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

//go:embed all:front/dist
var assets embed.FS

//go:embed build/appicon.png
var trayIcon []byte

const defaultPort = 8787

func main() {
	db, dataDir, port, idx, single := openInstance()
	initLog(dataDir)
	defer func() { db.Close() }() // db may be swapped by openInstance only before this point

	var quitting atomic.Bool
	var appPtr atomic.Pointer[application.App]
	var winPtr atomic.Pointer[application.WebviewWindow]
	notifier := notifications.New()

	emit := func(name string, data any) {
		// Once quitting, the UI thread is gone: touching the window (IsVisible/IsFocused) or the
		// event bus would block forever. Host.Close() fires disconnect events during shutdown.
		if quitting.Load() {
			return
		}
		if a := appPtr.Load(); a != nil {
			a.Event.Emit(name, data)
		}
		if w := winPtr.Load(); w != nil && !(w.IsVisible() && w.IsFocused()) {
			notify(notifier, name, data)
		}
	}

	// cross-network settings live in config.yaml; the first run migrates the old database values into it
	cfgPaths := configPaths()
	ncfg, cfgPath, err := loadNetConfig(cfgPaths)
	if err != nil {
		slog.Warn("config.yaml is invalid, cross-network settings ignored", "path", cfgPath, "err", err)
	} else if cfgPath == "" {
		if lc, ok := legacyNetConfig(db.Setting); ok {
			ncfg = lc
			if cfgPath, err = saveNetConfig("", ncfg, cfgPaths); err != nil {
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
		log.Fatalf("start node: %v", err)
	}
	defer n.Close()
	chatSvc := chat.New(n)
	transferSvc := transfer.New(n)
	roomSvc, err := room.New(n)
	if err != nil {
		slog.Warn("group chat disabled", "err", err)
	} else {
		defer roomSvc.Close()
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
				defer proxyTUN.Close()
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
			if socks != nil {
				defer socks.Close()
			}
		}
	}
	slog.Info("node started", "instance", idx, "peer", n.Host.ID(), "addrs", n.Host.Addrs())

	var apiHandler http.Handler // assigned below, before the app starts serving
	var quit func()
	opts := application.Options{
		Name:        "FST",
		Description: "LAN P2P file transfer and chat",
		Services:    []application.Service{application.NewService(notifier)},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
			// /api/* is answered in Go; everything else falls through to the embedded front end
			Middleware: func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(r.URL.Path, "/api/") {
						apiHandler.ServeHTTP(w, r)
						return
					}
					// exe upgrades change hashed chunk names; never let the webview reuse a stale index.html
					w.Header().Set("Cache-Control", "no-cache")
					next.ServeHTTP(w, r)
				})
			},
		},
		Mac:     application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: false},
		Windows: application.WindowsOptions{DisableQuitOnLastWindowClosed: true},
		Linux:   application.LinuxOptions{DisableQuitOnLastWindowClosed: true},
	}
	if single {
		opts.SingleInstance = &application.SingleInstanceOptions{
			UniqueID:      "com.fst.app",
			EncryptionKey: [32]byte{'f', 's', 't', '-', 'l', 'a', 'n', '-', 'p', '2', 'p', '-', 'i', 'n', 's', 't', 'a', 'n', 'c', 'e', '-', 'k', 'e', 'y', '-', '0', '1', '2', '3', '4', '5', '6'},
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				if w := winPtr.Load(); w != nil {
					showWindow(w)
				}
			},
		}
	}
	app := application.New(opts)
	appPtr.Store(app)

	quit = func() {
		if quitting.Swap(true) {
			return
		}
		// ponytail: watchdog so a stuck shutdown can never leave a zombie process that blocks the next launch;
		// remove it once every shutdown path is known to be non-blocking.
		time.AfterFunc(5*time.Second, func() { os.Exit(0) })
		app.Quit()
	}

	title := "FST"
	if idx > 1 {
		title = fmt.Sprintf("FST (%d)", idx)
	}
	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            title,
		Width:            1280,
		Height:           800,
		MinWidth:         900,
		MinHeight:        600,
		URL:              "/",
		EnableFileDrop:   true,
		BackgroundColour: application.NewRGB(255, 255, 255),
	})
	winPtr.Store(win)
	// the close button follows setting close_action: tray (hide) | quit | ask (the UI shows a dialog)
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if quitting.Load() {
			return
		}
		e.Cancel()
		switch db.Setting("close_action", "ask") {
		case "quit":
			quit()
		case "tray":
			win.Hide()
		default:
			app.Event.Emit("app:close-request", nil)
		}
	})
	// files dropped on a device card (<div id="peer-<PeerID>" data-file-drop-target>) are sent straight away
	win.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		d := e.Context().DropTargetDetails()
		files := e.Context().DroppedFiles()
		if d == nil || !strings.HasPrefix(d.ElementID, "peer-") || len(files) == 0 {
			return
		}
		if _, err := transferSvc.Send(strings.TrimPrefix(d.ElementID, "peer-"), files); err != nil {
			app.Event.Emit("app:error", err.Error())
		}
	})

	tray := app.SystemTray.New()
	tray.SetIcon(trayIcon)
	tray.SetTooltip("FST")
	menu := app.NewMenu()
	menu.Add("显示 FST").OnClick(func(*application.Context) { showWindow(win) })
	menu.AddSeparator()
	menu.Add("退出").OnClick(func(*application.Context) { quit() })
	tray.SetMenu(menu)
	tray.OnClick(func() { showWindow(win) })

	apiHandler = api.New(n, chatSvc, transferSvc, roomSvc, api.Hooks{
		PickFiles: func() ([]string, error) {
			return app.Dialog.OpenFile().SetTitle("选择要发送的文件").CanChooseFiles(true).PromptForMultipleSelection()
		},
		PickDir: func() (string, error) {
			return app.Dialog.OpenFile().SetTitle("选择下载目录").CanChooseFiles(false).CanChooseDirectories(true).CanCreateDirectories(true).PromptForSingleSelection()
		},
		PickFolder: func() (string, error) {
			return app.Dialog.OpenFile().SetTitle("选择要发送的文件夹").CanChooseFiles(false).CanChooseDirectories(true).PromptForSingleSelection()
		},
		Reveal:     func(path string) { _ = app.Env.OpenFileManager(path, true) },
		Quit:       quit,
		HideWindow: func() { win.Hide() },
		Autostart: func() bool {
			st, err := app.Autostart.Status()
			return err == nil && st.Enabled
		},
		NetConfig: func() (bool, []string, string, string) {
			if !unlocked {
				return false, nil, "", ""
			}
			cfgMu.Lock()
			defer cfgMu.Unlock()
			return ncfg.EnableRelay, ncfg.Relays, ncfg.Group, cfgPath
		},
		ProxyConfig: func() (bool, string, string, []string, []string) {
			if !unlocked {
				return false, "", "", nil, nil
			}
			cfgMu.Lock()
			defer cfgMu.Unlock()
			return ncfg.Proxy.Enable, ncfg.Proxy.Mode, ncfg.Proxy.Exit, ncfg.Proxy.Routes, ncfg.Proxy.Domains
		},
		SaveNetConfig: func(enable bool, relays []string, group string, proxyEnable bool, proxyMode, proxyExit string, proxyRoutes, proxyDomains []string) error {
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
			p, err := saveNetConfig(cfgPath, next, cfgPaths)
			if err != nil {
				return err
			}
			ncfg, cfgPath = next, p
			ncfg.Relays = cleanRelays(ncfg.Relays)
			return nil
		},
		ProxySecurity: func() api.ProxySecurity {
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
		},
		SaveProxySecurity: func(in api.ProxySecurity) error {
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
			p, err := saveNetConfig(cfgPath, next, cfgPaths)
			if err != nil {
				return err
			}
			ncfg, cfgPath = next, p
			return nil
		},
		SetAutostart: func(on bool) error {
			if on {
				return app.Autostart.Enable()
			}
			return app.Autostart.Disable()
		},
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

// socksOptions 把 config.yaml 的 SOCKS5 加固项转成代理包选项与监听地址。
//
// 参数:
//   - p: proxyConfig — config.yaml 的 proxy 段
//
// 返回:
//   - fstproxy.SocksOptions — 用户名/密码、来源 CIDR、并发上限；CIDR 解析失败时记录 Warn 并忽略该项
//   - string — 监听地址；空表示用默认 127.0.0.1:20300。配置了来源白名单但没写地址时绑 0.0.0.0
func socksOptions(p proxyConfig) (fstproxy.SocksOptions, string) {
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

func showWindow(w *application.WebviewWindow) {
	w.Show()
	w.Restore()
	w.Focus()
}

// notify raises a system notification for events the user should not miss while the window is hidden.
func notify(ns *notifications.NotificationService, name string, data any) {
	var title, body string
	switch name {
	case "chat:message":
		if m, ok := data.(store.Message); ok {
			title, body = "新消息", m.Content
		}
	case "transfer:offer":
		if m, ok := data.(map[string]any); ok {
			title = "收到文件请求"
			body = fmt.Sprintf("%v 想发送 %d 个文件", m["peer_name"], lenOf(m["files"]))
		}
	}
	if title == "" {
		return
	}
	_ = ns.SendNotification(notifications.NotificationOptions{ID: fmt.Sprintf("%s-%d", name, store.Now()), Title: title, Body: body})
}

func lenOf(v any) int {
	if f, ok := v.([]store.Transfer); ok {
		return len(f)
	}
	return 0
}

// initLog writes logs to <data>/fst.log (the GUI build has no console); stderr too when attached.
func initLog(dir string) {
	f, err := os.OpenFile(filepath.Join(dir, "fst.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(f, os.Stderr), nil)))
	log.SetOutput(io.MultiWriter(f, os.Stderr))
}
