//go:build !windows

package proxy

import (
	"bufio"
	"net/netip"
	"os"
	"strings"
)

func systemDNSv4() []netip.Addr {
	return resolvDNS(func(ip netip.Addr) bool { return ip.Is4() })
}

// systemDNSv6 返回 /etc/resolv.conf 中的 IPv6 DNS 服务器（IPv6 分流时用于 /128 劫持）。
//
// 返回:
//   - []netip.Addr — IPv6 地址；无配置时为空
func systemDNSv6() []netip.Addr {
	return resolvDNS(func(ip netip.Addr) bool { return ip.Is6() && !ip.Is4In6() })
}

// resolvDNS 读取 /etc/resolv.conf 的 nameserver 并按地址族过滤。
//
// 参数:
//   - keep: func(netip.Addr) bool — 地址过滤条件
//
// 返回:
//   - []netip.Addr — 命中的服务器地址（保持文件顺序）
func resolvDNS(keep func(netip.Addr) bool) []netip.Addr {
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []netip.Addr
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		ip, err := netip.ParseAddr(fields[1])
		if err == nil && keep(ip) {
			out = append(out, ip)
		}
	}
	return out
}
