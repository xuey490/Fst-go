//go:build windows

// Command mdnssniff 是一个临时诊断工具：在 5353 端口抓取并打印 mDNS 报文。
//
// 用途：判断「手机端到底有没有发出 mDNS 广播」以及「服务名是否匹配」，
// 这是区分「发现层问题」与「握手/加密层问题」的唯一可靠证据。
//
// 实现要点：Windows 下必须 SO_REUSEADDR + 绑定通配地址 + 显式 JoinGroup，
// 否则与已占用 5353 的进程抢端口失败或收不到组播（与 zeroconf 内部一致）。
//
// 参数:
//   - -t: 监听秒数（默认 25）
//   - -q: 为 true 时先主动发一轮 PTR 查询，触发对端应答（默认 true）
//
// 返回: 无（打印到 stdout，退出码恒为 0）
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/windows"
)

// mdnsPort 是 mDNS 固定端口。
const mdnsPort = 5353

// mdnsGroupIP 是 mDNS 固定的 IPv4 组播地址。
var mdnsGroupIP = net.IPv4(224, 0, 0, 251)

// probeNames 是主动查询的服务类型，覆盖新旧两套命名。
var probeNames = []string{
	"_fst._tcp.local.",
	"fst.local.",
	"_services._dns-sd._udp.local.",
}

// main 解析参数、绑定 5353、加入组播组、发查询、打印收到的每条记录。
//
// 参数:
//
//	无（从 os.Args 读取 -t / -q）
//
// 返回:
//
//	无
func main() {
	secs := flag.Int("t", 25, "监听秒数")
	probe := flag.Bool("q", true, "先主动发送 PTR 查询")
	qu := flag.Bool("qu", false, "查询里置 QU 位，要求对端单播应答（模拟 Android mdnsd）")
	flag.Parse()

	pc, err := listenShared()
	if err != nil {
		fmt.Println("bind 5353 failed:", err)
		os.Exit(1)
	}
	defer pc.Close()

	group := &net.UDPAddr{IP: mdnsGroupIP, Port: mdnsPort}
	conn := ipv4.NewPacketConn(pc)
	joined := 0
	ifaces, _ := net.Interfaces()
	for i := range ifaces {
		ifc := ifaces[i]
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 {
			continue
		}
		if err := conn.JoinGroup(&ifc, group); err != nil {
			continue
		}
		joined++
		fmt.Printf("joined %s (%s)\n", ifc.Name, addrsOf(&ifc))
	}
	if joined == 0 {
		fmt.Println("no interface could join the mDNS group")
	}

	fmt.Printf("listening on %s for %ds (local ip %s)\n", group, *secs, localIPv4())

	stop := time.Now().Add(time.Duration(*secs) * time.Second)
	if *probe {
		time.Sleep(200 * time.Millisecond)
		sendProbes(conn, group, *qu)
	}

	buf := make([]byte, 9000)
	for time.Now().Before(stop) {
		_ = conn.SetReadDeadline(stop)
		n, cm, src, err := conn.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				break
			}
			continue
		}
		var msg dns.Msg
		if err := msg.Unpack(buf[:n]); err != nil {
			fmt.Printf("[%s] unparsable %d bytes from %s\n", time.Now().Format("15:04:05"), n, src)
			continue
		}
		_ = cm
		printMsg(src, &msg)
	}
	fmt.Println("--- capture finished ---")
}

// listenShared 绑定 0.0.0.0:5353 并开启 SO_REUSEADDR，以便与其他 mDNS 进程共存。
//
// 参数:
//
//	无
//
// 返回:
//   - net.PacketConn — 可用于 JoinGroup 的连接
//   - error — 端口被独占或权限不足时返回
func listenShared() (net.PacketConn, error) {
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
		}); err != nil {
			return err
		}
		return serr
	}}
	return lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", mdnsPort))
}

// addrsOf 返回网卡的 IPv4 地址串，仅用于日志辨识。
//
// 参数:
//   - ifc: *net.Interface — 网卡
//
// 返回:
//   - string — 逗号分隔的 IPv4 列表
func addrsOf(ifc *net.Interface) string {
	addrs, err := ifc.Addrs()
	if err != nil {
		return "?"
	}
	var out []string
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			out = append(out, ipnet.IP.String())
		}
	}
	return strings.Join(out, ",")
}

// localIPv4 返回本机首个非回环 IPv4，便于识别「自己发的包」。
//
// 参数:
//
//	无
//
// 返回:
//   - string — 点分 IPv4；找不到时返回 "unknown"
func localIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "unknown"
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				if v4 := ipnet.IP.To4(); v4 != nil && !ipnet.IP.IsLoopback() {
					return v4.String()
				}
			}
		}
	}
	return "unknown"
}

// sendProbes 向每个网卡的组播组发送 PTR 查询，让正在广播的节点立刻回应。
//
// 参数:
//   - conn: *ipv4.PacketConn — 已加入组播组的连接
//   - group: *net.UDPAddr — mDNS 组播地址
//   - qu: bool — 是否置 QU 位要求单播应答
//
// 返回: 无
func sendProbes(conn *ipv4.PacketConn, group *net.UDPAddr, qu bool) {
	ifaces, _ := net.Interfaces()
	for i := range ifaces {
		ifc := ifaces[i]
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		for _, name := range probeNames {
			msg := new(dns.Msg)
			msg.SetQuestion(name, dns.TypePTR)
			msg.RecursionDesired = false
			if qu {
				// QU（unicast-response）位：与 Android mdnsd 的查询一致，让应答方
				// 直接单播回来。用来区分「对端没注册」与「对端收不到组播」。
				msg.Question[0].Qclass |= 1 << 15
			}
			raw, err := msg.Pack()
			if err != nil {
				continue
			}
			cm := &ipv4.ControlMessage{IfIndex: ifc.Index}
			if _, err := conn.WriteTo(raw, cm, group); err != nil {
				fmt.Printf("probe send failed on %s: %v\n", ifc.Name, err)
			}
		}
	}
	fmt.Println(">>> sent PTR probe for", strings.Join(probeNames, " "))
}

// printMsg 打印一条 mDNS 报文的问题与资源记录。
//
// 参数:
//   - src: net.Addr — 报文来源地址
//   - msg: *dns.Msg — 已解析的报文
//
// 返回: 无
func printMsg(src net.Addr, msg *dns.Msg) {
	kind := "QUERY"
	if msg.Response {
		kind = "RESP"
	}
	fmt.Printf("\n[%s] %s from %s (answers=%d)\n", time.Now().Format("15:04:05"), kind, src, len(msg.Answer))
	for _, q := range msg.Question {
		fmt.Printf("   Q  %s %s\n", q.Name, dns.TypeToString[q.Qtype])
	}
	for _, rr := range append(append([]dns.RR{}, msg.Answer...), msg.Extra...) {
		switch v := rr.(type) {
		case *dns.PTR:
			fmt.Printf("   PTR %s -> %s\n", v.Hdr.Name, v.Ptr)
		case *dns.SRV:
			fmt.Printf("   SRV %s -> %s:%d\n", v.Hdr.Name, v.Target, v.Port)
		case *dns.TXT:
			fmt.Printf("   TXT %s -> %s\n", v.Hdr.Name, strings.Join(v.Txt, " | "))
		case *dns.A:
			fmt.Printf("   A   %s -> %s\n", v.Hdr.Name, v.A)
		case *dns.AAAA:
			fmt.Printf("   AAAA %s -> %s\n", v.Hdr.Name, v.AAAA)
		}
	}
}
