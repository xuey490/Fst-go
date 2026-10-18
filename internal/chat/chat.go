// Package chat sends and receives messages over /fst/msg and persists them.
//
// A message may be plain/rich text (kind=text), an image bubble (kind=image) or a file card
// (kind=file); the latter two only carry transfer ids (Attach), never bytes.
package chat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"fst/internal/node"
	"fst/internal/proto"
	"fst/internal/store"

	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	// maxLen 是单条消息的字符上限；32000 个中文约 96KB，仍远小于 proto.MaxFrame。
	maxLen = 32000
	// recallWindow 是撤回时间窗：超过后拒绝撤回，避免无限期改写历史。
	recallWindow = 2 * time.Minute
	// maxAttach 是单条消息可引用的传输数量（图片九宫格）。
	maxAttach = 9
	// flushInterval 是离线队列的扫描周期。
	flushInterval = 15 * time.Second
	// maxRetryAttempts 后转为 failed，避免永久占用队列；用户仍可手动重试。
	maxRetryAttempts = 20
	// maxReplyPreview / maxReplySender 是引用块里预览与昵称的上限（rune），防止把整段历史塞进帧里。
	maxReplyPreview = 200
	maxReplySender  = 64
)

// Reply 是一条消息引用的目标及其展示信息（由前端从本地消息里取，服务端只做校验与截断）。
type Reply struct {
	ID      string `json:"id"`      // 被引用消息的 ID
	Preview string `json:"preview"` // 被引用正文的纯文本预览（≤200 runes）
	Sender  string `json:"sender"`  // 被引用消息的发送者昵称（≤64 runes）
}

type Service struct {
	n *node.Node
	// mu/flushing 保证同一对端同一时刻只有一个投递者，避免自动重试与手动重试重复发送。
	mu       sync.Mutex
	flushing map[string]bool
}

// New 构造聊天服务并注册 CHAT / CHAT_RECALL 处理器。
//
// 参数:
//   - n: *node.Node — 已启动的节点（提供 /fst/msg 分发与存储）
//
// 返回:
//   - *Service — 聊天服务
func New(n *node.Node) *Service {
	s := &Service{n: n, flushing: map[string]bool{}}
	n.Handle(proto.TChat, s.onChat)
	n.Handle(proto.TChatRecall, s.onRecall)
	n.SetOnPeerOnline(func(pid peer.ID) { go s.FlushPeer(pid.String()) })
	go s.flushLoop()
	return s
}

// NewID 生成 24 位十六进制随机 ID（消息 / 批次 / 文件共用）。
//
// 返回:
//   - string — 随机 ID；随机源不可用时仍返回（crypto/rand 在支持的平台上不会失败）
func NewID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// normalizeKind 归一化气泡类型：空串视为 text，非法值同样回落到 text。
//
// 参数:
//   - kind: string — 协议里的 kind
//
// 返回:
//   - string — text / image / file
func normalizeKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "image":
		return "image"
	case "file":
		return "file"
	default:
		return "text"
	}
}

// prepareMessage 校验并归一化一条待发送消息（无副作用，便于单测）。
//
// 参数:
//   - content: string — 原始正文（会被 TrimSpace）
//   - kind: string — "" / text / image / file
//   - attach: []string — 附件传输 ID 列表
//
// 返回:
//   - string — 归一化后的正文（图片/文件卡片为空时补「图片」/「文件」预览词）
//   - string — 归一化后的 kind（text / image / file）
//   - error — 文本为空、图片/文件缺附件、附件过多或正文超长时返回
func prepareMessage(content, kind string, attach []string) (string, string, error) {
	content = strings.TrimSpace(content)
	kind = normalizeKind(kind)
	if kind == "text" && content == "" {
		return "", kind, errors.New("消息不能为空")
	}
	if kind != "text" && len(attach) == 0 {
		return "", kind, errors.New("缺少附件")
	}
	if len(attach) > maxAttach {
		return "", kind, errors.New("单条消息最多 9 个附件")
	}
	if utf8.RuneCountInString(content) > maxLen {
		return "", kind, errors.New("消息过长")
	}
	if kind != "text" && content == "" {
		// 会话列表的预览文字；气泡本身按 kind 渲染卡片
		if kind == "image" {
			content = "图片"
		} else {
			content = "文件"
		}
	}
	return content, kind, nil
}

