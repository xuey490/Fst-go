package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// openRaw 在测试目录里打开一个裸连接（不跑迁移），用于模拟旧版本数据库。
//
// 参数:
//   - t: *testing.T — 测试上下文
//   - dir: string — 数据目录
//
// 返回:
//   - *sql.DB: 已连接的句柄（测试结束时自动关闭）
func openRaw(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "fst.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// userVersion 读取数据库的 PRAGMA user_version。
//
// 参数:
//   - t: *testing.T — 测试上下文
//   - db: *sql.DB — 目标库
//
// 返回:
//   - int: 当前版本号
func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestMigrateFreshDB 验证空目录首次打开会建库并落到最新版本。
func TestMigrateFreshDB(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	db := openRaw(t, dir)
	if got := userVersion(t, db); got != schemaVersion() {
		t.Fatalf("version = %d, want %d", got, schemaVersion())
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN
		('peers','messages','transfers','settings','rooms','room_messages')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Fatalf("tables = %d, want 6", n)
	}
	var rel int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('transfers') WHERE name='rel_path'`).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	if rel != 1 {
		t.Fatal("transfers.rel_path missing after migration")
	}
	var retry int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name IN ('attempts','next_retry')`).Scan(&retry); err != nil {
		t.Fatal(err)
	}
	if retry != 2 {
		t.Fatalf("messages retry columns = %d, want 2", retry)
	}
}

// TestMigrateLegacyDB 模拟「已有表但没有版本号」的旧库：升级后版本就位且数据不丢。
func TestMigrateLegacyDB(t *testing.T) {
	dir := t.TempDir()
	raw := openRaw(t, dir)
	if _, err := raw.Exec(schema); err != nil { // 旧版本：直接执行基线，user_version 保持 0
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO settings(key,value) VALUES('nickname','legacy')`); err != nil {
		t.Fatal(err)
	}
	if got := userVersion(t, raw); got != 0 {
		t.Fatalf("legacy version = %d, want 0", got)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := s.Setting("nickname", ""); got != "legacy" {
		t.Fatalf("nickname lost: %q", got)
	}
	if got := userVersion(t, raw); got != schemaVersion() {
		t.Fatalf("upgraded version = %d, want %d", got, schemaVersion())
	}
}

// TestRunMigrationsIncremental 验证多版本顺序执行、幂等重跑，以及新增列随后可用。
func TestRunMigrationsIncremental(t *testing.T) {
	dir := t.TempDir()
	db := openRaw(t, dir)
	list := []migration{
		{version: 1, name: "create", stmts: []string{`CREATE TABLE demo(id INTEGER PRIMARY KEY)`}},
		{version: 2, name: "add column", stmts: []string{`ALTER TABLE demo ADD COLUMN note TEXT`}},
	}
	if err := runMigrations(db, list); err != nil {
		t.Fatal(err)
	}
	if got := userVersion(t, db); got != 2 {
		t.Fatalf("version = %d, want 2", got)
	}
	if _, err := db.Exec(`INSERT INTO demo(id,note) VALUES(1,'x')`); err != nil {
		t.Fatalf("new column unusable: %v", err)
	}
	// 重跑不重复执行（ALTER 重复会报错），版本保持
	if err := runMigrations(db, list); err != nil {
		t.Fatalf("idempotent rerun: %v", err)
	}
	if got := userVersion(t, db); got != 2 {
		t.Fatalf("version after rerun = %d, want 2", got)
	}
}

// TestRunMigrationsRollback 验证失败迁移整体回滚且不推进版本号。
func TestRunMigrationsRollback(t *testing.T) {
	dir := t.TempDir()
	db := openRaw(t, dir)
	list := []migration{
		{version: 1, name: "create", stmts: []string{`CREATE TABLE demo(id INTEGER PRIMARY KEY)`}},
		{version: 2, name: "broken", stmts: []string{`CREATE TABLE other(id INTEGER)`, `THIS IS NOT SQL`}},
	}
	if err := runMigrations(db, list); err == nil {
		t.Fatal("broken migration must fail")
	}
	if got := userVersion(t, db); got != 1 {
		t.Fatalf("version = %d, want 1", got)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='other'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("failed migration must roll back all statements")
	}
}
