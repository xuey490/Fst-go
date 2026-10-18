//go:build windows

package proxy

import (
	"net/netip"
	"strings"
)

func systemDNSv4() []netip.Addr {
	return windowsDNS("IPv4", func(ip netip.Addr) bool { return ip.Is4() })
}

// systemDNSv6 返回 Windows 上配置的 IPv6 DNS 服务器（用于 IPv6 分流时的 /128 劫持）。
//
// 返回:
//   - []netip.Addr — 去重后的 IPv6 地址；读取失败时为空
func systemDNSv6() []netip.Addr {
	return windowsDNS("IPv6", func(ip netip.Addr) bool { return ip.Is6() && !ip.Is4In6() })
}

// windowsDNS 读取指定地址族的 DNS 服务器列表。
//
// 参数:
//   - family: string — PowerShell 的 -AddressFamily 取值（IPv4/IPv6）
//   - keep: func(netip.Addr) bool — 地址过滤
//
// 返回:
//   - []netip.Addr — 去重后的地址
func windowsDNS(family string, keep func(netip.Addr) bool) []netip.Addr {
	out, err := powershell(`(Get-DnsClientServerAddress -AddressFamily ` + family + ` | ForEach-Object { $_.ServerAddresses }) -join [char]10`)
	if err != nil {
		return nil
	}
	var ips []netip.Addr
	seen := map[netip.Addr]bool{}
	for _, line := range strings.Split(out, "\n") {
		ip, err := netip.ParseAddr(strings.TrimSpace(line))
		if err != nil || !keep(ip) || seen[ip] {
			continue
		}
		seen[ip] = true
		ips = append(ips, ip)
	}
	return ips
}
