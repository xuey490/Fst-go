package node

import "testing"

func TestFstRelayResources(t *testing.T) {
	r := fstRelayResources()
	if r.Limit != nil {
		t.Fatal("per-circuit data/time limit must stay off so files can pass through the relay")
	}
	if r.MaxReservations < 4096 || r.MaxCircuits < 128 || r.MaxReservationsPerIP < 32 {
		t.Fatalf("reservation caps too low: %+v", r)
	}
}
