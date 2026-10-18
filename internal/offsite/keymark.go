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

// keyMark stores only the key ID in plaintext so incorrect-key hosts are rejected before uploads or pruning.
const keyMark = "felis-key-id"

var ErrKeyMismatch = errors.New("offsite: the bucket's objects are sealed with another key")

const keyMismatchFix = "set FELIS_OFFSITE_KEY in /etc/felis/offsite.env to the key they were sealed with, or point [offsite] at an empty bucket or prefix (https://docs.felismc.com/en/operations/backup#offsite-key)"

var keyIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// KeyFit zero means CheckKey failed to establish a match.
type KeyFit int

const (
	KeyRecorded KeyFit = iota + 1
	KeyOpens
	KeyUnused
)

// Without a key ID, authenticate recent sealed segments with bounded attempts so corrupt objects cannot stall checking.
const (
	keyRefusals = 3
	keyTries    = 10
)

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

var errOpened = errors.New("offsite: first segment opened")

type firstSegment struct{}

func (firstSegment) Write([]byte) (int, error) { return 0, errOpened }

// CheckKey performs no writes and returns ErrKeyMismatch for a different encryption key.
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
			continue
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

// claim checks the key, acquires the writer lease, then records the key ID so wrong-key and standby hosts write nothing.
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
