package bootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

const configName = "config.yaml"

// NetConfig is the cross-network part of the configuration. It lives in config.yaml, not in the database,
// so several relays can be listed and shipped next to the executable. The settings page reads and writes it.
type NetConfig struct {
	EnableRelay bool         `yaml:"enable_relay"`    // same switch as "allow cross-network" in the settings page
	Relays      []string     `yaml:"relays"`          // static relay multiaddrs, each ending in /p2p/<PeerID>
	Group       string       `yaml:"group,omitempty"` // relay-group key: relays only introduce terminals of the same group
	Proxy       ProxyConfig  `yaml:"proxy,omitempty"`
}

// ProxyConfig 是 config.yaml 的 proxy 段（TUN 出口代理与 SOCKS5 入站加固）。
type ProxyConfig struct {
	Enable  bool     `yaml:"enable"`
	Mode    string   `yaml:"mode,omitempty"`    // split | global
	Exit    string   `yaml:"exit,omitempty"`    // relay multiaddr ending in /p2p/<PeerID>
	Routes  []string `yaml:"routes,omitempty"`  // IPv4/IPv6 CIDRs used in split mode
	Domains []string `yaml:"domains,omitempty"` // domain suffix rules used in split mode (fake-IP)

	// Token 是出口令牌（中继 -exit-token）；空串表示不携带（旧行为）。
	Token string `yaml:"token,omitempty"`
	// IPv6 打开 TUN 的 IPv6 栈与 AAAA 代理解析；默认关闭。
	IPv6 bool `yaml:"ipv6,omitempty"`
	// SocksEnable 控制是否启动本机 SOCKS5 入站；未配置（nil）时默认开启，保持旧行为。
	SocksEnable *bool `yaml:"socks_enable,omitempty"`
	// 本机 SOCKS5 入站加固：账号非空即启用 RFC1929；Allow 非空才允许局域网来源。
	SocksUser     string   `yaml:"socks_user,omitempty"`
	SocksPass     string   `yaml:"socks_pass,omitempty"`
	SocksAllow    []string `yaml:"socks_allow,omitempty"`
	SocksListen   string   `yaml:"socks_listen,omitempty"`
	SocksMaxConn  int      `yaml:"socks_max_conn,omitempty"`
	SocksMaxPerIP int      `yaml:"socks_max_per_ip,omitempty"`
}

const configHeader = "# FST 网络配置（设置页保存会改写本文件，重启生效）\n# relays: 中继节点，形如 /ip4/1.2.3.4/tcp/8787/p2p/<PeerID>\n# group: 中继分组密钥；填写后只与同组终端互相可见（中继需用 -group/-group-file 配同一个值），留空为不分组\n# proxy: TUN 出口代理（IPv4 TCP/UDP，可选 IPv6）\n#   split : 仅匹配 routes(CIDR) / domains(域名) 的流量走隧道，其余直连；填了域名时这些域名的 DNS 由本地 fake-IP 接管\n#   global: 全部流量（含 DNS）经出口，域名在出口侧解析；仅 LAN 与出口中继 IP 直连\n#   routes/domains 均可写多条（每行一条，也支持空格/逗号分隔）\n#   token : 中继 -exit-token 的出口令牌；留空表示不携带（旧行为）\n#   ipv6  : 实验；TUN 增加 IPv6 栈、fake AAAA 与 v6 路由，失败自动降级为 IPv4\n#   socks_enable: 本机 SOCKS5 入站开关（缺省开启）；socks_* 为认证/来源/并发加固\n"

// ConfigPaths lists where config.yaml is looked up, best first: next to the executable (packaged app),
// the working directory (wails3 dev runs in the project root), then the per-user config dir
// (for installs whose directory is read-only, e.g. Program Files or /usr/local/bin).
func ConfigPaths() []string {
	var p []string
	if exe, err := os.Executable(); err == nil {
		p = append(p, filepath.Join(filepath.Dir(exe), configName))
	}
	if wd, err := os.Getwd(); err == nil {
		p = append(p, filepath.Join(wd, configName))
	}
	if d, err := os.UserConfigDir(); err == nil {
		p = append(p, filepath.Join(d, "FST", configName))
	}
	return p
}

