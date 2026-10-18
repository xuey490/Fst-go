// serve 冒烟：进程内完成 bootstrap.Assemble + serve.New，验证浏览器链路
// （登录 → 认证 API → SSE 收到事件）在真实节点上端到端可用。
package e2e

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"fst/internal/bootstrap"
	"fst/internal/serve"
	"fst/internal/store"
)

// TestServeSmoke 测服务模式全链路：未认证 401、令牌登录、Cookie 后 /api/fst/self
// 可用、SSE 连接收到与 Wails 事件同名的广播帧。
func TestServeSmoke(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Assemble 需要 Emitter，serve.Server 需要 Assemble 产物：转发闭包解耦（同 main.go）。
	var sink func(name string, data any)
	res, err := bootstrap.Assemble(bootstrap.Options{
		DB: db, DataDir: dir, Port: 0,
		Emit: func(name string, data any) {
			if sink != nil {
				sink(name, data)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(res.Shutdown)

	s, err := serve.New(serve.Options{
		API: res.API, Dist: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>")}},
		DB: db, DataDir: dir, Node: res.Node, Transfer: res.Transfer,
	})
	if err != nil {
		t.Fatal(err)
	}
	sink = s.Broadcast
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// 未认证访问 API 必须 401
	resp, err := http.Get(ts.URL + "/api/fst/self")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: want 401, got %d", resp.StatusCode)
	}

	// SSE：登录前连不上，登录后能收到广播帧（事件名与 Wails 一致）
	req, _ := http.NewRequest("GET", ts.URL+"/api/fst/web/events", nil)
	req.AddCookie(&http.Cookie{Name: "fst_web", Value: s.Token()})
	sse, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer sse.Body.Close()
	if ct := sse.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("sse content-type: %q", ct)
	}
	frames := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(sse.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data: ") {
				frames <- line
				return
			}
		}
	}()
	go func() {
		// 订阅注册有竞态：轮询广播直到收到帧或测试超时
		for {
			select {
			case <-frames:
				return
			case <-time.After(100 * time.Millisecond):
				res.Node.Emit("peer:online", map[string]any{"peer_id": "smoke-peer", "online": true})
			}
		}
	}()
	select {
	case line := <-frames:
		var got struct {
			Name string         `json:"name"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &got); err != nil {
			t.Fatalf("bad frame: %v", err)
		}
		if got.Name != "peer:online" || got.Data["peer_id"] != "smoke-peer" {
			t.Fatalf("frame: %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no SSE frame within 10s")
	}

	// 登录：错误令牌拒绝，正确令牌种 Cookie 后 self 接口可用
	bad, err := http.Post(ts.URL+"/api/fst/web/login", "application/json", strings.NewReader(`{"token":"nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Code int `json:"code"`
	}
	_ = json.NewDecoder(bad.Body).Decode(&env)
	bad.Body.Close()
	if env.Code != 400 {
		t.Fatalf("wrong token: want code 400, got %d", env.Code)
	}

	ok, err := http.Post(ts.URL+"/api/fst/web/login", "application/json",
		strings.NewReader(`{"token":"`+s.Token()+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	cookies := ok.Cookies()
	ok.Body.Close()
	if len(cookies) == 0 {
		t.Fatal("login did not set cookie")
	}
	req, _ = http.NewRequest("GET", ts.URL+"/api/fst/self", nil)
	req.AddCookie(cookies[0])
	signed, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer signed.Body.Close()
	if signed.StatusCode != http.StatusOK {
		t.Fatalf("self after login: want 200, got %d", signed.StatusCode)
	}
	var selfEnv struct {
		Data struct {
			PeerID string `json:"peer_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(signed.Body).Decode(&selfEnv); err != nil {
		t.Fatal(err)
	}
	if selfEnv.Data.PeerID == "" {
		t.Fatal("self.peer_id is empty")
	}
}
