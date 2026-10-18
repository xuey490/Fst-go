// Package room 提供基于 GossipSub 的自动群聊房间：
// 局域网一个房间，配置了中继分组密钥时再加一个同组房间。
//
// 群聊只传文本（≤32000 runes），历史只存本机，不做离线补投与成员同步。
package room

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"fst/internal/feature"
	"fst/internal/node"
	"fst/internal/proto"
	"fst/internal/store"

	"github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

const (
	// lanRoomID 是局域网房间的固定 ID。
	lanRoomID = "lan"
	// maxContent 与聊天单条上限一致。
	maxContent = 32000
	// topicPrefix 是所有 FST 群聊 topic 的前缀，避免与公共 GossipSub 网络混淆。
	topicPrefix = "fst/room/1.0.0/"
)

// Service 管理本机的群聊订阅与历史。
type Service struct {
	n   *node.Node
	ps  *pubsub.PubSub
	ctx context.Context
	// mu 保护 topic/sub 与 groupKey；网络回调与设置保存会并发调用。
	mu     sync.Mutex
	cancel context.CancelFunc
	// wanOn 是启动时的门禁快照：锁定态只保留局域网房间（解锁入口见 docs/开发文档.md）。
	wanOn      bool
	groupKey   string
	groupRoom  string
	lanTopic   *pubsub.Topic
	lanSub     *pubsub.Subscription
	groupTopic *pubsub.Topic
	groupSub   *pubsub.Subscription
}

// New 构造群聊服务：始终加入局域网房间，有分组密钥时再加入分组房间。
//
// 参数:
//   - n: *node.Node — 已启动的节点（提供 Host/Store/事件）
//
// 返回:
//   - *Service — 群聊服务；调用方负责 Close
//   - error — GossipSub 初始化或局域网订阅失败时返回
//
// 副作用：注册分组状态回调；中继节点不应调用本函数。
func New(n *node.Node) (*Service, error) {
	ctx, cancel := context.WithCancel(n.Context())
	ps, err := pubsub.NewGossipSub(ctx, roomHost{n.Host})
	if err != nil {
		cancel()
		return nil, err
	}
	s := &Service{n: n, ps: ps, ctx: ctx, cancel: cancel, wanOn: feature.WANUnlocked(n.Store)}
	n.SetOnGroupChange(s.SetGroup)
	if err := s.joinLAN(); err != nil {
		s.Close()
		return nil, err
	}
	if key := n.GroupKey(); key != "" && s.wanOn {
		s.SetGroup(key)
	}
	return s, nil
}

// Close 取消订阅并关闭 GossipSub。
//
// 返回:
//   - 无（可重复调用；底层关闭错误忽略）
func (s *Service) Close() {
	s.cancel()
	s.mu.Lock()
	lanTopic, lanSub := s.lanTopic, s.lanSub
	groupTopic, groupSub := s.groupTopic, s.groupSub
	s.lanTopic, s.lanSub, s.groupTopic, s.groupSub = nil, nil, nil, nil
	s.mu.Unlock()
	if lanSub != nil {
		lanSub.Cancel()
	}
	if lanTopic != nil {
		_ = lanTopic.Close()
	}
	if groupSub != nil {
		groupSub.Cancel()
	}
	if groupTopic != nil {
		_ = groupTopic.Close()
	}
	// PubSub 没有 Close：取消 s.ctx 后内部循环退出。
}

// SetGroup 切换分组房间：退订旧 topic、按新密钥订阅；空密钥表示只保留局域网房间。
//
// 参数:
//   - key: string — 分组密钥（来自终端配置或中继回包）
//
// 返回:
//   - 无
//
// 副作用：写 rooms 表、启动消费 goroutine、emit room:update；失败只记录日志。
func (s *Service) SetGroup(key string) {
	if !s.wanOn {
		return
	}
	key = strings.TrimSpace(key)
	s.mu.Lock()
	if key == s.groupKey {
		s.mu.Unlock()
		return
	}
	oldTopic, oldSub := s.groupTopic, s.groupSub
	s.groupKey = key
	s.groupRoom = ""
	s.groupTopic, s.groupSub = nil, nil
	s.mu.Unlock()
	if oldSub != nil {
		oldSub.Cancel()
	}
	if oldTopic != nil {
		_ = oldTopic.Close()
	}

	if key != "" {
		id := groupRoomID(key)
		topic, sub, err := s.join(id)
		if err != nil {
			slog.Warn("room group subscribe failed", "err", err)
		} else {
			joined := false
			s.mu.Lock()
			// 订阅期间可能又被切换；只在 key 仍一致时保留。
			if s.groupKey == key {
				s.groupTopic, s.groupSub, s.groupRoom = topic, sub, id
				joined = true
			}
			s.mu.Unlock()
			if !joined {
				sub.Cancel()
				_ = topic.Close()
			} else {
				_ = s.n.Store.UpsertRoom(store.Room{ID: id, Name: s.groupName(), Kind: "group", LastTS: store.Now()})
			}
		}
	}
	s.n.Emit("room:update", nil)
}

