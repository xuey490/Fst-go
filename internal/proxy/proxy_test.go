package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"fst/internal/proto"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
)

// TestExitUnavailableHint 验证「对端未注册出口协议」被翻译成带 -exit 的提示，其它错误原样包装。
func TestExitUnavailableHint(t *testing.T) {
	err := exitUnavailable(errors.New("failed to negotiate protocol: protocols not supported: [/fst/proxy/1.0.0]"))
	if !strings.Contains(err.Error(), "-exit") {
		t.Fatalf("missing -exit hint: %v", err)
	}
	other := exitUnavailable(context.DeadlineExceeded)
	if !strings.Contains(other.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("other error not wrapped: %v", other)
	}
}

func TestProxyRoundTripAndAllowlist(t *testing.T) {
	r, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	a, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	x := ServeExit(r, ExitOptions{Allow: func(id peer.ID) bool { return id == a.ID() }})
	defer x.Close()
	server, client := net.Pipe()
	defer server.Close()
	x.dial = func(context.Context, string, string) (net.Conn, error) { return client, nil }

	exit := r.Addrs()[0].String() + "/p2p/" + r.ID().String()
	c, err := NewClient(a, exit)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got := make(chan string, 1)
	go func() {
		b := make([]byte, 4)
		_, _ = io.ReadFull(server, b)
		got <- string(b)
		_, _ = server.Write([]byte("pong"))
	}()
	conn, err := c.DialContext(ctx, "tcp", "203.0.113.9:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(conn, b); err != nil {
		t.Fatal(err)
	}
	serverGot := <-got
	if serverGot != "ping" || string(b) != "pong" {
		t.Fatalf("round trip: server=%q client=%q", serverGot, b)
	}

	bad, _ := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	defer bad.Close()
	denied, _ := NewClient(bad, exit)
	if _, err := denied.DialContext(ctx, "tcp", "203.0.113.9:443"); err == nil {
		t.Fatal("non-allowlisted peer must be rejected")
	}
}

func TestValidatedTarget(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1"} {
		if _, err := validatedTarget(proxyReq(ip)); err == nil {
			t.Fatalf("%s should be rejected", ip)
		}
	}
	if got, err := validatedTarget(proxyReq("203.0.113.9")); err != nil || got != "203.0.113.9:443" {
		t.Fatalf("public target: %q %v", got, err)
	}
}

func TestValidateTUNConfig(t *testing.T) {
	cfg := TUNConfig{Mode: "split", ExitIPv4: "203.0.113.9", Routes: []string{"198.51.100.0/24"}}
	p, p6, _, err := validateTUNConfig(cfg)
	if err != nil || len(p) != 1 || p[0].String() != "198.51.100.0/24" || len(p6) != 0 {
		t.Fatalf("valid split config: %v %v %v", p, p6, err)
	}
	cfg.Routes = []string{"203.0.113.0/24"}
	if _, _, _, err := validateTUNConfig(cfg); err == nil {
		t.Fatal("a route containing the relay must be rejected")
	}
	if _, _, _, err := validateTUNConfig(TUNConfig{Mode: "split", ExitIPv4: "203.0.113.9"}); err == nil {
		t.Fatal("split mode without routes or domains must be rejected")
	}
	if _, _, _, err := validateTUNConfig(TUNConfig{Mode: "split", ExitIPv4: "203.0.113.9", Domains: []string{"google.com"}}); err != nil {
		t.Fatalf("split with domains: %v", err)
	}
	if _, _, _, err := validateTUNConfig(TUNConfig{Mode: "global", ExitIPv4: "203.0.113.9"}); err != nil {
		t.Fatalf("global mode: %v", err)
	}
	// IPv6 CIDR 需要显式开启 IPv6；开启后进入 v6 列表。
	v6cfg := TUNConfig{Mode: "split", ExitIPv4: "203.0.113.9", IPv6: true, Routes: []string{"2001:db8::/32"}}
	if _, p6, _, err := validateTUNConfig(v6cfg); err != nil || len(p6) != 1 || p6[0].String() != "2001:db8::/32" {
		t.Fatalf("ipv6 split config: %v %v", p6, err)
	}
	v6cfg.IPv6 = false
	if _, _, _, err := validateTUNConfig(v6cfg); err == nil {
		t.Fatal("ipv6 CIDR without proxy.ipv6 must be rejected")
	}
}

func TestValidatedTargetUDPAndHost(t *testing.T) {
	if _, err := validatedTarget(proto.ProxyReq{Net: "udp", Addr: "203.0.113.9", Port: 53}); err != nil {
		t.Fatal(err)
	}
	got, err := validatedTarget(proto.ProxyReq{Net: "tcp", Addr: "example.com", Port: 443})
	if err != nil || got != "example.com:443" {
		t.Fatalf("host: %q %v", got, err)
	}
}

func TestDatagramRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() { _ = writeDatagram(a, []byte("hi")) }()
	buf := make([]byte, 16)
	n, err := readDatagram(b, buf)
	if err != nil || string(buf[:n]) != "hi" {
		t.Fatalf("datagram: %q %v", buf[:n], err)
	}
}

func TestProxyUDPRoundTrip(t *testing.T) {
	r, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	a, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	x := ServeExit(r, ExitOptions{Allow: func(id peer.ID) bool { return id == a.ID() }})
	defer x.Close()
	server, client := net.Pipe()
	defer server.Close()
	x.listenPacket = func(string) (net.PacketConn, error) {
		return &pipePC{Conn: client, remote: &net.UDPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 53}}, nil
	}

	exit := r.Addrs()[0].String() + "/p2p/" + r.ID().String()
	c, err := NewClient(a, exit)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got := make(chan string, 1)
	go func() {
		b := make([]byte, 4)
		_, _ = io.ReadFull(server, b)
		got <- string(b)
		_, _ = server.Write([]byte("pong"))
	}()
	pc, err := c.DialUDP(ctx, "203.0.113.9:53")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.WriteTo([]byte("ping"), nil); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	serverGot := <-got
	if serverGot != "ping" || string(buf[:n]) != "pong" {
		t.Fatalf("udp round trip: server=%q client=%q", serverGot, buf[:n])
	}
}

type pipePC struct {
	net.Conn
	remote net.Addr
}

func (p *pipePC) ReadFrom(b []byte) (int, net.Addr, error) {
	n, err := p.Conn.Read(b)
	return n, p.remote, err
}

func (p *pipePC) WriteTo(b []byte, _ net.Addr) (int, error) {
	return p.Conn.Write(b)
}

func proxyReq(ip string) proto.ProxyReq {
	return proto.ProxyReq{Net: "tcp", Addr: ip, Port: 443}
}