// normalizeReply 清洗引用信息：ID 或预览为空时视为普通消息，预览与昵称按 rune 截断。
//
// 参数:
//   - r: Reply — 前端提交的引用信息（可能带空白或超长内容）
//
// 返回:
//   - Reply — 可直接落库/发送的引用信息；普通消息返回零值
func normalizeReply(r Reply) Reply {
	r.ID = strings.TrimSpace(r.ID)
	r.Preview = truncateRunes(strings.TrimSpace(r.Preview), maxReplyPreview)
	r.Sender = truncateRunes(strings.TrimSpace(r.Sender), maxReplySender)
	if r.ID == "" || r.Preview == "" {
		return Reply{}
	}
	return r
}

// truncateRunes 按 rune 截断字符串，避免切断多字节字符。
//
// 参数:
//   - s: string — 原始文本
//   - n: int — 保留的最大 rune 数；<=0 表示不截断
//
// 返回:
//   - string — 截断后的文本
func truncateRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// Send 发送一条纯文本/富文本消息（保留旧调用方式）。
//
// 参数:
//   - peerID: string — 对方 PeerID
//   - content: string — 正文（≤ maxLen runes）
//
// 返回:
//   - store.Message — 已落库的本地消息（含最终状态）
//   - error — 参数非法或对方未确认时返回
func (s *Service) Send(peerID, content string) (store.Message, error) {
	return s.SendMessage(peerID, content, "text", nil)
}

// SendMessage 落库并投递一条聊天消息（文本 / 图片 / 文件卡片），不带引用。
//
// 参数:
//   - peerID: string — 对方 PeerID
//   - content: string — 正文；kind=image/file 时可为空（服务端补一个预览占位词）
//   - kind: string — "" / text / image / file
//   - attach: []string — 关联的传输记录 ID（≤ maxAttach 个）
//
// 返回:
//   - store.Message — 已落库的本地消息；网络失败时 Status=queued（稍后自动重投），对端明确拒绝时 Status=failed
//   - error — 仅参数非法、写库失败或对端明确拒绝时返回；离线/超时不再视为错误
func (s *Service) SendMessage(peerID, content, kind string, attach []string) (store.Message, error) {
	return s.SendMessageReply(peerID, content, kind, attach, Reply{})
}

// SendMessageReply 落库并投递一条可带引用的聊天消息。
//
// 参数:
//   - peerID: string — 对方 PeerID
//   - content: string — 正文；kind=image/file 时可为空（服务端补一个预览占位词）
//   - kind: string — "" / text / image / file
//   - attach: []string — 关联的传输记录 ID（≤ maxAttach 个）
//   - reply: Reply — 引用信息；ID 或 Preview 为空时按普通消息发送
//
// 返回:
//   - store.Message — 已落库的本地消息；网络失败时 Status=queued（稍后自动重投），对端明确拒绝时 Status=failed
//   - error — 仅参数非法、写库失败或对端明确拒绝时返回；离线/超时不再视为错误
//
// 副作用：先落库（失败也可见），再经 /fst/msg 投递并发出 chat:ack 事件。
func (s *Service) SendMessageReply(peerID, content, kind string, attach []string, reply Reply) (store.Message, error) {
	content, kind, err := prepareMessage(content, kind, attach)
	if err != nil {
		return store.Message{}, err
	}
	pid, err := peer.Decode(peerID)
	if err != nil {
		return store.Message{}, errors.New("无效的设备 ID")
	}
	reply = normalizeReply(reply)
	m := store.Message{
		ID: NewID(), PeerID: peerID, Direction: "out", Content: content, Status: "sending",
		TS: store.Now(), Read: true, Kind: kind, Attach: attach,
		ReplyID: reply.ID, ReplyPreview: reply.Preview, ReplySender: reply.Sender,
	}
	if _, err := s.n.Store.InsertMessage(m); err != nil {
		return m, err
	}
	if !s.n.IsOnline(pid) {
		return s.queue(m), nil
	}
	err = s.sendOne(m, 15*time.Second)
	if err == nil {
		m.Status = "delivered"
		_ = s.n.Store.SetMessageStatus(m.ID, m.Status)
		s.n.Emit("chat:ack", map[string]any{"id": m.ID, "peer_id": peerID, "status": m.Status})
		return m, nil
	}
	var remote *node.RemoteError
	if errors.As(err, &remote) {
		m.Status = "failed"
		_ = s.n.Store.SetMessageStatus(m.ID, m.Status)
		s.n.Emit("chat:ack", map[string]any{"id": m.ID, "peer_id": peerID, "status": m.Status})
		return m, err
	}
	// 离线/超时/连接失败：进入待发送队列，由后台重投；不向 UI 报错。
	return s.queue(m), nil
}

