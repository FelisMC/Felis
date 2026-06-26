package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/store"
)

// cmdMigrate implements `felis migrate up`: load config, open the database, and
// apply every pending embedded migration under the advisory lock (spec §6).
func cmdMigrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.Arg(0) != "up" {
		fmt.Fprintln(stderr, "usage: felis migrate up [-config path]")
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis migrate: %v\n", err)
		return 1
	}

	ctx := context.Background()
	drv, err := store.Open(ctx, cfg.Database.URL)
	if err != nil {
		fmt.Fprintf(stderr, "felis migrate: open database: %v\n", err)
		return 1
	}
	defer drv.Close()

	migrations, err := store.LoadMigrations()
	if err != nil {
		fmt.Fprintf(stderr, "felis migrate: load migrations: %v\n", err)
		return 1
	}

	applied, err := store.Up(ctx, drv, migrations)
	if err != nil {
		fmt.Fprintf(stderr, "felis migrate: %v\n", err)
		return 1
	}
	if len(applied) == 0 {
		fmt.Fprintln(stdout, "felis migrate: database already up to date")
	} else {
		fmt.Fprintf(stdout, "felis migrate: applied %d migration(s): %v\n", len(applied), applied)
	}
	return 0
}
