//go:build linux

package proxy

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

type linuxRoutes struct {
	iface         string
	added         []string
	added6        []string
	resolvBackup  []byte
	resolvRewrote bool
	resolvedDev   string
}

func newRouteManager(iface string) routeManager { return &linuxRoutes{iface: iface} }

func flushDNSCache() error {
	if err := linuxRun("resolvectl", "flush-caches"); err != nil {
		return linuxRun("systemd-resolve", "--flush-caches")
	}
	return nil
}

func (r *linuxRoutes) Setup(p routePlan) error {
	if err := linuxRun("ip", "addr", "replace", "198.18.0.1/15", "dev", r.iface); err != nil {
		return fmt.Errorf("配置 TUN 地址失败（需要 root/CAP_NET_ADMIN）: %w", err)
	}
	if err := linuxRun("ip", "link", "set", r.iface, "up"); err != nil {
		return err
	}
	// IPv6 失败只降级：跳过 v6 地址/路由，IPv4 继续工作。
	if p.ipv6 && !r.addAddr6() {
		p.ipv6 = false
	}
	if p.mode == "global" {
		gateway, dev, err := linuxDefaultRoute()
		if err != nil {
			return err
		}
		for _, b := range globalBypass(p.exit, p.hijackDNS) {
			args := []string{"route", "add", b.String()}
			if gateway != "" {
				args = append(args, "via", gateway)
			}
			if dev != "" {
				args = append(args, "dev", dev)
			}
			if err := linuxRun("ip", args...); err != nil {
				// A crashed previous run leaves the identical route behind; take
				// ownership by deleting it first.
				if strings.Contains(err.Error(), "File exists") {
					del := append([]string{"route", "del", b.String()}, args[3:]...)
					if delErr := linuxRun("ip", del...); delErr == nil {
						err = linuxRun("ip", args...)
					}
				}
				if err != nil {
					_ = r.Close()
					return fmt.Errorf("添加绕行路由 %s 失败: %w", b, err)
				}
			}
			r.added = append(r.added, b.String())
		}
	}
	for _, x := range tunRoutePrefixes(p) {
		if err := linuxRun("ip", "route", "add", x.String(), "dev", r.iface); err != nil {
			_ = r.Close()
			return fmt.Errorf("添加代理路由 %s 失败: %w", x, err)
		}
		r.added = append(r.added, x.String())
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
//   - bool — 成功时为 true；失败记录 Warn 并返回 false（IPv4 继续可用）
func (r *linuxRoutes) addAddr6() bool {
	if err := linuxRun("ip", "-6", "addr", "replace", "fdfe:1::1/64", "dev", r.iface); err != nil {
		slog.Warn("ipv6 tun address failed; IPv6 proxy disabled for this run", "err", err)
		return false
	}
	return true
}

// setupIPv6 安装 IPv6 路由；任何一条失败只 Warn，不阻断 TUN。
//
// 参数:
//   - p: routePlan — 含 ipv6 前缀、出口 v6 与全局模式开关
//
// 返回:
//   - 无
func (r *linuxRoutes) setupIPv6(p routePlan) {
	if p.mode == "global" {
		gw, dev, err := linuxDefaultRoute6()
		if err != nil {
			slog.Warn("ipv6 bypass routes skipped", "err", err)
		} else {
			for _, b := range globalBypass6(p.exit6) {
				args := []string{"-6", "route", "add", b.String()}
				if gw != "" {
					args = append(args, "via", gw)
				}
				if dev != "" {
					args = append(args, "dev", dev)
				}
				if err := linuxRun("ip", args...); err != nil {
					slog.Warn("ipv6 bypass route failed", "prefix", b, "err", err)
					continue
				}
				r.added6 = append(r.added6, b.String())
			}
		}
	}
	for _, x := range tunRoutePrefixes6(p) {
		if err := linuxRun("ip", "-6", "route", "add", x.String(), "dev", r.iface); err != nil {
			slog.Warn("ipv6 tun route failed", "prefix", x, "err", err)
			continue
		}
		r.added6 = append(r.added6, x.String())
	}
}

// linuxDefaultRoute6 读取 IPv6 默认路由的网关与网卡。
//
// 返回:
//   - gateway: string — 下一跳；可能为空
//   - dev: string — 出接口
//   - err: error — 没有默认路由时返回
func linuxDefaultRoute6() (gateway, dev string, err error) {
	out, err := exec.Command("ip", "-6", "route", "show", "default").Output()
	if err != nil {
		return "", "", fmt.Errorf("读取 IPv6 默认路由失败: %w", err)
	}
	fields := strings.Fields(string(out))
	for i := range fields {
		if fields[i] == "via" && i+1 < len(fields) {
			gateway = fields[i+1]
		}
		if fields[i] == "dev" && i+1 < len(fields) {
			dev = fields[i+1]
		}
	}
	if gateway == "" && dev == "" {
		return "", "", fmt.Errorf("没有可用的 IPv6 默认路由")
	}
	return gateway, dev, nil
}

func linuxDefaultRoute() (gateway, dev string, err error) {
	out, err := exec.Command("ip", "-4", "route", "show", "default").Output()
	if err != nil {
		return "", "", fmt.Errorf("读取默认网关失败: %w", err)
	}
	fields := strings.Fields(string(out))
	for i := range fields {
		if fields[i] == "via" && i+1 < len(fields) {
			gateway = fields[i+1]
		}
		if fields[i] == "dev" && i+1 < len(fields) {
			dev = fields[i+1]
		}
	}
	if gateway == "" && dev == "" {
		return "", "", fmt.Errorf("无法确定原默认网关")
	}
	return gateway, dev, nil
}

func (r *linuxRoutes) hijackDNS() error {
	const path = "/etc/resolv.conf"
	if _, err := os.Readlink(path); err == nil {
		if err := linuxRun("resolvectl", "dns", r.iface, fakeDNS); err != nil {
			return err
		}
		_ = linuxRun("resolvectl", "default-route", r.iface, "yes")
		r.resolvedDev = r.iface
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	r.resolvBackup = b
	if err := os.WriteFile(path, []byte("nameserver "+fakeDNS+"\n"), 0o644); err != nil {
		return err
	}
	r.resolvRewrote = true
	return nil
}

func (r *linuxRoutes) restoreDNS() {
	if r.resolvRewrote {
		_ = os.WriteFile("/etc/resolv.conf", r.resolvBackup, 0o644)
		r.resolvRewrote = false
	}
	if r.resolvedDev != "" {
		_ = linuxRun("resolvectl", "revert", r.resolvedDev)
		r.resolvedDev = ""
	}
}

func (r *linuxRoutes) Close() error {
	r.restoreDNS()
	var last error
	for i := len(r.added) - 1; i >= 0; i-- {
		if err := linuxRun("ip", "route", "del", r.added[i]); err != nil {
			last = err
		}
	}
	r.added = nil
	for i := len(r.added6) - 1; i >= 0; i-- {
		if err := linuxRun("ip", "-6", "route", "del", r.added6[i]); err != nil {
			last = err
		}
	}
	r.added6 = nil
	return last
}

func linuxRun(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
