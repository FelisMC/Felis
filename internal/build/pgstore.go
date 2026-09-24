package build

import (
	"context"
	"database/sql"
	"time"
)

// PGStore is the production Store backed by Postgres (spec §6, §16). It writes
// the two tables of the build subsystem — image_builds and image_whitelist —
// and is the *only* component that holds database credentials: the build Pod
// never does (the weak-SA red line). The SQL here is exercised by integration
// tests against a live database, not the hermetic build_test.go suite. Every
// statement is a narrow operation; there is no generic UPDATE escape hatch.
type PGStore struct {
	db *sql.DB
}

// NewPGStore wraps an existing pool (from store.PostgresDriver.DB()).
func NewPGStore(db *sql.DB) *PGStore { return &PGStore{db: db} }

func (s *PGStore) CreateBuild(ctx context.Context, b *Build) error {
	const q = `INSERT INTO image_builds
		(id, image_ref, status, dockerfile, context_ref, base_image, requested_by, created_at, context_digest)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7, $8, NULLIF($9, ''))`
	_, err := s.db.ExecContext(ctx, q,
		b.ID, b.ImageRef, string(b.Status), b.Dockerfile, b.ContextRef, b.BaseImage,
		b.RequestedBy, b.CreatedAt, b.ContextDigest)
	return err
}

func (s *PGStore) GetBuild(ctx context.Context, id string) (*Build, error) {
	const q = `SELECT id, image_ref, status, dockerfile, context_ref, base_image,
			requested_by, job_name, log_ref, error, created_at, finished_at, context_digest
		FROM image_builds WHERE id = $1`
	return s.scanBuild(s.db.QueryRowContext(ctx, q, id))
}

