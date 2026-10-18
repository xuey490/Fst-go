//go:build darwin

package proxy

import (
	"bufio"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

type darwinRoutes struct {
	iface  string
	added  []darwinRoute
	added6 []darwinRoute
	dns    []darwinDNS
}

type darwinRoute struct {
	kind, prefix string
	inet6        bool
}

type darwinDNS struct {
	service string
	addrs   []string // empty means restore with "empty" (DHCP)
}

func newRouteManager(iface string) routeManager { return &darwinRoutes{iface: iface} }

func flushDNSCache() error {
	if err := darwinRun("dscacheutil", "-flushcache"); err != nil {
		return err
	}
	return darwinRun("killall", "-HUP", "mDNSResponder")
}

func (r *darwinRoutes) Setup(p routePlan) error {
	if err := darwinRun("ifconfig", r.iface, "inet", "198.18.0.1", "198.18.0.1", "netmask", "255.254.0.0", "up"); err != nil {
		return fmt.Errorf("配置 TUN 地址失败（需要 root）: %w", err)
	}
	// IPv6 失败只降级：跳过 v6 地址/路由，IPv4 继续工作。
	if p.ipv6 && !r.addAddr6() {
		p.ipv6 = false
	}
	if p.mode == "global" {
		gateway, err := darwinDefaultGateway()
		if err != nil {
			return err
		}
		for _, b := range globalBypass(p.exit, p.hijackDNS) {
			kind, dest := "-net", b.String()
			if b.Bits() == 32 {
				kind, dest = "-host", b.Addr().String()
			}
			if err := darwinRun("route", "-n", "add", kind, dest, gateway); err != nil {
				// A crashed previous run leaves the identical route behind.
				if strings.Contains(err.Error(), "File exists") {
					_ = darwinRun("route", "-n", "delete", kind, dest)
					err = darwinRun("route", "-n", "add", kind, dest, gateway)
				}
				if err != nil {
					_ = r.Close()
					return fmt.Errorf("添加绕行路由 %s 失败: %w", b, err)
				}
			}
			r.added = append(r.added, darwinRoute{kind, dest})
		}
	}
	for _, x := range tunRoutePrefixes(p) {
		if err := darwinRun("route", "-n", "add", "-net", x.String(), "-interface", r.iface); err != nil {
			_ = r.Close()
			return fmt.Errorf("添加代理路由 %s 失败: %w", x, err)
		}
		r.added = append(r.added, darwinRoute{"-net", x.String(), false})
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
func (r *darwinRoutes) addAddr6() bool {
	if err := darwinRun("ifconfig", r.iface, "inet6", "fdfe:1::1/64", "up"); err != nil {
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
func (r *darwinRoutes) setupIPv6(p routePlan) {
	if p.mode == "global" {
		gateway, err := darwinDefaultGateway6()
		if err != nil {
			slog.Warn("ipv6 bypass routes skipped", "err", err)
		} else {
			for _, b := range globalBypass6(p.exit6) {
				if err := darwinRun("route", "-n", "add", "-inet6", b.String(), gateway); err != nil {
					slog.Warn("ipv6 bypass route failed", "prefix", b, "err", err)
					continue
				}
				r.added6 = append(r.added6, darwinRoute{"-net", b.String(), true})
			}
		}
	}
	for _, x := range tunRoutePrefixes6(p) {
		if err := darwinRun("route", "-n", "add", "-inet6", x.String(), "-interface", r.iface); err != nil {
			slog.Warn("ipv6 tun route failed", "prefix", x, "err", err)
			continue
		}
		r.added6 = append(r.added6, darwinRoute{"-net", x.String(), true})
	}
}

// darwinDefaultGateway6 读取 IPv6 默认路由的网关。
//
// 返回:
//   - string — 网关地址
//   - error — 没有 IPv6 默认路由时返回
func darwinDefaultGateway6() (string, error) {
	out, err := exec.Command("route", "-n", "get", "-inet6", "default").Output()
	if err != nil {
		return "", fmt.Errorf("没有可用的 IPv6 默认路由: %w", err)
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[0] == "gateway:" {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("无法确定 IPv6 默认网关")
}

func darwinDefaultGateway() (string, error) {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return "", fmt.Errorf("读取默认网关失败: %w", err)
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[0] == "gateway:" {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("无法确定原默认网关")
}

func (r *darwinRoutes) hijackDNS() error {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "*") || strings.HasPrefix(line, "An asterisk") {
			continue
		}
		cur, _ := exec.Command("networksetup", "-getdnsservers", line).Output()
		var addrs []string
		for _, a := range strings.Fields(string(cur)) {
			ip, err := netip.ParseAddr(a)
			if err != nil || !ip.Is4() || a == fakeDNS {
				continue
			}
			addrs = append(addrs, a)
		}
		if err := darwinRun("networksetup", "-setdnsservers", line, fakeDNS); err != nil {
			return err
		}
		r.dns = append(r.dns, darwinDNS{service: line, addrs: addrs})
	}
	return nil
}

func (r *darwinRoutes) restoreDNS() {
	for _, d := range r.dns {
		if len(d.addrs) == 0 {
			_ = darwinRun("networksetup", "-setdnsservers", d.service, "empty")
			continue
		}
		args := append([]string{"-setdnsservers", d.service}, d.addrs...)
		_ = darwinRun("networksetup", args...)
	}
	r.dns = nil
}

func (r *darwinRoutes) Close() error {
	r.restoreDNS()
	var last error
	for i := len(r.added) - 1; i >= 0; i-- {
		a := r.added[i]
		if err := darwinRun("route", "-n", "delete", a.kind, a.prefix); err != nil {
			last = err
		}
	}
	r.added = nil
	for i := len(r.added6) - 1; i >= 0; i-- {
		a := r.added6[i]
		if err := darwinRun("route", "-n", "delete", "-inet6", a.prefix); err != nil {
			last = err
		}
	}
	r.added6 = nil
	return last
}

func darwinRun(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
