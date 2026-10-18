//go:build windows

package proxy

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

type windowsRoutes struct {
	iface   string
	ifIndex int
	added   []string
	added6  []string
}

func newRouteManager(iface string) routeManager { return &windowsRoutes{iface: iface} }

func flushDNSCache() error {
	return run("powershell", "-NoProfile", "-NonInteractive", "-Command", "Clear-DnsClientCache")
}

func (r *windowsRoutes) Setup(p routePlan) error {
	if err := run("netsh", "interface", "ipv4", "set", "address", "name="+r.iface,
		"static", "198.18.0.1", "255.254.0.0"); err != nil {
		return fmt.Errorf("配置 TUN 地址失败（请以管理员身份运行）: %w", err)
	}
	idx, err := powershellInt("(Get-NetAdapter -Name '" + psQuote(r.iface) + "').ifIndex")
	if err != nil {
		return fmt.Errorf("读取 TUN 接口失败: %w", err)
	}
	r.ifIndex = idx
	// IPv6 失败只降级：不装 v6 路由，IPv4 继续工作。
	if p.ipv6 && !r.addAddr6() {
		p.ipv6 = false
	}
	if p.mode == "global" {
		gwIf, gw, err := r.defaultGateway()
		if err != nil {
			return err
		}
		for _, b := range globalBypass(p.exit, p.hijackDNS) {
			if err := r.add(b.String(), gwIf, gw, false); err != nil {
				_ = r.Close()
				return fmt.Errorf("添加绕行路由 %s 失败: %w", b, err)
			}
		}
	}
	for _, x := range tunRoutePrefixes(p) {
		if err := r.add(x.String(), strconv.Itoa(r.ifIndex), "0.0.0.0", true); err != nil {
			_ = r.Close()
			return fmt.Errorf("添加代理路由 %s 失败: %w", x, err)
		}
	}
	if p.ipv6 {
		r.setupIPv6(p)
	}
	if p.hijackDNS {
		if err := r.hijackDNS(); err != nil {
			_ = r.Close()
			return fmt.Errorf("劫持系统 DNS 失败: %w", err)
		}
	}
	return nil
}

// addAddr6 给 TUN 接口配置 IPv6 地址 fdfe:1::1/64。
//
// 返回:
//   - bool — 成功（或地址已存在）时为 true；失败记录 Warn 并返回 false
func (r *windowsRoutes) addAddr6() bool {
	if err := run("netsh", "interface", "ipv6", "add", "address", "name="+r.iface, "address=fdfe:1::1/64"); err != nil {
		if routeExistsErr(err) {
			return true
		}
		slog.Warn("ipv6 tun address failed; IPv6 proxy disabled for this run", "err", err)
		return false
	}
	return true
}

// setupIPv6 给 TUN 加 IPv6 路由；失败只记录 Warn（IPv4 继续可用）。
//
// 参数:
//   - p: routePlan — 含 ipv6 前缀、出口 v6 与全局模式开关
//
// 返回:
//   - 无
func (r *windowsRoutes) setupIPv6(p routePlan) {
	// 地址已在 Setup 里通过 ipv6 set/add 配置；这里只加路由。
	if p.mode == "global" {
		gwIf, gw, err := r.defaultGateway6()
		if err != nil {
			slog.Warn("ipv6 bypass routes skipped", "err", err)
		} else {
			for _, b := range globalBypass6(p.exit6) {
				if err := r.add6(b.String(), gwIf, gw, false); err != nil {
					slog.Warn("ipv6 bypass route failed", "prefix", b, "err", err)
				}
			}
		}
	}
	for _, x := range tunRoutePrefixes6(p) {
		if err := r.add6(x.String(), strconv.Itoa(r.ifIndex), "", true); err != nil {
			slog.Warn("ipv6 tun route failed", "prefix", x, "err", err)
		}
	}
}

