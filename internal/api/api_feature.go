package api

import (
	"net/http"

	"fst/internal/feature"
)

// features 返回高级网络功能当前是否解锁（只读，不解释入口）。
//
// 返回:
//   - any — {wan_unlocked: bool}
//   - error — 恒为 nil
func (a *API) features(*http.Request) (any, error) {
	return map[string]any{"wan_unlocked": feature.WANUnlocked(a.n.Store)}, nil
}

// setFeature 持久化高级网络功能开关（写 settings 表，不写 config.yaml）。
//
// 参数:
//   - r: *http.Request — JSON {enable: bool}
//
// 返回:
//   - any — {wan_unlocked, restart_required: true}
//   - error — 请求体非法、写库失败，或外部环境强制开启时请求屏蔽（「当前无法屏蔽该功能」）
//
// 说明：开关只在下次启动装配时生效，当前进程的中继/代理状态不变。
func (a *API) setFeature(r *http.Request) (any, error) {
	var in struct {
		Enable bool `json:"enable"`
	}
	if err := decode(r, &in); err != nil {
		return nil, err
	}
	if err := feature.SetWANUnlocked(a.n.Store, in.Enable); err != nil {
		return nil, err
	}
	return map[string]any{
		"wan_unlocked":     feature.WANUnlocked(a.n.Store),
		"restart_required": true,
	}, nil
}
