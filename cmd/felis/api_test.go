package main

import (
	"net/http"
	"testing"
)

// TestNewAPIServerSetsHardenedTimeouts pins the gosec-G112 hardening on every
// felis-api listener: the shared factory must bound the header and idle phases
// (Slowloris + idle-connection exhaustion) while leaving WriteTimeout UNSET, because
// the external and https faces stream Server-Sent Events for the life of a client's
// console/build-log attachment and a WriteTimeout would sever a healthy long stream.
func TestNewAPIServerSetsHardenedTimeouts(t *testing.T) {
	srv := newAPIServer(":0", http.NewServeMux())

	if srv.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want a positive Slowloris bound", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v, want a positive idle-connection bound", srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (unset) so long-lived SSE streams are not severed", srv.WriteTimeout)
	}
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0 (unset) so a slow SSE attach is not capped", srv.ReadTimeout)
	}
}
