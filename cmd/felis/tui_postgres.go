package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"felis.lolicon.best/internal/store"
)

// checkPostgres proves the configured database is reachable and accepts a
// connection. Host bootstrap provisions PostgreSQL, so in the normal setup flow
// this succeeds immediately; the preflight stage uses it to fail fast otherwise.
func checkPostgres(dbURL string) error {
	cfg, err := parseDBURL(dbURL)
	if err != nil {
		return fmt.Errorf("invalid database URL: %w", err)
	}
	conn, err := net.DialTimeout("tcp", cfg.addr, 2*time.Second)
	if err != nil {
		return fmt.Errorf("cannot reach PostgreSQL at %s: %w", cfg.addr, err)
	}
	conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	drv, err := store.Open(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	drv.Close()
	return nil
}

type dbCfg struct {
	addr string
	user string
	pass string
	db   string
}

func parseDBURL(url string) (dbCfg, error) {
	// Simple parser for postgres://user:pass@host:port/db?options
	s := strings.TrimPrefix(url, "postgres://")
	s = strings.TrimPrefix(s, "postgresql://")
	parts := strings.SplitN(s, "@", 2)
	if len(parts) != 2 {
		return dbCfg{}, fmt.Errorf("malformed URL")
	}
	auth := strings.SplitN(parts[0], ":", 2)
	rest := strings.SplitN(parts[1], "/", 2)
	if len(rest) < 2 {
		return dbCfg{}, fmt.Errorf("malformed URL: no database")
	}
	hostport := rest[0]
	dbname := strings.SplitN(rest[1], "?", 2)[0]
	if !strings.Contains(hostport, ":") {
		hostport += ":5432"
	}
	return dbCfg{
		addr: hostport,
		user: auth[0],
		pass: func() string {
			if len(auth) > 1 {
				return auth[1]
			}
			return ""
		}(),
		db: dbname,
	}, nil
}

func countMigrations(dbURL string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	drv, err := store.Open(ctx, dbURL)
	if err != nil {
		return 0, err
	}
	defer drv.Close()
	if err := drv.EnsureVersionTable(ctx); err != nil {
		return 0, err
	}
	applied, err := drv.AppliedVersions(ctx)
	if err != nil {
		return 0, err
	}
	return len(applied), nil
}
