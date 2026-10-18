// Package feature 是高级网络功能门禁：默认关闭，解锁入口见 docs/开发文档.md。
package feature

import (
	"errors"
	"os"

	"fst/internal/store"
)

// SettingKey 是持久化解锁标记在 settings 表里的键（不写 config.yaml）。
const SettingKey = "wan_unlocked"

// ErrForced 表示本次进程由外部环境强制开启，本机无法屏蔽。
var ErrForced = errors.New("当前无法屏蔽该功能")

// EnvForced 返回本次进程是否由外部环境强制开启（优先级最高，仅内部判断，不对外展示来源）。
//
// 返回:
//   - bool — 环境变量命中时为 true
func EnvForced() bool { return os.Getenv("FST_WAN") == "1" }

// WANUnlocked 返回高级网络功能是否解锁。
//
// 参数:
//   - s: *store.Store — 本机数据访问层；nil 时只认外部环境
//
// 返回:
//   - bool — 环境变量优先；否则读取 settings 里的持久标记；默认 false
func WANUnlocked(s *store.Store) bool {
	if EnvForced() {
		return true
	}
	if s == nil {
		return false
	}
	return s.Setting(SettingKey, "0") == "1"
}

// SetWANUnlocked 持久化写入解锁标记（settings 表，不影响 config.yaml）。
//
// 参数:
//   - s: *store.Store — 本机数据访问层
//   - enable: bool — true 解锁，false 屏蔽
//
// 返回:
//   - error — 数据层未就绪、写库失败，或外部环境强制开启时请求屏蔽（ErrForced）
//
// 说明：标记只在下次启动装配时生效；当前进程的装配状态不变。
func SetWANUnlocked(s *store.Store, enable bool) error {
	if s == nil {
		return errors.New("本机数据未就绪")
	}
	if !enable && EnvForced() {
		return ErrForced
	}
	v := "0"
	if enable {
		v = "1"
	}
	return s.SetSetting(SettingKey, v)
}
