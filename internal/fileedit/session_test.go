package fileedit

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// diskStage is a stage on a disk of total bytes whose free space is what the
// files in it leave of free: statfs sees parts land, as a real disk would.
func diskStage(t *testing.T, free, total uint64, minFree float64) *Stage {
	t.Helper()
	s := &Stage{Dir: filepath.Join(t.TempDir(), "stage"), MinFree: minFree}
	prev := statfs
	statfs = func(string) (uint64, uint64, error) {
		var used uint64
		des, _ := os.ReadDir(s.Dir)
		for _, de := range des {
			if info, err := de.Info(); err == nil {
				used += uint64(info.Size())
			}
		}
		return free - used, total, nil
	}
	t.Cleanup(func() { statfs = prev })
	return s
}

func appendString(s *Stage, user, server, id string, offset int64, part string) (Session, error) {
	return s.Append(user, server, id, offset, strings.NewReader(part), int64(len(part)))
}

func readStaged(t *testing.T, s *Stage, id, token string) string {
	t.Helper()
	f, size, err := s.Open(id, token)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(b)) {
		t.Fatalf("Open said %d bytes and served %d", size, len(b))
	}
	return string(b)
}

// TestSessionArrivesInParts: parts land in order, Seal hands the Job a token for
// exactly those bytes, and Served deletes them.
func TestSessionArrivesInParts(t *testing.T) {
	s := roomyStage(t)
	const whole = "PK\x03\x04 first part, second part"
	sess, err := s.Begin("u1", "survival", "plugins/big.jar", int64(len(whole)))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if !hexID.MatchString(sess.ID) || sess != (Session{ID: sess.ID, Path: "plugins/big.jar", Size: int64(len(whole))}) {
		t.Fatalf("Begin = %+v", sess)
	}
	got, err := appendString(s, "u1", "survival", sess.ID, 0, whole[:16])
	if err != nil || got.Received != 16 || got.Size != int64(len(whole)) {
		t.Fatalf("first part: %+v, %v", got, err)
	}
	if at, err := s.Status("u1", "survival", sess.ID); err != nil || at.Received != 16 || at.Path != "plugins/big.jar" {
		t.Fatalf("Status = %+v, %v", at, err)
	}
	if got, err = appendString(s, "u1", "survival", sess.ID, 16, whole[16:]); err != nil || got.Received != int64(len(whole)) {
		t.Fatalf("second part: %+v, %v", got, err)
	}

	st, err := s.Seal("u1", "survival", sess.ID)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if st.ID != sess.ID || !hexToken.MatchString(st.Token) || st.Size != int64(len(whole)) || st.SHA256 != digest([]byte(whole)) {
		t.Fatalf("Seal = %+v, want the digest of %q", st, whole)
	}
	if body := readStaged(t, s, st.ID, st.Token); body != whole {
		t.Fatalf("served %q, want %q", body, whole)
	}
	if _, _, err := s.Open(st.ID, st.Token); !errors.Is(err, ErrNotStaged) {
		t.Fatalf("second Open with the same token: err = %v, want ErrNotStaged", err)
	}

	s.Served(st.ID)
	if names := stagedNames(t, s); len(names) != 0 {
		t.Fatalf("after Served: %v", names)
	}
	if _, err := s.Status("u1", "survival", sess.ID); !errors.Is(err, ErrNotStaged) {
		t.Fatalf("Status after Served: err = %v, want ErrNotStaged", err)
	}
}