// queue 把消息置为 queued、记录退避信息并推送 chat:ack。
//
// 参数:
//   - m: store.Message — 已落库的本地消息
//
// 返回:
//   - store.Message — Status=queued 的消息
//
// 副作用：写库并 emit chat:ack。
func (s *Service) queue(m store.Message) store.Message {
	_ = s.n.Store.QueueMessage(m.ID)
	m.Status, m.Attempts, m.NextRetry = "queued", 0, 0
	s.n.Emit("chat:ack", map[string]any{"id": m.ID, "peer_id": m.PeerID, "status": m.Status})
	return m
}

// sendOne 经 /fst/msg 投递一条已落库消息并等待 CHAT_ACK。
//
// 参数:
//   - m: store.Message — 待投递消息（使用 ID/PeerID/Content/Kind/Attach）
//   - timeout: time.Duration — 连接与应答的总超时
//
// 返回:
//   - error — 网络失败、未确认或对端明确回 ERROR（*node.RemoteError）
func (s *Service) sendOne(m store.Message, timeout time.Duration) error {
	pid, err := peer.Decode(m.PeerID)
	if err != nil {
		return errors.New("无效的设备 ID")
	}
	ctx, cancel := context.WithTimeout(s.n.Context(), timeout)
	defer cancel()
	_ = s.n.EnsureConnected(ctx, pid)
	reply, err := s.n.Request(ctx, pid, &proto.Msg{
		Type: proto.TChat, ID: m.ID, TS: m.TS, Content: m.Content, Kind: m.Kind, Attach: m.Attach,
		ReplyID: m.ReplyID, ReplyPreview: m.ReplyPreview, ReplySender: m.ReplySender,
	}, timeout)
	if err != nil {
		return err
	}
	if reply.Type != proto.TChatAck {
		return errors.New("对方未确认")
	}
	return nil
}

// flushLoop 每隔 flushInterval 扫描一次到期的离线消息，直到节点关闭。
//
// 返回:
//   - 无（goroutine 随 n.Context() 取消退出）
func (s *Service) flushLoop() {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-s.n.Context().Done():
			return
		case <-t.C:
			s.FlushDue()
		}
	}
}

// FlushDue 投递当前所有到期且对端在线的离线消息（供 ticker 与手动重试调用）。
//
// 返回:
//   - 无（单次最多处理 50 条；同一对端由 tryFlush 串行化）
func (s *Service) FlushDue() {
	list, err := s.n.Store.DueMessages(store.Now(), 50)
	if err != nil {
		slog.Warn("chat outbox query failed", "err", err)
		return
	}
	for _, m := range list {
		if m.Direction != "out" {
			continue
		}
		pid, derr := peer.Decode(m.PeerID)
		if derr != nil || !s.n.IsOnline(pid) {
			continue
		}
		s.tryFlush(m)
	}
}

// FlushPeer 立即冲刷某个对端的到期离线消息（对端上线时触发）。
//
// 参数:
//   - peerID: string — 对端 PeerID
//
// 返回:
//   - 无（离线或查询失败时静默返回，由下一次 ticker 兜底）
func (s *Service) FlushPeer(peerID string) {
	list, err := s.n.Store.QueuedMessages(peerID, 200)
	if err != nil {
		return
	}
	for _, m := range list {
		if m.Direction == "out" {
			s.tryFlush(m)
		}
	}
}

