// Package store wraps the local SQLite database (peers, messages, transfers, settings).
package store

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no CGO
)

//go:embed schema.sql
var schema string

type Store struct{ db *sql.DB }

type Peer struct {
	PeerID    string `json:"peer_id"`
	Name      string `json:"name"`
	Platform  string `json:"platform"`
	LastAddrs string `json:"last_addrs"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
	Trusted   bool   `json:"trusted"`
	// Avatar is the peer's small base64 avatar (PNG/JPEG, <= 8KB) learned from HELLO; empty means none.
	Avatar string `json:"avatar"`
}

type Message struct {
	ID        string `json:"id"`
	PeerID    string `json:"peer_id"`
	Direction string `json:"direction"` // in | out
	Content   string `json:"content"`
	Status    string `json:"status"` // sending | sent | delivered | failed
	TS        int64  `json:"ts"`
	Read      bool   `json:"read"`
	// Kind is text | image | file; Attach holds the transfer ids the bubble renders.
	Kind     string   `json:"kind"`
	Attach   []string `json:"attach"`
	Recalled bool     `json:"recalled"`
	// Attempts 与 NextRetry 只对 status=queued 的离线消息有意义：
	// Attempts 是已失败次数，NextRetry 是下次可投递时间（unix ms，0 表示立即可投）。
	Attempts  int   `json:"attempts"`
	NextRetry int64 `json:"next_retry"`
	// ReplyID/ReplyPreview/ReplySender 是引用（回复）某条消息时保存的定位与展示信息，
	// 预览在发送端已截断；三者皆空表示普通消息。
	ReplyID      string `json:"reply_id"`
	ReplyPreview string `json:"reply_preview"`
	ReplySender  string `json:"reply_sender"`
}

type Conversation struct {
	PeerID      string `json:"peer_id"`
	LastContent string `json:"last_content"`
	LastTS      int64  `json:"last_ts"`
	Unread      int    `json:"unread"`
	// LastKind / LastRecalled let the UI render "[图片]" / "[已撤回]" previews without extra queries.
	LastKind     string `json:"last_kind"`
	LastRecalled bool   `json:"last_recalled"`
}

type Transfer struct {
	ID         string `json:"id"`
	BatchID    string `json:"batch_id"`
	PeerID     string `json:"peer_id"`
	Direction  string `json:"direction"` // in | out
	FileName   string `json:"file_name"`
	RelPath    string `json:"rel_path"` // folder transfers: slash-separated path under the download root; empty for a single file
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	LocalPath  string `json:"local_path"`
	Status     string `json:"status"` // pending|accepted|running|done|failed|cancelled|rejected
	BytesDone  int64  `json:"bytes_done"`
	Error      string `json:"error"`
	CreatedAt  int64  `json:"created_at"`
	FinishedAt int64  `json:"finished_at"`
}

// Room 是一个本机可见的群聊房间（局域网或中继分组房间）。
//
// LastContent/Unread 由查询派生，不单独落库；Joined 由房间服务按当前订阅状态填写。
type Room struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"` // lan | group
	LastTS      int64  `json:"last_ts"`
	LastContent string `json:"last_content"`
	Unread      int    `json:"unread"`
	Joined      bool   `json:"joined"`
}

// RoomMessage 是一条本机保存的群聊消息；群聊消息不走 messages 表，避免与会话按 peer_id 的字典冲突。
type RoomMessage struct {
	ID         string `json:"id"`
	RoomID     string `json:"room_id"`
	SenderID   string `json:"sender_id"`
	SenderName string `json:"sender_name"`
	Content    string `json:"content"`
	TS         int64  `json:"ts"`
	Read       bool   `json:"read"`
}

func Now() int64 { return time.Now().UnixMilli() }

