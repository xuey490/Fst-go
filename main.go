// FST 桌面客户端与 fst serve 服务模式的统一入口。
//
// 两种形态共用 internal/bootstrap.Assemble 装配同一个核心运行时（节点、聊天、
// 传输、群聊、代理、/api/fst handler），差别只在外壳：
//
//	fst          Wails 窗口 + 系统托盘 + 原生事件推送
//	fst serve    HTTP + SSE，浏览器打开与桌面版一致的界面
//
// 用 `fst serve -h` 查看服务模式参数。
package main

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"fst/internal/api"
	"fst/internal/bootstrap"
	"fst/internal/proto"
	"fst/internal/serve"
	"fst/internal/store"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

//go:embed all:front/dist
var assets embed.FS

//go:embed build/appicon.png
var trayIcon []byte

// BuildTag 标识当前二进制的构建批次，只用于现场排障。
//
// 为什么需要它：桌面端安装在不同机器上时，用户无法从界面判断某台机器跑的
// 是新版还是旧版（`fst version` 用的是协议版本，多次修复之间并不变化）。
// 启动日志里的 build= 一行可以让排障一眼确认「这台机器到底在跑哪份代码」。
const BuildTag = "2026-11-09-v1.2.0"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			os.Exit(runServe(os.Args[2:]))
		case "version", "-version", "--version", "-v":
			fmt.Println("FST " + proto.Version)
			return
		case "help", "-h", "--help":
			printUsage()
			return
		}
	}
	runDesktop()
}

// printUsage 打印子命令帮助。
//
// 参数:
//
//	无
//
// 返回:
//
//	无（直接写 stdout）
func printUsage() {
	fmt.Print(`FST — 局域网 P2P 文件传输与聊天

用法:
  fst                  启动桌面客户端（Wails 窗口 + 托盘）
  fst serve [-addr A]  启动无界面服务模式，浏览器访问 http://<addr>
  fst version          打印版本

环境变量:
  FST_DATA_DIR   覆盖数据目录（身份密钥、数据库、日志）
  FST_PORT       覆盖 libp2p 监听端口（默认 8787）
  FST_WAN=1      解锁跨网络（中继/DHT/代理）高级功能

桌面版配置文件: <数据目录>/config.yaml
`)
}

