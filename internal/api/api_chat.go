package api

import (
	"errors"
	"net/http"
	"strconv"

	"fst/internal/chat"
	"fst/internal/store"
)

func (a *API) conversations(*http.Request) (any, error) {
	list, err := a.n.Store.Conversations()
	if list == nil {
		list = []store.Conversation{}
	}
	return list, err
}

func (a *API) messages(r *http.Request) (any, error) {
	id := r.URL.Query().Get("peer_id")
	if id == "" {
		return nil, errors.New("缺少 peer_id")
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	list, err := a.n.Store.Messages(id, before, intQ(r, "limit", 50))
	if list == nil {
		list = []store.Message{}
	}
	return list, err
}

// sendMessage 发送文本、图片或文件卡片消息（附件只传传输记录 ID，不传字节）。
//
// 参数:
//   - r: *http.Request — JSON 体 {peer_id, content, kind?, attach?, reply?}；
//     reply 为可选的引用信息 {id, preview, sender}
//
// 返回:
//   - any — {message}；投递失败时附带 {error}（消息仍已落库）
//   - error — 仅当参数非法到无法落库时返回
func (a *API) sendMessage(r *http.Request) (any, error) {
	var in struct {
		PeerID  string   `json:"peer_id"`
		Content string   `json:"content"`
		Kind    string   `json:"kind"`
		Attach  []string `json:"attach"`
		Reply   *struct {
			ID      string `json:"id"`
			Preview string `json:"preview"`
			Sender  string `json:"sender"`
		} `json:"reply"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	var reply chat.Reply
	if in.Reply != nil {
		reply = chat.Reply{ID: in.Reply.ID, Preview: in.Reply.Preview, Sender: in.Reply.Sender}
	}
	// the stored message (with its final status) is returned even when delivery failed,
	// so the UI can show a "failed" bubble; the error text travels in `error`
	m, err := a.c.SendMessageReply(in.PeerID, in.Content, in.Kind, in.Attach, reply)
	if m.ID == "" {
		return nil, err
	}
	res := map[string]any{"message": m}
	if err != nil {
		res["error"] = err.Error()
	}
	return res, nil
}

// recallMessage 撤回一条 2 分钟内自己发送的消息。
//
// 参数:
//   - r: *http.Request — 路径参数 {id} 为消息 ID
//
// 返回:
//   - any — 撤回后的消息（正文已清空、recalled=true）
//   - error — 非本人消息、已撤回或超出时间窗时返回
func (a *API) recallMessage(r *http.Request) (any, error) {
	return a.c.Recall(r.PathValue("id"))
}

// retryMessage 手动重投一条待发送/失败消息（清零退避后立即尝试）。
//
// 参数:
//   - r: *http.Request — 路径参数 {id} 为消息 ID
//
// 返回:
//   - any — 已重置为 queued 的消息
//   - error — 消息不存在、非本人、已撤回或状态不可重试时返回
func (a *API) retryMessage(r *http.Request) (any, error) {
	return a.c.Retry(r.PathValue("id"))
}

func (a *API) markRead(r *http.Request) (any, error) {
	var in struct {
		PeerID string `json:"peer_id"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	return nil, a.n.Store.MarkRead(in.PeerID)
}
