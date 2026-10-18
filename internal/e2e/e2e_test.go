// Package e2e runs several real libp2p nodes in-process (loopback, no mDNS) to check
// presence, chat, file transfer with resume, and peer-exchange meshing.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fst/internal/chat"
	"fst/internal/node"
	"fst/internal/store"
	"fst/internal/transfer"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	circuit "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
)

type tn struct {
	n      *node.Node
	chat   *chat.Service
	xfer   *transfer.Service
	db     *store.Store
	dl     string
	mu     sync.Mutex
	once   sync.Once
	events []event
}

// shutdown 关闭节点与数据库（可重复调用），用于模拟“应用退出后重启”。
func (x *tn) shutdown() {
	x.once.Do(func() {
		x.n.Close()
		x.db.Close()
	})
}

type event struct {
	name string
	data any
}

// wan switches newNode to the opt-in phase-5 libp2p options (AutoNATv2, relay service, hole punching).
var wan bool

// directory makes newNode build a relay-role node (public, hands out the online-peer directory).
var directory bool

// group is the relay-group key terminals present; relayGroup is the key a relay-role node serves.
var group, relayGroup string

func newNode(t *testing.T, name string, relays ...string) *tn {
	return newNodeIn(t, t.TempDir(), name, relays...)
}

// newNodeIn 在指定数据目录构造节点，便于用同一份 identity.key 模拟重启。
//
// 参数:
//   - t: *testing.T — 测试上下文
//   - dir: string — 数据目录（重复使用时必须已 shutdown 旧节点）
//   - name: string — 昵称
//   - relays: ...string — 静态中继地址
//
// 返回:
//   - *tn — 节点夹具，测试结束自动关闭
func newNodeIn(t *testing.T, dir, name string, relays ...string) *tn {
	t.Helper()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	x := &tn{dl: filepath.Join(dir, "dl"), db: db}
	grp := group
	if directory {
		grp = relayGroup
	}
	n, err := node.New(node.Config{
		DataDir: dir, Store: db, DisableMDNS: true, WAN: wan, Relays: relays, Directory: directory, ForcePublic: directory, Group: grp,
		Emit: func(name string, data any) {
			x.mu.Lock()
			x.events = append(x.events, event{name, data})
			x.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(x.shutdown)
	db.SetSetting("nickname", name)
	db.SetSetting("download_dir", x.dl)
	x.n, x.chat, x.xfer = n, chat.New(n), transfer.New(n)
	return x
}

func (a *tn) connect(t *testing.T, b *tn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.n.Host.Connect(ctx, peer.AddrInfo{ID: b.n.Host.ID(), Addrs: b.n.Host.Addrs()}); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func (a *tn) online(b *tn) bool { return a.n.IsOnline(b.n.Host.ID()) }

func (a *tn) find(name, status string) *store.Transfer {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.events {
		if tr, ok := e.data.(store.Transfer); ok && e.name == name && tr.Status == status {
			c := tr
			return &c
		}
	}
	return nil
}

func randFile(t *testing.T, dir string, size int) (string, []byte) {
	t.Helper()
	b := make([]byte, size)
	rand.Read(b)
	p := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, b
}

func TestPresenceChatAndTransfer(t *testing.T) {
	a, b := newNode(t, "alice"), newNode(t, "bob")
	a.connect(t, b)
	waitFor(t, "mutual online", func() bool { return a.online(b) && b.online(a) })

	// HELLO carried the nickname
	if got := a.n.PeerName(b.n.Host.ID().String()); got != "bob" {
		t.Fatalf("peer name = %q", got)
	}

	// chat both ways, persisted on both sides
	m, err := a.chat.Send(b.n.Host.ID().String(), "你好 bob")
	if err != nil || m.Status != "delivered" {
		t.Fatalf("send: %v %+v", err, m)
	}
	msgs, _ := b.n.Store.Messages(a.n.Host.ID().String(), 0, 10)
	if len(msgs) != 1 || msgs[0].Content != "你好 bob" || msgs[0].Direction != "in" {
		t.Fatalf("bob inbox: %+v", msgs)
	}
	if _, err := b.chat.Send(a.n.Host.ID().String(), "hi alice"); err != nil {
		t.Fatal(err)
	}

	// file transfer, auto accepted
	b.n.Store.SetSetting("auto_accept", "1")
	src, data := randFile(t, t.TempDir(), 3<<20+123)
	if _, err := a.xfer.Send(b.n.Host.ID().String(), []string{src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "receiver done", func() bool { return b.find("transfer:done", "done") != nil })
	got, err := os.ReadFile(b.find("transfer:done", "done").LocalPath)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("received file differs (err=%v)", err)
	}
	waitFor(t, "sender done", func() bool { return a.find("transfer:done", "done") != nil })

	// offline detection
	b.n.Close()
	waitFor(t, "alice sees bob offline", func() bool { return !a.online(b) })
}

// TestChatReplyRoundTrip 验证引用信息随 CHAT 帧送达，并完整体现在接收端的本地历史里。
func TestChatReplyRoundTrip(t *testing.T) {
	a, b := newNode(t, "alice"), newNode(t, "bob")
	a.connect(t, b)
	waitFor(t, "mutual online", func() bool { return a.online(b) && b.online(a) })

	first, err := a.chat.Send(b.n.Host.ID().String(), "question")
	if err != nil || first.Status != "delivered" {
		t.Fatalf("first: %v %+v", err, first)
	}
	if _, err := a.chat.SendMessageReply(b.n.Host.ID().String(), "answer", "text", nil,
		chat.Reply{ID: first.ID, Preview: "question", Sender: "alice"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reply persisted on receiver", func() bool {
		list, _ := b.n.Store.Messages(a.n.Host.ID().String(), 0, 10)
		for _, m := range list {
			if m.Content == "answer" {
				return m.ReplyID == first.ID && m.ReplyPreview == "question" && m.ReplySender == "alice"
			}
		}
		return false
	})
}

// TestOfflineMessageQueue 验证对端离线时消息转 queued，对端用同一身份重启上线后自动投递。
func TestOfflineMessageQueue(t *testing.T) {
	a := newNode(t, "alice")
	bdir := t.TempDir()
	b := newNodeIn(t, bdir, "bob")
	a.connect(t, b)
	waitFor(t, "mutual online", func() bool { return a.online(b) && b.online(a) })
	bpid := b.n.Host.ID().String()

	b.shutdown()
	waitFor(t, "alice sees bob offline", func() bool { return !a.online(b) })

	m, err := a.chat.Send(bpid, "offline hello")
	if err != nil || m.Status != "queued" {
		t.Fatalf("offline send = %+v err=%v", m, err)
	}
	// 离线时手动冲刷不应丢消息
	a.chat.FlushPeer(bpid)
	if got, _ := a.n.Store.Message(m.ID); got.Status != "queued" {
		t.Fatalf("message mutated while offline: %+v", got)
	}

	// 同一 identity.key 重启后，消息应自动投递
	b2 := newNodeIn(t, bdir, "bob")
	if b2.n.Host.ID().String() != bpid {
		t.Fatalf("identity changed: %s != %s", b2.n.Host.ID(), bpid)
	}
	// 旧连接的断开可能滞后于 shutdown；重试拨号直到真正建立新连接。
	info := peer.AddrInfo{ID: b2.n.Host.ID(), Addrs: b2.n.Host.Addrs()}
	waitFor(t, "bob back online", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = a.n.Host.Connect(ctx, info)
		cancel()
		return a.n.Host.Network().Connectedness(info.ID) == network.Connected && a.online(b2)
	})
	a.chat.FlushPeer(bpid)
	waitFor(t, "alice message delivered", func() bool {
		got, _ := a.n.Store.Message(m.ID)
		if got.Status == "failed" {
			t.Fatalf("queued message failed: %+v", got)
		}
		return got.Status == "delivered"
	})
	waitFor(t, "bob received offline message", func() bool {
		list, _ := b2.n.Store.Messages(a.n.Host.ID().String(), 0, 20)
		for _, got := range list {
			if got.Content == "offline hello" {
				return true
			}
		}
		return false
	})
}

func TestResumeFromPartFile(t *testing.T) {
	a, b := newNode(t, "alice"), newNode(t, "bob")
	a.connect(t, b)
	waitFor(t, "online", func() bool { return a.online(b) && b.online(a) })
	b.n.Store.SetSetting("auto_accept", "1")

	src, data := randFile(t, t.TempDir(), 2<<20)
	const have = 1 << 20
	os.MkdirAll(b.dl, 0o755)
	part := filepath.Join(b.dl, fmt.Sprintf("%s.%d.fstpart", filepath.Base(src), len(data)))
	if err := os.WriteFile(part, data[:have], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.xfer.Send(b.n.Host.ID().String(), []string{src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "done", func() bool { return b.find("transfer:done", "done") != nil })
	if acc := b.find("transfer:update", "accepted"); acc == nil || acc.BytesDone != have {
		t.Fatalf("receiver did not offer resume offset: %+v", acc)
	}
	got, _ := os.ReadFile(b.find("transfer:done", "done").LocalPath)
	if !bytes.Equal(got, data) {
		t.Fatal("resumed file differs")
	}
}

func TestCorruptPartIsRejectedAndDiscarded(t *testing.T) {
	a, b := newNode(t, "alice"), newNode(t, "bob")
	a.connect(t, b)
	waitFor(t, "online", func() bool { return a.online(b) && b.online(a) })
	b.n.Store.SetSetting("auto_accept", "1")

	src, data := randFile(t, t.TempDir(), 1<<20)
	os.MkdirAll(b.dl, 0o755)
	part := filepath.Join(b.dl, fmt.Sprintf("%s.%d.fstpart", filepath.Base(src), len(data)))
	os.WriteFile(part, bytes.Repeat([]byte{7}, 4096), 0o644) // wrong prefix
	a.xfer.Send(b.n.Host.ID().String(), []string{src})
	waitFor(t, "failure", func() bool { return b.find("transfer:failed", "failed") != nil })
	if _, err := os.Stat(part); err == nil {
		t.Fatal("corrupt part file must be deleted")
	}
	// retry now starts from zero and succeeds
	waitFor(t, "sender failed", func() bool { return a.find("transfer:failed", "failed") != nil })
	if _, err := a.xfer.Retry(a.find("transfer:failed", "failed").ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "retry done", func() bool { return b.find("transfer:done", "done") != nil })
}

func TestRejectAndCancel(t *testing.T) {
	a, b := newNode(t, "alice"), newNode(t, "bob")
	a.connect(t, b)
	waitFor(t, "online", func() bool { return a.online(b) && b.online(a) })

	src, _ := randFile(t, t.TempDir(), 1024)
	batch, err := a.xfer.Send(b.n.Host.ID().String(), []string{src})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "offer shown", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		for _, e := range b.events {
			if e.name == "transfer:offer" {
				return true
			}
		}
		return false
	})
	if err := b.xfer.Decide(batch, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sender sees rejection", func() bool { return a.find("transfer:update", "rejected") != nil })
}

// C only knows A; A introduces B and C to each other through HELLO peer lists.
func TestPeerExchangeFormsMesh(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	b.connect(t, a)
	waitFor(t, "a-b", func() bool { return a.online(b) && b.online(a) })
	c.connect(t, a)
	waitFor(t, "b-c via introduction", func() bool { return b.online(c) && c.online(b) })
}

// The WAN options must not break local connectivity (they only add capabilities).
func TestWANModeStillWorksLocally(t *testing.T) {
	wan = true
	defer func() { wan = false }()
	a, b := newNode(t, "alice"), newNode(t, "bob")
	a.connect(t, b)
	waitFor(t, "mutual online", func() bool { return a.online(b) && b.online(a) })
	if m, err := a.chat.Send(b.n.Host.ID().String(), "hi"); err != nil || m.Status != "delivered" {
		t.Fatalf("send: %v %+v", err, m)
	}
}

// A and B never learn each other's direct addresses; they meet only through relay R (circuit v2).
// Covers: dialing a /p2p-circuit multiaddr via ConnectString, HELLO/chat over the relayed connection,
// and the Relayed flag shown in the UI. Hole punching is only smoke-tested elsewhere (needs real NATs).
func TestRelayedChat(t *testing.T) {
	wan = true
	defer func() { wan = false }()
	r, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"), libp2p.ForceReachabilityPublic(),
		libp2p.EnableRelayService(relay.WithInfiniteLimits()))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	a, b := newNode(t, "alice"), newNode(t, "bob")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := circuit.Reserve(ctx, b.n.Host, peer.AddrInfo{ID: r.ID(), Addrs: r.Addrs()}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	addr := fmt.Sprintf("%s/p2p/%s/p2p-circuit/p2p/%s", r.Addrs()[0], r.ID(), b.n.Host.ID())
	if err := a.n.ConnectString(ctx, addr); err != nil {
		t.Fatalf("dial via relay: %v", err)
	}
	waitFor(t, "mutual online via relay", func() bool { return a.online(b) && b.online(a) })

	if v, _ := a.n.PeerView(b.n.Host.ID().String()); !v.Relayed {
		t.Fatalf("peer should be shown as relayed: %+v", v)
	}
	if m, err := a.chat.Send(b.n.Host.ID().String(), "via relay"); err != nil || m.Status != "delivered" {
		t.Fatalf("send: %v %+v", err, m)
	}
	if msgs, _ := b.n.Store.Messages(a.n.Host.ID().String(), 0, 10); len(msgs) != 1 || msgs[0].Content != "via relay" {
		t.Fatalf("bob inbox: %+v", msgs)
	}
}

// A and B only know seed R. Adding B by PeerID alone uses DHT if it knows B, otherwise the circuit
// through R (terminals are DHT clients, so loopback tests go through the circuit path).
func TestAddByPeerIDViaDHT(t *testing.T) {
	wan = true
	defer func() { wan = false }()
	r, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"), libp2p.ForceReachabilityPublic(),
		libp2p.EnableRelayService(relay.WithInfiniteLimits()))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	d, err := dht.New(r, dht.Mode(dht.ModeServer), dht.ProtocolPrefix("/fst"), dht.DisableValues(), dht.DisableProviders(),
		dht.QueryFilter(func(any, peer.AddrInfo) bool { return true }),
		dht.RoutingTableFilter(func(any, peer.ID) bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	seed := fmt.Sprintf("%s/p2p/%s", r.Addrs()[0], r.ID())
	a, b := newNode(t, "alice", seed), newNode(t, "bob", seed)
	ctxR, cancelR := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelR()
	for _, x := range []*tn{a, b} {
		if _, err := circuit.Reserve(ctxR, x.n.Host, peer.AddrInfo{ID: r.ID(), Addrs: r.Addrs()}); err != nil {
			t.Fatalf("reserve: %v", err)
		}
	}

	var last error
	waitFor(t, "PeerID dial", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		last = a.n.ConnectString(ctx, b.n.Host.ID().String())
		return last == nil
	})
	waitFor(t, "mutual online", func() bool { return a.online(b) && b.online(a) })
	if m, err := a.chat.Send(b.n.Host.ID().String(), "found you"); err != nil || m.Status != "delivered" {
		t.Fatalf("send: %v %+v (last lookup err %v)", err, m, last)
	}
}

// With relays configured, the node must declare itself private: AutoRelay only starts after a reachability
// event, and AutoNAT never emits one for a node with no public address (live symptom: NO_RESERVATION).
// ponytail: the reservation itself cannot be asserted here, AutoRelay drops loopback relay addresses;
// it was verified against a real public relay.
func TestRelaysForcePrivate(t *testing.T) {
	wan = true
	defer func() { wan = false }()
	r, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b := newNode(t, "bob", fmt.Sprintf("%s/p2p/%s", r.Addrs()[0], r.ID()))
	waitFor(t, "private reachability", func() bool { return b.n.Self().Reachability == "private" })
}

// R is an FST relay (Directory on). A and B only list R in their config; they never see each other's
// address. B must learn A from R's HELLO reply and reach it through R. A third node with cross-network
// off (C) gets the same directory and must ignore it (LAN-only behaviour unchanged).
// ponytail: A and B Reserve by hand, AutoRelay drops loopback relay addresses (see TestRelaysForcePrivate).
func TestRelayDirectory(t *testing.T) {
	wan, directory = true, true
	rn := newNode(t, "relay")
	directory = false
	defer func() { wan = false }()
	var raddr string
	for _, a := range rn.n.Host.Addrs() {
		if strings.HasPrefix(a.String(), "/ip4/127.0.0.1/tcp/") {
			raddr = fmt.Sprintf("%s/p2p/%s", a, rn.n.Host.ID())
		}
	}
	if raddr == "" {
		t.Fatal("relay has no loopback address")
	}
	a, b := newNode(t, "alice", raddr), newNode(t, "bob", raddr)
	wan = false
	c := newNode(t, "carol", raddr) // cross-network off

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, x := range []*tn{a, b} {
		if _, err := circuit.Reserve(ctx, x.n.Host, peer.AddrInfo{ID: rn.n.Host.ID(), Addrs: rn.n.Host.Addrs()}); err != nil {
			t.Fatalf("reserve: %v", err)
		}
	}
	// A joins first: the directory it receives is empty. Then B joins and must be pointed at A.
	a.connect(t, rn)
	waitFor(t, "relay sees alice", func() bool { return rn.online(a) && a.online(rn) })
	// no Relayed assertion: DCUtR may upgrade the circuit to a direct link on loopback within milliseconds
	b.connect(t, rn)
	waitFor(t, "bob finds alice via directory", func() bool { return a.online(b) && b.online(a) })
	if v, ok := a.n.PeerView(rn.n.Host.ID().String()); !ok || !v.IsRelay {
		t.Fatalf("relay should be marked IsRelay: ok=%v %+v", ok, v)
	}
	if v, _ := a.n.PeerView(b.n.Host.ID().String()); v.IsRelay {
		t.Fatalf("bob is a terminal, not a relay: %+v", v)
	}

	// carol is told about alice and bob but, with WAN off, must not dial them
	c.connect(t, rn)
	waitFor(t, "carol online with relay", func() bool { return c.online(rn) })
	time.Sleep(2 * time.Second)
	if c.online(a) || c.online(b) {
		t.Fatal("a WAN-off node must ignore the relay directory")
	}
}

// 中继分组：同组终端经目录互见；异组终端既拿不到目录，也收不到到达通知。
func TestRelayGroupIsolation(t *testing.T) {
	wan, directory, relayGroup = true, true, "team-a"
	rn := newNode(t, "relay")
	directory = false
	defer func() { wan, relayGroup, group = false, "", "" }()

	var raddr string
	for _, a := range rn.n.Host.Addrs() {
		if strings.HasPrefix(a.String(), "/ip4/127.0.0.1/tcp/") {
			raddr = fmt.Sprintf("%s/p2p/%s", a, rn.n.Host.ID())
		}
	}
	if raddr == "" {
		t.Fatal("relay has no loopback address")
	}

	group = "team-a"
	a, b := newNode(t, "alice", raddr), newNode(t, "bob", raddr)
	group = "team-b"
	c := newNode(t, "carol", raddr)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, x := range []*tn{a, b} {
		if _, err := circuit.Reserve(ctx, x.n.Host, peer.AddrInfo{ID: rn.n.Host.ID(), Addrs: rn.n.Host.Addrs()}); err != nil {
			t.Fatalf("reserve: %v", err)
		}
	}

	a.connect(t, rn)
	waitFor(t, "relay sees alice", func() bool { return rn.online(a) && a.online(rn) })
	b.connect(t, rn)
	waitFor(t, "bob finds alice in the same group", func() bool { return a.online(b) && b.online(a) })
	c.connect(t, rn)
	waitFor(t, "carol online with relay", func() bool { return c.online(rn) })
	time.Sleep(2 * time.Second)
	if c.online(a) || c.online(b) || a.online(c) || b.online(c) {
		t.Fatal("a terminal of another group must stay invisible")
	}
}
