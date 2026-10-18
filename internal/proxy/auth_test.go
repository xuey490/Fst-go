package proxy

import (
	"strings"
	"testing"
	"time"
)

// TestTokenSet 验证令牌解析与常量时间校验：命中、过期、未知令牌与重复行。
func TestTokenSet(t *testing.T) {
	set, err := ParseTokens(strings.NewReader("good\nold:2020-01-01T00:00:00Z\n# comment\n"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if !set.Valid("good", now) || set.Valid("bad", now) {
		t.Fatal("token validation mismatch")
	}
	if set.Valid("old", now) {
		t.Fatal("expired token must be rejected")
	}
	if set.Valid("", now) {
		t.Fatal("empty token must be rejected")
	}
	if _, err := ParseTokens(strings.NewReader("dup\ndup\n")); err == nil {
		t.Fatal("duplicate tokens must be rejected")
	}
	if _, err := ParseTokens(strings.NewReader("x:not-a-time\n")); err == nil {
		t.Fatal("invalid expiry must be rejected")
	}
}

// TestExitGroupSlots 验证同组免令牌判断与按组并发槽位（0 表示不限）。
func TestExitGroupSlots(t *testing.T) {
	x := &Exit{opt: ExitOptions{ExitGroup: "office", SlotsPerGroup: 1}, groupSlots: map[string]chan struct{}{}}
	if !x.groupExempt("office") || x.groupExempt("home") || x.groupExempt("") {
		t.Fatal("group exemption mismatch")
	}
	release, ok := x.acquireGroup("office")
	if !ok {
		t.Fatal("first slot must succeed")
	}
	if _, ok := x.acquireGroup("office"); ok {
		t.Fatal("second slot must be rejected when the cap is 1")
	}
	release()
	if release2, ok := x.acquireGroup("office"); !ok {
		t.Fatal("slot must be reusable after release")
	} else {
		release2()
	}
	y := &Exit{opt: ExitOptions{}, groupSlots: map[string]chan struct{}{}}
	if _, ok := y.acquireGroup("anything"); !ok {
		t.Fatal("zero SlotsPerGroup means unlimited")
	}
}

// TestValidatedTargetIPv6 验证 IPv6 字面量、地址族匹配与禁止范围。
func TestValidatedTargetIPv6(t *testing.T) {
	req := proxyReq("2001:db8::1")
	req.Family = "ip6"
	got, err := validatedTarget(req)
	if err != nil || got != "[2001:db8::1]:443" {
		t.Fatalf("public ipv6: %q %v", got, err)
	}
	req.Family = "ip4"
	if _, err := validatedTarget(req); err == nil {
		t.Fatal("ip6 literal with ip4 family must be rejected")
	}
	for _, ip := range []string{"::1", "fc00::1", "fe80::1", "ff02::1", "2001:db8::1"} {
		r := proxyReq(ip)
		r.Family = "ip4"
		if _, err := validatedTarget(r); err == nil {
			t.Fatalf("%s must be rejected", ip)
		}
	}
	for _, ip := range []string{"::1", "fc00::1", "fe80::1", "ff02::1"} {
		r := proxyReq(ip)
		r.Family = "ip6"
		if _, err := validatedTarget(r); err == nil {
			t.Fatalf("%s must be rejected for ip6", ip)
		}
	}
}
