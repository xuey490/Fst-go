package proxy

// Local SOCKS5 inbound (RFC1928) that exposes the relay exit as a standard
// proxy, in the spirit of v2ray's proxy/socks inbound. It shares the same
// libp2p Client as the TUN front end:
//
//   TUN (transparent capture, optional) ─┐
//   browser / system manual proxy ───────┤→ Client → /fst/proxy/1.0.0 → exit
//
// CONNECT carries the hostname untouched (socks5h semantics), so name
// resolving happens on the exit and a polluted local resolver cannot hurt it.
// UDP ASSOCIATE forwards datagrams via one tunnel conn per target.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

const (
	socksVersion      = 5
	socksDialTimeout  = 10 * time.Second
	socksMaxTargets   = 64
	socksDatagramSize = 65535
	socksAuthFailWait = 500 * time.Millisecond
)

const (
	socksMethodNone         = 0x00
	socksMethodUserPass     = 0x02
	socksMethodUnacceptable = 0xFF

	socksCmdConnect      = 0x01
	socksCmdBind         = 0x02
	socksCmdUDPAssociate = 0x03

	socksRepSucceeded           = 0x00
	socksRepGeneralFailure      = 0x01
	socksRepHostUnreachable     = 0x04
	socksRepCommandNotSupported = 0x07

	socksATypIPv4   = 0x01
	socksATypDomain = 0x03
	socksATypIPv6   = 0x04
)

// SocksOptions 是本机 SOCKS5 入站的加固选项；零值等于「仅回环、免认证、不限并发」。
type SocksOptions struct {
	// User/Pass 非空即启用 RFC1929 用户名密码认证，并要求客户端只提供 0x02。
	User, Pass string
	// Allow 是允许的来源 CIDR；空表示只允许回环。非空时来源还必须是回环/私网/ULA。
	Allow []netip.Prefix
	// MaxTotal/MaxPerIP 是并发连接上限；0 表示不限制。
	MaxTotal, MaxPerIP int
}

// SOCKS5 是本地入站服务器。默认只监听回环；开放给局域网时必须启用认证。
type SOCKS5 struct {
	client *Client
	ln     net.Listener
	opt    SocksOptions
	mu     sync.Mutex
	closed bool
	total  int
	perIP  map[string]int
}

// StartSOCKS5 启动本地 SOCKS5 入站。
//
// 参数:
//   - client: *Client — 与 TUN 共用的出口客户端
//   - listen: string — 监听地址；空串表示 127.0.0.1 随机端口
//   - opt: SocksOptions — 认证/来源/并发；零值为旧行为
//
// 返回:
//   - *SOCKS5 — 运行中的服务器（Close 停止）
//   - error — 地址非法、监听失败，或「非回环监听/允许局域网来源但未设置密码」
func StartSOCKS5(client *Client, listen string, opt SocksOptions) (*SOCKS5, error) {
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" {
		return nil, fmt.Errorf("SOCKS5 监听地址无效: %q", listen)
	}
	if opt.User != "" && opt.Pass == "" {
		return nil, errors.New("SOCKS5 密码不能为空")
	}
	if len(opt.Allow) > 0 && opt.User == "" {
		return nil, errors.New("允许局域网来源时必须设置 SOCKS5 用户名密码")
	}
	if !isLoopbackHost(host) && opt.User == "" {
		return nil, errors.New("监听非回环地址时必须设置 SOCKS5 用户名密码")
	}
	for _, p := range opt.Allow {
		if addr := p.Addr().Unmap(); !isLocalSourceAddr(addr) {
			return nil, fmt.Errorf("SOCKS5 来源白名单只允许回环/私网/ULA: %s", p)
		}
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("SOCKS5 监听失败: %w", err)
	}
	s := &SOCKS5{client: client, ln: ln, opt: opt, perIP: map[string]int{}}
	go s.serve()
	return s, nil
}