// joinLAN 订阅局域网房间并登记房间元数据。
//
// 返回:
//   - error — Join/Subscribe 失败时返回
func (s *Service) joinLAN() error {
	topic, sub, err := s.join(lanRoomID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.lanTopic, s.lanSub = topic, sub
	s.mu.Unlock()
	return s.n.Store.UpsertRoom(store.Room{ID: lanRoomID, Name: "局域网", Kind: "lan", LastTS: store.Now()})
}

// join 加入 topic 并启动消费 goroutine。
//
// 参数:
//   - roomID: string — 房间 ID（lan 或 g-<hash>）
//
// 返回:
//   - *pubsub.Topic — 用于发布
//   - *pubsub.Subscription — 用于接收
//   - error — Join/Subscribe 失败时返回
func (s *Service) join(roomID string) (*pubsub.Topic, *pubsub.Subscription, error) {
	topic, err := s.ps.Join(topicPrefix + roomID)
	if err != nil {
		return nil, nil, err
	}
	sub, err := topic.Subscribe()
	if err != nil {
		_ = topic.Close()
		return nil, nil, err
	}
	go s.consume(roomID, sub)
	return topic, sub, nil
}

// consume 持续读取某个房间的消息，直到订阅取消或服务关闭。
//
// 参数:
//   - roomID: string — 房间 ID
//   - sub: *pubsub.Subscription — 已订阅的 topic
//
// 返回:
//   - 无
func (s *Service) consume(roomID string, sub *pubsub.Subscription) {
	for {
		msg, err := sub.Next(s.ctx)
		if err != nil {
			return
		}
		s.onMessage(roomID, msg)
	}
}

// onMessage 校验、去重并落库一条群聊消息。
//
// 参数:
//   - roomID: string — 房间 ID
//   - msg: *pubsub.Message — GossipSub 信封（签名已由 pubsub 校验）
//
// 返回:
//   - 无
//
// 副作用：首次收到时写 room_messages 并 emit room:message；重复消息静默丢弃。
func (s *Service) onMessage(roomID string, msg *pubsub.Message) {
	from := msg.GetFrom()
	if from == "" {
		return
	}
	if roomID == lanRoomID && !s.isLANPeer(from) {
		return
	}
	var in proto.RoomMsg
	if err := json.Unmarshal(msg.Data, &in); err != nil {
		return
	}
	content := strings.TrimSpace(in.Content)
	if in.Type != proto.TRoomChat || in.ID == "" || content == "" || utf8.RuneCountInString(content) > maxContent {
		return
	}
	name := s.n.PeerName(from.String())
	if name == from.String() {
		name = strings.TrimSpace(in.Name)
	}
	ts := in.TS
	if ts <= 0 {
		ts = store.Now()
	}
	m := store.RoomMessage{
		ID: in.ID, RoomID: roomID, SenderID: from.String(), SenderName: name,
		Content: content, TS: ts, Read: from == s.n.Host.ID(),
	}
	fresh, err := s.n.Store.InsertRoomMessage(m)
	if err != nil {
		slog.Warn("room message persist failed", "err", err)
		return
	}
	if fresh {
		s.n.Emit("room:message", map[string]any{"room_id": roomID, "message": m})
	}
}

// isLANPeer 判断发送者是否是本机的直连局域网邻居（排除 circuit 与公网直连）。
//
// 参数:
//   - pid: peer.ID — 原始发送者
//
// 返回:
//   - bool — 至少一条非 circuit 的连接来自私网/回环地址时为 true
func (s *Service) isLANPeer(pid peer.ID) bool {
	if s.n.Host.Network().Connectedness(pid) != network.Connected {
		return false
	}
	for _, c := range s.n.Host.Network().ConnsToPeer(pid) {
		addr := c.RemoteMultiaddr()
		if strings.Contains(addr.String(), "/p2p-circuit") {
			continue
		}
		if node.IsLANAddr(addr) {
			return true
		}
	}
	return false
}

// Publish 发送一条群聊文本：先落本机历史，再广播到当前房间。
//
// 参数:
//   - roomID: string — 房间 ID（必须是当前已加入的局域网/分组房间）
//   - content: string — 正文（去空白后 1..32000 runes）
//
// 返回:
//   - store.RoomMessage — 已落库的消息
//   - error — 房间未加入、正文非法或写库失败时返回
//
// 说明：没有在线订阅者时 Publish 不报错，消息仅保留在本机历史。
func (s *Service) Publish(roomID, content string) (store.RoomMessage, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return store.RoomMessage{}, errors.New("消息不能为空")
	}
	if utf8.RuneCountInString(content) > maxContent {
		return store.RoomMessage{}, errors.New("消息过长")
	}
	s.mu.Lock()
	var topic *pubsub.Topic
	var kind string
	switch {
	case roomID == lanRoomID:
		topic, kind = s.lanTopic, "lan"
	case roomID == s.groupRoom && s.groupRoom != "":
		topic, kind = s.groupTopic, "group"
	default:
		s.mu.Unlock()
		return store.RoomMessage{}, errors.New("尚未加入该房间")
	}
	s.mu.Unlock()
	if topic == nil {
		return store.RoomMessage{}, errors.New("尚未加入该房间")
	}
	m := store.RoomMessage{
		ID: newID(), RoomID: roomID, SenderID: s.n.Host.ID().String(), SenderName: s.n.Name(),
		Content: content, TS: store.Now(), Read: true,
	}
	if _, err := s.n.Store.InsertRoomMessage(m); err != nil {
		return m, err
	}
	if err := s.n.Store.UpsertRoom(store.Room{ID: roomID, Name: s.roomName(roomID), Kind: kind, LastTS: m.TS}); err != nil {
		slog.Warn("room metadata update failed", "err", err)
	}
	s.n.Emit("room:message", map[string]any{"room_id": roomID, "message": m})
	payload, err := json.Marshal(proto.RoomMsg{
		Type: proto.TRoomChat, ID: m.ID, Room: roomID, Name: m.SenderName,
		Content: m.Content, Kind: "text", TS: m.TS,
	})
	if err != nil {
		return m, err
	}
	if err := topic.Publish(s.ctx, payload); err != nil {
		// 本地历史已保存；断网/无订阅者不应让气泡消失。
		slog.Warn("room publish failed; kept locally", "err", err)
	}
	return m, nil
}

