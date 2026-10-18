package node

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// initGroups 构建中继的组表（key -> spec 与 name -> spec）。
//
// 参数:
//   - specs: []GroupSpec — 来自 -group-file 的组表；旧 -group 会作为 name=default 追加
//
// 返回:
//   - 无
//
// 说明：仅中继（directory）使用；空表表示不做分组校验（旧行为）。重复 key 后者覆盖前者并记录 Warn。
func (n *Node) initGroups(specs []GroupSpec) {
	n.groupSpec = map[string]GroupSpec{}
	n.byName = map[string]GroupSpec{}
	if !n.directory {
		return
	}
	n.groupSpec, n.byName = buildGroupMaps(specs, n.group)
	slog.Info("relay group table loaded", "groups", len(n.byName), "legacy", n.group != "")
}

// buildGroupMaps 把组表规范化为 key->spec 与 name->spec 两张表。
//
// 参数:
//   - specs: []GroupSpec — 文件解析结果；空名/空 key 的条目会被忽略
//   - legacy: string — 旧 -group 单密钥；非空时追加为 name=default
//
// 返回:
//   - map[string]GroupSpec — key -> spec
//   - map[string]GroupSpec — name -> spec
//
// 说明：仅中继使用；重复 key 后者覆盖前者并记录 Warn。
func buildGroupMaps(specs []GroupSpec, legacy string) (map[string]GroupSpec, map[string]GroupSpec) {
	byKey := map[string]GroupSpec{}
	byName := map[string]GroupSpec{}
	all := append([]GroupSpec{}, specs...)
	if strings.TrimSpace(legacy) != "" {
		all = append(all, GroupSpec{Name: "default", Key: legacy})
	}
	for _, sp := range all {
		sp.Name = strings.TrimSpace(sp.Name)
		sp.Key = strings.TrimSpace(sp.Key)
		if sp.Name == "" || sp.Key == "" {
			slog.Warn("relay group entry ignored: empty name or key")
			continue
		}
		if _, dup := byKey[sp.Key]; dup {
			slog.Warn("relay group key duplicated; later entry wins", "group", sp.Name)
		}
		byKey[sp.Key] = sp
		byName[sp.Name] = sp
	}
	return byKey, byName
}

// watchGroupFile 轮询组表文件，mtime/size 变化时原子换表并向已连接终端重发 HELLO。
//
// 返回:
//   - 无（goroutine 随节点关闭退出；文件暂时不可读时保留旧表并记录 Warn）
func (n *Node) watchGroupFile() {
	interval := n.groupReload
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if interval < 2*time.Second {
		interval = 2 * time.Second
	}
	n.groupStamp = groupFileStamp(n.groupFile)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-t.C:
		}
		stamp := groupFileStamp(n.groupFile)
		if stamp == "" || stamp == n.groupStamp {
			continue
		}
		specs, err := ParseGroupFile(n.groupFile)
		if err != nil {
			slog.Warn("relay group file reload failed; keeping previous table", "err", err)
			continue
		}
		n.groupStamp = stamp
		n.applyGroupSpecs(specs)
	}
}

// groupFileStamp 返回文件 mtime+size 的字符串标识；读不到文件时为空串。
//
// 参数:
//   - path: string — 组表路径
//
// 返回:
//   - string — 变更标识；空串表示不可用
func groupFileStamp(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", st.Size(), st.ModTime().UnixNano())
}

// applyGroupSpecs 原子替换组表，并撤销已删除/过期/换钥分组里在线终端的资格。
//
// 参数:
//   - specs: []GroupSpec — 新组表（来自 -group-file）
//
// 返回:
//   - 无
//
// 副作用：清空被撤销终端的 group 记录并写 groupErr；随后异步向所有已连接终端重发 HELLO。
// 说明：只停发目录/通知与出口资格，不主动断开已有连接（与 GroupMaxTerminals 的软限制一致）。
func (n *Node) applyGroupSpecs(specs []GroupSpec) {
	newByKey, newByName := buildGroupMaps(specs, n.group)
	n.mu.Lock()
	oldByName := n.byName
	n.groupSpec, n.byName = newByKey, newByName
	for pid, name := range n.groups {
		oldSp, hadOld := oldByName[name]
		newSp, ok := newByName[name]
		switch {
		case !ok:
			delete(n.groups, pid)
			n.groupErr[pid] = "分组已撤销"
		case groupExpired(newSp):
			delete(n.groups, pid)
			n.groupErr[pid] = "分组已过期"
		case hadOld && oldSp.Key != newSp.Key:
			delete(n.groups, pid)
			n.groupErr[pid] = "分组密钥已轮换"
		}
	}
	n.mu.Unlock()
	if n.Host != nil {
		for _, pid := range n.Host.Network().Peers() {
			go n.hello(pid)
		}
	}
	slog.Info("relay group table reloaded", "groups", len(newByName))
}

// assignGroup 校验终端出示的分组密钥并记录其组名。
//
// 参数:
//   - pid: peer.ID — 终端
//   - key: string — HELLO 中出示的密钥
//
// 返回:
//   - string — 失败原因（中文）；成功或中继未配置分组时为空
//
// 副作用：组表为空时清空该终端的组记录（兼容旧行为）；失败会清空记录，使目录/通知停发。
func (n *Node) assignGroup(pid peer.ID, key string) string {
	key = strings.TrimSpace(key)
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.groupSpec) == 0 {
		delete(n.groups, pid)
		delete(n.groupErr, pid)
		return ""
	}
	if key == "" {
		delete(n.groups, pid)
		n.groupErr[pid] = "缺少分组密钥"
		return n.groupErr[pid]
	}
	sp, ok := n.groupSpec[key]
	if !ok {
		delete(n.groups, pid)
		n.groupErr[pid] = "分组密钥无效"
		return n.groupErr[pid]
	}
	if groupExpired(sp) {
		delete(n.groups, pid)
		n.groupErr[pid] = "分组已过期"
		return n.groupErr[pid]
	}
	n.groups[pid] = sp.Name
	delete(n.groupErr, pid)
	return ""
}

