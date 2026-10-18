package room

import (
	"context"
	"testing"
	"time"

	"fst/internal/feature"
	"fst/internal/node"
	"fst/internal/store"

	"github.com/libp2p/go-libp2p/core/peer"
)

// TestGroupRoomID 验证房间 ID 由密钥哈希派生：稳定、互不相同、不含密钥明文。
func TestGroupRoomID(t *testing.T) {
	a := groupRoomID("secret-key")
	b := groupRoomID("secret-key")
	c := groupRoomID("other-key")
	if a != b {
		t.Fatalf("unstable id: %q != %q", a, b)
	}
	if a == c {
		t.Fatal("different keys must map to different rooms")
	}
	if len(a) != len("g-")+16 {
		t.Fatalf("id length = %d (%q)", len(a), a)
	}
	if a == "g-secret-key" || a[2:] == "secret-key" {
		t.Fatal("room id must not contain the raw key")
	}
}

// TestLANRoomChat 验证两个直连节点在局域网房间互相收到消息，并各自落本机历史。
func TestLANRoomChat(t *testing.T) {
	a := newRoomNode(t, "alice")
	b := newRoomNode(t, "bob")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.n.Host.Connect(ctx, peer.AddrInfo{ID: b.n.Host.ID(), Addrs: b.n.Host.Addrs()}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := a.r.Publish(lanRoomID, "hello room"); err != nil {
			t.Fatal(err)
		}
		list, err := b.n.Store.RoomMessages(lanRoomID, 0, 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range list {
			if m.Content == "hello room" && m.SenderID == a.n.Host.ID().String() {
				rooms, err := b.r.Rooms()
				if err != nil || len(rooms) == 0 || !rooms[0].Joined {
					t.Fatalf("rooms = %+v err=%v", rooms, err)
				}
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("timeout waiting for LAN room message")
}

// TestGroupRoomIsolation 验证同组节点互通、异组节点收不到消息。
func TestGroupRoomIsolation(t *testing.T) {
	a := newRoomNodeWithGroup(t, "alice", "k1")
	b := newRoomNodeWithGroup(t, "bob", "k1")
	c := newRoomNodeWithGroup(t, "carol", "k2")
	for _, x := range []*roomNode{b, c} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := a.n.Host.Connect(ctx, peer.AddrInfo{ID: x.n.Host.ID(), Addrs: x.n.Host.Addrs()}); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
	}
	roomID := groupRoomID("k1")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := a.r.Publish(roomID, "only k1"); err != nil {
			t.Fatal(err)
		}
		list, _ := b.n.Store.RoomMessages(roomID, 0, 20)
		if len(list) > 0 {
			if leaked, _ := c.n.Store.RoomMessages(groupRoomID("k2"), 0, 20); len(leaked) > 0 {
				t.Fatalf("k2 received k1 message: %+v", leaked)
			}
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("timeout waiting for group room message")
}

// newRoomNode 构造一个带群聊服务的进程内节点（关闭 mDNS，直连即可）。
//
// 参数:
//   - t: *testing.T — 测试上下文
//   - name: string — 昵称
//
// 返回:
//   - *roomNode — 节点 + 群聊服务，测试结束自动关闭
func newRoomNode(t *testing.T, name string) *roomNode {
	return newRoomNodeWithGroup(t, name, "")
}

// newRoomNodeWithGroup 构造指定分组密钥的群聊测试节点。
//
// 参数:
//   - t: *testing.T — 测试上下文
//   - name: string — 昵称
//   - group: string — 分组密钥；空表示仅局域网房间
//
// 返回:
//   - *roomNode — 节点 + 群聊服务，测试结束自动关闭
func newRoomNodeWithGroup(t *testing.T, name, group string) *roomNode {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	n, err := node.New(node.Config{DataDir: dir, Store: db, DisableMDNS: true, Group: group})
	if err != nil {
		t.Fatal(err)
	}
	db.SetSetting("nickname", name)
	// 测试夹具默认走解锁态；锁定态由 TestRoomsLocked 单独覆盖。
	if err := feature.SetWANUnlocked(db, true); err != nil {
		t.Fatal(err)
	}
	r, err := New(n)
	if err != nil {
		n.Close()
		db.Close()
		t.Fatal(err)
	}
	x := &roomNode{n: n, r: r}
	t.Cleanup(func() { r.Close(); n.Close(); db.Close() })
	return x
}

// TestRoomsLocked 验证门禁锁定态只返回局域网房间，历史分组房间一并隐藏。
func TestRoomsLocked(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.UpsertRoom(store.Room{ID: "g-old", Name: "旧分组", Kind: "group", LastTS: 1}); err != nil {
		t.Fatal(err)
	}
	n, err := node.New(node.Config{DataDir: dir, Store: db, DisableMDNS: true, Group: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	r, err := New(n)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	list, err := r.Rooms()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != lanRoomID || !list[0].Joined {
		t.Fatalf("locked rooms = %+v, want only lan", list)
	}
}

// TestRoomsMarksHistory 验证换过密钥的历史分组房间仍可见但 joined=false，只有局域网与当前分组可收发。
func TestRoomsMarksHistory(t *testing.T) {
	x := newRoomNodeWithGroup(t, "alice", "k1")
	if err := x.n.Store.UpsertRoom(store.Room{ID: "g-old", Name: "旧分组", Kind: "group", LastTS: 1}); err != nil {
		t.Fatal(err)
	}

	list, err := x.r.Rooms()
	if err != nil {
		t.Fatal(err)
	}
	joined := map[string]bool{}
	for _, r := range list {
		joined[r.ID] = r.Joined
	}
	if !joined[lanRoomID] || !joined[groupRoomID("k1")] {
		t.Fatalf("current rooms not joined: %+v", list)
	}
	if ok, exists := joined["g-old"]; !exists || ok {
		t.Fatalf("history room joined=%v exists=%v (%+v)", ok, exists, list)
	}
}

// roomNode 是群聊测试用的最小节点夹具。
type roomNode struct {
	n *node.Node
	r *Service
}