func (s *PGStore) scanBuild(row *sql.Row) (*Build, error) {
	var (
		b                                   Build
		status                              string
		ctxRef, base, jobName, logRef, eMsg sql.NullString
		finished                            sql.NullTime
		digest                              sql.NullString
	)
	switch err := row.Scan(&b.ID, &b.ImageRef, &status, &b.Dockerfile, &ctxRef, &base,
		&b.RequestedBy, &jobName, &logRef, &eMsg, &b.CreatedAt, &finished, &digest); {
	case err == sql.ErrNoRows:
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	b.Status = Status(status)
	b.ContextRef = ctxRef.String
	b.BaseImage = base.String
	b.JobName = jobName.String
	b.LogRef = logRef.String
	b.Error = eMsg.String
	b.ContextDigest = digest.String
	if finished.Valid {
		t := finished.Time
		b.FinishedAt = &t
	}
	return &b, nil
}

func (s *PGStore) SetBuildJob(ctx context.Context, id, jobName string) error {
	const q = `UPDATE image_builds SET job_name = $2, status = 'building'
		WHERE id = $1 AND status = 'pending'`
	res, err := s.db.ExecContext(ctx, q, id, jobName)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PGStore) FinishBuild(ctx context.Context, id string, status Status, errMsg string, at time.Time) error {
	const q = `UPDATE image_builds SET status = $2, error = NULLIF($3, ''), finished_at = $4
		WHERE id = $1`
	res, err := s.db.ExecContext(ctx, q, id, string(status), errMsg, at)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PGStore) ListUnfinishedBuilds(ctx context.Context) ([]Build, error) {
	const q = `SELECT id, image_ref, status, dockerfile, context_ref, base_image,
			requested_by, job_name, log_ref, error, created_at, finished_at, context_digest
		FROM image_builds WHERE status IN ('pending', 'building') ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Build
	for rows.Next() {
		var (
			b                                   Build
			status                              string
			ctxRef, base, jobName, logRef, eMsg sql.NullString
			finished                            sql.NullTime
			digest                              sql.NullString
		)
		if err := rows.Scan(&b.ID, &b.ImageRef, &status, &b.Dockerfile, &ctxRef, &base,
			&b.RequestedBy, &jobName, &logRef, &eMsg, &b.CreatedAt, &finished, &digest); err != nil {
			return nil, err
		}
		b.Status = Status(status)
		b.ContextRef = ctxRef.String
		b.BaseImage = base.String
		b.JobName = jobName.String
		b.LogRef = logRef.String
		b.Error = eMsg.String
		b.ContextDigest = digest.String
		if finished.Valid {
			t := finished.Time
			b.FinishedAt = &t
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// AdmitBuiltImage upserts the whitelist row on scan-gate success. ON CONFLICT
// re-enables and re-stamps a previously-removed or superseded ref, so a rebuild
// of the same tag re-admits it (spec §16).
//
// source is the ONE column the conflict path does not overwrite unconditionally:
// a 'recommended' row is platform curation (0018), while every other field here
// describes the build that just succeeded and must win. Rebuilding a curated tag
// is the expected way to patch it, and that rebuild arrives through this exact
// path — so a blind `SET source = 'built'` would silently demote the curation on
// the first rebuild, with nothing in the request saying so. Preserving it keeps
// the marker a deliberate admin decision: DELETE /images is the way to clear it,
// not a build completing. Only 'recommended' is sticky; an 'external' row still
// becomes 'built', because a real build genuinely supersedes a hand-pushed ref.
func (s *PGStore) AdmitBuiltImage(ctx context.Context, img Image) error {
	const q = `INSERT INTO image_whitelist
		(image_ref, source, build_id, added_by, enabled, added_at)
		VALUES ($1, 'built', NULLIF($2, ''), $3, true, $4)
		ON CONFLICT (image_ref) DO UPDATE
		SET source = CASE WHEN image_whitelist.source = 'recommended'
			THEN 'recommended' ELSE 'built' END,
			build_id = EXCLUDED.build_id, added_by = EXCLUDED.added_by,
			enabled = true, added_at = EXCLUDED.added_at`
	_, err := s.db.ExecContext(ctx, q, img.ImageRef, img.BuildID, img.AddedBy, img.AddedAt)
	return err
}

func (s *PGStore) ListImages(ctx context.Context) ([]Image, error) {
	const q = `SELECT image_ref, source, build_id, added_by, enabled, added_at
		FROM image_whitelist ORDER BY added_at DESC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		var (
			img     Image
			buildID sql.NullString
		)
		if err := rows.Scan(&img.ImageRef, &img.Source, &buildID, &img.AddedBy,
			&img.Enabled, &img.AddedAt); err != nil {
			return nil, err
		}
		img.BuildID = buildID.String
		out = append(out, img)
	}
	return out, rows.Err()
}

// AddExternalImage upserts a hand-pushed ref. Unlike AdmitBuiltImage this path
// does NOT preserve a 'recommended' source, and the asymmetry is deliberate: an
// admin POSTing this exact ref is an explicit, named re-admission, not a build
// completing behind their back, so demoting the curated row is the outcome they
// asked for. It also keeps the 201 body honest — AddExternalImage returns the
// Image it constructed (source=external) without re-reading the row, so a sticky
// source here would report a value the database does not hold.
//
// Note that this demote branch is REACHABLE for today's recommended rows: 0021
// re-pointed the seeds at host-qualified registry refs (registry.<ns>.svc:5000/…),
// which ValidateImageRef accepts — so an admin re-admitting one of those refs
// demotes the curated row, by design. It was dead only while the seeds were bare
// local containerd tags (0018's felis-lobby:demo), which the validation refuses.
func (s *PGStore) AddExternalImage(ctx context.Context, img Image) error {
	const q = `INSERT INTO image_whitelist
		(image_ref, source, added_by, enabled, added_at)
		VALUES ($1, 'external', $2, true, $3)
		ON CONFLICT (image_ref) DO UPDATE
		SET source = 'external', build_id = NULL, added_by = EXCLUDED.added_by,
			enabled = true, added_at = EXCLUDED.added_at`
	_, err := s.db.ExecContext(ctx, q, img.ImageRef, img.AddedBy, img.AddedAt)
	return err
}

func (s *PGStore) RemoveImage(ctx context.Context, imageRef string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM image_whitelist WHERE image_ref = $1`, imageRef)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
