package bootstrap

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestNetConfigRoundTripAndLegacy 测 config.yaml 的读写回路：多候选路径选择、
// 保存时清洗（去空白/去重）、清空回写、损坏文件报错、只读目录兜底以及旧库键迁移。
func TestNetConfigRoundTripAndLegacy(t *testing.T) {
	dir := t.TempDir()
	exe, user := filepath.Join(dir, "exe", configName), filepath.Join(dir, "user", configName)
	paths := []string{exe, filepath.Join(dir, "cwd", configName), user}

	if cfg, p, err := LoadNetConfig(paths); err != nil || p != "" || cfg.EnableRelay {
		t.Fatalf("empty: %+v %q %v", cfg, p, err)
	}
	// first save goes next to the exe, with duplicates and blanks removed
	got, err := SaveNetConfig("", NetConfig{
		EnableRelay: true,
		Relays:      []string{" /a/p2p/X ", "/a/p2p/X", "", "/b/p2p/Y", "/c/p2p/Z"},
		Proxy: ProxyConfig{Enable: true, Mode: "split", Exit: "/a/p2p/X",
			Routes:  []string{" 203.0.113.0/24 ", "203.0.113.0/24", "198.51.100.0/24", " 192.0.2.0/24 "},
			Domains: []string{" google.com ", "google.com", "youtube.com", "*.example.org"}},
	}, paths)
	if err != nil || got != exe {
		t.Fatalf("save: %q %v", got, err)
	}
	cfg, p, err := LoadNetConfig(paths)
	if err != nil || p != exe || !cfg.EnableRelay || len(cfg.Relays) != 3 || cfg.Relays[2] != "/c/p2p/Z" ||
		!cfg.Proxy.Enable || cfg.Proxy.Mode != "split" {
		t.Fatalf("load: %+v %q %v", cfg, p, err)
	}
	// every distinct entry survives the round trip, in order. Domain
	// normalization ("*." stripping, lowercasing) happens in the API layer
	// before saving, so this layer stores what it is handed.
	wantRoutes := []string{"203.0.113.0/24", "198.51.100.0/24", "192.0.2.0/24"}
	wantDomains := []string{"google.com", "youtube.com", "*.example.org"}
	if !slices.Equal(cfg.Proxy.Routes, wantRoutes) || !slices.Equal(cfg.Proxy.Domains, wantDomains) {
		t.Fatalf("multi-entry lost: routes=%v domains=%v", cfg.Proxy.Routes, cfg.Proxy.Domains)
	}
	// saving back to the loaded file keeps it in place; an empty list is written as [] and read back empty
	if _, err := SaveNetConfig(p, NetConfig{}, paths); err != nil {
		t.Fatal(err)
	}
	if cfg, _, _ := LoadNetConfig(paths); cfg.EnableRelay || len(cfg.Relays) != 0 {
		t.Fatalf("cleared: %+v", cfg)
	}
	// a broken file is an error, not a silent reset
	os.WriteFile(exe, []byte("relays: [unclosed"), 0o644)
	if _, _, err := LoadNetConfig(paths); err == nil {
		t.Fatal("broken yaml should be reported")
	}
	// unwritable exe dir (a file where the dir should be) falls back to the user dir
	os.Remove(exe)
	os.RemoveAll(filepath.Dir(exe))
	os.WriteFile(filepath.Dir(exe), []byte("x"), 0o644)
	if got, err := SaveNetConfig("", NetConfig{EnableRelay: true}, paths); err != nil || got != user {
		t.Fatalf("fallback: %q %v", got, err)
	}

	set := map[string]string{"wan_enabled": "1", "relays": "/a/p2p/X\n/a/p2p/X /b/p2p/Y"}
	lc, ok := LegacyNetConfig(func(k, d string) string {
		if v, in := set[k]; in {
			return v
		}
		return d
	})
	if !ok || !lc.EnableRelay || len(lc.Relays) != 2 {
		t.Fatalf("legacy: %+v %v", lc, ok)
	}
	if _, ok := LegacyNetConfig(func(_, d string) string { return d }); ok {
		t.Fatal("no legacy values must report ok=false")
	}
}