// defaultGateway6 读取（除 TUN 外）的 IPv6 默认路由网关。
//
// 返回:
//   - ifIndex: string — 网关接口索引
//   - nextHop: string — 网关地址
//   - err: error — 没有 IPv6 默认路由时返回
func (r *windowsRoutes) defaultGateway6() (ifIndex, nextHop string, err error) {
	out, err := powershell(`$r=Get-NetRoute -DestinationPrefix '::/0' | Where-Object {$_.InterfaceIndex -ne ` +
		strconv.Itoa(r.ifIndex) + `} | Sort-Object RouteMetric | Select-Object -First 1; "$($r.InterfaceIndex)|$($r.NextHop)"`)
	if err != nil {
		return "", "", fmt.Errorf("读取 IPv6 默认网关失败: %w", err)
	}
	parts := strings.Split(strings.TrimSpace(out), "|")
	if len(parts) != 2 || parts[1] == "" {
		return "", "", fmt.Errorf("没有可用的 IPv6 默认网关")
	}
	return parts[0], parts[1], nil
}

// add6 添加一条 IPv6 路由（gateway 为空表示 on-link）。
//
// 参数:
//   - prefix: string — IPv6 前缀
//   - index: string — 接口索引
//   - gateway: string — 下一跳；空串表示直连 TUN
//
// 返回:
//   - error — netsh 失败时返回（takeOver 且已存在时先删除再重加）
func (r *windowsRoutes) add6(prefix, index, gateway string, takeOver bool) error {
	args := []string{"interface", "ipv6", "add", "route", prefix, "interface=" + index, "metric=5", "store=active"}
	if gateway != "" {
		args = append(args, "nexthop="+gateway)
	}
	if err := run("netsh", args...); err != nil {
		if !routeExistsErr(err) {
			return err
		}
		if !takeOver {
			return nil
		}
		_ = r.del6(prefix, index, gateway)
		if err = run("netsh", args...); err != nil {
			return err
		}
	}
	r.added6 = append(r.added6, prefix+"|"+index+"|"+gateway)
	return nil
}

// del6 删除一条 IPv6 路由（Close 与“已存在”自愈共用）。
//
// 参数:
//   - prefix: string — IPv6 前缀
//   - index: string — 接口索引
//   - gateway: string — 下一跳；空串表示 on-link
//
// 返回:
//   - error — netsh 失败时返回
func (r *windowsRoutes) del6(prefix, index, gateway string) error {
	args := []string{"interface", "ipv6", "delete", "route", prefix, "interface=" + index, "store=active"}
	if gateway != "" {
		args = append(args, "nexthop="+gateway)
	}
	return run("netsh", args...)
}

func (r *windowsRoutes) defaultGateway() (ifIndex, nextHop string, err error) {
	bypass, err := powershell(`$r=Get-NetRoute -DestinationPrefix '0.0.0.0/0' | Where-Object {$_.InterfaceIndex -ne ` +
		strconv.Itoa(r.ifIndex) + `} | Sort-Object RouteMetric | Select-Object -First 1; "$($r.InterfaceIndex)|$($r.NextHop)"`)
	if err != nil {
		return "", "", fmt.Errorf("读取默认网关失败: %w", err)
	}
	parts := strings.Split(strings.TrimSpace(bypass), "|")
	if len(parts) != 2 || parts[1] == "" {
		return "", "", fmt.Errorf("无法确定原默认网关: %q", bypass)
	}
	return parts[0], parts[1], nil
}

