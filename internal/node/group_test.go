package node

import (
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// testPeer 返回一个稳定的 PeerID（只用作 map key / String()，无需可拨号）。
func testPeer(t *testing.T, seed byte) peer.ID {
	t.Helper()
	return peer.ID([]byte{'p', 'e', 'e', 'r', seed})
}

// TestParseGroupSpecs 验证组表解析：组名/密钥/过期时间、注释与错误行。
func TestParseGroupSpecs(t *testing.T) {
	specs, err := ParseGroupSpecs(strings.NewReader("office:key1:2030-01-01T00:00:00Z\n# comment\nhome:key2\n"))
	if err != nil || len(specs) != 2 {
		t.Fatalf("specs = %+v err = %v", specs, err)
	}
	if specs[0].Name != "office" || specs[0].Key != "key1" || specs[0].ExpiresAt == 0 {
		t.Fatalf("office spec = %+v", specs[0])
	}
	if specs[1].Name != "home" || specs[1].ExpiresAt != 0 {
		t.Fatalf("home spec = %+v", specs[1])
	}
	if _, err := ParseGroupSpecs(strings.NewReader("onlyname\n")); err == nil {
		t.Fatal("line without key must fail")
	}
	if _, err := ParseGroupSpecs(strings.NewReader("a:k\na:k2\n")); err == nil {
		t.Fatal("duplicate name must fail")
	}
	if _, err := ParseGroupSpecs(strings.NewReader("a:k:a:k\n")); err == nil {
		t.Fatal("invalid expiry must fail")
	}
}

// TestGroupIsolationAndExpiry 验证同组过滤、过期后目录/通知停发与软上限。
func TestGroupIsolationAndExpiry(t *testing.T) {
	n := &Node{
		directory: true,
		online:    map[peer.ID]bool{},
		groups:    map[peer.ID]string{},
		groupErr:  map[peer.ID]string{},
	}
	n.initGroups([]GroupSpec{
		{Name: "office", Key: "k1"},
		{Name: "expired", Key: "k2", ExpiresAt: time.Now().Add(-time.Minute).UnixMilli()},
	})
	a, b, c := testPeer(t, 1), testPeer(t, 2), testPeer(t, 3)
	n.online[a], n.online[b], n.online[c] = true, true, true

	if errText := n.assignGroup(a, "k1"); errText != "" {
		t.Fatalf("office key: %s", errText)
	}
	if errText := n.assignGroup(b, "k1"); errText != "" {
		t.Fatalf("office key b: %s", errText)
	}
	if errText := n.assignGroup(c, "k2"); errText != "分组已过期" {
		t.Fatalf("expired key reason = %q", errText)
	}
	if errText := n.assignGroup(c, "nope"); errText != "分组密钥无效" {
		t.Fatalf("invalid key reason = %q", errText)
	}
	if errText := n.assignGroup(c, ""); errText != "缺少分组密钥" {
		t.Fatalf("empty key reason = %q", errText)
	}
	if n.sameGroupLocked(c) {
		t.Fatal("invalid/expired terminal must not be in the directory")
	}
	got := n.directoryFor(a)
	if len(got) != 1 || got[0] != b.String() {
		t.Fatalf("directory = %v, want only b", got)
	}
	if _, _, errText := n.groupInfo(b); errText != "" {
		t.Fatalf("groupInfo b: %q", errText)
	}
	if n.GroupOf(a) != "office" || n.GroupOf(c) != "" {
		t.Fatalf("GroupOf mismatch: a=%q c=%q", n.GroupOf(a), n.GroupOf(c))
	}

	// 软上限：组内在线超过 1 时只下发 1 个（不含提问者）。
	n.groupMax = 1
	got = n.directoryFor(a)
	if len(got) != 1 {
		t.Fatalf("soft cap directory = %v, want 1 entry", got)
	}
}

// TestGroupHotReloadRevoke 验证组表热替换：删组、换钥、过期都会立即收回目录与出口资格。
func TestGroupHotReloadRevoke(t *testing.T) {
	n := &Node{directory: true, online: map[peer.ID]bool{}, groups: map[peer.ID]string{}, groupErr: map[peer.ID]string{}}
	n.initGroups([]GroupSpec{{Name: "office", Key: "k1"}, {Name: "home", Key: "k2"}})
	a, b := testPeer(t, 6), testPeer(t, 7)
	n.online[a], n.online[b] = true, true
	if errText := n.assignGroup(a, "k1"); errText != "" {
		t.Fatal(errText)
	}
	if errText := n.assignGroup(b, "k2"); errText != "" {
		t.Fatal(errText)
	}

	// 删除 office：a 立即失去组资格与目录
	n.applyGroupSpecs([]GroupSpec{{Name: "home", Key: "k2"}})
	if _, _, errText := n.groupInfo(a); errText != "分组已撤销" {
		t.Fatalf("revoked reason = %q", errText)
	}
	if n.GroupOf(a) != "" || n.sameGroupLocked(a) {
		t.Fatal("revoked terminal must lose group/directory")
	}
	if got := n.directoryFor(b); len(got) != 0 {
		t.Fatalf("revoked terminal still visible: %v", got)
	}

	// 同名换钥：b 也必须重新出示新密钥
	n.applyGroupSpecs([]GroupSpec{{Name: "home", Key: "k3"}})
	if _, _, errText := n.groupInfo(b); errText != "分组密钥已轮换" {
		t.Fatalf("rotated reason = %q", errText)
	}
	if errText := n.assignGroup(b, "k3"); errText != "" {
		t.Fatalf("new key rejected: %s", errText)
	}
	if n.GroupOf(b) != "home" {
		t.Fatalf("group after re-auth = %q", n.GroupOf(b))
	}

	// 过期：立即失效
	n.applyGroupSpecs([]GroupSpec{{Name: "home", Key: "k3", ExpiresAt: time.Now().Add(-time.Second).UnixMilli()}})
	if n.GroupOf(b) != "" {
		t.Fatal("expired group must lose exit/directory")
	}
}

// TestLegacySingleGroup 验证旧 -group 仍按 default 组工作，且空表等于不分组。
func TestLegacySingleGroup(t *testing.T) {
	n := &Node{directory: true, group: "legacy", online: map[peer.ID]bool{}, groups: map[peer.ID]string{}, groupErr: map[peer.ID]string{}}
	n.initGroups(nil)
	a, b := testPeer(t, 4), testPeer(t, 5)
	n.online[a], n.online[b] = true, true
	if errText := n.assignGroup(a, "legacy"); errText != "" {
		t.Fatalf("legacy key: %s", errText)
	}
	if errText := n.assignGroup(b, "legacy"); errText != "" {
		t.Fatalf("legacy key b: %s", errText)
	}
	if n.GroupOf(a) != "default" {
		t.Fatalf("legacy group name = %q", n.GroupOf(a))
	}
	if got := n.directoryFor(a); len(got) != 1 || got[0] != b.String() {
		t.Fatalf("legacy directory = %v", got)
	}

	plain := &Node{directory: true, online: map[peer.ID]bool{}, groups: map[peer.ID]string{}, groupErr: map[peer.ID]string{}}
	plain.initGroups(nil)
	if errText := plain.assignGroup(a, ""); errText != "" {
		t.Fatalf("no group table must accept empty key: %s", errText)
	}
	if !plain.sameGroupLocked(a) {
		t.Fatal("no group table means everyone is visible")
	}
}
