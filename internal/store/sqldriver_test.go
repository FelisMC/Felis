package store

import "testing"

func TestConnConfigFillsSessionDefaults(t *testing.T) {
	cfg, err := connConfig("postgres://felis:pw@127.0.0.1:5432/felis?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"statement_timeout":                   "15s",
		"idle_in_transaction_session_timeout": "60s",
	} {
		if got := cfg.RuntimeParams[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestConnConfigKeepsTheDSNsOwnLimits(t *testing.T) {
	cfg, err := connConfig("postgres://felis:pw@127.0.0.1:5432/felis?statement_timeout=2500")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RuntimeParams["statement_timeout"]; got != "2500" {
		t.Errorf("statement_timeout = %q, want the DSN's 2500", got)
	}
	if got := cfg.RuntimeParams["idle_in_transaction_session_timeout"]; got != "60s" {
		t.Errorf("idle_in_transaction_session_timeout = %q, want the default 60s", got)
	}

	// Set through options=-c, it is left out: the server applies startup
	// parameters after options, so a default sent beside it would win.
	cfg, err = connConfig("postgres://felis:pw@127.0.0.1:5432/felis?options=-c%20statement_timeout%3D0")
	if err != nil {
		t.Fatal(err)
	}
	if v, set := cfg.RuntimeParams["statement_timeout"]; set {
		t.Errorf("statement_timeout = %q beside options=-c, want it unset", v)
	}
}

func TestPoolIsBounded(t *testing.T) {
	db, err := newPool("postgres://felis:pw@127.0.0.1:1/felis")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := db.Stats().MaxOpenConnections; got != MaxOpenConns || got == 0 {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, MaxOpenConns)
	}
}