// isLoopbackHost 判断监听主机名是否是回环（含空串/localhost）。
//
// 参数:
//   - host: string — 监听地址中的主机部分
//
// 返回:
//   - bool — 回环或 localhost 时为 true
func isLoopbackHost(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// isLocalSourceAddr 判断来源地址是否属于回环/私网/ULA（链路本地也拒绝）。
//
// 参数:
//   - addr: netip.Addr — 已 Unmap 的来源地址
//
// 返回:
//   - bool — 本地可达来源为 true；公网地址为 false
func isLocalSourceAddr(addr netip.Addr) bool {
	return addr.IsLoopback() || addr.IsPrivate()
}

// remoteAddr 取连接的远端 IP。
//
// 参数:
//   - conn: net.Conn — 已接受的连接
//
// 返回:
//   - netip.Addr — Unmap 后的地址；解析失败时返回零值
func remoteAddr(conn net.Conn) netip.Addr {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return netip.Addr{}
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

// sourceAllowed 校验来源是否被允许（未配置 Allow 时只允许回环）。
//
// 参数:
//   - conn: net.Conn — 已接受的连接
//
// 返回:
//   - bool — 允许接入时为 true
func (s *SOCKS5) sourceAllowed(conn net.Conn) bool {
	addr := remoteAddr(conn)
	if !addr.IsValid() {
		return false
	}
	if len(s.opt.Allow) == 0 {
		return addr.IsLoopback()
	}
	if !isLocalSourceAddr(addr) {
		return false
	}
	for _, p := range s.opt.Allow {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// acquire 占用一个并发名额（按 IP 与全局分别计数）。
//
// 参数:
//   - conn: net.Conn — 已接受的连接（用于来源 IP）
//
// 返回:
//   - func() — 释放函数；拒绝时为 nil
//   - bool — 是否允许建立会话
func (s *SOCKS5) acquire(conn net.Conn) (func(), bool) {
	ip := remoteAddr(conn).String()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opt.MaxTotal > 0 && s.total >= s.opt.MaxTotal {
		return nil, false
	}
	if s.opt.MaxPerIP > 0 && s.perIP[ip] >= s.opt.MaxPerIP {
		return nil, false
	}
	s.total++
	s.perIP[ip]++
	return func() {
		s.mu.Lock()
		s.total--
		if s.perIP[ip]--; s.perIP[ip] <= 0 {
			delete(s.perIP, ip)
		}
		s.mu.Unlock()
	}, true
}

// sameSecret 常量时间比较两个口令（先做 SHA-256，避免长度泄露）。
//
// 参数:
//   - a: string — 客户端提交值
//   - b: string — 配置值
//
// 返回:
//   - bool — 完全一致时为 true
func sameSecret(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

func (s *SOCKS5) Addr() string { return s.ln.Addr().String() }

func (s *SOCKS5) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.ln.Close()
}

func (s *SOCKS5) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			continue
		}
		go s.handle(conn)
	}
}

// handle 处理一条入站连接：来源校验 → 并发名额 → 认证 → 请求分发。
//
// 参数:
//   - conn: net.Conn — 客户端连接（函数返回时关闭）
//
// 返回:
//   - 无
func (s *SOCKS5) handle(conn net.Conn) {
	if !s.sourceAllowed(conn) {
		slog.Warn("socks5 rejected source", "from", conn.RemoteAddr().String())
		_ = conn.Close()
		return
	}
	release, ok := s.acquire(conn)
	if !ok {
		slog.Warn("socks5 concurrency limit reached", "from", conn.RemoteAddr().String())
		_ = conn.Close()
		return
	}
	defer release()
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(socksDialTimeout))

	if err := s.handshake(conn); err != nil {
		return
	}
	req, err := socksRequest(conn)
	if err != nil {
		return
	}
	switch req.cmd {
	case socksCmdConnect:
		s.handleConnect(conn, req)
	case socksCmdUDPAssociate:
		s.handleUDPAssociate(conn)
	default:
		_ = socksReply(conn, socksRepCommandNotSupported)
	}
}

