package bootstrap

import (
	"bytes"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// InitLog 把日志写入 <dataDir>/fst.log；检测到 stderr 已连接时同时输出到 stderr。
// 打开日志文件失败时静默跳过（应用仍需正常启动，不能因日志失败退出）。
//
// 参数:
//   - dir: string — 数据目录；日志文件固定命名为 fst.log
//
// 返回:
//   - 无
func InitLog(dir string) {
	f, err := os.OpenFile(filepath.Join(dir, "fst.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(f, os.Stderr), nil)))
	// 标准库 log 这一路只有 zeroconf 在用。它启动时会对每块网卡尝试加入 IPv6
	// 组播组，纯 IPv4 的 Wi-Fi 上必定失败并打印
	// "[zeroconf] no suitable IPv6 interface"，每个服务名一条，纯噪声：
	// zeroconf 的 ServerOption 里没有关掉 IPv6 的开关，只能在这里折叠。
	// 只过滤这一条，其它标准库日志照常输出。
	log.SetOutput(newLineFilter(io.MultiWriter(f, os.Stderr), "[zeroconf] no suitable IPv6 interface"))
}

// lineFilter 是把「按行」写入的 Writer 包装器，丢弃匹配给定前缀的行。
//
// 为什么需要它：标准库 log 是逐条 Write 的，但底层 Writer 不保证按行对齐，
// 因此必须自己缓冲到换行为止，否则多字节截断会把日志写坏。
type lineFilter struct {
	w    io.Writer
	drop []string

	mu  sync.Mutex
	buf []byte
}

// newLineFilter 创建按行过滤器。
//
// 参数:
//   - w: io.Writer — 下游 Writer
//   - drop: ...string — 需要丢弃的行前缀
//
// 返回:
//   - *lineFilter — 可安全并发使用的过滤器
func newLineFilter(w io.Writer, drop ...string) *lineFilter {
	return &lineFilter{w: w, drop: drop}
}

// Write 接收一段日志，按行转发不匹配的内容。
//
// 参数:
//   - p: []byte — 原始字节
//
// 返回:
//   - int — 始终返回 len(p)，避免调用方重试
//   - error — 下游错误（丢弃的行不会产生错误）
func (f *lineFilter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			break
		}
		line := f.buf[:i+1]
		if !f.match(line) {
			if _, err := f.w.Write(line); err != nil {
				return len(p), err
			}
		}
		f.buf = f.buf[i+1:]
	}
	return len(p), nil
}

// match 判断一行是否命中丢弃前缀。
//
// 参数:
//   - line: []byte — 单行内容（含换行）
//
// 返回:
//   - bool — true 表示应丢弃
func (f *lineFilter) match(line []byte) bool {
	for _, p := range f.drop {
		if bytes.Contains(line, []byte(p)) {
			return true
		}
	}
	return false
}
