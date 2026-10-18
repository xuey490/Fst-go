package main

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

// openInstance decides which data dir / port / ordinal this process uses and opens its database.
//
//   - FST_DATA_DIR (+ FST_PORT) override everything: tests and manual multi-instance runs.
//   - setting single_instance=1 (default, read from the primary dir): one process, fixed port.
//   - single_instance=0: the first free port from defaultPort picks the ordinal n; n>1 gets its own
//     data dir "FST-n" (own identity key and database, so its PeerID differs) and port defaultPort+n-1.
func openInstance() (db *store.Store, dir string, port, idx int, single bool) {
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
	port = defaultPort
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
		dir, port = fmt.Sprintf("%s-%d", dir, idx), defaultPort+idx-1
		db = open(dir)
	}
	return db, dir, port, idx, false
}

// freeSlot returns the lowest ordinal whose port no other instance is listening on.
// Dial instead of Listen: libp2p binds with SO_REUSEADDR/PORT, and on Windows a plain Listen then
// succeeds on a port that is already taken.
// ponytail: probe-then-bind leaves a small race window; the node falls back to a random port if
// the real bind loses it. Ceiling: 16 instances, then everyone shares ordinal 1.
func freeSlot() int {
	for i := 1; i <= 16; i++ {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", defaultPort+i-1), 500*time.Millisecond)
		if err != nil {
			return i
		}
		c.Close()
	}
	return 1
}
