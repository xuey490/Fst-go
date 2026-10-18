package bootstrap

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"fst/internal/store"
)

// DefaultPort 是 FST 的 libp2p 监听端口（TCP + QUIC/UDP 同号）。
const DefaultPort = 8787

// OpenInstance 决定本进程使用的数据目录 / 端口 / 实例序号，并打开数据库。
//
//   - FST_DATA_DIR（+ FST_PORT）覆盖一切：测试与手动多实例运行。
//   - 主目录里 single_instance=1（默认）：单进程、固定端口。
//   - single_instance=0：用 freeSlot 选出第一个空闲端口的序号 n；n>1 使用
//     独立数据目录 "FST-n"（独立身份密钥与数据库，PeerID 不同）与端口 DefaultPort+n-1。
//
// 参数:
//
//	无
//
// 返回:
//   - db: *store.Store — 已打开的数据库（调用方负责 Close）
//   - dir: string — 实际使用的数据目录
//   - port: int — libp2p 监听端口
//   - idx: int — 实例序号（从 1 开始，用于窗口标题与多实例区分）
//   - single: bool — 是否为单实例模式（交给 Wails SingleInstance 使用）
func OpenInstance() (db *store.Store, dir string, port, idx int, single bool) {
	open := func(d string) *store.Store {
		if err := os.MkdirAll(d, 0o700); err != nil {
			log.Fatal(err)
		}
		s, err := store.Open(d)
		if err != nil {
			log.Fatalf("open database: %v", err)
		}
		return s
	}
	port = DefaultPort
	if dir = os.Getenv("FST_DATA_DIR"); dir != "" {
		if v, err := strconv.Atoi(os.Getenv("FST_PORT")); err == nil {
			port = v
		}
		return open(dir), dir, port, 1, false
	}
	base, err := os.UserConfigDir()
	if err != nil {
		log.Fatal(err)
	}
	dir = filepath.Join(base, "FST")
	db = open(dir)
	if single = db.Setting("single_instance", "1") == "1"; single {
		return db, dir, port, 1, true
	}
	if idx = freeSlot(); idx > 1 {
		db.Close()
		dir, port = fmt.Sprintf("%s-%d", dir, idx), DefaultPort+idx-1
		db = open(dir)
	}
	return db, dir, port, idx, false
}

// freeSlot 返回没有其他实例监听的最低序号端口对应的序号。
// 用 Dial 而不是 Listen 探测：libp2p 绑定时带 SO_REUSEADDR/PORT，在 Windows 上
// 先 Listen 再判断会误判端口未被占用。
// ponytail: 探测-再绑定之间存在小的竞态窗口；真正绑定时输给别人的实例会回退到
// 随机端口。上限 16 个实例，超过后大家共用序号 1。
//
// 返回:
//   - int — 可用实例序号（1..16）
func freeSlot() int {
	for i := 1; i <= 16; i++ {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", DefaultPort+i-1), 500*time.Millisecond)
		if err != nil {
			return i
		}
		c.Close()
	}
	return 1
}
