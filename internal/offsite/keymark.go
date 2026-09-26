package offsite

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

// keyMark is the one object the bucket holds in the clear: the KeyID of the
// key every other object is sealed with. The id names the key without
// revealing it, so a host holding another key (a reinstall that generated a
// fresh one, an offsite.env from elsewhere) is refused before it writes an
// object or prunes one the right key still opens.
const keyMark = "felis-key-id"

// ErrKeyMismatch is a bucket whose objects are sealed with another key.
var ErrKeyMismatch = errors.New("offsite: the bucket's objects are sealed with another key")

const keyMismatchFix = "set FELIS_OFFSITE_KEY in /etc/felis/offsite.env to the key they were sealed with, or point [offsite] at an empty bucket or prefix (docs/troubleshooting.md §16)"

var keyIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// KeyFit is how CheckKey matched a key to a bucket. The zero value is no match:
// what CheckKey returns with an error.
type KeyFit int

const (
	// KeyRecorded: the bucket records this key's id.
	KeyRecorded KeyFit = iota + 1
	// KeyOpens: the bucket records no id, and the key opens its newest objects.
	KeyOpens
	// KeyUnused: the bucket holds no sealed object yet.
	KeyUnused
)

// A bucket without a recorded id is judged by its newest sealed objects: the
// key must open the first segment of one of them. keyRefusals objects that
// refuse the key decide a mismatch; at most keyTries are read, so a run of
// damaged objects cannot stall the check.
const (
	keyRefusals = 3
	keyTries    = 10
)

// BucketKeyID reads the key id the bucket records, "" when it records none.
func BucketKeyID(ctx context.Context, b Bucket) (string, error) {
	rc, err := b.Get(ctx, keyMark)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", keyMark, err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, 256))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", keyMark, err)
	}
	id := strings.TrimSpace(string(raw))
	if !keyIDPattern.MatchString(id) {
		return "", fmt.Errorf("offsite: %s in the bucket is not a key id Felis wrote", keyMark)
	}
	return id, nil
}

// errOpened stops Decrypt once the first segment has authenticated.
var errOpened = errors.New("offsite: first segment opened")

type firstSegment struct{}

func (firstSegment) Write([]byte) (int, error) { return 0, errOpened }

// CheckKey tells whether key is the one the bucket's objects are sealed with.
// It writes nothing; a mismatch is ErrKeyMismatch.
func CheckKey(ctx context.Context, b Bucket, key []byte) (KeyFit, error) {
	mine := KeyID(key)
	id, err := BucketKeyID(ctx, b)
	if err != nil {
		return 0, err
	}
	if id != "" {
		if id != mine {
			return 0, fmt.Errorf("%w: the bucket records key id %s, this key is %s; %s", ErrKeyMismatch, id, mine, keyMismatchFix)
		}
		return KeyRecorded, nil
	}
	var sealed []Object
	for _, dir := range []string{dbDir, worldsDir, registryDir, uploadsDir} {
		objs, err := b.List(ctx, dir)
		if err != nil {
			return 0, fmt.Errorf("list %s: %w", dir, err)
		}
		for _, o := range objs {
			if strings.HasSuffix(o.Key, objExt) {
				sealed = append(sealed, o)
			}
		}
	}
	if len(sealed) == 0 {
		return KeyUnused, nil
	}
	sort.Slice(sealed, func(i, j int) bool { return sealed[i].Modified.After(sealed[j].Modified) })
	var refused []string
	var unjudged error
	for i, o := range sealed {
		if i == keyTries || len(refused) == keyRefusals {
			break
		}
		rc, err := b.Get(ctx, o.Key)
		if errors.Is(err, ErrNotFound) {
			continue // pruned since the listing
		}
		if err != nil {
			return 0, fmt.Errorf("%s: %w", o.Key, err)
		}
		err = Decrypt(firstSegment{}, rc, key)
		rc.Close()
		switch {
		case err == nil || errors.Is(err, errOpened):
			return KeyOpens, nil
		case errors.Is(err, ErrAuth):
			refused = append(refused, o.Key)
		case unjudged == nil:
			unjudged = fmt.Errorf("%s: %w", o.Key, err)
		}
	}
	if len(refused) > 0 {
		return 0, fmt.Errorf("%w: this key (key id %s) opens none of %s; %s", ErrKeyMismatch, mine, strings.Join(refused, ", "), keyMismatchFix)
	}
	if unjudged != nil {
		return 0, fmt.Errorf("offsite: cannot tell which key sealed the bucket's objects: %w", unjudged)
	}
	return KeyUnused, nil
}

// claim is the check before a run writes anything: CheckKey, then the lease
// (nil checks none), then key's id recorded in a bucket that has none, so
// that every later check reads the id. The key goes first, so a host with the
// wrong key never records itself as the writer, and the lease before the key
// id, so a standby host writes nothing at all.
func claim(ctx context.Context, b Bucket, key []byte, lease *Lease, now time.Time) error {
	fit, err := CheckKey(ctx, b, key)
	if err != nil {
		return err
	}
	if lease != nil {
		if err := lease.Acquire(ctx, b, fit == KeyUnused, now); err != nil {
			return err
		}
	}
	if fit == KeyRecorded {
		return nil
	}
	id := KeyID(key) + "\n"
	if err := b.Put(ctx, keyMark, strings.NewReader(id), int64(len(id))); err != nil {
		return fmt.Errorf("record the key id in %s: %w", keyMark, err)
	}
	return nil
}
