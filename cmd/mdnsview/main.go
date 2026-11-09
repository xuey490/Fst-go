//go:build windows

// Command mdnsview 打印「本机的 mDNS 浏览器实际看到了什么」。
//
// 用途：现场排障的最后一块拼图。FST 的界面只显示「最终发现的对端」，当某台
// 机器看不到手机时，无法区分下面两种完全不同的情况：
//
//  1. 根本没收到组播（防火墙拦入站、AP 开客户端隔离、驱动过滤组播）；
//  2. 收到了记录但解析失败（TXT 被吞、实例名不规范、端口不对）。
//
// 本工具用与 App 完全相同的网卡选择（lanmdns.LANInterfaces）和相同的浏览参数，
// 把每条记录原样打印出来：收到即为 1，解析不出 peer 即为 2。
//
// 参数:
//   - -t: 监听秒数（默认 25）
//
// 返回: 无（打印到 stdout，退出码恒为 0）
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/zeroconf/v2"

	"fst/internal/lanmdns"
)

// serviceNames 覆盖新旧两套服务名，与 App 里的双浏览保持一致。
var serviceNames = []string{"_fst._tcp", "fst"}

// record 是一条浏览结果，按「服务名+实例名」去重后打印。
type record struct {
	service  string
	instance string
	host     string
	port     int
	ipv4     []string
	ipv6     []string
	text     []string
	peers    []string
}

// main 解析参数、启动两个服务的浏览、到时打印汇总。
//
// 参数:
//
//	无（从 os.Args 读取 -t）
//
// 返回:
//
//	无
func main() {
	secs := flag.Int("t", 25, "监听秒数")
	flag.Parse()

	ifaces := lanmdns.LANInterfaces()
	fmt.Printf("interfaces: %s\n", describe(ifaces))
	fmt.Printf("listening %ds on _fst._tcp.local. + fst.local. ...\n\n", *secs)

	var (
		mu   sync.Mutex
		seen = map[string]*record{}
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*secs)*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, name := range serviceNames {
		wg.Add(1)
		go func(svc string) {
			defer wg.Done()
			entries := make(chan *zeroconf.ServiceEntry, 64)
			go func() {
				for e := range entries {
					r := toRecord(e, svc)
					mu.Lock()
					seen[r.service+"/"+r.instance] = r
					mu.Unlock()
				}
			}()
			opts := []zeroconf.ClientOption{}
			if len(ifaces) > 0 {
				opts = append(opts, zeroconf.SelectIfaces(ifaces), zeroconf.SelectIPTraffic(zeroconf.IPv4))
			}
			if err := zeroconf.Browse(ctx, svc, "local", entries, opts...); err != nil && ctx.Err() == nil {
				fmt.Printf("browse %s stopped: %v\n", svc, err)
			}
		}(name)
	}
	wg.Wait()

	mu.Lock()
	list := make([]*record, 0, len(seen))
	for _, r := range seen {
		list = append(list, r)
	}
	mu.Unlock()
	sort.Slice(list, func(i, j int) bool {
		if list[i].service != list[j].service {
			return list[i].service < list[j].service
		}
		return list[i].instance < list[j].instance
	})

	fmt.Printf("=== %d record(s) ===\n", len(list))
	for _, r := range list {
		fmt.Printf("service=%s instance=%s\n", r.service, r.instance)
		fmt.Printf("   host=%s port=%d ipv4=%s ipv6=%s\n", r.host, r.port,
			strings.Join(r.ipv4, ","), strings.Join(r.ipv6, ","))
		fmt.Printf("   txt=%s\n", strings.Join(r.text, " | "))
		if len(r.peers) == 0 {
			fmt.Printf("   peers=<UNPARSED>  <- TXT/实例名都还原不出 PeerID\n")
		} else {
			fmt.Printf("   peers=%s\n", strings.Join(r.peers, ","))
		}
		fmt.Println()
	}
	if len(list) == 0 {
		fmt.Println("没有收到任何记录：说明这台机器收不到组播（防火墙/AP 隔离/驱动过滤），")
		fmt.Println("或局域网里确实没有其它 FST 实例在广播。")
	}
}

// toRecord 把 zeroconf 条目转成可打印结构，并尝试还原 PeerID。
//
// 参数:
//   - e: *zeroconf.ServiceEntry — 浏览结果
//   - svc: string — 来源服务名
//
// 返回:
//   - *record — 汇总后的记录
func toRecord(e *zeroconf.ServiceEntry, svc string) *record {
	r := &record{
		service:  svc,
		instance: e.Instance,
		host:     e.HostName,
		port:     e.Port,
		text:     append([]string(nil), e.Text...),
	}
	for _, ip := range e.AddrIPv4 {
		r.ipv4 = append(r.ipv4, ip.String())
	}
	for _, ip := range e.AddrIPv6 {
		r.ipv6 = append(r.ipv6, ip.String())
	}
	// 与 App 相同的两条还原路径：TXT 里的 dnsaddr，以及实例名里的 PeerID。
	ids := map[string]struct{}{}
	for _, t := range e.Text {
		if !strings.HasPrefix(t, "dnsaddr=") {
			continue
		}
		if id := peerIDFrom(t); id != "" {
			ids[id] = struct{}{}
		}
	}
	if id := peerIDFromInstance(e.Instance); id != "" {
		ids[id] = struct{}{}
	}
	for id := range ids {
		r.peers = append(r.peers, id)
	}
	sort.Strings(r.peers)
	return r
}

// peerIDFrom 从一条 dnsaddr 里取出 PeerID。
//
// 参数:
//   - txt: string — 形如 "dnsaddr=/ip4/1.2.3.4/tcp/8787/p2p/12D3Koo..."
//
// 返回:
//   - string — PeerID；解析失败返回空串
func peerIDFrom(txt string) string {
	v := strings.TrimPrefix(txt, "dnsaddr=")
	i := strings.LastIndex(v, "/p2p/")
	if i < 0 {
		return ""
	}
	id, err := peer.Decode(v[i+len("/p2p/"):])
	if err != nil {
		return ""
	}
	return id.String()
}

// peerIDFromInstance 从实例名 "FST-<peerID>[-br]" 里取出 PeerID。
//
// 参数:
//   - instance: string — DNS-SD 实例名
//
// 返回:
//   - string — PeerID；解析失败返回空串
func peerIDFromInstance(instance string) string {
	s := instance
	if i := strings.Index(s, " ("); i > 0 {
		s = s[:i]
	}
	s = strings.TrimPrefix(s, "FST-")
	s = strings.TrimSuffix(s, "-br")
	id, err := peer.Decode(s)
	if err != nil {
		return ""
	}
	return id.String()
}

// describe 把网卡列表压成一行日志。
//
// 参数:
//   - ifaces: []net.Interface — 网卡
//
// 返回:
//   - string — "name#idx=ip" 逗号分隔
func describe(ifaces []net.Interface) string {
	out := make([]string, 0, len(ifaces))
	for i := range ifaces {
		ip := ""
		if addrs, err := ifaces[i].Addrs(); err == nil {
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
					ip = n.IP.String()
					break
				}
			}
		}
		out = append(out, fmt.Sprintf("%s#%d=%s", ifaces[i].Name, ifaces[i].Index, ip))
	}
	return "[" + strings.Join(out, " ") + "]"
}
