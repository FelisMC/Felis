package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrSchemaNewer indicates applied versions absent from this build.
var ErrSchemaNewer = errors.New("database schema is newer than this felis build")

// ErrSchemaBehind indicates pending embedded migrations.
var ErrSchemaBehind = errors.New("database schema is behind this felis build")

// Schema compares migration sets; equal counts can still contain different versions.
type Schema struct {
	Applied int   // embedded migrations the database has
	Total   int   // embedded migrations
	Latest  int   // highest embedded version
	Pending []int // embedded, not yet applied, ascending
	Unknown []int // applied, but not embedded here, ascending
}

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

func (s Schema) Newer() error {
	if len(s.Unknown) == 0 {
		return nil
	}
	return fmt.Errorf("%w: it records migration %s, and this build knows %s. Run the Felis release that migrated it, or restore the pre-migrate snapshot it took (felis db restore)",
		ErrSchemaNewer, versionList(s.Unknown), knownRange(s.Latest))
}

// Err rejects unknown applied versions before reporting pending migrations.
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