// tryFlush 串行投递一条离线消息，失败时写回指数退避；对端明确拒绝则直接 failed。
//
// 参数:
//   - m: store.Message — 待投递的 queued 消息
//
// 返回:
//   - 无
//
// 副作用：更新消息状态/重试时间并 emit chat:ack。
func (s *Service) tryFlush(m store.Message) {
	if !s.lockPeer(m.PeerID) {
		return
	}
	defer s.unlockPeer(m.PeerID)
	fresh, ok := s.n.Store.Message(m.ID)
	if !ok || fresh.Status != "queued" || fresh.Recalled {
		return
	}
	err := s.sendOne(fresh, 10*time.Second)
	if err == nil {
		_ = s.n.Store.SetMessageStatus(fresh.ID, "delivered")
		s.n.Emit("chat:ack", map[string]any{"id": fresh.ID, "peer_id": fresh.PeerID, "status": "delivered"})
		return
	}
	var remote *node.RemoteError
	if errors.As(err, &remote) {
		slog.Warn("chat retry rejected by peer", "id", fresh.ID, "err", err)
		_ = s.n.Store.SetMessageStatus(fresh.ID, "failed")
		s.n.Emit("chat:ack", map[string]any{"id": fresh.ID, "peer_id": fresh.PeerID, "status": "failed"})
		return
	}
	slog.Debug("chat retry failed", "id", fresh.ID, "err", err)
	attempts := fresh.Attempts + 1
	if attempts >= maxRetryAttempts {
		_ = s.n.Store.SetMessageStatus(fresh.ID, "failed")
		s.n.Emit("chat:ack", map[string]any{"id": fresh.ID, "peer_id": fresh.PeerID, "status": "failed"})
		return
	}
	_ = s.n.Store.SetMessageRetry(fresh.ID, attempts, store.Now()+retryDelay(attempts).Milliseconds())
	s.n.Emit("chat:ack", map[string]any{"id": fresh.ID, "peer_id": fresh.PeerID, "status": "queued"})
}

// retryDelay 计算第 attempts 次失败后的退避：15s 起步、指数增长、上限 10 分钟。
//
// 参数:
//   - attempts: int — 已失败次数（>=1）
//
// 返回:
//   - time.Duration — 下次重试等待时间
func retryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 7 {
		attempts = 7
	}
	d := 15 * time.Second << (attempts - 1)
	if d > 10*time.Minute {
		return 10 * time.Minute
	}
	return d
}

// Retry 手动重投一条自己发出的待发送/失败消息；清零退避后立即尝试。
//
// 参数:
//   - id: string — 消息 ID
//
// 返回:
//   - store.Message — 已重置为 queued 的消息
//   - error — 消息不存在、不是自己发的、已撤回或状态不可重试时返回
//
// 副作用：写库、emit chat:ack，并异步冲刷该对端队列。
func (s *Service) Retry(id string) (store.Message, error) {
	m, ok := s.n.Store.Message(id)
	if !ok {
		return store.Message{}, errors.New("消息不存在")
	}
	if m.Direction != "out" {
		return store.Message{}, errors.New("只能重试自己发送的消息")
	}
	if m.Recalled {
		return m, errors.New("消息已撤回")
	}
	if m.Status != "queued" && m.Status != "failed" {
		return m, errors.New("消息无需重试")
	}
	if err := s.n.Store.QueueMessage(id); err != nil {
		return m, err
	}
	m.Status, m.Attempts, m.NextRetry = "queued", 0, 0
	s.n.Emit("chat:ack", map[string]any{"id": m.ID, "peer_id": m.PeerID, "status": "queued"})
	go s.FlushPeer(m.PeerID)
	return m, nil
}

// lockPeer 占用某对端的投递锁；已被占用时返回 false。
//
// 参数:
//   - peerID: string — 对端 PeerID
//
// 返回:
//   - bool — true 表示拿到锁，调用方必须配对 unlockPeer
func (s *Service) lockPeer(peerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flushing[peerID] {
		return false
	}
	s.flushing[peerID] = true
	return true
}

// unlockPeer 释放某对端的投递锁。
//
// 参数:
//   - peerID: string — 对端 PeerID
//
// 返回:
//   - 无
func (s *Service) unlockPeer(peerID string) {
	s.mu.Lock()
	delete(s.flushing, peerID)
	s.mu.Unlock()
}

