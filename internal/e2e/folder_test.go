package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 文件夹传输：选中的目录被递归展开，接收端保留嵌套结构与内容，历史记录带 rel_path。
func TestFolderTransfer(t *testing.T) {
	a, b := newNode(t, "alice"), newNode(t, "bob")
	a.connect(t, b)
	waitFor(t, "online", func() bool { return a.online(b) && b.online(a) })
	b.n.Store.SetSetting("auto_accept", "1")

	root := filepath.Join(t.TempDir(), "album")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	text := []byte("hello folder")
	bin := bytes.Repeat([]byte{7}, 128<<10)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), text, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.bin"), bin, 0o644); err != nil {
		t.Fatal(err)
	}

	batch, err := a.xfer.Send(b.n.Host.ID().String(), []string{root})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "folder received", func() bool {
		rows, _ := b.n.Store.BatchTransfers(batch)
		if len(rows) != 2 {
			return false
		}
		for _, r := range rows {
			if r.Status != "done" {
				return false
			}
		}
		return true
	})

	rows, _ := b.n.Store.BatchTransfers(batch)
	want := map[string][]byte{"album/a.txt": text, "album/sub/b.bin": bin}
	if len(rows) != len(want) {
		t.Fatalf("received %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for _, r := range rows {
		w, ok := want[r.RelPath]
		if !ok {
			t.Fatalf("unexpected rel_path %q", r.RelPath)
		}
		if !strings.HasPrefix(r.LocalPath, b.dl) {
			t.Fatalf("%q landed outside the download dir: %q", r.RelPath, r.LocalPath)
		}
		got, err := os.ReadFile(r.LocalPath)
		if err != nil || !bytes.Equal(got, w) {
			t.Fatalf("%q content mismatch (err=%v)", r.RelPath, err)
		}
		// 结构和相对路径一致：album/sub/b.bin 落在 <download>/album/sub/b.bin
		if wantPath := filepath.Join(b.dl, filepath.FromSlash(r.RelPath)); wantPath != r.LocalPath {
			t.Fatalf("local path = %q, want %q", r.LocalPath, wantPath)
		}
	}

	// 发送端的历史也带 rel_path，便于界面展示
	waitFor(t, "sender rows done", func() bool {
		out, _ := a.n.Store.BatchTransfers(batch)
		if len(out) != 2 {
			return false
		}
		for _, r := range out {
			if r.Status != "done" || r.RelPath == "" {
				return false
			}
		}
		return true
	})
}
