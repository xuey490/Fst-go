package serve

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"fst/internal/store"
)

// newTestServer 起一个用临时数据库与内存前端资源的 Server，返回 server 与 httptest Server。
func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	dist := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>FST</html>")},
	}
	s, err := New(Options{
		API:      http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		Dist:     dist,
		DB:       db,
		DataDir:  t.TempDir(),
		Transfer: nil, // 本测试不触发上传
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// TestTokenPersistence 测试首次启动生成随机令牌并落库，重启后复用同一令牌。
func TestTokenPersistence(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dist := fstest.MapFS{}
	s1, err := New(Options{API: http.NotFoundHandler(), Dist: dist, DB: db, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(s1.Token()) != 64 {
		t.Fatalf("token should be 64 hex chars, got %d", len(s1.Token()))
	}
	s2, err := New(Options{API: http.NotFoundHandler(), Dist: dist, DB: db, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if s1.Token() != s2.Token() {
		t.Fatal("token must persist across restarts")
	}
}

// TestAuthGate 测试无 Cookie 访问 API 返回 401、带正确 Cookie 放行、静态资源免认证。
func TestAuthGate(t *testing.T) {
	s, ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/api/fst/self")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("api without cookie: want 401, got %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", ts.URL+"/api/fst/self", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: s.Token()})
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("api with cookie: want 200, got %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("static without cookie: want 200, got %d", resp.StatusCode)
	}
}

// TestLogin 测试错误令牌返回业务码 400，正确令牌种 Cookie 且后续请求通过认证。
func TestLogin(t *testing.T) {
	s, ts := newTestServer(t)

	resp, err := http.Post(ts.URL+"/api/fst/web/login", "application/json",
		strings.NewReader(`{"token":"wrong"}`))
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if env.Code != 400 || env.Message != "访问令牌不正确" {
		t.Fatalf("wrong token: %+v", env)
	}

	resp, err = http.Post(ts.URL+"/api/fst/web/login", "application/json",
		strings.NewReader(`{"token":"`+s.Token()+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	cookies := resp.Cookies()
	if len(cookies) == 0 || cookies[0].Value != s.Token() {
		t.Fatalf("login must set session cookie, got %v", cookies)
	}
}

// TestSSEStream 测试认证后的 SSE 连接能收到广播事件，未认证连接被拒绝。
func TestSSEStream(t *testing.T) {
	s, ts := newTestServer(t)

	req, _ := http.NewRequest("GET", ts.URL+"/api/fst/web/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sse without cookie: want 401, got %d", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", ts.URL+"/api/fst/web/events", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: s.Token()})
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type: %q", ct)
	}
	frames := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data: ") {
				frames <- line
				return
			}
		}
	}()
	// 等订阅注册完成后广播；轮询直到收到或超时。
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-frames:
			payload := strings.TrimPrefix(line, "data: ")
			var got struct {
				Name string `json:"name"`
				Data any    `json:"data"`
			}
			if err := json.Unmarshal([]byte(payload), &got); err != nil {
				t.Fatalf("bad frame %q: %v", payload, err)
			}
			if got.Name != "peer:online" || got.Data != "peer-1" {
				t.Fatalf("frame: %+v", got)
			}
			return
		case <-deadline:
			t.Fatal("no SSE frame within 5s")
		default:
			s.Hub().broadcast("peer:online", "peer-1")
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// TestSessionEndpoint 测试免认证的 session 端点正确反映登录状态。
func TestSessionEndpoint(t *testing.T) {
	s, ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/api/fst/web/session")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Data struct {
			Authenticated bool `json:"authenticated"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if env.Data.Authenticated {
		t.Fatal("anonymous session must be unauthenticated")
	}

	req, _ := http.NewRequest("GET", ts.URL+"/api/fst/web/session", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: s.Token()})
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	env.Data.Authenticated = false
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if !env.Data.Authenticated {
		t.Fatal("cookie session must be authenticated")
	}
}
