// Command netdiag 打印本机网卡的 mDNS 可用性诊断。
//
// 用途：现场排查 "[WARN] mdns: Failed to set multicast interface" 时，
// 它能逐块网卡给出真实 setsockopt 探测结果，从而区分三种情况：
//
//  1. 网卡没有可路由 IPv4（169.254.x，DHCP 失败）→ 不该参与 mDNS；
//  2. 网卡有地址但系统拒绝作为组播出口（探测 FAIL）→ zeroconf 必然刷告警，
//     且报文可能从别的网卡发出；
//  3. 一切正常 → 告警另有来源（通常是跑的是旧版 fst）。
//
// 用法:
//
//	netdiag                打印全部网卡
//	netdiag -v             额外列出 IPv6 地址
//
// 输出中 SELECTED=yes 表示 fst 的 mDNS 会使用该网卡。
package main

import (
	"flag"
	"fmt"
	"strings"

	"fst/internal/lanmdns"
)

// main 打印网卡诊断表。
func main() {
	verbose := flag.Bool("v", false, "显示 IPv6 地址")
	flag.Parse()

	rows := lanmdns.Diagnose()
	if len(rows) == 0 {
		fmt.Println("未能枚举网卡（net.Interfaces 失败）")
		return
	}

	fmt.Printf("%-4s %-34s %-20s %-11s %-11s %-9s %s\n",
		"IDX", "NAME", "IPv4", "MCAST_V4", "MCAST_V6", "SELECTED", "FLAGS")
	for _, r := range rows {
		selected := "no"
		if r.Selected {
			selected = "yes"
		}
		fmt.Printf("%-4d %-34s %-20s %-11s %-11s %-9s %s\n",
			r.Index, cut(r.Name, 34), joinOr(r.IPv4, "-"),
			state(r.MulticastV4Err), state(r.MulticastV6Err),
			selected, strings.Join(r.Flags, ","))
		if *verbose && len(r.IPv6) > 0 {
			fmt.Printf("     %-34s %s\n", "", "ipv6: "+joinOr(r.IPv6, "-"))
		}
	}

	fmt.Println()
	fmt.Println("说明：")
	fmt.Println("  MCAST_V4=FAIL 的网卡就是 \"Failed to set multicast interface\" 的来源；")
	fmt.Println("  SELECTED=no 且带 169.254.x 的网卡是被跳过的 APIPA/虚拟网卡。")
	fmt.Println("  若所有网卡都不理想，可用 FST_MDNS_IFACE=<名字或序号> 强制指定。")
}

// state 把探测结果压成 ok/FAIL 短文本。
//
// 参数:
//   - err: error — 探测结果
//
// 返回:
//   - string — "ok" 或 "FAIL"
func state(err error) string {
	if err == nil {
		return "ok"
	}
	return "FAIL"
}

// joinOr 把字符串切片拼成逗号分隔文本。
//
// 参数:
//   - items: []string — 待拼接内容
//   - dash: string — 空切片时的占位符
//
// 返回:
//   - string — 拼接结果
func joinOr(items []string, dash string) string {
	if len(items) == 0 {
		return dash
	}
	return strings.Join(items, ",")
}

// cut 把文本截断到指定长度，避免超长网卡名破坏表格对齐。
//
// 参数:
//   - s: string — 原始文本
//   - n: int — 最大显示长度
//
// 返回:
//   - string — 不超过 n 个字符的文本
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "~"
}
