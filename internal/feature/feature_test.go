package feature

import (
	"errors"
	"testing"

	"fst/internal/store"
)

// TestWANUnlockedDefaults 验证默认关闭、nil store 只认环境变量。
func TestWANUnlockedDefaults(t *testing.T) {
	t.Setenv("FST_WAN", "")
	if WANUnlocked(nil) {
		t.Fatal("nil store without env must stay locked")
	}

	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if WANUnlocked(s) {
		t.Fatal("fresh store must default to locked")
	}
}

// TestWANUnlockedPersisted 验证解锁标记持久化在 settings，锁定可撤销。
func TestWANUnlockedPersisted(t *testing.T) {
	t.Setenv("FST_WAN", "")
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := SetWANUnlocked(s, true); err != nil {
		t.Fatal(err)
	}
	if !WANUnlocked(s) || s.Setting(SettingKey, "0") != "1" {
		t.Fatal("unlock flag not persisted")
	}
	if err := SetWANUnlocked(s, false); err != nil {
		t.Fatal(err)
	}
	if WANUnlocked(s) {
		t.Fatal("lock did not take effect")
	}
}

// TestEnvOverridesSettings 验证环境变量优先于本机标记，且此时拒绝屏蔽。
func TestEnvOverridesSettings(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := SetWANUnlocked(s, false); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FST_WAN", "1")
	if !WANUnlocked(s) {
		t.Fatal("env must override stored flag")
	}
	if err := SetWANUnlocked(s, false); !errors.Is(err, ErrForced) {
		t.Fatalf("lock while forced = %v, want ErrForced", err)
	}
}