// A session answers only the user and the server it was begun for.
func TestSessionAnswersItsOwnerOnly(t *testing.T) {
	s := roomyStage(t)
	sess, err := s.Begin("u1", "survival", "a.zip", 4)
	if err != nil {
		t.Fatal(err)
	}
	for name, who := range map[string][2]string{
		"another user":   {"u2", "survival"},
		"another server": {"u1", "creative"},
	} {
		if _, err := s.Status(who[0], who[1], sess.ID); !errors.Is(err, ErrNotStaged) {
			t.Errorf("%s: Status err = %v", name, err)
		}
		if _, err := appendString(s, who[0], who[1], sess.ID, 0, "abcd"); !errors.Is(err, ErrNotStaged) {
			t.Errorf("%s: Append err = %v", name, err)
		}
		if _, err := s.Seal(who[0], who[1], sess.ID); !errors.Is(err, ErrNotStaged) {
			t.Errorf("%s: Seal err = %v", name, err)
		}
		if err := s.Drop(who[0], who[1], sess.ID); !errors.Is(err, ErrNotStaged) {
			t.Errorf("%s: Drop err = %v", name, err)
		}
	}
	if _, err := s.Status("u1", "survival", strings.Repeat("0", 32)); !errors.Is(err, ErrNotStaged) {
		t.Errorf("unknown id: err = %v", err)
	}
	if at, err := s.Status("u1", "survival", sess.ID); err != nil || at.Received != 0 {
		t.Fatalf("the owner's session after the others tried: %+v, %v", at, err)
	}
}