// handshake 完成 SOCKS5 方法协商：配置了账号走 RFC1929，否则维持无认证。
//
// 参数:
//   - conn: net.Conn — 客户端连接（调用方已设置 deadline）
//
// 返回:
//   - error — 版本不支持、客户端未提供所需方法、认证失败或读写失败
//
// 副作用：认证失败会先回 0x01 再等待 500ms，降低暴力枚举速度。
func (s *SOCKS5) handshake(conn net.Conn) error {
	var head [2]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return err
	}
	if head[0] != socksVersion {
		return errors.New("不支持的 SOCKS 版本")
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}
	want := byte(socksMethodNone)
	if s.opt.User != "" {
		want = socksMethodUserPass
	}
	ok := false
	for _, m := range methods {
		if m == want {
			ok = true
			break
		}
	}
	if !ok {
		_, _ = conn.Write([]byte{socksVersion, socksMethodUnacceptable})
		return errors.New("客户端不支持所需认证方式")
	}
	if _, err := conn.Write([]byte{socksVersion, want}); err != nil {
		return err
	}
	if want == socksMethodNone {
		return nil
	}
	// RFC1929: VER=1, ULEN, UNAME, PLEN, PASSWD
	var ver [2]byte
	if _, err := io.ReadFull(conn, ver[:]); err != nil {
		return err
	}
	if ver[0] != 0x01 {
		_, _ = conn.Write([]byte{0x01, 0x01})
		return errors.New("不支持的认证版本")
	}
	user := make([]byte, ver[1])
	if _, err := io.ReadFull(conn, user); err != nil {
		return err
	}
	var plen [1]byte
	if _, err := io.ReadFull(conn, plen[:]); err != nil {
		return err
	}
	pass := make([]byte, plen[0])
	if _, err := io.ReadFull(conn, pass); err != nil {
		return err
	}
	if !sameSecret(string(user), s.opt.User) || !sameSecret(string(pass), s.opt.Pass) {
		_, _ = conn.Write([]byte{0x01, 0x01})
		time.Sleep(socksAuthFailWait)
		return errors.New("SOCKS5 用户名或密码错误")
	}
	if _, err := conn.Write([]byte{0x01, 0x00}); err != nil {
		return err
	}
	return nil
}

type socksReq struct {
	cmd  byte
	addr string // host:port; hostnames are kept for the exit to resolve
}

// socksRequest parses VER/CMD/ATYP/ADDR/PORT.
func socksRequest(conn net.Conn) (*socksReq, error) {
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return nil, err
	}
	if head[0] != socksVersion {
		return nil, errors.New("不支持的 SOCKS 版本")
	}
	host, err := socksReadAddr(conn, head[3])
	if err != nil {
		return nil, err
	}
	var portBuf [2]byte
	if _, err := io.ReadFull(conn, portBuf[:]); err != nil {
		return nil, err
	}
	return &socksReq{
		cmd:  head[1],
		addr: net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBuf[:])))),
	}, nil
}

func socksReadAddr(conn net.Conn, atyp byte) (string, error) {
	switch atyp {
	case socksATypIPv4:
		var b [4]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			return "", err
		}
		return net.IP(b[:]).String(), nil
	case socksATypDomain:
		var n [1]byte
		if _, err := io.ReadFull(conn, n[:]); err != nil {
			return "", err
		}
		name := make([]byte, n[0])
		if _, err := io.ReadFull(conn, name); err != nil {
			return "", err
		}
		return string(name), nil
	case socksATypIPv6:
		var b [16]byte
		if _, err := io.ReadFull(conn, b[:]); err != nil {
			return "", err
		}
		return net.IP(b[:]).String(), nil
	default:
		return "", errors.New("不支持的地址类型")
	}
}

