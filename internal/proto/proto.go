// Package proto defines FST's libp2p protocol IDs, control messages and framing.
//
// Control traffic is request/response: one stream carries one JSON frame each way.
// ponytail: JSON + uvarint length prefix is plenty for tiny control messages; move to
// protobuf only if a non-Go client ever needs to speak this protocol.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	MsgProtocol   = "/fst/msg/1.0.0"
	FileProtocol  = "/fst/file/1.0.0"
	ProxyProtocol = "/fst/proxy/1.0.0"
	MDNSService   = "fst"
	Version       = "0.1.0"

	// MaxFrame bounds a single control frame (Hello peer lists and offers stay far below this).
	MaxFrame = 1 << 20
)

// Control message types.
const (
	THello       = "HELLO"
	TChat        = "CHAT"
	TChatAck     = "CHAT_ACK"
	TChatRecall  = "CHAT_RECALL" // ID = recalled message id, sent by its author only
	TRoomChat    = "ROOM_CHAT"   // GossipSub payload only; never sent over /fst/msg
	TFileOffer   = "FILE_OFFER"
	TFileAccept  = "FILE_ACCEPT"
	TFileReject  = "FILE_REJECT"
	TFileCancel  = "FILE_CANCEL"
	TError       = "ERROR"
)

// RoomMsg 是群聊消息的 GossipSub 载荷；room 与发送者由 topic/签名信封确定，不信任载荷里的伪造字段。
type RoomMsg struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Room    string `json:"room"`
	Name    string `json:"name,omitempty"`
	Content string `json:"content"`
	Kind    string `json:"kind,omitempty"`
	TS      int64  `json:"ts"`
}

type FileInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	// RelPath is the slash-separated path under the sender's chosen folder (name included), empty for a
	// plain file. Receivers must treat it as untrusted and sanitise it before touching the filesystem.
	RelPath string `json:"rel_path,omitempty"`
	// SHA256 is the lowercase hex digest of the whole file when the sender pre-hashed it (offer-time
	// duplicate detection). Empty means "sender did not compute it"; old peers simply ignore it.
	SHA256 string `json:"sha256,omitempty"`
}

// Msg is the single envelope for /fst/msg; only the fields relevant to Type are set.
type Msg struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	TS   int64  `json:"ts"`

	// HELLO
	Name      string   `json:"name,omitempty"`
	Platform  string   `json:"platform,omitempty"`
	Version   string   `json:"version,omitempty"`
	Peers     []string `json:"peers,omitempty"`     // peer exchange: full multiaddrs of peers I am connected to
	Directory []string `json:"directory,omitempty"` // relay only: PeerIDs of the other FST peers online on this relay
	// Group is the terminal's relay-group key, sent only to configured relays (HELLO only). Relays use it to
	// keep terminals of different groups from seeing each other; empty means "no group" (legacy behaviour).
	Group string `json:"group,omitempty"`
	// GroupName/GroupExpires/GroupError are relay replies: which group the key matched, when it expires
	// (unix ms, 0 = never) and why it was rejected. Old terminals ignore them.
	GroupName    string `json:"group_name,omitempty"`
	GroupExpires int64  `json:"group_expires,omitempty"`
	GroupError   string `json:"group_error,omitempty"`

	// CHAT
	Content string `json:"content,omitempty"`
	// Kind is the chat bubble type: "" / "text" (default), "image" or "file".
	Kind string `json:"kind,omitempty"`
	// Attach lists transfer ids owned by this chat message (images or file cards). Receivers resolve
	// them through the media endpoint / transfer history; no bytes travel inside the message.
	Attach []string `json:"attach,omitempty"`
	// RecallID is set on CHAT_RECALL (the message being withdrawn); ID carries the recall's own id.
	RecallID string `json:"recall_id,omitempty"`
	// ReplyID/ReplyPreview/ReplySender describe the message this one quotes. The preview is truncated by
	// the sender (<=200 runes) so the receiver can render the quote block without looking up history.
	// All empty means "not a reply"; old peers ignore these fields.
	ReplyID      string `json:"reply_id,omitempty"`
	ReplyPreview string `json:"reply_preview,omitempty"`
	ReplySender  string `json:"reply_sender,omitempty"`
	// Avatar is the sender's small avatar image as base64 (<= 8KB, PNG/JPEG), sent in HELLO only.
	Avatar string `json:"avatar,omitempty"`

	// FILE_OFFER / FILE_ACCEPT / FILE_REJECT / FILE_CANCEL (ID = batch id)
	Files   []FileInfo       `json:"files,omitempty"`
	Offsets map[string]int64 `json:"offsets,omitempty"` // file id -> bytes already held by receiver
	// Dirs lists empty folders (slash-separated, root name included) in a FILE_OFFER so the receiver can
	// recreate the tree; still untrusted, sanitised with the same rules as RelPath.
	Dirs []string `json:"dirs,omitempty"`
	// Skip lists file ids the receiver already holds (same SHA-256); the sender marks them done without
	// opening a stream. Ignored by old senders, which just stream as usual.
	Skip   []string `json:"skip,omitempty"`
	FileID string   `json:"file_id,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

// FileHeader opens a /fst/file stream; raw bytes [Offset,size) and a 32-byte SHA-256 trailer follow.
type FileHeader struct {
	BatchID string `json:"batch_id"`
	FileID  string `json:"file_id"`
	Offset  int64  `json:"offset"`
}

// FileResult is the receiver's final verdict on a /fst/file stream.
type FileResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ProxyReq starts one outbound TCP or UDP session on an fst-relay.
// Addr is a numeric IP or a hostname (resolved on the exit).
type ProxyReq struct {
	Net  string `json:"net"`
	Addr string `json:"addr"`
	Port int    `json:"port"`
	// Family forces the address family used to resolve a hostname: "" / "ip4" / "ip6".
	Family string `json:"family,omitempty"`
	// Token is the optional exit token; relays started with -exit-token reject sessions without a
	// matching token. Old relays ignore the field, old terminals simply send nothing.
	Token string `json:"token,omitempty"`
}

type ProxyResp struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func Now() int64 { return time.Now().UnixMilli() }

// WriteFrame writes v as one uvarint-length-prefixed JSON frame.
func WriteFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return fmt.Errorf("frame too large: %d", len(b))
	}
	buf := binary.AppendUvarint(make([]byte, 0, len(b)+binary.MaxVarintLen64), uint64(len(b)))
	_, err = w.Write(append(buf, b...))
	return err
}

type byteReader struct{ r io.Reader }

func (b byteReader) ReadByte() (byte, error) {
	var p [1]byte
	_, err := io.ReadFull(b.r, p[:])
	return p[0], err
}

// ReadFrame reads exactly one frame into v. It never reads past the frame, so raw
// bytes may follow on the same stream.
func ReadFrame(r io.Reader, v any) error {
	n, err := binary.ReadUvarint(byteReader{r})
	if err != nil {
		return err
	}
	if n > MaxFrame {
		return errors.New("frame too large")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
