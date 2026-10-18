// Package store applies embedded schema migrations under a PostgreSQL advisory lock.
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// AdvisoryLockKey serializes migrations across replicas.
const AdvisoryLockKey int64 = 0x66656c6973

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migration is a single ordered schema step loaded from the embedded FS.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

type Driver interface {
	// Lock blocks until the migration advisory lock is acquired.
	Lock(ctx context.Context) error
	Unlock(ctx context.Context) error
	EnsureVersionTable(ctx context.Context) error
	AppliedVersions(ctx context.Context) (map[int]struct{}, error)
	// Apply runs a migration and records its version atomically.
	Apply(ctx context.Context, m Migration) error
}

// LoadMigrations returns migrations in ascending order, rejecting duplicate versions.
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var ms []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		data, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", e.Name(), err)
		}
		if strings.TrimSpace(string(data)) == "" {
			return nil, fmt.Errorf("migration %q is empty", e.Name())
		}
		ms = append(ms, Migration{Version: version, Name: name, SQL: string(data)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Version < ms[j].Version })
	for i := 1; i < len(ms); i++ {
		if ms[i].Version == ms[i-1].Version {
			return nil, fmt.Errorf("duplicate migration version %d (%s, %s)", ms[i].Version, ms[i-1].Name, ms[i].Name)
		}
	}
	if len(ms) == 0 {
		return nil, fmt.Errorf("no migrations found")
	}
	return ms, nil
}

// Up applies pending migrations under the advisory lock and rejects unknown applied versions.
func Up(ctx context.Context, d Driver, migrations []Migration) (applied []int, err error) {
	if err := d.Lock(ctx); err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		if uerr := d.Unlock(ctx); uerr != nil && err == nil {
			err = fmt.Errorf("release migration lock: %w", uerr)
		}
	}()

	if err := d.EnsureVersionTable(ctx); err != nil {
		return nil, fmt.Errorf("ensure version table: %w", err)
	}
	done, err := d.AppliedVersions(ctx)
	if err != nil {
		return nil, fmt.Errorf("read applied versions: %w", err)
	}
	if err := CompareSchema(done, migrations).Newer(); err != nil {
		return nil, err
	}

	ordered := append([]Migration(nil), migrations...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Version < ordered[j].Version })
	for _, m := range ordered {
		if _, ok := done[m.Version]; ok {
			continue
		}
		if err := d.Apply(ctx, m); err != nil {
			return applied, fmt.Errorf("apply migration %04d_%s: %w", m.Version, m.Name, err)
		}
		applied = append(applied, m.Version)
	}
	return applied, nil
}

func parseMigrationName(filename string) (int, string, error) {
	base := strings.TrimSuffix(filename, ".sql")
	idx := strings.IndexByte(base, '_')
	if idx <= 0 {
		return 0, "", fmt.Errorf("migration %q must be named NNNN_name.sql", filename)
	}
	version, err := strconv.Atoi(base[:idx])
	if err != nil {
		return 0, "", fmt.Errorf("migration %q has a non-numeric version: %w", filename, err)
	}
	name := base[idx+1:]
	if name == "" {
		return 0, "", fmt.Errorf("migration %q is missing a name", filename)
	}
	return version, name, nil
}