// LoadNetConfig returns the first readable config.yaml of paths and where it came from; path is "" if none.
// A broken file is reported by the caller's log, not silently replaced.
//
// 参数:
//   - paths: []string — 候选配置文件路径，按优先级排列
//
// 返回:
//   - cfg: NetConfig — 读到的配置；无可用文件时为零值
//   - path: string — 实际读取的文件路径；无可用文件时为空串
//   - err: error — 文件存在但 YAML 非法时返回
func LoadNetConfig(paths []string) (cfg NetConfig, path string, err error) {
	for _, p := range paths {
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			continue
		}
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return NetConfig{}, p, err
		}
		cfg.Relays = CleanRelays(cfg.Relays)
		cfg.Group = strings.TrimSpace(cfg.Group)
		cfg.Proxy.Routes = CleanRelays(cfg.Proxy.Routes)
		cfg.Proxy.Domains = CleanRelays(cfg.Proxy.Domains)
		cfg.Proxy.SocksAllow = CleanRelays(cfg.Proxy.SocksAllow)
		cfg.Proxy.SocksUser = strings.TrimSpace(cfg.Proxy.SocksUser)
		cfg.Proxy.SocksListen = strings.TrimSpace(cfg.Proxy.SocksListen)
		return cfg, p, nil
	}
	return NetConfig{}, "", nil
}

// SaveNetConfig writes cfg to path (atomically), or, when path is empty, to the first writable default:
// the executable's directory, else the user config dir. It returns the path actually written.
//
// 参数:
//   - path: string — 目标文件；空串表示按 defaults 兜底选择
//   - cfg: NetConfig — 待写入的配置
//   - defaults: []string — ConfigPaths 的结果（用 [0] exe 目录与 [2] 用户配置目录兜底）
//
// 返回:
//   - string — 实际写入的文件路径
//   - error — 所有候选路径都写失败时返回
func SaveNetConfig(path string, cfg NetConfig, defaults []string) (string, error) {
	cfg.Relays = CleanRelays(cfg.Relays)
	cfg.Group = strings.TrimSpace(cfg.Group)
	cfg.Proxy.Routes = CleanRelays(cfg.Proxy.Routes)
	cfg.Proxy.Domains = CleanRelays(cfg.Proxy.Domains)
	cfg.Proxy.SocksAllow = CleanRelays(cfg.Proxy.SocksAllow)
	cfg.Proxy.SocksUser = strings.TrimSpace(cfg.Proxy.SocksUser)
	cfg.Proxy.SocksListen = strings.TrimSpace(cfg.Proxy.SocksListen)
	if cfg.Relays == nil {
		cfg.Relays = []string{}
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	data := append([]byte(configHeader), body...)
	targets := []string{path}
	if path == "" {
		targets = nil
		if len(defaults) > 0 {
			targets = append(targets, defaults[0]) // exe dir
		}
		if len(defaults) > 2 {
			targets = append(targets, defaults[2]) // user config dir
		}
	}
	var last error = errors.New("no place to write config.yaml")
	for _, t := range targets {
		if last = writeAtomic(t, data); last == nil {
			return t, nil
		}
	}
	return "", last
}

// ponytail: rewriting from the struct drops hand-written comments; keep them with a yaml.Node round trip if that matters.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// SocksEnabled 返回 SOCKS5 入站是否启用；未配置 socks_enable 时默认开启（兼容旧配置）。
//
// 返回:
//   - bool — 启用为 true
func (p ProxyConfig) SocksEnabled() bool {
	return p.SocksEnable == nil || *p.SocksEnable
}

// CleanRelays trims, drops blanks and removes duplicates, keeping order.
//
// 参数:
//   - in: []string — 原始条目
//
// 返回:
//   - []string — 去空白、去空项、按首次出现顺序去重后的条目
func CleanRelays(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// LegacyNetConfig reads the pre-yaml settings (database keys wan_enabled / relays). ok is false when none were set.
//
// 参数:
//   - setting: func(key, def string) string — settings 读取函数
//
// 返回:
//   - cfg: NetConfig — 旧库迁移出的配置
//   - ok: bool — 库中存在旧键时为 true
func LegacyNetConfig(setting func(key, def string) string) (cfg NetConfig, ok bool) {
	cfg.EnableRelay = setting("wan_enabled", "0") == "1"
	cfg.Relays = CleanRelays(strings.Fields(setting("relays", "")))
	return cfg, cfg.EnableRelay || len(cfg.Relays) > 0
}