// groupInfo 返回某终端的分组名、有效期与失败原因（用于 HELLO 回包）。
//
// 参数:
//   - pid: peer.ID — 终端
//
// 返回:
//   - name: string — 命中的组名；未分组为空
//   - expires: int64 — unix ms；0 表示不过期
//   - errText: string — 最近一次密钥校验失败原因
func (n *Node) groupInfo(pid peer.ID) (name string, expires int64, errText string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if e := n.groupErr[pid]; e != "" {
		return "", 0, e
	}
	name = n.groups[pid]
	if name == "" {
		return "", 0, ""
	}
	sp := n.byName[name]
	return name, sp.ExpiresAt, ""
}

// RelayGroup 返回终端视角的中继分组状态（来自最近一次 HELLO 回包）。
//
// 返回:
//   - name: string — 命中的组名
//   - expires: int64 — unix ms；0 表示不过期
//   - errText: string — 中继给出的失败原因（密钥无效/已过期）
func (n *Node) RelayGroup() (name string, expires int64, errText string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.relayGroupName, n.relayGroupExp, n.relayGroupError
}

// GroupOf 返回某终端当前有效的分组名（供出口鉴权使用）。
//
// 参数:
//   - pid: peer.ID — 终端
//
// 返回:
//   - string — 组名；未分组/未配置分组/已过期时为空
func (n *Node) GroupOf(pid peer.ID) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.groupSpec) == 0 {
		return ""
	}
	name := n.groups[pid]
	sp, ok := n.byName[name]
	if !ok || groupExpired(sp) {
		return ""
	}
	return name
}

// sameGroupLocked 判断 pid 是否属于本中继服务的分组（未配置分组时恒为 true）。
//
// 参数:
//   - pid: peer.ID — 待判断的在线终端
//
// 返回:
//   - bool — 未配置组表恒为 true；否则要求该终端命中的组仍有效
//
// 调用方必须持有 n.mu。
func (n *Node) sameGroupLocked(pid peer.ID) bool {
	if len(n.groupSpec) == 0 {
		return true
	}
	name := n.groups[pid]
	sp, ok := n.byName[name]
	return ok && !groupExpired(sp)
}

// groupExpired 判断组是否已过期。
//
// 参数:
//   - sp: GroupSpec — 组表一行
//
// 返回:
//   - bool — ExpiresAt>0 且已早于当前时间时为 true
func groupExpired(sp GroupSpec) bool {
	return sp.ExpiresAt > 0 && time.Now().UnixMilli() > sp.ExpiresAt
}

// ParseGroupSpecs 解析组表文件内容。
//
// 参数:
//   - r: io.Reader — 每行 `name:key[:expiry]`；空行与 `#` 注释忽略
//
// 返回:
//   - []GroupSpec — 解析结果；expiry 支持 RFC3339 或 Unix 秒
//   - error — 行格式错误、组名/密钥为空或重复
//
// 说明：key 中不允许出现冒号（用 SplitN 3 截断），避免过期时间被误解析。
func ParseGroupSpecs(r io.Reader) ([]GroupSpec, error) {
	var out []GroupSpec
	seenName := map[string]bool{}
	seenKey := map[string]bool{}
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		parts := strings.SplitN(text, ":", 3)
		if len(parts) < 2 {
			return nil, fmt.Errorf("组表第 %d 行格式应为 name:key[:expiry]", line)
		}
		name, key := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if name == "" || key == "" {
			return nil, fmt.Errorf("组表第 %d 行组名/密钥不能为空", line)
		}
		if seenName[name] {
			return nil, fmt.Errorf("组表第 %d 行组名重复: %s", line, name)
		}
		if seenKey[key] {
			return nil, fmt.Errorf("组表第 %d 行密钥重复", line)
		}
		sp := GroupSpec{Name: name, Key: key}
		if len(parts) == 3 && strings.TrimSpace(parts[2]) != "" {
			ts, err := parseExpiry(parts[2])
			if err != nil {
				return nil, fmt.Errorf("组表第 %d 行过期时间无效: %w", line, err)
			}
			sp.ExpiresAt = ts
		}
		seenName[name], seenKey[key] = true, true
		out = append(out, sp)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ParseGroupFile 读取并解析组表文件。
//
// 参数:
//   - path: string — 文件路径（-group-file）
//
// 返回:
//   - []GroupSpec — 解析结果
//   - error — 打开/读取/解析失败
func ParseGroupFile(path string) ([]GroupSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseGroupSpecs(f)
}

// parseExpiry 解析过期时间（RFC3339 或 Unix 秒），统一成 unix ms。
//
// 参数:
//   - text: string — 原始时间文本
//
// 返回:
//   - int64 — unix 毫秒
//   - error — 两种格式都不匹配时返回
func parseExpiry(text string) (int64, error) {
	text = strings.TrimSpace(text)
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		return t.UnixMilli(), nil
	}
	if n, err := strconv.ParseInt(text, 10, 64); err == nil && n > 0 {
		return n * 1000, nil
	}
	return 0, fmt.Errorf("需要 RFC3339 或 Unix 秒: %q", text)
}
