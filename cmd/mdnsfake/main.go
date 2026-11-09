//go:build windows

// Command mdnsfake 是一个临时诊断工具：在局域网上模拟一台「只会注册标准
// 服务名 _fst._tcp 的移动端」，用于验证桌面端的「发现代理」逻辑。
//
// 场景对应：手机受 NsdManager 限制只能注册 _fst._tcp，旧桌面端只浏览
// fst.local.。新的桌面端发现手机后应当把手机补播到 fst.local. 上，
// 这个工具就是用来制造这种「单标签对端」的。
//
// 参数:
//   - -ip: 写进 A 记录的本机 IPv4（必填，需与运行抓包工具的机器一致）
//   - -port: SRV 端口，默认 18788（不需要真的有人监听）
//   - -t: 保持注册的秒数，默认 40
//
// 返回: 无（打印模拟出的 PeerID，便于在抓包里比对）
package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/zeroconf/v2"
)

// main 生成一个随机 PeerID，按移动端的格式注册标准服务名并保持指定时长。
//
// 参数:
//
//	无（从 os.Args 读取 -ip / -port / -t）
//
// 返回:
//
//	无
func main() {
	ip := flag.String("ip", "", "本机 IPv4（写进 A 记录）")
	port := flag.Int("port", 18788, "SRV 端口")
	secs := flag.Int("t", 40, "保持注册的秒数")
	flag.Parse()
	if *ip == "" {
		fmt.Println("usage: mdnsfake -ip 192.168.1.15 [-port 18788] [-t 40]")
		return
	}
	_, pub, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		fmt.Println("keygen failed:", err)
		return
	}
	id, err := peer.IDFromPublicKey(pub)
	if err != nil {
		fmt.Println("peer id failed:", err)
		return
	}
	instance := "FST-" + id.String()
	txt := []string{fmt.Sprintf("dnsaddr=/ip4/%s/tcp/%d/p2p/%s", *ip, *port, id)}
	srv, err := zeroconf.RegisterProxy(instance, "_fst._tcp", "local", *port, instance, []string{*ip}, txt, nil)
	if err != nil {
		fmt.Println("register failed:", err)
		return
	}
	defer srv.Shutdown()
	fmt.Printf("fake mobile peer=%s ip=%s port=%d, holding %ds\n", id, *ip, *port, *secs)
	time.Sleep(time.Duration(*secs) * time.Second)
}
