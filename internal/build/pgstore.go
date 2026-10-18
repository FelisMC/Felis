package build

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// PGStore is the Postgres-backed Store; only felis-api holds database credentials, never the build Pod.
type PGStore struct {
	db *sql.DB
}

// NewPGStore wraps an existing pool.
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

func (s *PGStore) GetBuilds(ctx context.Context, ids []string) ([]Build, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, image_ref, status, '', context_ref, base_image,
			requested_by, job_name, log_ref, error, created_at, finished_at, context_digest
		FROM image_builds WHERE id = ANY($1::text[])`, ids)
	if err != nil {
		return nil, err
	}
	return scanBuilds(rows)
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
	return scanBuilds(rows)
}

// ListBuilds pages newest first; the id breaks created_at ties, and the Dockerfile column is left empty.
func (s *PGStore) ListBuilds(ctx context.Context, opts ListOpts) ([]Build, int, error) {
	const match = ` WHERE $1::text = '' OR id = $1::text OR status::text = lower($1::text)
		OR strpos(lower(image_ref), lower($1::text)) > 0`
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM image_builds`+match, opts.Query).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, image_ref, status, '', context_ref, base_image,
			requested_by, job_name, log_ref, error, created_at, finished_at, context_digest
		FROM image_builds`+match+` ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`,
		opts.Query, opts.Limit, opts.Offset)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanBuilds(rows)
	return out, total, err
}

func scanBuilds(rows *sql.Rows) ([]Build, error) {
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

// AdmitBuiltImage upserts the whitelist row, re-enabling a removed ref.
// A 'recommended' source is kept so rebuilding a curated tag does not silently demote it.
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

// Manual readmission intentionally changes a recommended image's source to external.
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

// SaveScan upserts, so Sync may retry freely.
func (s *PGStore) SaveScan(ctx context.Context, sc Scan) error {
	summary, err := json.Marshal(sc.Summary)
	if err != nil {
		return err
	}
	const q = `INSERT INTO image_build_scans
		(build_id, blocked, summary, report_gz, sbom_gz, scanned_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (build_id) DO UPDATE
		SET blocked = EXCLUDED.blocked, summary = EXCLUDED.summary,
			report_gz = EXCLUDED.report_gz, sbom_gz = EXCLUDED.sbom_gz,
			scanned_at = EXCLUDED.scanned_at`
	_, err = s.db.ExecContext(ctx, q, sc.BuildID, sc.Summary.Blocked, summary, sc.ReportGz, sc.SBOMGz, sc.ScannedAt)
	return err
}

func (s *PGStore) GetScan(ctx context.Context, buildID string) (*Scan, error) {
	const q = `SELECT build_id, summary, report_gz, sbom_gz, scanned_at
		FROM image_build_scans WHERE build_id = $1`
	var (
		sc      Scan
		summary []byte
	)
	switch err := s.db.QueryRowContext(ctx, q, buildID).Scan(&sc.BuildID, &summary, &sc.ReportGz, &sc.SBOMGz, &sc.ScannedAt); {
	case err == sql.ErrNoRows:
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(summary, &sc.Summary); err != nil {
		return nil, err
	}
	return &sc, nil
}
