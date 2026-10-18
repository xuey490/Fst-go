package serve

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"
)

// webEnvelope 与 /api/fst 的 {code, message, data} 信封一致，前端 axios 层无需适配。
type webEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

// respond 按 HTTP 200 + 业务码输出信封；err 非 nil 时转为 code 400（模板靠 message 提示）。
//
// 参数:
//   - w: http.ResponseWriter — 响应流
//   - data: any — 成功时的载荷（可为 nil）
//   - err: error — 业务错误；nil 表示成功
func respond(w http.ResponseWriter, data any, err error) {
	env := webEnvelope{Code: 200, Message: "ok", Data: data}
	if err != nil {
		env = webEnvelope{Code: 400, Message: err.Error(), Data: nil}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(env)
}

// login 校验访问令牌，成功则种 HttpOnly 会话 Cookie（30 天有效）。
// 失败时延迟 300ms 再返回，增加在线暴力猜测的成本。
//
// 参数:
//   - w: http.ResponseWriter — 响应流
//   - r: *http.Request — JSON 体 {"token": "..."}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		respond(w, nil, errInvalidToken)
		return
	}
	if subtle.ConstantTimeCompare([]byte(in.Token), []byte(s.token)) != 1 {
		time.Sleep(300 * time.Millisecond)
		respond(w, nil, errInvalidToken)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: s.token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600,
	})
	respond(w, map[string]any{"authenticated": true}, nil)
}

// session 返回当前浏览器的登录状态（免认证，供前端启动时探测）。
//
// 参数:
//   - w: http.ResponseWriter — 响应流
//   - r: *http.Request — 无需请求体
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]any{"authenticated": checkToken(r, s.token)}, nil)
}