// Rooms 返回本机见过的房间：局域网、当前分组，以及换过密钥后留下的历史分组房间。
//
// 返回:
//   - []store.Room — 按最后活跃时间倒序；Joined 表示当前是否在收发（历史房间只能只读浏览）
//   - error — 查询失败时返回
//
// 说明：房间 ID 由分组密钥哈希派生，换密钥等于换房间；历史房间的消息仍在 room_messages，
// 换回同一密钥即可重新加入。这里不删除历史行，避免用户永久丢失入口。
func (s *Service) Rooms() ([]store.Room, error) {
	list, err := s.n.Store.Rooms(s.n.Host.ID().String())
	if err != nil {
		return nil, err
	}
	if !s.wanOn {
		// 锁定态只暴露局域网房间，历史分组房间一并隐藏（消息仍在 room_messages）。
		out := make([]store.Room, 0, 1)
		for _, r := range list {
			if r.ID == lanRoomID {
				r.Joined = true
				out = append(out, r)
			}
		}
		return out, nil
	}
	s.mu.Lock()
	groupRoom := s.groupRoom
	s.mu.Unlock()
	// 组名来自中继最近一次 HELLO 回包，可能晚于房间行创建（当时只能写「分组」），这里用实时值覆盖展示名。
	title := ""
	if groupRoom != "" {
		title = s.groupName()
	}
	for i := range list {
		switch {
		case list[i].ID == lanRoomID:
			list[i].Joined = true
		case groupRoom != "" && list[i].ID == groupRoom:
			list[i].Joined, list[i].Name = true, title
		}
	}
	return list, nil
}

// Messages 返回房间历史并把该房间标记为已读。
//
// 参数:
//   - roomID: string — 房间 ID
//   - before: int64 — 游标（ts）；0 表示最新
//   - limit: int — 条数（默认 50，最大 500）
//
// 返回:
//   - []store.RoomMessage — 按时间升序
//   - error — 查询或更新已读状态失败时返回
//
// 副作用：清除该房间的未读计数。
func (s *Service) Messages(roomID string, before int64, limit int) ([]store.RoomMessage, error) {
	list, err := s.n.Store.RoomMessages(roomID, before, limit)
	if err != nil {
		return nil, err
	}
	if err := s.n.Store.MarkRoomRead(roomID); err != nil {
		return list, err
	}
	return list, nil
}

// RoomName 返回房间展示名，供 API 在房间未落库时兜底。
//
// 参数:
//   - roomID: string — 房间 ID
//
// 返回:
//   - string — 局域网/分组/群聊
func (s *Service) RoomName(roomID string) string { return s.roomName(roomID) }

// roomName 计算当前房间的展示名。
//
// 参数:
//   - roomID: string — 房间 ID
//
// 返回:
//   - string — 中文房间名
func (s *Service) roomName(roomID string) string {
	if roomID == lanRoomID {
		return "局域网"
	}
	s.mu.Lock()
	isGroup := roomID == s.groupRoom
	s.mu.Unlock()
	if isGroup {
		return s.groupName()
	}
	return "群聊"
}

// groupName 返回中继给出的组名；尚未收到 HELLO 时回退为「分组」。
//
// 返回:
//   - string — 组名或「分组」
func (s *Service) groupName() string {
	name, _, _ := s.n.RelayGroup()
	if strings.TrimSpace(name) == "" {
		return "分组"
	}
	return name
}

// groupRoomID 由分组密钥派生房间 ID：sha256("fst-room-v1:"+key) 前 8 字节的十六进制。
//
// 参数:
//   - key: string — 分组密钥
//
// 返回:
//   - string — `g-` + 16 位十六进制；不包含密钥明文
func groupRoomID(key string) string {
	sum := sha256.Sum256([]byte("fst-room-v1:" + key))
	return "g-" + hex.EncodeToString(sum[:8])
}

// newID 生成 24 位十六进制随机消息 ID。
//
// 返回:
//   - string — 随机 ID
func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
