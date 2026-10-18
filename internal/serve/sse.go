package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// hub 维护所有 SSE 订阅者并负责广播。事件载荷与 Wails Events 完全一致，
// 线上格式为单行 JSON：{"name":"<事件名>","data":<载荷>}。
type hub struct {
	mu      sync.Mutex
	closed  bool
	clients map[*subscriber]struct{}
}

// subscriber 是一条浏览器 SSE 长连接。缓冲队列满即视为慢客户端：丢弃该帧并计数，
// 绝不阻塞广播方（聊天/传输事件频率低，正常客户端不会丢）。
type subscriber struct {
	ch      chan []byte
	dropped atomic.Int64
}

// sseBuffer 是单个订阅者的待发帧缓冲上限（帧数）。事件为低频推送，64 足以吸收瞬时突发。
const sseBuffer = 64

// newHub 创建空的广播中心。
func newHub() *hub {
	return &hub{clients: map[*subscriber]struct{}{}}
}

// subscribe 注册一个新订阅者；已关闭的 hub 返回 nil。
//
// 返回:
//   - *subscriber — 订阅者句柄；用完必须调用 unsubscribe
func (h *hub) subscribe() *subscriber {
	s := &subscriber{ch: make(chan []byte, sseBuffer)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.clients[s] = struct{}{}
	return s
}

// unsubscribe 移除订阅者并关闭其队列，后续广播不再投递。
func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[s]; ok {
		delete(h.clients, s)
		close(s.ch)
	}
}

// broadcast 把一次 Go → UI 事件编码后投递给全部订阅者；队列满的慢客户端丢弃该帧并计数。
//
// 参数:
//   - name: string — 事件名，与 Wails 事件名一致（如 peer:online / chat:message）
//   - data: any — 可 json.Marshal 的载荷
func (h *hub) broadcast(name string, data any) {
	payload, err := json.Marshal(struct {
		Name string `json:"name"`
		Data any    `json:"data"`
	}{Name: name, Data: data})
	if err != nil {
		return
	}
	frame := []byte("data: " + string(payload) + "\n\n")
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.clients {
		select {
		case s.ch <- frame:
		default:
			s.dropped.Add(1)
		}
	}
}

// close 关闭 hub 并唤醒所有还在等待的连接（其 handler 退出时清理）。
func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.clients {
		delete(h.clients, s)
		close(s.ch)
	}
	h.clients = nil
}

// ServeHTTP 实现 GET /api/fst/web/events：一条 text/event-stream 长连接承载全部事件。
// 每 15s 写一行注释帧当心跳，用于穿透中间代理的空闲超时并尽早发现断连。
//
// 参数:
//   - w: http.ResponseWriter — 响应流
//   - r: *http.Request — 客户端断开经 r.Context() 感知
func (h *hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	s := h.subscribe()
	if s == nil {
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	defer h.unsubscribe(s)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if n := s.dropped.Load(); n > 0 {
				fmt.Fprintf(w, ": dropped %d slow frames\n", n)
				s.dropped.Store(0)
			}
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case frame, ok := <-s.ch:
			if !ok {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
