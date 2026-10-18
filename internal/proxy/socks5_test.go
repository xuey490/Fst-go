package proxy

import (
	"context"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
)

// socksExit builds a relay+exit pair where every outbound dial is a pipe back
// to the test, and returns the SOCKS5 server bound to a loopback auto port.
func socksExit(t *testing.T, allowAll bool) (*SOCKS5, net.Conn) {
	t.Helper()
	r, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	a, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })

	x := ServeExit(r, ExitOptions{Allow: func(id peer.ID) bool { return allowAll && id == a.ID() }})
	t.Cleanup(x.Close)
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	x.dial = func(context.Context, string, string) (net.Conn, error) { return client, nil }

	exit := r.Addrs()[0].String() + "/p2p/" + r.ID().String()
	c, err := NewClient(a, exit)
	if err != nil {
		t.Fatal(err)
	}
	s, err := StartSOCKS5(c, "127.0.0.1:0", SocksOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, server
}

func socksConn(t *testing.T, s *SOCKS5) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", s.Addr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestSocks5Connect(t *testing.T) {
	s, server := socksExit(t, true)
	conn := socksConn(t, s)

	// greeting: offer no-auth, expect 05 00
	if _, err := conn.Write([]byte{socksVersion, 1, socksMethodNone}); err != nil {
		t.Fatal(err)
	}
	var head [2]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil || head != [2]byte{5, socksMethodNone} {
		t.Fatalf("greeting: %v %v", head, err)
	}

	// CONNECT 203.0.113.9:443
	req := []byte{socksVersion, socksCmdConnect, 0, socksATypIPv4, 203, 0, 113, 9, 1, 187}
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	var rep [10]byte
	if _, err := io.ReadFull(conn, rep[:]); err != nil {
		t.Fatal(err)
	}
	if rep[1] != socksRepSucceeded {
		t.Fatalf("CONNECT should succeed, got rep=%d", rep[1])
	}

	// payload round trip through the exit's fake dial target
	go func() {
		b := make([]byte, 4)
		if _, err := io.ReadFull(server, b); err == nil {
			_, _ = server.Write([]byte("pong"))
		}
	}()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "pong" {
		t.Fatalf("round trip: %q", got)
	}
}

func TestSocks5DeniedByAllowlist(t *testing.T) {
	s, _ := socksExit(t, false) // allowlist rejects everyone
	conn := socksConn(t, s)
	if _, err := conn.Write([]byte{socksVersion, 1, socksMethodNone}); err != nil {
		t.Fatal(err)
	}
	var head [2]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		t.Fatal(err)
	}
	req := []byte{socksVersion, socksCmdConnect, 0, socksATypIPv4, 203, 0, 113, 9, 1, 187}
	if _, err := conn.Write(req); err != nil {
		t.Fatal(err)
	}
	var rep [10]byte
	if _, err := io.ReadFull(conn, rep[:]); err != nil {
		t.Fatal(err)
	}
	if rep[1] == socksRepSucceeded {
		t.Fatal("denied peer must not get a success reply")
	}
}

func TestSocks5HandshakeGreeting(t *testing.T) {
	// A client that offers only username/password auth is refused cleanly.
	s, _ := socksExit(t, true)
	conn := socksConn(t, s)
	if _, err := conn.Write([]byte{socksVersion, 1, 0x02}); err != nil {
		t.Fatal(err)
	}
	var head [2]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		t.Fatal(err)
	}
	if head[1] != socksMethodUnacceptable {
		t.Fatalf("unsupported methods must be rejected, got %v", head)
	}
}

func TestSocksDatagramBuildParse(t *testing.T) {
	v4 := &net.UDPAddr{IP: netip.MustParseAddr("203.0.113.9").AsSlice(), Port: 53}
	d := buildDatagram(v4, []byte("hi"))
	target, payload, err := parseDatagram(d)
	if err != nil || target.String() != v4.String() || string(payload) != "hi" {
		t.Fatalf("v4: %v %q %v", target, payload, err)
	}

	v6 := &net.UDPAddr{IP: netip.MustParseAddr("2001:db8::1").AsSlice(), Port: 443}
	d = buildDatagram(v6, []byte("x"))
	target, payload, err = parseDatagram(d)
	if err != nil || target.String() != v6.String() || string(payload) != "x" {
		t.Fatalf("v6: %v %q %v", target, payload, err)
	}

	if _, _, err := parseDatagram([]byte{0, 0, 1, socksATypIPv4}); err == nil {
		t.Fatal("fragmented flag must be rejected")
	}
}

// TestSOCKS5AuthHandshake 验证 RFC1929 协商成功/失败与客户端未提供用户名密码方法时的拒绝。
func TestSOCKS5AuthHandshake(t *testing.T) {
	s := &SOCKS5{opt: SocksOptions{User: "u", Pass: "p"}}
	server, client := net.Pipe()
	errCh := make(chan error, 1)
	go func() { errCh <- s.handshake(server) }()
	if _, err := client.Write([]byte{socksVersion, 1, socksMethodUserPass}); err != nil {
		t.Fatal(err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(client, method); err != nil || method[1] != socksMethodUserPass {
		t.Fatalf("method reply = %v %v", method, err)
	}
	if _, err := client.Write([]byte{0x01, 1, 'u', 1, 'p'}); err != nil {
		t.Fatal(err)
	}
	status := make([]byte, 2)
	if _, err := io.ReadFull(client, status); err != nil || status[1] != 0x00 {
		t.Fatalf("auth status = %v %v", status, err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("handshake: %v", err)
	}
	_ = server.Close()
	_ = client.Close()

	// 客户端只提供 no-auth：必须回 0xFF 并失败。
	server, client = net.Pipe()
	errCh = make(chan error, 1)
	go func() { errCh <- s.handshake(server) }()
	_, _ = client.Write([]byte{socksVersion, 1, socksMethodNone})
	reply := make([]byte, 2)
	if _, err := io.ReadFull(client, reply); err != nil || reply[1] != socksMethodUnacceptable {
		t.Fatalf("reject reply = %v %v", reply, err)
	}
	if err := <-errCh; err == nil {
		t.Fatal("missing username/password method must fail")
	}
	_ = server.Close()
	_ = client.Close()
}

// TestSOCKS5SourceAndLimits 验证来源分类、仅回环默认行为与并发上限计数。
func TestSOCKS5SourceAndLimits(t *testing.T) {
	if !isLocalSourceAddr(netip.MustParseAddr("127.0.0.1")) ||
		!isLocalSourceAddr(netip.MustParseAddr("192.168.1.5")) ||
		!isLocalSourceAddr(netip.MustParseAddr("fd00::1")) ||
		isLocalSourceAddr(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("source classification mismatch")
	}
	s := &SOCKS5{opt: SocksOptions{MaxTotal: 1}, perIP: map[string]int{}}
	c := fakeAddrConn{}
	release, ok := s.acquire(c)
	if !ok {
		t.Fatal("first acquire must succeed")
	}
	if _, ok := s.acquire(c); ok {
		t.Fatal("global limit must reject the second connection")
	}
	release()
	if _, ok := s.acquire(c); !ok {
		t.Fatal("slot must be reusable after release")
	}
	if !sameSecret("secret", "secret") || sameSecret("secret", "other") {
		t.Fatal("secret comparison mismatch")
	}
}

// fakeAddrConn 是只有 RemoteAddr 的假连接，供并发计数测试使用。
type fakeAddrConn struct{ net.Conn }

// RemoteAddr 实现 net.Conn，固定返回 127.0.0.1（实现 net.Conn 的地址部分）。
func (fakeAddrConn) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234} }
