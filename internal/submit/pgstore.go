package submit

import (
	"context"
	"database/sql"
	"time"
)

// PGStore is the Postgres-backed Store (image_submissions, migration 0002). It
// is the only submit component that holds database credentials and exposes only
// the narrow operations the Manager needs — no generic UPDATE escape hatch. It
// compiles here but is exercised by integration tests against a live database;
// the Manager's logic is unit-tested against the in-memory fake instead.
type PGStore struct {
	db *sql.DB
}

// NewPGStore wraps an open *sql.DB.
func NewPGStore(db *sql.DB) *PGStore { return &PGStore{db: db} }

// Compile-time proof PGStore satisfies the Store interface.
var _ Store = (*PGStore)(nil)

const submissionColumns = `id, submitted_by, display_name, context_ref, status,
	image_ref, build_id, reviewed_by, reject_reason, created_at, reviewed_at`

func (s *PGStore) CreateSubmission(ctx context.Context, sub *Submission) error {
	const q = `INSERT INTO image_submissions
		(id, submitted_by, display_name, context_ref, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := s.db.ExecContext(ctx, q,
		sub.ID, sub.SubmittedBy, sub.DisplayName, sub.ContextRef, string(sub.Status), sub.CreatedAt)
	return err
}

func (s *PGStore) CountPendingSubmissionsBy(ctx context.Context, submittedBy string) (int, error) {
	const q = `SELECT count(*) FROM image_submissions
		WHERE submitted_by = $1 AND status = 'pending_review'`
	var n int
	err := s.db.QueryRowContext(ctx, q, submittedBy).Scan(&n)
	return n, err
}

func (s *PGStore) GetSubmission(ctx context.Context, id string) (*Submission, error) {
	const q = `SELECT ` + submissionColumns + ` FROM image_submissions WHERE id = $1`
	return scanSubmission(s.db.QueryRowContext(ctx, q, id))
}

func (s *PGStore) ListSubmissions(ctx context.Context) ([]Submission, error) {
	const q = `SELECT ` + submissionColumns + ` FROM image_submissions ORDER BY created_at DESC`
	return s.querySubmissions(ctx, q)
}

func (s *PGStore) ListSubmissionsBy(ctx context.Context, submittedBy string) ([]Submission, error) {
	const q = `SELECT ` + submissionColumns + `
		FROM image_submissions WHERE submitted_by = $1 ORDER BY created_at DESC`
	return s.querySubmissions(ctx, q, submittedBy)
}

// cas executes a single-statement compare-and-set — an UPDATE or DELETE whose
// WHERE clause is the predicate — and reports whether THIS call moved a row. The
// `status = 'pending_review'` guard is the actual CAS predicate in each caller's
// query and is deliberately kept INLINE — it is security-visible, so a reader
// auditing "can a non-pending row be flipped or deleted?" must see it next to the
// SET or DELETE. cas only folds the shared ExecContext + RowsAffected tail so
// the reviewers and deleters cannot drift in how they report a lost race or a
// RowsAffected error. n == 0 means a concurrent actor already won the row —
// reported as won=false (never an error), which the Manager maps to
// ErrAlreadyReviewed / ErrNotFound.
func (s *PGStore) cas(ctx context.Context, q string, args ...any) (bool, error) {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ApproveSubmission is the approve CAS: it flips the row only while it is still
// pending_review, so a concurrent reviewer cannot also win.
func (s *PGStore) ApproveSubmission(ctx context.Context, id, reviewedBy, imageRef string, at time.Time) (bool, error) {
	const q = `UPDATE image_submissions
		SET status = 'approved', image_ref = $2, reviewed_by = $3, reviewed_at = $4
		WHERE id = $1 AND status = 'pending_review'`
	return s.cas(ctx, q, id, imageRef, reviewedBy, at)
}

// RejectSubmission is the reject CAS, mirroring ApproveSubmission.
func (s *PGStore) RejectSubmission(ctx context.Context, id, reviewedBy, reason string, at time.Time) (bool, error) {
	const q = `UPDATE image_submissions
		SET status = 'rejected', reviewed_by = $2, reject_reason = $3, reviewed_at = $4
		WHERE id = $1 AND status = 'pending_review'`
	return s.cas(ctx, q, id, reviewedBy, reason, at)
}

func (s *PGStore) LinkBuild(ctx context.Context, id, buildID string) error {
	const q = `UPDATE image_submissions SET build_id = $2 WHERE id = $1`
	res, err := s.db.ExecContext(ctx, q, id, buildID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSubmission is the admin delete: any status, one row, no predicate beyond
// the id. False means the id was already gone (a concurrent delete won).
func (s *PGStore) DeleteSubmission(ctx context.Context, id string) (bool, error) {
	const q = `DELETE FROM image_submissions WHERE id = $1`
	return s.cas(ctx, q, id)
}

// DeletePendingSubmission is the withdraw CAS: owner + pending_review must both
// still hold, so a reviewed submission can never be deleted through this path.
func (s *PGStore) DeletePendingSubmission(ctx context.Context, id, submittedBy string) (bool, error) {
	const q = `DELETE FROM image_submissions
		WHERE id = $1 AND submitted_by = $2 AND status = 'pending_review'`
	return s.cas(ctx, q, id, submittedBy)
}

func (s *PGStore) querySubmissions(ctx context.Context, q string, args ...any) ([]Submission, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Submission
	for rows.Next() {
		sub, err := scanSubmissionRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sub)
	}
	return out, rows.Err()
}

// rowScanner is the read surface shared by *sql.Row (single) and *sql.Rows (in a
// list loop), so one scan body serves both without copy-paste.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanSubmission scans a single row, translating no-rows into ErrNotFound.
func scanSubmission(row *sql.Row) (*Submission, error) {
	sub, err := scanSubmissionRows(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return sub, err
}

// scanSubmissionRows decodes one row's columns, mapping nullable text/time
// columns through sql.Null* (NULLIF/absent values become zero, omitted in JSON).
func scanSubmissionRows(row rowScanner) (*Submission, error) {
	var (
		sub                                         Submission
		status                                      string
		imageRef, buildID, reviewedBy, rejectReason sql.NullString
		reviewedAt                                  sql.NullTime
	)
	if err := row.Scan(
		&sub.ID, &sub.SubmittedBy, &sub.DisplayName, &sub.ContextRef, &status,
		&imageRef, &buildID, &reviewedBy, &rejectReason, &sub.CreatedAt, &reviewedAt,
	); err != nil {
		return nil, err
	}
	sub.Status = Status(status)
	sub.ImageRef = imageRef.String
	sub.BuildID = buildID.String
	sub.ReviewedBy = reviewedBy.String
	sub.RejectReason = rejectReason.String
	if reviewedAt.Valid {
		t := reviewedAt.Time
		sub.ReviewedAt = &t
	}
	return &sub, nil
}
