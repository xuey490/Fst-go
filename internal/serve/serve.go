// Package serve 提供 FST 的无界面服务模式（fst serve）：复用桌面版的
// /api/fst handler 与前端静态资源，用 SSE 替代 Wails Events 推送，用
// 访问令牌 + HttpOnly Cookie 做认证，浏览器打开即获得与桌面版一致的界面。
package serve

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"fst/internal/node"
	"fst/internal/store"
	"fst/internal/transfer"
)

// cookieName 是认证会话 Cookie；tokenKey 是 settings 表里的令牌键。
const (
	cookieName = "fst_web"
	tokenKey   = "web_token"
)

// DefaultAddr 是 serve 模式的默认监听地址（仅本机回环，不产生对外暴露面）。
const DefaultAddr = "127.0.0.1:9797"

// Server 是服务模式的一个实例。API handler 与静态资源由调用方注入（与桌面版
// 同源同份）；token 持久化在本机 SQLite settings，首次启动自动生成。
type Server struct {
	api      http.Handler
	assets   http.Handler
	token    string
	hub      *hub
	db       *store.Store
	dataDir  string
	node     *node.Node
	transfer *transfer.Service
}

// Options 是 New 的注入参数；API/Dist/DB 必填，其余服务上传与头像端点需要。
type Options struct {
	API      http.Handler      // bootstrap.Assemble 产出的 /api/fst handler
	Dist     fs.FS             // 前端构建产物（index.html 所在根）
	DB       *store.Store      // 本机数据库（settings.web_token）
	DataDir  string            // 数据目录（Web 上传临时文件放这里）
	Node     *node.Node        // 头像上传直接写节点档案
	Transfer *transfer.Service // Web 发文件走既有传输流程
}

// New 创建服务模式服务器。令牌取自 settings 的 web_token，不存在则生成
// 32 字节随机 hex 并写回。
//
// 参数:
//   - o: Options — 注入的 handler/资源/数据库与核心服务
//
// 返回:
//   - *Server — 就绪的服务器；用 Serve 开始监听
//   - error — 随机数或令牌落库失败时返回
func New(o Options) (*Server, error) {
	token, err := ensureToken(o.DB)
	if err != nil {
		return nil, err
	}
	s := &Server{
		api:      o.API,
		assets:   http.FileServerFS(o.Dist),
		token:    token,
		hub:      newHub(),
		db:       o.DB,
		dataDir:  o.DataDir,
		node:     o.Node,
		transfer: o.Transfer,
	}
	cleanUploads(s.uploadRoot())
	return s, nil
}

// ensureToken 返回持久化的访问令牌；settings 里没有时生成并写入。
//
// 参数:
//   - db: *store.Store — settings 存取
//
// 返回:
//   - string — 访问令牌（64 个 hex 字符）
//   - error — 随机数读取或落库失败
func ensureToken(db *store.Store) (string, error) {
	if t := db.Setting(tokenKey, ""); t != "" {
		return t, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("无法生成访问令牌")
	}
	t := hex.EncodeToString(b)
	if err := db.SetSetting(tokenKey, t); err != nil {
		return "", errors.New("无法保存访问令牌")
	}
	return t, nil
}

// Token 返回当前访问令牌（供启动日志与设置页展示）。
func (s *Server) Token() string { return s.token }

// Broadcast 把一次 Go → UI 事件推给所有浏览器（SSE）。
//
// 参数:
//   - name: string — 事件名，与 Wails 事件名一致
//   - data: any — 可 json.Marshal 的载荷
func (s *Server) Broadcast(name string, data any) { s.hub.broadcast(name, data) }

// Hub 返回事件广播中心；调用方把 bootstrap.Assemble 的 Emitter 指向 hub.broadcast。
func (s *Server) Hub() *hub { return s.hub }

// Handler 装配完整的 HTTP 路由：
//
//	POST /api/fst/web/login          免认证，令牌换会话 Cookie
//	GET  /api/fst/web/session        免认证，查询登录状态
//	GET  /api/fst/web/events         需认证，SSE 事件流
//	POST /api/fst/transfers/upload   需认证，multipart 发文件
//	POST /api/fst/self/avatar/upload 需认证，multipart 设头像
//	/api/*                           其余 API 全部需认证
//	/*                               前端静态资源（SPA 外壳，不含敏感数据，免认证）
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/fst/web/login", s.login)
	mux.HandleFunc("GET /api/fst/web/session", s.session)
	mux.Handle("GET /api/fst/web/events", authed(s.token, http.HandlerFunc(s.hub.ServeHTTP)))
	mux.Handle("POST /api/fst/transfers/upload", authedFn(s.token, s.upload))
	mux.Handle("POST /api/fst/self/avatar/upload", authedFn(s.token, s.avatarUpload))
	mux.Handle("/api/", authed(s.token, s.api))
	mux.Handle("/", s.assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p != "/api/fst/web/login" && p != "/api/fst/web/session" && strings.HasPrefix(p, "/api/") {
			if !checkToken(r, s.token) {
				unauthorized(w)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// Serve 在 addr 上开始监听并立即返回；默认 DefaultAddr，绑定非回环地址时打日志警告。
//
// 参数:
//   - addr: string — 监听地址，形如 "127.0.0.1:9797"；空串用默认值
//
// 返回:
//   - func() — 幂等的关停函数（优雅关闭 HTTP 并结束 SSE hub）；进程退出前调用
//   - error — 端口监听失败（被占用等）时返回
func (s *Server) Serve(addr string) (shutdown func(), err error) {
	if addr == "" {
		addr = DefaultAddr
	}
	if host, _, e := net.SplitHostPort(addr); e == nil {
		if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
			slog.Warn("监听地址不是本机回环，界面将对局域网开放；请确认已设置强访问令牌", "addr", addr)
		}
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, errors.New("Web 服务监听失败：" + err.Error())
	}
	slog.Info("web 服务已启动", "addr", "http://"+addr, "token", s.token)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-done
		s.hub.close()
	}, nil
}

// checkToken 校验请求携带的会话 Cookie 是否等于访问令牌（常数时间比较）。
//
// 参数:
//   - r: *http.Request — 客户端请求
//   - token: string — 期望的令牌
//
// 返回:
//   - bool — 匹配为 true
func checkToken(r *http.Request, token string) bool {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(token)) == 1
}

// unauthorized 按 HTTP 401 输出与 /api/fst 一致的 JSON 信封。
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"code":401,"message":"未登录或访问令牌不正确","data":null}`))
}

// authed 把需要认证的 handler 包一层 Cookie 校验，失败返回 401 JSON 信封。
func authed(token string, next http.Handler) http.Handler {
	return authedFn(token, next.ServeHTTP)
}

// authedFn 是 authed 的函数形式，供非 http.Handler 的处理函数使用。
func authedFn(token string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !checkToken(r, token) {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}
