package bootstrap

import (
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
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
	log.SetOutput(io.MultiWriter(f, os.Stderr))
}
