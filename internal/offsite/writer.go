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

// writerMark names the host that writes the bucket. A rehearsal on a spare
// machine restores the production host's /etc/felis, [offsite] and its key
// included: without the record the spare would copy its bundles into the
// production prefix and prune the production host's by DBKeep. The writer
// rewrites it on every run; a host restored from its backup finds another
// host named there and stands by, writing nothing, until `felis offsite
// take-over`.
const writerMark = "felis-writer"

// WriterLive is how recently the writer must have run for a standby host to
// count it as alive. Both run hourly, so a writer seen within it is one that
// ran in the last two hours.
const WriterLive = 3 * time.Hour

// HostIDFile is the file, next to the status file, holding this host's id. It
// is written when the host first writes the bucket and never leaves the host
// (a bundle carries /etc/felis only), so a host restored from a bundle has
// none.
const HostIDFile = "host-id"

var (
	// ErrStandby is a run refused because another host writes the bucket and
	// this one never has.
	ErrStandby = errors.New("offsite: another host writes this bucket")
	// ErrDisplaced is a run refused because another host took the bucket
	// over from this one.
	ErrDisplaced = errors.New("offsite: another host took this bucket over")
)

const takeOverHint = "sudo felis offsite take-over -yes (docs/troubleshooting.md §16)"

// Writer is the felis-writer record.
type Writer struct {
	HostID string    `json:"host_id"`
	Host   string    `json:"host"`
	At     time.Time `json:"at"`
}

func (w *Writer) String() string {
	return fmt.Sprintf("host %s (id %s)", w.Host, w.HostID)
}

// WriterError is a run refused because another host writes the bucket.
type WriterError struct {
	// Kind is ErrStandby or ErrDisplaced.
	Kind error
	// Writer is the host named in the bucket; nil for a bucket that holds
	// another host's copies and names no writer.
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

// Role is what a host's next run does with the bucket.
type Role int

const (
	// RoleWrites: the bucket names this host.
	RoleWrites Role = iota + 1
	// RoleClaims: the bucket names no host, and this one records itself.
	RoleClaims
	// RoleStandby: another host writes the bucket, and this one never has.
	RoleStandby
	// RoleDisplaced: another host took the bucket over from this one.
	RoleDisplaced
)

// Lease is this host's side of felis-writer.
type Lease struct {
	// IDFile is this host's id (HostIDFile next to the status file).
	IDFile string
	// Host is this host's name, shown to the others.
	Host string
	// Inherited is a host that copied to the bucket before writers were
	// recorded: a status file an older release wrote. It claims a bucket
	// that names no writer.
	Inherited bool
}

// BucketWriter reads the felis-writer record, nil when there is none.
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

// Plan says what this host's next run does with the bucket, and the host the
// bucket names (nil for none). empty is a bucket holding no sealed object
// (CheckKey's KeyUnused), which any host may claim.
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

// Acquire is Plan before a run writes anything: a host that writes or claims
// the bucket records itself there at now; one that stands by or was displaced
// gets a *WriterError and writes nothing.
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

// TakeOver records this host as the bucket's writer whatever the bucket named,
// and returns the writer it replaced (nil for none). That host's next run is
// refused with ErrDisplaced.
func (l Lease) TakeOver(ctx context.Context, b Bucket, now time.Time) (*Writer, error) {
	prev, err := BucketWriter(ctx, b)
	if err != nil {
		return nil, err
	}
	return prev, l.record(ctx, b, now)
}

// record writes this host into felis-writer, creating its id first: a host
// whose id is on disk but not in the bucket claims the bucket on its next run,
// where one named in the bucket without an id on disk would stand by for
// itself.
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

// ID is this host's id, "" when it has never written a bucket.
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

// printable keeps a host name fit for a message: at most 64 printable runes,
// "unknown" for none.
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