// Open 打开（并按版本迁移）dir 下的 fst.db。
//
// 参数:
//   - dir: string — 数据目录（不存在时由调用方提前创建）
//
// 返回:
//   - *Store: 可用的存储句柄
//   - error: 连接、迁移失败，或 SQLite 无法打开文件时返回
//
// 副作用：配置 WAL/busy_timeout、执行 schema 迁移，并把中断的传输标为 failed。
func Open(dir string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "fst.db"))+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // ponytail: one writer; plenty for a desktop app, avoids SQLITE_BUSY
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	s := &Store{db: db}
	// transfers cut short by a crash/exit can never continue on their own
	_, _ = db.Exec(`UPDATE transfers SET status='failed', error='interrupted', finished_at=? WHERE status IN ('pending','accepted','running')`, Now())
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ---- settings ----

func (s *Store) Setting(key, def string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ---- peers ----

// UpsertPeer inserts or refreshes a peer; empty name/platform/addrs never overwrite known values.
func (s *Store) UpsertPeer(p Peer) error {
	now := Now()
	_, err := s.db.Exec(`INSERT INTO peers(peer_id,name,platform,last_addrs,avatar,first_seen,last_seen)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(peer_id) DO UPDATE SET
		  name=CASE WHEN excluded.name<>'' THEN excluded.name ELSE name END,
		  platform=CASE WHEN excluded.platform<>'' THEN excluded.platform ELSE platform END,
		  last_addrs=CASE WHEN excluded.last_addrs<>'' THEN excluded.last_addrs ELSE last_addrs END,
		  avatar=CASE WHEN excluded.avatar<>'' THEN excluded.avatar ELSE avatar END,
		  last_seen=excluded.last_seen`,
		p.PeerID, p.Name, p.Platform, p.LastAddrs, p.Avatar, now, now)
	return err
}

func (s *Store) Peers() ([]Peer, error) {
	rows, err := s.db.Query(`SELECT peer_id,name,platform,last_addrs,first_seen,last_seen,trusted,avatar FROM peers ORDER BY last_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Peer
	for rows.Next() {
		var p Peer
		if err := rows.Scan(&p.PeerID, &p.Name, &p.Platform, &p.LastAddrs, &p.FirstSeen, &p.LastSeen, &p.Trusted, &p.Avatar); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Peer(id string) (Peer, bool) {
	var p Peer
	err := s.db.QueryRow(`SELECT peer_id,name,platform,last_addrs,first_seen,last_seen,trusted,avatar FROM peers WHERE peer_id=?`, id).
		Scan(&p.PeerID, &p.Name, &p.Platform, &p.LastAddrs, &p.FirstSeen, &p.LastSeen, &p.Trusted, &p.Avatar)
	return p, err == nil
}

func (s *Store) SetTrusted(id string, trusted bool) error {
	_, err := s.db.Exec(`UPDATE peers SET trusted=? WHERE peer_id=?`, trusted, id)
	return err
}

// DeletePeer removes a peer and its local chat/transfer history in one transaction.
func (s *Store) DeletePeer(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}
	for _, query := range []string{
		`DELETE FROM messages WHERE peer_id=?`,
		`DELETE FROM transfers WHERE peer_id=?`,
		`DELETE FROM peers WHERE peer_id=?`,
	} {
		if _, err := tx.Exec(query, id); err != nil {
			return rollback(err)
		}
	}
	return tx.Commit()
}

// ---- messages ----

// messageColumns 是 messages 表的固定读取列顺序，与 scanMessage 一一对应。
const messageColumns = `id,peer_id,direction,content,status,ts,read,kind,recalled,attach,attempts,next_retry,reply_id,reply_preview,reply_sender`

// rowScanner 是 *sql.Rows 与 *sql.Row 的共同读取能力。
type rowScanner interface{ Scan(dest ...any) error }

// scanMessage 按 messageColumns 的顺序读取一条消息并解析 attach。
//
// 参数:
//   - sc: rowScanner — *sql.Rows 或 *sql.Row
//
// 返回:
//   - Message — 扫描结果（Attach 已解码）
//   - error — 列数/类型不匹配或读取失败时返回
func scanMessage(sc rowScanner) (Message, error) {
	var m Message
	var attach string
	err := sc.Scan(&m.ID, &m.PeerID, &m.Direction, &m.Content, &m.Status, &m.TS, &m.Read, &m.Kind, &m.Recalled, &attach,
		&m.Attempts, &m.NextRetry, &m.ReplyID, &m.ReplyPreview, &m.ReplySender)
	m.Attach = decodeAttach(attach)
	return m, err
}

// InsertMessage returns false when the id already exists (duplicate delivery).
func (s *Store) InsertMessage(m Message) (bool, error) {
	kind := m.Kind
	if kind == "" {
		kind = "text"
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO messages(id,peer_id,direction,content,status,ts,read,kind,recalled,attach,reply_id,reply_preview,reply_sender)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.PeerID, m.Direction, m.Content, m.Status, m.TS, m.Read, kind, m.Recalled, encodeAttach(m.Attach),
		m.ReplyID, m.ReplyPreview, m.ReplySender)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) SetMessageStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE messages SET status=? WHERE id=?`, status, id)
	return err
}

// QueueMessage 把一条消息标记为待发送（离线队列）。
//
// 参数:
//   - id: string — 消息 ID
//
// 返回:
//   - error — 写库失败时返回
//
// 副作用：重置 attempts/next_retry，使 worker 下一轮即可尝试。
func (s *Store) QueueMessage(id string) error {
	_, err := s.db.Exec(`UPDATE messages SET status='queued', attempts=0, next_retry=0 WHERE id=?`, id)
	return err
}

// SetMessageRetry 记录一次投递失败后的退避信息。
//
// 参数:
//   - id: string — 消息 ID
//   - attempts: int — 累计失败次数
//   - next: int64 — 下次可投递时间（unix ms）
//
// 返回:
//   - error — 写库失败时返回
func (s *Store) SetMessageRetry(id string, attempts int, next int64) error {
	_, err := s.db.Exec(`UPDATE messages SET status='queued', attempts=?, next_retry=? WHERE id=?`, attempts, next, id)
	return err
}

// QueuedMessages 返回某个对端所有待发送消息（忽略退避时间），用于上线/手动重试时立即冲刷。
//
// 参数:
//   - peerID: string — 对端 PeerID
//   - limit: int — 最多返回条数；<=0 或 >200 时按 50 处理
//
// 返回:
//   - []Message — status=queued 且未撤回的消息，按时间升序
//   - error — 查询失败时返回
func (s *Store) QueuedMessages(peerID string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+messageColumns+` FROM messages
		WHERE status='queued' AND recalled=0 AND peer_id=?
		ORDER BY ts ASC, rowid ASC LIMIT ?`, peerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DueMessages 返回到期可投递的离线消息，按时间升序（同一对端保持原发送顺序）。
//
// 参数:
//   - now: int64 — 当前时间（unix ms）
//   - limit: int — 本次最多返回条数；<=0 或 >200 时按 50 处理
//
// 返回:
//   - []Message — status=queued、未撤回且 next_retry<=now 的消息
//   - error — 查询失败时返回
func (s *Store) DueMessages(now int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+messageColumns+` FROM messages
		WHERE status='queued' AND recalled=0 AND next_retry<=?
		ORDER BY ts ASC, rowid ASC LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Message returns one message by id.
//
// 参数:
//   - id: string — 消息 ID
//
// 返回:
//   - Message — 找到的消息（未找到时为零值）
//   - bool — 是否存在
func (s *Store) Message(id string) (Message, bool) {
	m, err := scanMessage(s.db.QueryRow(`SELECT `+messageColumns+` FROM messages WHERE id=?`, id))
	return m, err == nil
}

// RecallMessage 把某条消息标记为已撤回并清空正文；重复撤回返回 false。
//
// 参数:
//   - id: string — 消息 ID
//   - peerID: string — 该消息必须属于的对端（防止跨会话撤回）
//
// 返回:
//   - bool — 本次是否真的改动了记录
//   - error — 写库失败时返回
func (s *Store) RecallMessage(id, peerID string) (bool, error) {
	res, err := s.db.Exec(`UPDATE messages SET recalled=1, content='' WHERE id=? AND peer_id=? AND recalled=0`, id, peerID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Messages returns up to limit messages older than before (0 = newest), oldest first.
func (s *Store) Messages(peerID string, before int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if before <= 0 {
		before = 1<<62 - 1
	}
	rows, err := s.db.Query(`SELECT `+messageColumns+` FROM messages
		WHERE peer_id=? AND ts<? ORDER BY ts DESC, rowid DESC LIMIT ?`, peerID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

func (s *Store) Conversations() ([]Conversation, error) {
	rows, err := s.db.Query(`SELECT m.peer_id, m.content, m.ts, m.kind, m.recalled,
		(SELECT COUNT(*) FROM messages u WHERE u.peer_id=m.peer_id AND u.direction='in' AND u.read=0)
		FROM messages m
		WHERE m.rowid = (SELECT rowid FROM messages x WHERE x.peer_id=m.peer_id ORDER BY ts DESC, rowid DESC LIMIT 1)
		ORDER BY m.ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conversation
	for rows.Next() {
		var c Conversation
		if err := rows.Scan(&c.PeerID, &c.LastContent, &c.LastTS, &c.LastKind, &c.LastRecalled, &c.Unread); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// encodeAttach 把附件 transfer id 列表编码为库里存的 JSON 文本；空列表存空串。
//
// 参数:
//   - ids: []string — transfer id 列表
//
// 返回:
//   - string — JSON 数组或空串
func encodeAttach(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeAttach 把库里的 JSON 文本还原成 transfer id 列表；损坏或空值时返回 nil。
//
// 参数:
//   - raw: string — encodeAttach 的输出
//
// 返回:
//   - []string — 解码结果；无附件时为 nil
func decodeAttach(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func (s *Store) MarkRead(peerID string) error {
	_, err := s.db.Exec(`UPDATE messages SET read=1 WHERE peer_id=? AND direction='in' AND read=0`, peerID)
	return err
}

// ---- rooms ----

// UpsertRoom 记录/更新一个房间的展示信息；last_ts 只增不减。
//
// 参数:
//   - r: Room — 至少提供 ID/Name/Kind；LastTS 为 0 时保留旧值
//
// 返回:
//   - error — 写库失败时返回
func (s *Store) UpsertRoom(r Room) error {
	_, err := s.db.Exec(`INSERT INTO rooms(room_id,name,kind,last_ts) VALUES(?,?,?,?)
		ON CONFLICT(room_id) DO UPDATE SET
		  name=CASE WHEN excluded.name<>'' THEN excluded.name ELSE name END,
		  kind=CASE WHEN excluded.kind<>'' THEN excluded.kind ELSE kind END,
		  last_ts=MAX(last_ts, excluded.last_ts)`,
		r.ID, r.Name, r.Kind, r.LastTS)
	return err
}

// InsertRoomMessage 保存一条群聊消息，按消息 ID 去重。
//
// 参数:
//   - m: RoomMessage — 消息内容；ID 为空时直接返回错误
//
// 返回:
//   - bool — true 表示首次插入；false 表示重复
//   - error — 写库失败时返回
//
// 副作用：首次插入时同步推进 rooms.last_ts（同一事务）。
func (s *Store) InsertRoomMessage(m RoomMessage) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	res, err := tx.Exec(`INSERT OR IGNORE INTO room_messages(id,room_id,sender_id,sender_name,content,ts,read)
		VALUES(?,?,?,?,?,?,?)`,
		m.ID, m.RoomID, m.SenderID, m.SenderName, m.Content, m.TS, m.Read)
	if err != nil {
		_ = tx.Rollback()
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		if _, err := tx.Exec(`INSERT INTO rooms(room_id,last_ts) VALUES(?,?)
			ON CONFLICT(room_id) DO UPDATE SET last_ts=MAX(last_ts, excluded.last_ts)`, m.RoomID, m.TS); err != nil {
			_ = tx.Rollback()
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

// Rooms 返回本机见过的房间，带最后一条消息预览与未读数。
//
// 参数:
//   - selfID: string — 本机 PeerID；用于排除自己发送的未读消息
//
// 返回:
//   - []Room — 按最后活跃时间倒序；Joined 由调用方按订阅状态填写
//   - error — 查询失败时返回
func (s *Store) Rooms(selfID string) ([]Room, error) {
	rows, err := s.db.Query(`SELECT r.room_id,r.name,r.kind,r.last_ts,
		COALESCE((SELECT m.content FROM room_messages m WHERE m.room_id=r.room_id ORDER BY m.ts DESC, m.rowid DESC LIMIT 1),''),
		(SELECT COUNT(*) FROM room_messages u WHERE u.room_id=r.room_id AND u.read=0 AND u.sender_id<>?)
		FROM rooms r ORDER BY r.last_ts DESC`, selfID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Room
	for rows.Next() {
		var r Room
		if err := rows.Scan(&r.ID, &r.Name, &r.Kind, &r.LastTS, &r.LastContent, &r.Unread); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RoomMessages 返回某个房间在本机保存的历史消息（按时间升序）。
//
// 参数:
//   - roomID: string — 房间 ID
//   - before: int64 — 游标（只取 ts 更早的消息）；0 表示从最新开始
//   - limit: int — 条数，默认 50，最大 500
//
// 返回:
//   - []RoomMessage — 升序结果
//   - error — 查询失败时返回
func (s *Store) RoomMessages(roomID string, before int64, limit int) ([]RoomMessage, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if before <= 0 {
		before = 1<<62 - 1
	}
	rows, err := s.db.Query(`SELECT id,room_id,sender_id,sender_name,content,ts,read FROM room_messages
		WHERE room_id=? AND ts<? ORDER BY ts DESC, rowid DESC LIMIT ?`, roomID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoomMessage
	for rows.Next() {
		var m RoomMessage
		if err := rows.Scan(&m.ID, &m.RoomID, &m.SenderID, &m.SenderName, &m.Content, &m.TS, &m.Read); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// MarkRoomRead 把某房间收到的消息标记为已读。
//
// 参数:
//   - roomID: string — 房间 ID
//
// 返回:
//   - error — 写库失败时返回
func (s *Store) MarkRoomRead(roomID string) error {
	_, err := s.db.Exec(`UPDATE room_messages SET read=1 WHERE room_id=? AND read=0`, roomID)
	return err
}

// ---- transfers ----

func (s *Store) InsertTransfer(t Transfer) error {
	_, err := s.db.Exec(`INSERT INTO transfers(id,batch_id,peer_id,direction,file_name,rel_path,size,sha256,local_path,status,bytes_done,error,created_at,finished_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.BatchID, t.PeerID, t.Direction, t.FileName, t.RelPath, t.Size, t.SHA256, t.LocalPath, t.Status, t.BytesDone, t.Error, t.CreatedAt, t.FinishedAt)
	return err
}

// UpdateTransfer sets status/progress; terminal statuses stamp finished_at. Empty sha/path/err keep old values.
func (s *Store) UpdateTransfer(id, status string, done int64, errMsg, sha, path string) error {
	var fin int64
	switch status {
	case "done", "failed", "cancelled", "rejected":
		fin = Now()
	}
	_, err := s.db.Exec(`UPDATE transfers SET status=?, bytes_done=?, error=?,
		sha256=CASE WHEN ?<>'' THEN ? ELSE sha256 END,
		local_path=CASE WHEN ?<>'' THEN ? ELSE local_path END,
		finished_at=CASE WHEN ?>0 THEN ? ELSE finished_at END WHERE id=?`,
		status, done, errMsg, sha, sha, path, path, fin, fin, id)
	return err
}

// SetTransferSHA 记录发送前的预计算摘要（不改变状态与进度）。
//
// 参数:
//   - id: string — 传输记录 ID
//   - sha: string — 小写十六进制 SHA-256；空串表示不写
//
// 返回:
//   - error — 写库失败时返回
func (s *Store) SetTransferSHA(id, sha string) error {
	if sha == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE transfers SET sha256=? WHERE id=?`, sha, id)
	return err
}

const transferCols = `id,batch_id,peer_id,direction,file_name,rel_path,size,sha256,local_path,status,bytes_done,error,created_at,finished_at`

func scanTransfer(sc interface{ Scan(...any) error }) (Transfer, error) {
	var t Transfer
	err := sc.Scan(&t.ID, &t.BatchID, &t.PeerID, &t.Direction, &t.FileName, &t.RelPath, &t.Size, &t.SHA256, &t.LocalPath, &t.Status, &t.BytesDone, &t.Error, &t.CreatedAt, &t.FinishedAt)
	return t, err
}

func (s *Store) Transfer(id string) (Transfer, bool) {
	t, err := scanTransfer(s.db.QueryRow(`SELECT `+transferCols+` FROM transfers WHERE id=?`, id))
	return t, err == nil
}

func (s *Store) BatchTransfers(batchID string) ([]Transfer, error) {
	rows, err := s.db.Query(`SELECT `+transferCols+` FROM transfers WHERE batch_id=? ORDER BY created_at, rowid`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transfer
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DoneTransferBySHA 查找本机已完成的同摘要入站文件，用于 offer 阶段的重复文件跳过。
//
// 参数:
//   - peerID: string — 发送方 PeerID
//   - sha: string — 整文件 SHA-256（小写十六进制）
//   - size: int64 — 文件字节数（摘要相同但大小不同视为不同文件）
//
// 返回:
//   - Transfer — 命中的已完成传输（未命中时为零值）
//   - bool — 是否存在
func (s *Store) DoneTransferBySHA(peerID, sha string, size int64) (Transfer, bool) {
	if sha == "" {
		return Transfer{}, false
	}
	t, err := scanTransfer(s.db.QueryRow(`SELECT `+transferCols+` FROM transfers
		WHERE peer_id=? AND direction='in' AND status='done' AND sha256=? AND size=?
		ORDER BY finished_at DESC LIMIT 1`, peerID, sha, size))
	return t, err == nil
}

type TransferFilter struct {
	PeerID    string
	Direction string
	Status    string
	Keyword   string
	Page      int
	Size      int
}

func (s *Store) Transfers(f TransferFilter) ([]Transfer, int, error) {
	var where []string
	var args []any
	add := func(cond string, v any) { where = append(where, cond); args = append(args, v) }
	if f.PeerID != "" {
		add("peer_id=?", f.PeerID)
	}
	if f.Direction != "" {
		add("direction=?", f.Direction)
	}
	if f.Status != "" {
		add("status=?", f.Status)
	}
	if f.Keyword != "" {
		add("file_name LIKE ?", "%"+f.Keyword+"%")
	}
	w := ""
	if len(where) > 0 {
		w = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM transfers`+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Size <= 0 || f.Size > 200 {
		f.Size = 20
	}
	if f.Page <= 0 {
		f.Page = 1
	}
	rows, err := s.db.Query(`SELECT `+transferCols+` FROM transfers`+w+` ORDER BY created_at DESC, rowid DESC LIMIT ? OFFSET ?`,
		append(args, f.Size, (f.Page-1)*f.Size)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Transfer
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// DeleteTransfers 清理传输历史记录：只删已结束且未被聊天附件引用的行，文件本身不动。
//
// 参数:
//   - ids: []string — 指定要清理的记录 ID；为空表示清理全部可清理记录
//
// 返回:
//   - int64 — 实际删除的行数（运行中/被聊天引用的记录会被跳过）
//   - error — SQL 执行失败
//
// 说明：聊天里的图片/文件卡片通过 messages.attach 引用 transfer id，删除后会变成
// 「图片待接收」，因此这里按 attach LIKE 排除，避免清理日志连带破坏历史消息。
func (s *Store) DeleteTransfers(ids []string) (int64, error) {
	q := `DELETE FROM transfers WHERE status IN ('done','failed','cancelled','rejected')
		AND NOT EXISTS (SELECT 1 FROM messages WHERE messages.attach LIKE '%"' || transfers.id || '"%')`
	args := make([]any, 0, len(ids))
	if len(ids) > 0 {
		q += ` AND id IN (?` + strings.Repeat(",?", len(ids)-1) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return n, err
}
