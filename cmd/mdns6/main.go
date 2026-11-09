//go:build windows

// Command mdns6 是一个临时诊断工具：加入 IPv6 的 mDNS 组播组 ff02::fb，
// 发一轮 PTR 查询并打印所有收到的 IPv6 mDNS 报文。
//
// 用途：部分 Android ROM 的 mdnsd 只在 IPv6 链路上注册与应答，IPv4 抓包会
// 完全看不到记录。用它确认「手机到底有没有广播」。
//
// 参数:
//   - -t: 监听秒数（默认 12）
//
// 返回: 无（打印到 stdout）
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/windows"
)

// group6 是 mDNS 的 IPv6 组播地址。
const group6 = "ff02::fb"

// probeNames 是要主动查询的服务名。
var probeNames = []string{
	"_fst._tcp.local.",
	"fst.local.",
	"_services._dns-sd._udp.local.",
}

// main 加入组播组、发查询、打印报文。
//
// 参数:
//
//	无（从 os.Args 读取 -t）
//
// 返回:
//
//	无
func main() {
	secs := flag.Int("t", 12, "监听秒数")
	flag.Parse()

	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
		}); err != nil {
			return err
		}
		return serr
	}}
	pc, err := lc.ListenPacket(context.Background(), "udp6", "[::]:5353")
	if err != nil {
		fmt.Println("bind [::]:5353 failed:", err)
		return
	}
	defer pc.Close()
	conn := ipv6.NewPacketConn(pc)

	_ = conn.SetControlMessage(ipv6.FlagInterface, true)
	ifaces, _ := net.Interfaces()
	group := &net.UDPAddr{IP: net.ParseIP(group6), Port: 5353}
	joined := 0
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 {
			continue
		}
		if err := conn.JoinGroup(&ifc, group); err != nil {
			continue
		}
		joined++
		fmt.Println("joined", ifc.Name, ifc.HardwareAddr)
	}
	fmt.Printf("joined %d interfaces, listening %ds\n", joined, *secs)

	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 {
			continue
		}
		for _, name := range probeNames {
			msg := new(dns.Msg)
			msg.SetQuestion(name, dns.TypePTR)
			raw, _ := msg.Pack()
			if _, err := conn.WriteTo(raw, &ipv6.ControlMessage{IfIndex: ifc.Index}, group); err != nil {
				fmt.Println("send failed on", ifc.Name, err)
			}
		}
	}

	buf := make([]byte, 9000)
	stop := time.Now().Add(time.Duration(*secs) * time.Second)
	for time.Now().Before(stop) {
		_ = conn.SetReadDeadline(stop)
		n, cm, src, err := conn.ReadFrom(buf)
		if err != nil {
			break
		}
		var msg dns.Msg
		if err := msg.Unpack(buf[:n]); err != nil {
			continue
		}
		kind := "QUERY"
		if msg.Response {
			kind = "RESP"
		}
		ifidx := -1
		if cm != nil {
			ifidx = cm.IfIndex
		}
		fmt.Printf("\n[%s] %s from %s (if=%d, answers=%d)\n", time.Now().Format("15:04:05"), kind, src, ifidx, len(msg.Answer))
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
			case *dns.AAAA:
				fmt.Printf("   AAAA %s -> %s\n", v.Hdr.Name, v.AAAA)
			case *dns.A:
				fmt.Printf("   A   %s -> %s\n", v.Hdr.Name, v.A)
			}
		}
	}
	fmt.Println("--- done ---")
}
