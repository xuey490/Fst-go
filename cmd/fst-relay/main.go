// Command fst-relay is a headless FST node for a server with a public IP: it runs the circuit relay
// (unlimited), the private FST DHT and nothing else (no UI, no mDNS). Point clients' "中继地址" at it.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"fst/internal/node"
	fstproxy "fst/internal/proxy"
	"fst/internal/store"

	"github.com/libp2p/go-libp2p/core/peer"
)

func main() {
	dir := flag.String("data", "/var/lib/fst-relay", "data dir (identity.key keeps the PeerID stable)")
	port := flag.Int("port", 8787, "TCP + UDP(QUIC) listen port; open both in the firewall")
	name := flag.String("name", "fst-relay", "device name shown to other FST nodes")
	exit := flag.Bool("exit", false, "enable the outbound TCP/UDP proxy (requires -exit-allow)")
	exitAllow := flag.String("exit-allow", "", "file containing one allowed terminal PeerID per line")
	exitToken := flag.String("exit-token", "", "file of exit tokens (token[:expiry] per line; expiry = RFC3339 or unix seconds)")
	exitGroup := flag.String("exit-group", "", "only terminals of this relay group may use the exit")
	exitGroupSlots := flag.Int("exit-group-slots", 0, "max concurrent exit sessions per group (0 = only the global cap)")
	group := flag.String("group", "", "relay group key: only terminals presenting the same key are introduced to each other")
	groupFile := flag.String("group-file", "", "relay group table file (name:key[:expiry] per line); mutually exclusive with -group")
	groupMax := flag.Int("group-max-terminals", 0, "soft cap per group: no directory/arrival notices beyond it (0 = unlimited)")
	flag.Parse()

	if *group != "" && *groupFile != "" {
		log.Fatal("-group and -group-file are mutually exclusive")
	}
	var groups []node.GroupSpec
	if *groupFile != "" {
		specs, err := node.ParseGroupFile(*groupFile)
		if err != nil {
			log.Fatalf("read group file: %v", err)
		}
		groups = specs
	}

	if err := os.MkdirAll(*dir, 0o700); err != nil {
		log.Fatal(err)
	}
	db, err := store.Open(*dir)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	db.SetSetting("nickname", *name)

	n, err := node.New(node.Config{
		DataDir: *dir, Port: *port, Store: db, DisableMDNS: true, WAN: true, ForcePublic: true,
		Directory: true, Group: *group, Groups: groups, GroupMaxTerminals: *groupMax,
		GroupFile: *groupFile,
	})
	if err != nil {
		log.Fatalf("start node: %v", err)
	}
	defer n.Close()
	if *exit {
		allowed, err := readAllowlist(*exitAllow)
		if err != nil {
			log.Fatalf("read exit allowlist: %v", err)
		}
		var tokens *fstproxy.TokenSet
		if *exitToken != "" {
			tokens, err = fstproxy.ParseTokenFile(*exitToken)
			if err != nil {
				log.Fatalf("read exit tokens: %v", err)
			}
			if tokens.Len() == 0 {
				log.Fatal("exit token file is empty; refusing to start a token-gated exit")
			}
		}
		opt := fstproxy.ExitOptions{
			Allow:         func(id peer.ID) bool { return allowed[id] },
			ExitGroup:     *exitGroup,
			SlotsPerGroup: *exitGroupSlots,
			GroupOf:       n.GroupOf,
		}
		if tokens != nil {
			opt.Token = func(text string) bool { return tokens.Valid(text, time.Now()) }
		}
		x := fstproxy.ServeExit(n.Host, opt)
		defer x.Close()
		log.Printf("proxy exit enabled for %d terminal(s), %d token(s), group=%q, group-slots=%d",
			len(allowed), tokens.Len(), *exitGroup, *exitGroupSlots)
	}

	switch {
	case *group != "":
		log.Printf("relay group isolation enabled with the legacy single key (key not printed)")
	case len(groups) > 0:
		log.Printf("relay group isolation enabled with %d group(s) (keys not printed)", len(groups))
	}
	fmt.Printf("fst-relay started, PeerID %s\n", n.Host.ID())
	fmt.Printf("clients: put this in Settings > Relay addresses:\n  /ip4/<this server's public IP>/tcp/%d/p2p/%s\n", *port, n.Host.ID())
	for _, a := range n.Host.Addrs() {
		fmt.Printf("  listening %s\n", a)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}

func readAllowlist(path string) (map[peer.ID]bool, error) {
	out := map[peer.ID]bool{}
	if path == "" {
		return out, nil // fail closed: -exit alone authorises nobody
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.SplitN(sc.Text(), "#", 2)[0])
		if line == "" {
			continue
		}
		id, err := peer.Decode(line)
		if err != nil {
			return nil, fmt.Errorf("invalid PeerID %q: %w", line, err)
		}
		out[id] = true
	}
	return out, sc.Err()
}