// socksReply answers with BND.ADDR 0.0.0.0:0 (we never accept inbound binds).
func socksReply(conn net.Conn, rep byte) error {
	_, err := conn.Write([]byte{socksVersion, rep, 0x00, socksATypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

// socksReplyAddr 用实际地址（IPv4/IPv6）填写 BND.ADDR 的应答。
//
// 参数:
//   - conn: net.Conn — 客户端连接
//   - rep: byte — SOCKS5 应答码
//   - addr: net.Addr — 本地中继地址（UDP ASSOCIATE 的 BND）
//
// 返回:
//   - error — 写失败；地址不可识别时回退到 0.0.0.0:0
func socksReplyAddr(conn net.Conn, rep byte, addr net.Addr) error {
	udp, ok := addr.(*net.UDPAddr)
	if !ok || udp == nil || udp.IP == nil {
		return socksReply(conn, rep)
	}
	out := []byte{socksVersion, rep, 0x00}
	if ip4 := udp.IP.To4(); ip4 != nil {
		out = append(out, socksATypIPv4)
		out = append(out, ip4...)
	} else {
		ip16 := udp.IP.To16()
		if ip16 == nil {
			return socksReply(conn, rep)
		}
		out = append(out, socksATypIPv6)
		out = append(out, ip16...)
	}
	var port [2]byte
	binary.BigEndian.PutUint16(port[:], uint16(udp.Port))
	out = append(out, port[:]...)
	_, err := conn.Write(out)
	return err
}

func (s *SOCKS5) handleConnect(conn net.Conn, req *socksReq) {
	ctx, cancel := context.WithTimeout(context.Background(), socksDialTimeout)
	defer cancel()
	dst, err := s.client.DialContext(ctx, "tcp", req.addr)
	if err != nil {
		_ = socksReply(conn, socksRepHostUnreachable)
		return
	}
	if err := socksReply(conn, socksRepSucceeded); err != nil {
		dst.Close()
		return
	}
	_ = conn.SetDeadline(time.Time{})
	pipeTCP(conn, dst)
}

func pipeTCP(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(b, a); _ = b.Close() }()
	go func() { defer wg.Done(); _, _ = io.Copy(a, b); _ = a.Close() }()
	wg.Wait()
}

// handleUDPAssociate answers with a local UDP relay socket, then shuttles
// datagrams between the client socket and per-target tunnel conns. The
// association ends when the TCP control connection closes.
func (s *SOCKS5) handleUDPAssociate(conn net.Conn) {
	// 把 UDP 中继绑在与 TCP 监听相同的地址上：只监听回环时不会暴露到局域网。
	listenHost, _, _ := net.SplitHostPort(s.ln.Addr().String())
	udp, err := net.ListenPacket("udp", net.JoinHostPort(listenHost, "0"))
	if err != nil {
		_ = socksReply(conn, socksRepGeneralFailure)
		return
	}
	defer udp.Close()
	if err := socksReplyAddr(conn, socksRepSucceeded, udp.LocalAddr()); err != nil {
		return
	}

	r := &socksUDPRelay{
		client: s.client,
		udp:    udp,
		tcp:    conn,
		done:   make(chan struct{}),
	}
	go r.readControl()
	r.readClient()
}

type socksUDPRelay struct {
	client  *Client
	udp     net.PacketConn
	tcp     net.Conn // control connection; closing it ends the association
	mu      sync.Mutex
	targets map[string]net.PacketConn
	sender  net.Addr // last client datagram source, replies go back there
	done    chan struct{}
	once    sync.Once
}

// readClient consumes datagrams from the client and pushes them into the
// tunnel, one tunnel conn per target address.
func (r *socksUDPRelay) readClient() {
	buf := make([]byte, socksDatagramSize)
	for {
		n, from, err := r.udp.ReadFrom(buf)
		if err != nil {
			r.shutdown()
			return
		}
		r.setSender(from)
		target, payload, err := parseDatagram(buf[:n])
		if err != nil || target == nil {
			continue
		}
		pc, err := r.connFor(target)
		if err != nil {
			continue
		}
		_, _ = pc.WriteTo(payload, target)
	}
}

func (r *socksUDPRelay) setSender(a net.Addr) {
	r.mu.Lock()
	r.sender = a
	r.mu.Unlock()
}

func (r *socksUDPRelay) senderAddr() net.Addr {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sender
}

func (r *socksUDPRelay) connFor(target *net.UDPAddr) (net.PacketConn, error) {
	key := target.String()
	r.mu.Lock()
	if pc, ok := r.targets[key]; ok {
		r.mu.Unlock()
		return pc, nil
	}
	if len(r.targets) >= socksMaxTargets {
		// Stay bounded: drop an arbitrary entry (its client can re-associate).
		for k, pc := range r.targets {
			pc.Close()
			delete(r.targets, k)
			break
		}
	}
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), socksDialTimeout)
	defer cancel()
	pc, err := r.client.DialUDP(ctx, target.String())
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.targets[key] = pc
	r.mu.Unlock()
	go r.readTarget(pc, target)
	return pc, nil
}