// runServe 运行 fst serve：装配核心运行时，把 Go 事件转成 SSE，并在 addr 上监听。
//
// 参数:
//   - args: []string — serve 子命令后面的参数，支持 -addr
//
// 返回:
//   - int — 进程退出码：0 正常退出；1 启动失败；2 参数错误
func runServe(args []string) int {
	flagset := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flagset.String("addr", serve.DefaultAddr, "监听地址，例如 127.0.0.1:9797 或 0.0.0.0:9797")
	if err := flagset.Parse(args); err != nil {
		return 2
	}

	db, dataDir, port, _, _ := bootstrap.OpenInstance()
	defer db.Close()
	bootstrap.InitLog(dataDir)
	slog.Info("fst started", "version", proto.Version, "build", BuildTag)

	// srv 在 Assemble 之后才创建；装配期间的少量事件会丢失（与桌面版启动早期一致）。
	var srv *serve.Server
	res, err := bootstrap.Assemble(bootstrap.Options{
		DB:      db,
		DataDir: dataDir,
		Port:    port,
		Emit: func(name string, data any) {
			if srv != nil {
				srv.Broadcast(name, data)
			}
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动节点失败: %v\n", err)
		return 1
	}
	defer res.Shutdown()

	dist, err := iofs.Sub(assets, "front/dist")
	if err != nil {
		fmt.Fprintf(os.Stderr, "前端资源未打包（请先构建 front/dist）: %v\n", err)
		return 1
	}
	srv, err = serve.New(serve.Options{
		API:      res.API,
		Dist:     dist,
		DB:       db,
		DataDir:  dataDir,
		Node:     res.Node,
		Transfer: res.Transfer,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化 Web 服务失败: %v\n", err)
		return 1
	}
	shutdown, err := srv.Serve(*addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	fmt.Printf("FST 服务模式已启动\n  地址: http://%s\n  令牌: %s\n  节点: %s\n", *addr, srv.Token(), res.Node.Host.ID())

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	shutdown()
	return 0
}

// runDesktop 运行桌面客户端：Wails 窗口、托盘、原生通知与事件推送。
//
// 参数:
//
//	无
//
// 返回:
//
//	无；致命错误直接退出进程
func runDesktop() {
	db, dataDir, port, idx, single := bootstrap.OpenInstance()
	defer db.Close()
	bootstrap.InitLog(dataDir)
	slog.Info("fst started", "version", proto.Version, "build", BuildTag)

	var quitting atomic.Bool
	var appPtr atomic.Pointer[application.App]
	var winPtr atomic.Pointer[application.WebviewWindow]
	var quitFn atomic.Value // func()
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

	// 桌面专属钩子：装配时 App 还不存在，用原子指针延迟取；调用发生在窗口启动之后。
	app := func() *application.App { return appPtr.Load() }
	hooks := api.Hooks{
		PickFiles: func() ([]string, error) {
			a := app()
			if a == nil {
				return nil, errors.New("界面尚未就绪")
			}
			return a.Dialog.OpenFile().SetTitle("选择要发送的文件").CanChooseFiles(true).PromptForMultipleSelection()
		},
		PickDir: func() (string, error) {
			a := app()
			if a == nil {
				return "", errors.New("界面尚未就绪")
			}
			return a.Dialog.OpenFile().SetTitle("选择下载目录").CanChooseFiles(false).CanChooseDirectories(true).CanCreateDirectories(true).PromptForSingleSelection()
		},
		PickFolder: func() (string, error) {
			a := app()
			if a == nil {
				return "", errors.New("界面尚未就绪")
			}
			return a.Dialog.OpenFile().SetTitle("选择要发送的文件夹").CanChooseFiles(false).CanChooseDirectories(true).PromptForSingleSelection()
		},
		Reveal: func(path string) {
			if a := app(); a != nil {
				_ = a.Env.OpenFileManager(path, true)
			}
		},
		Quit: func() {
			if f, ok := quitFn.Load().(func()); ok && f != nil {
				f()
			}
		},
		HideWindow: func() {
			if w := winPtr.Load(); w != nil {
				w.Hide()
			}
		},
		Autostart: func() bool {
			a := app()
			if a == nil {
				return false
			}
			st, err := a.Autostart.Status()
			return err == nil && st.Enabled
		},
		SetAutostart: func(on bool) error {
			a := app()
			if a == nil {
				return errors.New("界面尚未就绪")
			}
			if on {
				return a.Autostart.Enable()
			}
			return a.Autostart.Disable()
		},
	}

	res, err := bootstrap.Assemble(bootstrap.Options{
		DB:      db,
		DataDir: dataDir,
		Port:    port,
		Emit:    emit,
		GUI:     hooks,
	})
	if err != nil {
		log.Fatalf("start node: %v", err)
	}
	defer res.Shutdown()
	slog.Info("node started", "instance", idx, "peer", res.Node.Host.ID(), "addrs", res.Node.Host.Addrs())

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
						res.API.ServeHTTP(w, r)
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
				// 这条日志由**正在运行的旧进程**写出：用户双击新 exe 时，
				// 新进程会把请求交给它然后自行退出。现场很容易误判成
				// 「新版没生效」——日志里出现这行就说明旧进程还占着位置，
				// 必须先从托盘退出（或结束进程）再启动新版本。
				slog.Warn("second launch handed off to the running instance; quit it before running a new build",
					"build", BuildTag)
				if w := winPtr.Load(); w != nil {
					showWindow(w)
				}
			},
		}
	}
	mainApp := application.New(opts)
	appPtr.Store(mainApp)

	quit := func() {
		if quitting.Swap(true) {
			return
		}
		// ponytail: watchdog so a stuck shutdown can never leave a zombie process that blocks the next launch;
		// remove it once every shutdown path is known to be non-blocking.
		time.AfterFunc(5*time.Second, func() { os.Exit(0) })
		mainApp.Quit()
	}
	quitFn.Store(quit)

	title := "FST"
	if idx > 1 {
		title = fmt.Sprintf("FST (%d)", idx)
	}
	win := mainApp.Window.NewWithOptions(application.WebviewWindowOptions{
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
			mainApp.Event.Emit("app:close-request", nil)
		}
	})
	// files dropped on a device card (<div id="peer-<PeerID>" data-file-drop-target>) are sent straight away
	win.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		d := e.Context().DropTargetDetails()
		files := e.Context().DroppedFiles()
		if d == nil || !strings.HasPrefix(d.ElementID, "peer-") || len(files) == 0 {
			return
		}
		if _, err := res.Transfer.Send(strings.TrimPrefix(d.ElementID, "peer-"), files); err != nil {
			mainApp.Event.Emit("app:error", err.Error())
		}
	})

	tray := mainApp.SystemTray.New()
	tray.SetIcon(trayIcon)
	tray.SetTooltip("FST")
	menu := mainApp.NewMenu()
	menu.Add("显示 FST").OnClick(func(*application.Context) { showWindow(win) })
	menu.AddSeparator()
	menu.Add("退出").OnClick(func(*application.Context) { quit() })
	tray.SetMenu(menu)
	tray.OnClick(func() { showWindow(win) })

	if err := mainApp.Run(); err != nil {
		log.Fatal(err)
	}
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