func TestSessionRefusesAPartThatDoesNotFit(t *testing.T) {
	s := roomyStage(t)
	sess, err := s.Begin("u1", "survival", "a.zip", 6)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendString(s, "u1", "survival", sess.ID, 0, "abc"); err != nil {
		t.Fatal(err)
	}

	var off *OffsetError
	for _, offset := range []int64{0, 2, 4} {
		at, err := appendString(s, "u1", "survival", sess.ID, offset, "d")
		if !errors.As(err, &off) || off.Received != 3 || at.Received != 3 {
			t.Fatalf("offset %d: %+v, err = %v; want an OffsetError at 3", offset, at, err)
		}
	}
	if _, err := appendString(s, "u1", "survival", sess.ID, 3, "defg"); !errors.Is(err, ErrPartTooLarge) {
		t.Fatalf("past the declared size: err = %v, want ErrPartTooLarge", err)
	}
	if _, err := s.Append("u1", "survival", sess.ID, 3, strings.NewReader(""), -1); !errors.Is(err, ErrPartTooLarge) {
		t.Fatalf("negative length: err = %v, want ErrPartTooLarge", err)
	}
	if at, err := appendString(s, "u1", "survival", sess.ID, 3, "def"); err != nil || at.Received != 6 {
		t.Fatalf("the part that fits exactly: %+v, %v", at, err)
	}

	big, err := s.Begin("u1", "survival", "b.zip", PartBytes+2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("u1", "survival", big.ID, 0, strings.NewReader(""), PartBytes+1); !errors.Is(err, ErrPartTooLarge) {
		t.Fatalf("a part over PartBytes: err = %v, want ErrPartTooLarge", err)
	}
}

// A part that breaks or runs long leaves the session as it was: the file is cut
// back and the digest forgets it, so the resent part makes the right file.
func TestSessionRollsBackAFailedPart(t *testing.T) {
	for name, tc := range map[string]struct {
		body  io.Reader
		short bool
	}{
		"breaks":    {io.MultiReader(strings.NewReader("XY"), errReader{io.ErrUnexpectedEOF}), true},
		"ends":      {strings.NewReader("XY"), true},
		"runs long": {strings.NewReader("XYZWV"), false},
	} {
		t.Run(name, func(t *testing.T) {
			s := roomyStage(t)
			sess, err := s.Begin("u1", "survival", "a.zip", 7)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := appendString(s, "u1", "survival", sess.ID, 0, "abc"); err != nil {
				t.Fatal(err)
			}
			at, err := s.Append("u1", "survival", sess.ID, 3, tc.body, 4)
			if err == nil || errors.Is(err, ErrShortUpload) != tc.short || at.Received != 3 {
				t.Fatalf("%+v, err = %v; want a failure at 3 (short = %v)", at, err, tc.short)
			}
			info, err := os.Stat(filepath.Join(s.Dir, stagedNames(t, s)[0]))
			if err != nil || info.Size() != 3 {
				t.Fatalf("staged file is %v bytes (%v), want it cut back to 3", info.Size(), err)
			}
			if _, err := appendString(s, "u1", "survival", sess.ID, 3, "defg"); err != nil {
				t.Fatalf("resent part: %v", err)
			}
			st, err := s.Seal("u1", "survival", sess.ID)
			if err != nil || st.SHA256 != digest([]byte("abcdefg")) {
				t.Fatalf("Seal = %+v, %v; want the digest of abcdefg", st, err)
			}
			if body := readStaged(t, s, st.ID, st.Token); body != "abcdefg" {
				t.Fatalf("served %q", body)
			}
		})
	}
}

// While a part is arriving nothing else may touch the session, and it is never
// idle.
func TestSessionIsBusyWhileAPartArrives(t *testing.T) {
	s := roomyStage(t)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	sess, err := s.Begin("u1", "survival", "a.zip", 4)
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := s.Append("u1", "survival", sess.ID, 0, pr, 4)
		done <- err
	}()
	// The write returns once Append is copying, which is after it marked busy.
	if _, err := pw.Write([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	if _, err := appendString(s, "u1", "survival", sess.ID, 0, "abcd"); !errors.Is(err, ErrUploadBusy) {
		t.Errorf("a second part: err = %v, want ErrUploadBusy", err)
	}
	if _, err := s.Seal("u1", "survival", sess.ID); !errors.Is(err, ErrUploadBusy) {
		t.Errorf("Seal: err = %v, want ErrUploadBusy", err)
	}
	if err := s.Drop("u1", "survival", sess.ID); !errors.Is(err, ErrUploadBusy) {
		t.Errorf("Drop: err = %v, want ErrUploadBusy", err)
	}
	now = now.Add(SessionIdle + time.Hour)
	if n := s.Expire(); n != 0 {
		t.Errorf("Expire dropped %d sessions with a part arriving", n)
	}
	if _, err := pw.Write([]byte("cd")); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatalf("the part in flight: %v", err)
	}
	if _, err := s.Seal("u1", "survival", sess.ID); err != nil {
		t.Fatalf("Seal once the part is in: %v", err)
	}
}

// Each Seal arms one fetch with a fresh token, so a Job that failed can be
// started again on the same bytes.
func TestSessionSealArmsOneFetch(t *testing.T) {
	s := roomyStage(t)
	sess, err := s.Begin("u1", "survival", "a.zip", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendString(s, "u1", "survival", sess.ID, 0, "abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Seal("u1", "survival", sess.ID); !errors.Is(err, ErrUploadIncomplete) {
		t.Fatalf("Seal at 3 of 4: err = %v, want ErrUploadIncomplete", err)
	}
	if _, _, err := s.Open(sess.ID, ""); !errors.Is(err, ErrNotStaged) {
		t.Fatalf("Open before any Seal: err = %v, want ErrNotStaged", err)
	}
	if _, err := appendString(s, "u1", "survival", sess.ID, 3, "d"); err != nil {
		t.Fatal(err)
	}

	first, err := s.Seal("u1", "survival", sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Seal("u1", "survival", sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token || first.SHA256 != second.SHA256 {
		t.Fatalf("two Seals: %+v and %+v; want fresh tokens for the same bytes", first, second)
	}
	if _, _, err := s.Open(sess.ID, first.Token); !errors.Is(err, ErrNotStaged) {
		t.Fatalf("the replaced token: err = %v, want ErrNotStaged", err)
	}
	if _, _, err := s.Open(sess.ID, strings.Repeat("0", 64)); !errors.Is(err, ErrNotStaged) {
		t.Fatalf("a wrong token: err = %v, want ErrNotStaged", err)
	}
	// Neither wrong token spent the armed one.
	if body := readStaged(t, s, sess.ID, second.Token); body != "abcd" {
		t.Fatalf("served %q", body)
	}
	if _, _, err := s.Open(sess.ID, second.Token); !errors.Is(err, ErrNotStaged) {
		t.Fatalf("the spent token: err = %v, want ErrNotStaged", err)
	}

	// Opened but not served whole: the Job broke midway, and a new Seal serves
	// the same bytes again.
	third, err := s.Seal("u1", "survival", sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if body := readStaged(t, s, sess.ID, third.Token); body != "abcd" {
		t.Fatalf("served %q after a new Seal", body)
	}
}

// Served deletes only sessions: an upload staged by Put belongs to the release
// func Put returned.
func TestServedLeavesPutAlone(t *testing.T) {
	s := roomyStage(t)
	st, release, err := s.Put(strings.NewReader("abc"), 3)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	s.Served(st.ID)
	if body := readStaged(t, s, st.ID, st.Token); body != "abc" {
		t.Fatalf("served %q", body)
	}
}

// A session reserves its whole size when it begins, and gives back what it has
// not yet received when it is dropped or expires.
func TestSessionReservesItsSize(t *testing.T) {
	t.Run("begin reserves the whole size", func(t *testing.T) {
		s := diskStage(t, 1000, 1200, 0.5) // room for 400
		if _, err := s.Begin("u1", "survival", "a.zip", 300); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Begin("u2", "survival", "b.zip", 101); !errors.Is(err, ErrStageFull) {
			t.Fatalf("101 bytes beside a 300-byte session: err = %v, want ErrStageFull", err)
		}
		if _, err := s.Begin("u2", "survival", "b.zip", 100); err != nil {
			t.Fatalf("100 bytes beside a 300-byte session: %v", err)
		}
	})

	t.Run("a part moves its room from the reservation to the disk", func(t *testing.T) {
		s := diskStage(t, 1000, 1200, 0.5)
		sess, err := s.Begin("u1", "survival", "a.zip", 300)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := appendString(s, "u1", "survival", sess.ID, 0, strings.Repeat("x", 200)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Begin("u2", "survival", "b.zip", 101); !errors.Is(err, ErrStageFull) {
			t.Fatalf("after a part landed: err = %v, want ErrStageFull", err)
		}
		if _, err := s.Begin("u2", "survival", "b.zip", 100); err != nil {
			t.Fatalf("after a part landed: %v", err)
		}
	})

	t.Run("drop gives it all back", func(t *testing.T) {
		s := diskStage(t, 1000, 1200, 0.5)
		sess, err := s.Begin("u1", "survival", "a.zip", 300)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := appendString(s, "u1", "survival", sess.ID, 0, strings.Repeat("x", 200)); err != nil {
			t.Fatal(err)
		}
		if err := s.Drop("u1", "survival", sess.ID); err != nil {
			t.Fatal(err)
		}
		if names := stagedNames(t, s); len(names) != 0 {
			t.Fatalf("after Drop: %v", names)
		}
		if _, err := s.Begin("u2", "survival", "b.zip", 400); err != nil {
			t.Fatalf("after Drop: %v", err)
		}
		if _, err := s.Status("u1", "survival", sess.ID); !errors.Is(err, ErrNotStaged) {
			t.Fatalf("Status after Drop: err = %v", err)
		}
	})

	t.Run("expiry gives it all back", func(t *testing.T) {
		s := diskStage(t, 1000, 1200, 0.5)
		now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
		s.Now = func() time.Time { return now }
		sess, err := s.Begin("u1", "survival", "a.zip", 300)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := appendString(s, "u1", "survival", sess.ID, 0, strings.Repeat("x", 200)); err != nil {
			t.Fatal(err)
		}
		now = now.Add(SessionIdle + time.Nanosecond)
		if n := s.Expire(); n != 1 {
			t.Fatalf("Expire dropped %d, want 1", n)
		}
		if _, err := s.Begin("u2", "survival", "b.zip", 400); err != nil {
			t.Fatalf("after Expire: %v", err)
		}
	})
}

func TestSessionsPerUserAreBounded(t *testing.T) {
	s := diskStage(t, 1000, 1200, 0.5)
	var ids []string
	for i := range MaxSessionsPerUser {
		sess, err := s.Begin("u1", "survival", "a.zip", 10)
		if err != nil {
			t.Fatalf("session %d: %v", i+1, err)
		}
		ids = append(ids, sess.ID)
	}
	if _, err := s.Begin("u1", "creative", "a.zip", 10); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("one more on another server: err = %v, want ErrTooManySessions", err)
	}
	// The refused Begin kept neither a file nor its reservation: room for 400
	// less the four sessions' 40.
	if names := stagedNames(t, s); len(names) != MaxSessionsPerUser {
		t.Fatalf("staged %d files, want %d", len(names), MaxSessionsPerUser)
	}
	if _, err := s.Begin("u2", "survival", "b.zip", 360); err != nil {
		t.Fatalf("another user: %v", err)
	}
	if err := s.Drop("u1", "survival", ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin("u1", "survival", "a.zip", 10); err != nil {
		t.Fatalf("after dropping one: %v", err)
	}
	// With room reserved, a negative size would wrap the reservation sum round
	// to a small number and pass the room check.
	if _, err := s.Begin("u3", "survival", "a.zip", -1); err == nil || errors.Is(err, ErrStageFull) {
		t.Fatalf("a negative size: err = %v, want it refused for being negative", err)
	}
}

// Expire drops what has sat untouched for longer than SessionIdle, sealed or
// not, and a part keeps a session alive.
func TestSessionExpiry(t *testing.T) {
	s := roomyStage(t)
	start := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	now := start
	s.Now = func() time.Time { return now }
	idle, err := s.Begin("u1", "survival", "idle.zip", 2)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s.Begin("u1", "survival", "sealed.zip", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendString(s, "u1", "survival", sealed.ID, 0, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Seal("u1", "survival", sealed.ID); err != nil {
		t.Fatal(err)
	}
	active, err := s.Begin("u1", "survival", "active.zip", 2)
	if err != nil {
		t.Fatal(err)
	}

	now = start.Add(time.Hour)
	if _, err := appendString(s, "u1", "survival", active.ID, 0, "a"); err != nil {
		t.Fatal(err)
	}
	now = start.Add(SessionIdle)
	if n := s.Expire(); n != 0 {
		t.Fatalf("at exactly SessionIdle Expire dropped %d", n)
	}
	now = start.Add(SessionIdle + time.Minute)
	if n := s.Expire(); n != 2 {
		t.Fatalf("Expire dropped %d, want the idle and the sealed one", n)
	}
	for _, id := range []string{idle.ID, sealed.ID} {
		if _, err := s.Status("u1", "survival", id); !errors.Is(err, ErrNotStaged) {
			t.Errorf("%s survived Expire: %v", id, err)
		}
	}
	if at, err := s.Status("u1", "survival", active.ID); err != nil || at.Received != 1 {
		t.Fatalf("the session a part touched: %+v, %v", at, err)
	}
	if names := stagedNames(t, s); len(names) != 1 {
		t.Fatalf("files left: %v, want the active session's", names)
	}

	// A Seal, and the Job's Open, each start the idle clock again: a Job begun
	// on an upload that sat for hours still finds it there.
	for _, tc := range []struct {
		name string
		// sealAt and openAt are how long after the last part the Seal and the
		// Job's Open come; a negative openAt is no Open.
		sealAt, openAt time.Duration
	}{
		{"seal", 4 * time.Hour, -1},
		{"open", 0, 4 * time.Hour},
	} {
		begun := now
		sess, err := s.Begin("u1", "survival", tc.name+".zip", 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := appendString(s, "u1", "survival", sess.ID, 0, "x"); err != nil {
			t.Fatal(err)
		}
		now = begun.Add(tc.sealAt)
		st, err := s.Seal("u1", "survival", sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if tc.openAt >= 0 {
			now = begun.Add(tc.openAt)
			readStaged(t, s, sess.ID, st.Token)
		}
		now = begun.Add(SessionIdle + time.Minute)
		s.Expire()
		if _, err := s.Status("u1", "survival", sess.ID); err != nil {
			t.Errorf("%s: a session touched 4h after its last part expired 6h after it: %v", tc.name, err)
		}
		if err := s.Drop("u1", "survival", sess.ID); err != nil {
			t.Fatal(err)
		}
	}
}
