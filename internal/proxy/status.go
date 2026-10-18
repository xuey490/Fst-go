package proxy

import "sync/atomic"

// Status is the user-facing proxy state, published by the app on startup and
// after the connectivity probe, consumed by the settings UI and the Wails
// event proxy:status.
type Status struct {
	Enabled   bool   `json:"enabled"`              // proxy.enable in config.yaml
	Running   bool   `json:"running"`              // TUN (and SOCKS5) up
	Mode      string `json:"mode,omitempty"`       // split | global
	ExitPeer  string `json:"exit_peer,omitempty"`  // exit relay PeerID
	SocksAddr string `json:"socks_addr,omitempty"` // local SOCKS5 listen addr, e.g. 127.0.0.1:57317
	Probe     string `json:"probe,omitempty"`      // "" unknown | "ok" | error text
	Error     string `json:"error,omitempty"`      // startup failure reason
}

var statusPtr atomic.Pointer[Status]

// SetStatus publishes a snapshot (nil clears).
func SetStatus(s *Status) { statusPtr.Store(s) }

// Current returns the latest snapshot or a disabled default.
func Current() *Status {
	if s := statusPtr.Load(); s != nil {
		cp := *s
		return &cp
	}
	return &Status{}
}
