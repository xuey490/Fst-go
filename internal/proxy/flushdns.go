package proxy

import "log/slog"

// FlushSystemDNSCache drops cached answers (including stale fake-IPs from a
// previous run) so the first connection after starting the TUN does not hit
// "unknown fake-IP". Best effort: failures are logged, never fatal.
func FlushSystemDNSCache() {
	if err := flushDNSCache(); err != nil {
		slog.Warn("could not flush system DNS cache", "err", err)
	} else {
		slog.Info("system DNS cache flushed")
	}
}