// readTarget wraps replies into SOCKS5 UDP datagrams and sends them back to
// the client's last source address.
func (r *socksUDPRelay) readTarget(pc net.PacketConn, target *net.UDPAddr) {
	buf := make([]byte, socksDatagramSize)
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		sender := r.senderAddr()
		if sender == nil {
			continue
		}
		out := buildDatagram(target, buf[:n])
		if _, err := r.udp.WriteTo(out, sender); err != nil {
			return
		}
	}
}

// readControl ends the association when the TCP control connection goes away.
func (r *socksUDPRelay) readControl() {
	buf := make([]byte, 1)
	for {
		if _, err := r.tcp.Read(buf); err != nil {
			r.shutdown()
			return
		}
	}
}

func (r *socksUDPRelay) shutdown() {
	r.once.Do(func() { close(r.done) })
	_ = r.tcp.Close()
}

// parseDatagram strips RSV/FRAG/ATYP/ADDR/PORT from a SOCKS5 UDP datagram.
// Only unfragmented datagrams (FRAG=0) are supported.
func parseDatagram(p []byte) (*net.UDPAddr, []byte, error) {
	if len(p) < 4 || p[2] != 0 {
		return nil, nil, errors.New("不支持的 SOCKS UDP 数据报")
	}
	switch atyp := p[3]; atyp {
	case socksATypIPv4:
		if len(p) < 10 {
			return nil, nil, errors.New("数据报过短")
		}
		return &net.UDPAddr{IP: net.IP(p[4:8]), Port: int(binary.BigEndian.Uint16(p[8:10]))}, p[10:], nil
	case socksATypDomain:
		n := int(p[4])
		if len(p) < 7+n {
			return nil, nil, errors.New("数据报过短")
		}
		host := string(p[5 : 5+n])
		port := int(binary.BigEndian.Uint16(p[5+n : 7+n]))
		addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return nil, nil, err
		}
		return addr, p[7+n:], nil
	case socksATypIPv6:
		if len(p) < 22 {
			return nil, nil, errors.New("数据报过短")
		}
		return &net.UDPAddr{IP: net.IP(p[4:20]), Port: int(binary.BigEndian.Uint16(p[20:22]))}, p[22:], nil
	default:
		return nil, nil, errors.New("不支持的地址类型")
	}
}

// buildDatagram wraps payload into a SOCKS5 UDP datagram header for target.
func buildDatagram(target *net.UDPAddr, payload []byte) []byte {
	var head []byte
	if ip4 := target.IP.To4(); ip4 != nil {
		head = []byte{0, 0, 0, socksATypIPv4}
		head = append(head, ip4...)
	} else if target.IP != nil && target.IP.To16() != nil {
		head = []byte{0, 0, 0, socksATypIPv6}
		head = append(head, target.IP.To16()...)
	} else {
		return payload
	}
	var port [2]byte
	binary.BigEndian.PutUint16(port[:], uint16(target.Port))
	head = append(head, port[:]...)
	return append(head, payload...)
}
