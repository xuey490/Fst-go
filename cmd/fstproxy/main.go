// fstproxy 是一个极小的 TCP 转发器，用于验证与绕过「无线客户端隔离」。
//
// 场景：A（有线）能连手机，B（无线）连不上手机，但 A 与 B 互通。
// 把它跑在 A 上：B 拨 A 的转发端口，本工具把字节原样转给手机 8787，
// libp2p 的 TLS/Noise 握手在两端之间照常完成（对端 PeerID 仍是手机），
// 因此 B 能真的连上手机，只是链路经过 A。
//
// 它不是长期方案：真正的修法是关掉路由器的 AP 隔离，或把 B 接网线。
//
// 用法：
//
//	fstproxy -listen :18787 -target 192.168.1.13:8787
//
// 然后在 B 上用「添加设备」粘贴：
//
//	/ip4/<A 的 IP>/tcp/18787/p2p/<手机的 PeerID>
package main

import (
	"flag"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

func main() {
	listen := flag.String("listen", ":18787", "本地监听地址")
	target := flag.String("target", "", "转发目标，形如 192.168.1.13:8787")
	flag.Parse()
	if *target == "" {
		slog.Error("必须指定 -target")
		os.Exit(2)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		slog.Error("监听失败", "listen", *listen, "err", err)
		os.Exit(1)
	}
	slog.Info("fstproxy started", "listen", ln.Addr().String(), "target", *target)

	var n int
	var mu sync.Mutex
	for {
		c, err := ln.Accept()
		if err != nil {
			slog.Warn("accept failed", "err", err)
			return
		}
		mu.Lock()
		n++
		id := n
		mu.Unlock()
		go func(id int, c net.Conn) {
			defer c.Close()
			up, err := net.DialTimeout("tcp", *target, 5*time.Second)
			if err != nil {
				slog.Warn("upstream dial failed", "conn", id, "target", *target, "err", err)
				return
			}
			defer up.Close()
			slog.Info("proxying", "conn", id, "from", c.RemoteAddr().String(), "to", *target)

			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
			<-done
			slog.Info("closed", "conn", id)
		}(id, c)
	}
}
