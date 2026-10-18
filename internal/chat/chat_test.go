package chat

import (
	"strings"
	"testing"
	"time"
)

// TestRetryDelay 验证离线重投退避：15s 起步、指数增长、10 分钟封顶。
func TestRetryDelay(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, 15 * time.Second},
		{1, 15 * time.Second},
		{2, 30 * time.Second},
		{3, time.Minute},
		{7, 16 * time.Minute}, // 超过上限时被封顶
	}
	for _, c := range cases {
		got := retryDelay(c.attempts)
		if c.attempts >= 7 {
			if got != 10*time.Minute {
				t.Fatalf("attempts %d: got %v, want cap 10m", c.attempts, got)
			}
			continue
		}
		if got != c.want {
			t.Fatalf("attempts %d: got %v, want %v", c.attempts, got, c.want)
		}
	}
}

// TestPrepareMessage 验证消息校验：文本非空、图片/文件需附件、32000 字符上限与预览占位。
func TestPrepareMessage(t *testing.T) {
	cases := []struct {
		name    string
		content string
		kind    string
		attach  []string
		want    string
		wantK   string
		err     bool
	}{
		{name: "text", content: " hi ", kind: "", want: "hi", wantK: "text"},
		{name: "empty text", content: "  ", kind: "text", err: true},
		{name: "image placeholder", kind: "image", attach: []string{"t1"}, want: "图片", wantK: "image"},
		{name: "file placeholder", kind: "file", attach: []string{"t1"}, want: "文件", wantK: "file"},
		{name: "image without attach", kind: "image", err: true},
		{name: "too many attach", content: "x", kind: "image", attach: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}, err: true},
		{name: "max length", content: strings.Repeat("好", maxLen), want: strings.Repeat("好", maxLen), wantK: "text"},
		{name: "over max length", content: strings.Repeat("好", maxLen+1), err: true},
		{name: "unknown kind falls back", content: "x", kind: "voice", want: "x", wantK: "text"},
	}
	for _, c := range cases {
		got, kind, err := prepareMessage(c.content, c.kind, c.attach)
		if c.err {
			if err == nil {
				t.Fatalf("%s: accepted (%q, %q)", c.name, got, kind)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want || kind != c.wantK {
			t.Fatalf("%s: got (%q, %q), want (%q, %q)", c.name, got, kind, c.want, c.wantK)
		}
	}
}
