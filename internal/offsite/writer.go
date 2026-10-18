package offsite

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// writerMark prevents a restored spare host from overwriting and pruning production backups until explicit take-over.
const writerMark = "felis-writer"

const WriterLive = 3 * time.Hour

// HostIDFile stays outside backup bundles so restored hosts receive a distinct writer identity.
const HostIDFile = "host-id"

var (
	ErrStandby   = errors.New("offsite: another host writes this bucket")
	ErrDisplaced = errors.New("offsite: another host took this bucket over")
)

const takeOverHint = "sudo felis offsite take-over -yes (https://docs.felismc.com/en/operations/backup#take-over)"

type Writer struct {
	HostID string    `json:"host_id"`
	Host   string    `json:"host"`
	At     time.Time `json:"at"`
}

func (w *Writer) String() string {
	return fmt.Sprintf("host %s (id %s)", w.Host, w.HostID)
}

type WriterError struct {
	Kind error
	// Writer=nil can mean the bucket contains another host's backups without a writer record.
	Writer *Writer
}

func (e *WriterError) Error() string {
	switch {
	case e.Kind == ErrDisplaced:
		return fmt.Sprintf("%v: %s writes it now (last at %s), and this host copies nothing there any more; if that host is a rehearsal machine, take the bucket back here: %s",
			e.Kind, e.Writer, e.Writer.At.UTC().Format(time.RFC3339), takeOverHint)
	case e.Writer != nil:
		return fmt.Sprintf("%v: %s writes it (last at %s); this host was built from its backup and copies nothing there, until it replaces that host for good: %s",
			e.Kind, e.Writer, e.Writer.At.UTC().Format(time.RFC3339), takeOverHint)
	default:
		return fmt.Sprintf("%v: the bucket holds copies this host did not write and names no host writing it; this host copies nothing there, until it replaces that host for good: %s",
			e.Kind, takeOverHint)
	}
}

func (e *WriterError) Unwrap() error { return e.Kind }

type Role int

const (
	RoleWrites Role = iota + 1
	RoleClaims
	RoleStandby
	RoleDisplaced
)

type Lease struct {
	IDFile string
	Host   string
	// Inherited permits a legacy writer to claim a bucket without an existing writer record.
	Inherited bool
}

func BucketWriter(ctx context.Context, b Bucket) (*Writer, error) {
	rc, err := b.Get(ctx, writerMark)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", writerMark, err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, 1024))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", writerMark, err)
	}
	var w Writer
	if json.Unmarshal(raw, &w) != nil || !keyIDPattern.MatchString(w.HostID) {
		return nil, fmt.Errorf("offsite: %s in the bucket is not a record Felis wrote", writerMark)
	}
	w.Host = printable(w.Host)
	return &w, nil
}

// Plan allows any host to claim a bucket containing no sealed objects.
func (l Lease) Plan(ctx context.Context, b Bucket, empty bool) (Role, *Writer, error) {
	w, err := BucketWriter(ctx, b)
	if err != nil {
		return 0, nil, err
	}
	mine, err := l.ID()
	if err != nil {
		return 0, nil, err
	}
	switch {
	case w != nil && w.HostID == mine:
		return RoleWrites, w, nil
	case w != nil && mine != "":
		return RoleDisplaced, w, nil
	case w != nil:
		return RoleStandby, w, nil
	case mine != "" || l.Inherited || empty:
		return RoleClaims, nil, nil
	default:
		return RoleStandby, nil, nil
	}
}

// Acquire records the writer before copying; standby and displaced hosts return WriterError without writes.
func (l Lease) Acquire(ctx context.Context, b Bucket, empty bool, now time.Time) error {
	role, w, err := l.Plan(ctx, b, empty)
	if err != nil {
		return err
	}
	switch role {
	case RoleStandby:
		return &WriterError{Kind: ErrStandby, Writer: w}
	case RoleDisplaced:
		return &WriterError{Kind: ErrDisplaced, Writer: w}
	}
	return l.record(ctx, b, now)
}

// TakeOver replaces the writer unconditionally; the prior host receives ErrDisplaced on its next run.
func (l Lease) TakeOver(ctx context.Context, b Bucket, now time.Time) (*Writer, error) {
	prev, err := BucketWriter(ctx, b)
	if err != nil {
		return nil, err
	}
	return prev, l.record(ctx, b, now)
}

// Persist the local writer ID before the bucket record so failures cannot make the host stand by for its own unknown identity.
func (l Lease) record(ctx context.Context, b Bucket, now time.Time) error {
	id, err := l.ID()
	if err != nil {
		return err
	}
	if id == "" {
		if id, err = l.newID(); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(Writer{HostID: id, Host: printable(l.Host), At: now.UTC()})
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := b.Put(ctx, writerMark, strings.NewReader(string(raw)), int64(len(raw))); err != nil {
		return fmt.Errorf("record this host in %s: %w", writerMark, err)
	}
	return nil
}

func (l Lease) ID() (string, error) {
	raw, err := os.ReadFile(l.IDFile)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read this host's id: %w", err)
	}
	id := strings.TrimSpace(string(raw))
	if !keyIDPattern.MatchString(id) {
		return "", fmt.Errorf("offsite: %s is not a host id Felis wrote; remove it and run sudo felis offsite take-over -yes", l.IDFile)
	}
	return id, nil
}

func (l Lease) newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	if err := os.MkdirAll(filepath.Dir(l.IDFile), 0o700); err != nil {
		return "", fmt.Errorf("record this host's id: %w", err)
	}
	tmp := l.IDFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("record this host's id: %w", err)
	}
	if err := os.Rename(tmp, l.IDFile); err != nil {
		return "", fmt.Errorf("record this host's id: %w", err)
	}
	return id, nil
}

func printable(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
	if r := []rune(s); len(r) > 64 {
		s = string(r[:64])
	}
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}