// add 添加一条 IPv4 路由。
//
// 参数:
//   - prefix: string — 目标前缀（如 10.0.0.0/8）
//   - index: string — 接口索引
//   - gateway: string — 下一跳；TUN 路由传 0.0.0.0
//   - takeOver: bool — 同参数路由已存在时是否接管（删掉重建并纳入 Close 清理）。
//     TUN 路由必须接管（旧会话可能留下指向已消失接口的残留）；
//     绕行路由传 false：已有同参数路由说明绕行本来就成立，跳过即可，避免退出时删掉用户/系统自带的路由。
//
// 返回:
//   - error — 首次添加失败且不是「已存在」，或接管后仍失败时返回（输出已转 UTF-8）
func (r *windowsRoutes) add(prefix, index, gateway string, takeOver bool) error {
	args := []string{"interface", "ipv4", "add", "route", prefix,
		"interface=" + index, "nexthop=" + gateway, "metric=5", "store=active"}
	if err := run("netsh", args...); err != nil {
		if !routeExistsErr(err) {
			return err
		}
		if !takeOver {
			return nil
		}
		_ = run("netsh", "interface", "ipv4", "delete", "route", prefix,
			"interface="+index, "nexthop="+gateway, "store=active")
		if err = run("netsh", args...); err != nil {
			return err
		}
	}
	r.added = append(r.added, prefix+"|"+index+"|"+gateway)
	return nil
}

// routeExistsErr 判断 netsh 的失败是否只是「同参数路由已存在」。
// 输出在 decodeConsole 里已转成 UTF-8，所以这里可以按中英文关键词判断；
// 同时接受更短的中文片段，避免 Windows 版本换措辞（对象已存在 / 路由已存在）时漏判。
func routeExistsErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "对象已存在") || strings.Contains(s, "已存在") ||
		strings.Contains(s, "already exists") || strings.Contains(s, "ALREADY_EXISTS")
}

// hijackDNS puts the fake DNS on the TUN interface itself and leaves the
// physical adapters untouched. Windows sends DNS queries from the interface
// that owns the server config (strong-host send): pointing the LAN adapter at
// 198.18.0.2 makes queries leave through the physical gateway with a LAN
// source IP and vanish. On the TUN interface they originate from 198.18.0.1
// and ride the on-link route straight into the netstack. When the adapter is
// removed its DNS config goes with it — nothing to restore, no stale state.
func (r *windowsRoutes) hijackDNS() error {
	return run("powershell", "-NoProfile", "-NonInteractive", "-Command",
		fmt.Sprintf("Set-DnsClientServerAddress -InterfaceIndex %d -ServerAddresses @('%s')", r.ifIndex, fakeDNS))
}

func (r *windowsRoutes) Close() error {
	var last error
	for i := len(r.added) - 1; i >= 0; i-- {
		p := strings.Split(r.added[i], "|")
		if err := run("netsh", "interface", "ipv4", "delete", "route", p[0],
			"interface="+p[1], "nexthop="+p[2], "store=active"); err != nil {
			last = err
		}
	}
	r.added = nil
	for i := len(r.added6) - 1; i >= 0; i-- {
		p := strings.Split(r.added6[i], "|")
		if len(p) != 3 {
			continue
		}
		if err := r.del6(p[0], p[1], p[2]); err != nil {
			last = err
		}
	}
	r.added6 = nil
	return last
}

// decodeConsole 把 Windows 控制台程序的输出转成 UTF-8。
// 中文系统上 netsh/route/PowerShell 写管道时用 ACP（936/GBK），直接 string() 会变
// "�����Ѵ��ڡ�" 这种乱码，还会让 routeExistsErr 的文本判断失效。先按 UTF-8 校验，
// 不合法再按 GBK 解码；两者都失败时退回原始字节，至少不丢信息。
//
// 参数:
//   - b: []byte — 命令的 CombinedOutput
//
// 返回:
//   - string — 去首尾空白的 UTF-8 文本
func decodeConsole(b []byte) string {
	if utf8.Valid(b) {
		return strings.TrimSpace(string(b))
	}
	if s, err := simplifiedchinese.GBK.NewDecoder().Bytes(b); err == nil {
		return strings.TrimSpace(string(s))
	}
	return strings.TrimSpace(string(b))
}

func powershell(script string) (string, error) {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, decodeConsole(out))
	}
	return decodeConsole(out), nil
}

func powershellInt(script string) (int, error) {
	out, err := powershell(script)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

func psQuote(s string) string { return strings.ReplaceAll(s, "'", "''") }

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, decodeConsole(out))
	}
	return nil
}
