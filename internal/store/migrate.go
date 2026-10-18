package store

import (
	"database/sql"
	"fmt"
	"log/slog"
)

// migration 描述一次 schema 升级。
//
// 版本号写入 SQLite 的 PRAGMA user_version（与数据同事务提交），因此不需要
// 额外的版本表；语句必须可重复执行或由版本号保证只跑一次。
type migration struct {
	version int      // 目标版本号，必须在 migrations 中连续递增
	name    string   // 变更说明，只写日志
	stmts   []string // 顺序在同一事务内执行的 SQL
}

// migrations 是全部 schema 变更的有序清单，也是唯一的建库入口。
//
// v1 是历史全量基线（schema.sql，全部 IF NOT EXISTS，对已有库幂等）；
// 之后每次改表都在末尾追加一版，禁止再直接改基线去「兼容」旧库。
var migrations = []migration{
	{version: 1, name: "baseline", stmts: []string{schema}},
	// v2: 文件夹传输的相对路径（单文件传输为空串），历史记录保持原样
	{version: 2, name: "transfers.rel_path", stmts: []string{
		`ALTER TABLE transfers ADD COLUMN rel_path TEXT NOT NULL DEFAULT ''`,
	}},
	// v3: 聊天增强——气泡类型（text/image/file）、撤回标记、附件 transfer id 列表
	{version: 3, name: "messages.kind+recalled+attach", stmts: []string{
		`ALTER TABLE messages ADD COLUMN kind TEXT NOT NULL DEFAULT 'text'`,
		`ALTER TABLE messages ADD COLUMN recalled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ADD COLUMN attach TEXT NOT NULL DEFAULT ''`,
	}},
	// v4: 对端头像（HELLO 携带的 base64 小图）
	{version: 4, name: "peers.avatar", stmts: []string{
		`ALTER TABLE peers ADD COLUMN avatar TEXT NOT NULL DEFAULT ''`,
	}},
	// v5: 离线消息队列——已尝试次数与下次重试时间（unix ms，0 表示立即可投）
	{version: 5, name: "messages.retry", stmts: []string{
		`ALTER TABLE messages ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ADD COLUMN next_retry INTEGER NOT NULL DEFAULT 0`,
		`CREATE INDEX IF NOT EXISTS idx_messages_queue ON messages (status, next_retry)`,
	}},
	// v6: 群聊——本机房间元数据与房间消息（只存本机，不同步给其他节点）
	{version: 6, name: "rooms", stmts: []string{
		`CREATE TABLE IF NOT EXISTS rooms (
			room_id      TEXT PRIMARY KEY,
			name         TEXT NOT NULL DEFAULT '',
			kind         TEXT NOT NULL DEFAULT '',
			last_ts      INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS room_messages (
			id          TEXT PRIMARY KEY,
			room_id     TEXT NOT NULL,
			sender_id   TEXT NOT NULL,
			sender_name TEXT NOT NULL DEFAULT '',
			content     TEXT NOT NULL,
			ts          INTEGER NOT NULL,
			read        INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_room_messages_room_ts ON room_messages (room_id, ts)`,
	}},
	// v7: 引用（回复）——被引用消息的 ID、预览与发送者；普通消息三列为空串
	{version: 7, name: "messages.reply", stmts: []string{
		`ALTER TABLE messages ADD COLUMN reply_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE messages ADD COLUMN reply_preview TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE messages ADD COLUMN reply_sender TEXT NOT NULL DEFAULT ''`,
	}},
}

// migrate 把数据库升级到 migrations 的最新版本。
//
// 参数:
//   - db: *sql.DB — 已连接的目标库（单连接、WAL 由 Open 配置）
//
// 返回:
//   - error — 读取当前版本或任一迁移事务失败时返回；成功为 nil
//
// 副作用：按版本号顺序建表/改表并写回 PRAGMA user_version；每版一个事务，
// 失败即回滚且不推进版本号。
func migrate(db *sql.DB) error {
	return runMigrations(db, migrations)
}

// runMigrations 是 migrate 的可测试内核：按 list 顺序应用所有大于当前版本的迁移。
//
// 参数:
//   - db: *sql.DB — 目标库
//   - list: []migration — 有序迁移清单（版本号严格递增）
//
// 返回:
//   - error — 版本读取失败、语句执行失败或提交失败时返回；成功为 nil
func runMigrations(db *sql.DB, list []migration) error {
	var cur int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&cur); err != nil {
		return fmt.Errorf("读取 schema 版本: %w", err)
	}
	for _, m := range list {
		if m.version <= cur {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("迁移 v%d 开启事务: %w", m.version, err)
		}
		for _, stmt := range m.stmts {
			if _, err := tx.Exec(stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("迁移 v%d(%s) 执行失败: %w", m.version, m.name, err)
			}
		}
		// PRAGMA 不支持占位参数，版本号来自代码常量而非外部输入。
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("迁移 v%d 写版本号: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("迁移 v%d 提交: %w", m.version, err)
		}
		slog.Info("schema migrated", "version", m.version, "name", m.name)
	}
	return nil
}

// schemaVersion 返回 migrations 的最新版本号（0 表示清单为空）。
//
// 返回:
//   - int — 最新版本；测试与诊断使用
func schemaVersion() int {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].version
}
