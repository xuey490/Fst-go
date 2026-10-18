package transfer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFile 写一个测试文件，失败即终止用例。
//
// 参数:
//   - t: *testing.T — 测试上下文
//   - path: string — 目标文件路径（父目录需已存在）
//   - body: string — 内容
//
// 返回:
//   - 无
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestExpandPaths 验证文件夹递归展开：相对路径以所选根目录名为前缀、跳过 .fstpart、直接选择的文件不带相对路径。
func TestExpandPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "album")
	for _, d := range []string{root, filepath.Join(root, "sub", "deep")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "a.txt"), "a")
	writeFile(t, filepath.Join(root, "sub", "b.txt"), "b")
	writeFile(t, filepath.Join(root, "sub", "deep", "c.txt"), "c")
	writeFile(t, filepath.Join(root, "x.fstpart"), "part")
	single := filepath.Join(t.TempDir(), "top.bin")
	writeFile(t, single, "top")

	got, dirs, skipped, err := expandPaths([]string{root, single})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 0 || len(skipped) != 0 {
		t.Fatalf("dirs = %v, skipped = %v", dirs, skipped)
	}
	want := []struct{ name, rel string }{
		{"a.txt", "album/a.txt"},
		{"b.txt", "album/sub/b.txt"},
		{"c.txt", "album/sub/deep/c.txt"},
		{"top.bin", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %d, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].name != w.name || got[i].rel != w.rel {
			t.Fatalf("entry %d = %+v, want %+v", i, got[i], w)
		}
		if got[i].size <= 0 {
			t.Fatalf("entry %d size = %d", i, got[i].size)
		}
	}
}

// TestExpandPathsEmptyAndLimit 验证空文件夹被单独收集、符号链接被记录，且超过单批上限不再报错（交给分批）。
func TestExpandPathsEmptyAndLimit(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	files, dirs, skipped, err := expandPaths([]string{empty})
	if err != nil {
		t.Fatalf("empty folder must be sendable as a directory entry: %v", err)
	}
	if len(files) != 0 || len(dirs) != 1 || dirs[0] != "empty" || len(skipped) != 0 {
		t.Fatalf("empty folder expand = files %d dirs %v skipped %v", len(files), dirs, skipped)
	}

	// 只有符号链接的目录：链接被跳过，目录本身按空目录还原
	linkDir := filepath.Join(t.TempDir(), "links")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(empty, "missing"), filepath.Join(linkDir, "l")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	files, dirs, skipped, err = expandPaths([]string{linkDir})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 || len(skipped) != 1 || len(dirs) != 1 {
		t.Fatalf("symlink dir expand = files %d dirs %v skipped %v", len(files), dirs, skipped)
	}

	// 超过单批上限（maxFiles）不再报错：SendReport 会拆批
	full := filepath.Join(t.TempDir(), "many")
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= maxFiles; i++ {
		writeFile(t, filepath.Join(full, fmt.Sprintf("f%04d.txt", i)), "x")
	}
	files, _, _, err = expandPaths([]string{full})
	if err != nil {
		t.Fatalf("files above one batch must be accepted: %v", err)
	}
	if len(files) != maxFiles+1 {
		t.Fatalf("expanded %d files, want %d", len(files), maxFiles+1)
	}
}

// TestLimiterWaits 验证限速器按速率放行，并在上下文取消时立即返回。
func TestLimiterWaits(t *testing.T) {
	l := newLimiter(256 << 10) // 256KB/s，桶 256KB
	start := time.Now()
	if err := l.wait(context.Background(), 192<<10); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("first chunk should be paid by the initial burst, took %s", d)
	}
	if err := l.wait(context.Background(), 512<<10); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 1*time.Second {
		t.Fatalf("512KB at 256KB/s should take >=1s, took %s", d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.wait(ctx, 64<<10); err == nil {
		t.Fatal("cancelled wait must return an error")
	}
}

// TestSafeRelPath 验证对端相对路径的清洗：合法路径保留结构，越界/绝对/控制字符一律拒绝。
func TestSafeRelPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // 期望输出；出错时忽略
		err  bool
	}{
		{name: "single file", in: "", want: ""},
		{name: "nested", in: "album/sub/a.txt", want: "album/sub/a.txt"},
		{name: "backslashes", in: `album\sub\a.txt`, want: "album/sub/a.txt"},
		{name: "trims segments", in: "album/  a.txt  ", want: "album/a.txt"},
		{name: "absolute unix", in: "/etc/passwd", err: true},
		{name: "drive letter", in: `C:\Windows\a.txt`, err: true},
		{name: "drive letter portable", in: "C:/Windows/a.txt", err: true},
		{name: "parent segment", in: "../a.txt", err: true},
		{name: "parent inside", in: "album/../../a.txt", err: true},
		{name: "current segment", in: "album/./a.txt", err: true},
		{name: "empty segment", in: "album//a.txt", err: true},
		{name: "control char", in: "album/a\x01.txt", err: true},
		{name: "only dots", in: "...", err: true},
	}
	for _, c := range cases {
		got, err := safeRelPath(c.in)
		if c.err {
			if err == nil {
				t.Fatalf("%s: %q accepted as %q", c.name, c.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %q rejected: %v", c.name, c.in, err)
		}
		if got != c.want {
			t.Fatalf("%s: %q -> %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// TestLocalTargetStaysUnderRoot 验证任何被接受的相对路径都落在下载根目录之内。
func TestLocalTargetStaysUnderRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dl")
	for _, rel := range []string{"album/a.txt", "album/sub/deep/c.txt", "a.txt"} {
		dir, name := localTarget(root, rel, "ignored.txt")
		full := filepath.Join(dir, name)
		inside, err := filepath.Rel(root, full)
		if err != nil || strings.HasPrefix(inside, "..") {
			t.Fatalf("%q escaped the download dir: %q (%v)", rel, full, err)
		}
		if name != filepath.Base(rel) {
			t.Fatalf("%q name = %q", rel, name)
		}
	}
	if dir, name := localTarget(root, "", "a.txt"); dir != root || name != "a.txt" {
		t.Fatalf("single file target = %q/%q", dir, name)
	}
}
