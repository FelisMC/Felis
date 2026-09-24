package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrSchemaNewer marks a database that a newer Felis has migrated: it records versions
// this build does not embed. Up only rolls forward, so running this build against it
// would read and write tables whose shape it was never written for.
var ErrSchemaNewer = errors.New("database schema is newer than this felis build")

// ErrSchemaBehind marks a database that still lacks migrations this build embeds.
var ErrSchemaBehind = errors.New("database schema is behind this felis build")

// Schema is how a database's recorded migrations line up with the ones embedded in
// this build. Comparing the two sets, rather than counting, is what tells "behind"
// from "migrated by a newer release": both can have the same number of rows.
type Schema struct {
	Applied int   // embedded migrations the database has
	Total   int   // embedded migrations
	Latest  int   // highest embedded version
	Pending []int // embedded, not yet applied, ascending
	Unknown []int // applied, but not embedded here, ascending
}

// CompareSchema lines applied up with migrations.
func CompareSchema(applied map[int]struct{}, migrations []Migration) Schema {
	s := Schema{Total: len(migrations)}
	known := make(map[int]struct{}, len(migrations))
	for _, m := range migrations {
		known[m.Version] = struct{}{}
		s.Latest = max(s.Latest, m.Version)
		if _, ok := applied[m.Version]; ok {
			s.Applied++
		} else {
			s.Pending = append(s.Pending, m.Version)
		}
	}
	for v := range applied {
		if _, ok := known[v]; !ok {
			s.Unknown = append(s.Unknown, v)
		}
	}
	sort.Ints(s.Pending)
	sort.Ints(s.Unknown)
	return s
}

// Newer returns ErrSchemaNewer, with the versions and what to do, when a newer Felis
// migrated this database; nil otherwise.
func (s Schema) Newer() error {
	if len(s.Unknown) == 0 {
		return nil
	}
	return fmt.Errorf("%w: it records migration %s, and this build knows %s. Run the Felis release that migrated it, or restore the pre-migrate snapshot it took (felis db restore)",
		ErrSchemaNewer, versionList(s.Unknown), knownRange(s.Latest))
}

// Err is Newer, else ErrSchemaBehind when migrations are pending: the check a server
// makes before it serves anything from the database.
func (s Schema) Err() error {
	if err := s.Newer(); err != nil {
		return err
	}
	if len(s.Pending) > 0 {
		return fmt.Errorf("%w: migration %s not applied yet. Run `felis migrate up` (the installer does), then start this again",
			ErrSchemaBehind, versionList(s.Pending))
	}
	return nil
}

// ReadSchema compares what the database records with the embedded migrations.
func ReadSchema(ctx context.Context, d Driver) (Schema, error) {
	migrations, err := LoadMigrations()
	if err != nil {
		return Schema{}, err
	}
	applied, err := d.AppliedVersions(ctx)
	if err != nil {
		return Schema{}, fmt.Errorf("read applied migrations: %w", err)
	}
	return CompareSchema(applied, migrations), nil
}

// CheckSchema fails unless the database carries exactly the migrations this build
// embeds.
func CheckSchema(ctx context.Context, d Driver) error {
	s, err := ReadSchema(ctx, d)
	if err != nil {
		return err
	}
	return s.Err()
}

func versionList(vs []int) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = fmt.Sprintf("%04d", v)
	}
	return strings.Join(parts, ", ")
}

func knownRange(latest int) string {
	return fmt.Sprintf("migrations up to %04d", latest)
}