// Recall 撤回一条自己在 2 分钟内发出的消息，并通知对端同步撤回。
//
// 参数:
//   - id: string — 消息 ID
//
// 返回:
//   - store.Message — 撤回后的本地消息（Content 已清空、Recalled=true）
//   - error — 消息不存在 / 不是自己发的 / 已撤回 / 超过时间窗时返回
//
// 副作用：清空本地正文并落库，发出 chat:recall 事件，后台向对端发送 CHAT_RECALL。
func (s *Service) Recall(id string) (store.Message, error) {
	m, ok := s.n.Store.Message(id)
	if !ok {
		return store.Message{}, errors.New("消息不存在")
	}
	if m.Direction != "out" {
		return store.Message{}, errors.New("只能撤回自己发送的消息")
	}
	if m.Recalled {
		return m, errors.New("消息已撤回")
	}
	if store.Now()-m.TS > recallWindow.Milliseconds() {
		return m, errors.New("超过 2 分钟，无法撤回")
	}
	changed, err := s.n.Store.RecallMessage(id, m.PeerID)
	if err != nil {
		return m, err
	}
	if !changed {
		return m, errors.New("消息已撤回")
	}
	m.Recalled, m.Content = true, ""
	s.n.Emit("chat:recall", map[string]any{"id": id, "peer_id": m.PeerID})
	go func() {
		pid, derr := peer.Decode(m.PeerID)
		if derr != nil {
			return
		}
		ctx, cancel := context.WithTimeout(s.n.Context(), 10*time.Second)
		defer cancel()
		_, _ = s.n.Request(ctx, pid, &proto.Msg{
			Type: proto.TChatRecall, ID: NewID(), RecallID: id, TS: proto.Now(),
		}, 10*time.Second)
	}()
	return m, nil
}

// onChat 处理入站聊天消息：校验后落库，首次收到时推送 chat:message。
//
// 参数:
//   - from: peer.ID — 发送方（连接层已认证）
//   - in: *proto.Msg — CHAT 帧（ID/Content/Kind/Attach）
//
// 返回:
//   - *proto.Msg — CHAT_ACK；参数非法时返回 ERROR
func (s *Service) onChat(_ context.Context, from peer.ID, in *proto.Msg) *proto.Msg {
	kind := normalizeKind(in.Kind)
	content := strings.TrimSpace(in.Content)
	if in.ID == "" || utf8.RuneCountInString(content) > maxLen ||
		(kind == "text" && content == "") ||
		(kind != "text" && len(in.Attach) == 0) || len(in.Attach) > maxAttach {
		return &proto.Msg{Type: proto.TError, Reason: "bad message"}
	}
	reply := normalizeReply(Reply{ID: in.ReplyID, Preview: in.ReplyPreview, Sender: in.ReplySender})
	m := store.Message{
		ID: in.ID, PeerID: from.String(), Direction: "in", Content: content,
		Status: "delivered", TS: store.Now(), Kind: kind, Attach: in.Attach,
		ReplyID: reply.ID, ReplyPreview: reply.Preview, ReplySender: reply.Sender,
	}
	// the sender's id is the primary key: a retransmission is acked but not stored twice
	if fresh, err := s.n.Store.InsertMessage(m); err == nil && fresh {
		s.n.Emit("chat:message", m)
	}
	return &proto.Msg{Type: proto.TChatAck, ID: in.ID, TS: proto.Now()}
}

// onRecall 处理对端撤回：只接受消息作者本人的撤回，并且只改本机落库的入站消息。
//
// 参数:
//   - from: peer.ID — 撤回发起方（必须与该消息的 peer_id 一致）
//   - in: *proto.Msg — CHAT_RECALL（RecallID 为被撤回的消息 ID）
//
// 返回:
//   - *proto.Msg — CHAT_ACK；消息不存在或不属于该对端时返回 ERROR
func (s *Service) onRecall(_ context.Context, from peer.ID, in *proto.Msg) *proto.Msg {
	if in.RecallID == "" {
		return &proto.Msg{Type: proto.TError, Reason: "bad recall"}
	}
	m, ok := s.n.Store.Message(in.RecallID)
	if !ok || m.PeerID != from.String() || m.Direction != "in" {
		return &proto.Msg{Type: proto.TError, Reason: "unknown message"}
	}
	if changed, err := s.n.Store.RecallMessage(in.RecallID, from.String()); err == nil && changed {
		s.n.Emit("chat:recall", map[string]any{"id": in.RecallID, "peer_id": from.String()})
	}
	return &proto.Msg{Type: proto.TChatAck, ID: in.ID, TS: proto.Now()}
}
