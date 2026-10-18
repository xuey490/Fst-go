//go:build windows

package proxy

import (
	"errors"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// TestDecodeConsoleGBK 验证中文系统 netsh 的 GBK 输出能被正确解码，UTF-8 输出保持不变。
func TestDecodeConsoleGBK(t *testing.T) {
	text := "对象已存在。\r\n"
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeConsole(gbk); got != "对象已存在。" {
		t.Fatalf("GBK decode = %q", got)
	}
	if got := decodeConsole([]byte("The object already exists.\r\n")); got != "The object already exists." {
		t.Fatalf("UTF-8 decode = %q", got)
	}
}

// TestRouteExistsErr 验证中英文「已存在」都能被识别（解码后不再依赖乱码文本）。
func TestRouteExistsErr(t *testing.T) {
	for _, s := range []string{
		"exit status 1: 对象已存在。",
		"exit status 1: 路由已存在。",
		"exit status 1: The object already exists.",
	} {
		if !routeExistsErr(errors.New(s)) {
			t.Fatalf("not detected: %q", s)
		}
	}
	if routeExistsErr(errors.New("exit status 1: 参数错误")) {
		t.Fatal("unrelated error treated as exists")
	}
}
