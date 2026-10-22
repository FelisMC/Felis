package fileedit

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stubStatfs makes every staging disk report avail of total bytes free.
func stubStatfs(t *testing.T, avail, total uint64) {
	t.Helper()
	prev := statfs
	statfs = func(string) (uint64, uint64, error) { return avail, total, nil }
	t.Cleanup(func() { statfs = prev })
}

// roomyStage is a stage on a disk with room for anything a test stages.
func roomyStage(t *testing.T) *Stage {
	t.Helper()
	stubStatfs(t, 1<<40, 1<<41)
	return &Stage{Dir: filepath.Join(t.TempDir(), "stage")}
}

func stagedNames(t *testing.T, s *Stage) []string {
	t.Helper()
	des, err := os.ReadDir(s.Dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

// sumOf is the SHA-256 a client sends with s.
func sumOf(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)
var hexToken = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestStageOpensOnce(t *testing.T) {
	s := roomyStage(t)
	const body = "PK\x03\x04 staged"
	st, release, err := s.Put(strings.NewReader(body), int64(len(body)), sumOf(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !hexID.MatchString(st.ID) || !hexToken.MatchString(st.Token) ||
		st.Size != int64(len(body)) || st.SHA256 != digest([]byte(body)) {
		t.Fatalf("staged = %+v", st)
	}
	other, releaseOther, err := s.Put(strings.NewReader(body), int64(len(body)), sumOf(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	defer releaseOther()
	if other.ID == st.ID || other.Token == st.Token {
		t.Fatal("two uploads share an id or a token")
	}

	for name, try := range map[string][2]string{
		"unknown id":             {strings.Repeat("0", 32), st.Token},
		"wrong token":            {st.ID, strings.Repeat("0", 64)},
		"another upload's token": {st.ID, other.Token},
		"no token":               {st.ID, ""},
	} {
		if f, _, err := s.Open(try[0], try[1]); !errors.Is(err, ErrNotStaged) {
			if f != nil {
				f.Close()
			}
			t.Fatalf("%s: err = %v, want ErrNotStaged", name, err)
		}
	}

	f, size, err := s.Open(st.ID, st.Token)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != body || size != int64(len(body)) {
		t.Fatalf("opened %q (%d bytes), want %q", got, size, body)
	}
	if _, _, err := s.Open(st.ID, st.Token); !errors.Is(err, ErrNotStaged) {
		t.Fatalf("second Open: err = %v, want ErrNotStaged", err)
	}

	release()
	if names := stagedNames(t, s); len(names) != 1 {
		t.Fatalf("after release: %v, want only the other upload", names)
	}
	if _, _, err := s.Open(other.ID, other.Token); err != nil {
		t.Fatalf("releasing one upload spent another: %v", err)
	}
}

func TestStageIsPrivate(t *testing.T) {
	s := roomyStage(t)
	_, release, err := s.Put(strings.NewReader("x"), 1, sumOf("x"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	defer release()
	dir, err := os.Stat(s.Dir)
	if err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatalf("stage folder = %v, %v; want 0700", dir.Mode().Perm(), err)
	}
	names := stagedNames(t, s)
	file, err := os.Stat(filepath.Join(s.Dir, names[0]))
	if err != nil || file.Mode().Perm() != 0o600 {
		t.Fatalf("staged file = %v, %v; want 0600", file.Mode().Perm(), err)
	}
}

// eofReader serves r and records whether it was read at all, and to its end.
type eofReader struct {
	r            io.Reader
	read, hitEOF bool
}

func (e *eofReader) Read(p []byte) (int, error) {
	e.read = true
	n, err := e.r.Read(p)
	if err == io.EOF {
		e.hitEOF = true
	}
	return n, err
}

func TestStagePutReadsToTheEnd(t *testing.T) {
	s := roomyStage(t)
	body := &eofReader{r: strings.NewReader("abc")}
	_, release, err := s.Put(body, 3, sumOf("abc"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	defer release()
	if !body.hitEOF {
		t.Fatal("Put stopped at the declared size without reading the end of the body")
	}
}

func TestStageChecksTheDigest(t *testing.T) {
	stubStatfs(t, 1000, 1200) // floor 600 at MinFree 0.5: room for 400
	s := &Stage{Dir: t.TempDir(), MinFree: 0.5}
	body := strings.Repeat("x", 400)
	_, _, err := s.Put(strings.NewReader(body), 400, sumOf(strings.Repeat("y", 400)))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("a wrong digest: err = %v, want ErrDigestMismatch", err)
	}
	if names := stagedNames(t, s); len(names) != 0 {
		t.Fatalf("left behind: %v", names)
	}
	st, release, err := s.Put(strings.NewReader(body), 400, sumOf(body))
	if err != nil {
		t.Fatalf("the same bytes with their digest, in the room the refused ones held: %v", err)
	}
	defer release()
	if st.SHA256 != digest([]byte(body)) {
		t.Fatalf("staged %+v", st)
	}

	for name, want := range map[string][]byte{"none": nil, "too short": sumOf("x")[:31]} {
		unread := &eofReader{r: strings.NewReader("abc")}
		if _, _, err := roomyStage(t).Put(unread, 3, want); !errors.Is(err, ErrNoDigest) || unread.read {
			t.Errorf("%s: err = %v, body read = %v; want ErrNoDigest before any read", name, err, unread.read)
		}
	}
}

func TestStageRefusesABodyOfTheWrongLength(t *testing.T) {
	for name, tc := range map[string]struct {
		body  io.Reader
		size  int64
		short bool
	}{
		"ends early": {strings.NewReader("abc"), 5, true},
		"breaks":     {io.MultiReader(strings.NewReader("ab"), errReader{io.ErrUnexpectedEOF}), 5, true},
		// The client's own failure is a short upload, whatever errno it carries.
		"breaks with ENOSPC": {io.MultiReader(strings.NewReader("ab"), errReader{syscall.ENOSPC}), 5, true},
		"runs long":          {strings.NewReader("abcdef"), 3, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := roomyStage(t)
			_, release, err := s.Put(tc.body, tc.size, sumOf(""))
			if err == nil {
				release()
				t.Fatal("Put accepted it")
			}
			if errors.Is(err, ErrShortUpload) != tc.short || errors.Is(err, ErrStageFull) {
				t.Fatalf("err = %v, want short upload = %v", err, tc.short)
			}
			if names := stagedNames(t, s); len(names) != 0 {
				t.Fatalf("left behind: %v", names)
			}
		})
	}
	if _, _, err := roomyStage(t).Put(strings.NewReader(""), -1, sumOf("")); err == nil {
		t.Fatal("a negative size was accepted")
	}
}

func TestStageKeepsItsFloor(t *testing.T) {
	put := func(s *Stage, size int64) error {
		_, release, err := s.Put(strings.NewReader(strings.Repeat("x", int(size))), size, sumOf(strings.Repeat("x", int(size))))
		if err == nil {
			release()
		}
		return err
	}

	t.Run("exactly at the floor is allowed, one byte past is not", func(t *testing.T) {
		stubStatfs(t, 1000, 1200) // floor 600 at MinFree 0.5: room for 400
		s := &Stage{Dir: t.TempDir(), MinFree: 0.5}
		if err := put(s, 400); err != nil {
			t.Fatalf("400 bytes: %v", err)
		}
		if err := put(s, 401); !errors.Is(err, ErrStageFull) {
			t.Fatalf("401 bytes: err = %v, want ErrStageFull", err)
		}
	})

	t.Run("an unset MinFree is the default", func(t *testing.T) {
		stubStatfs(t, 1400, 10000) // floor 1000 at 10%: room for 400
		s := &Stage{Dir: t.TempDir()}
		if err := put(s, 400); err != nil {
			t.Fatalf("400 bytes: %v", err)
		}
		if err := put(s, 401); !errors.Is(err, ErrStageFull) {
			t.Fatalf("401 bytes: err = %v, want ErrStageFull", err)
		}
	})

	t.Run("more than is free at all", func(t *testing.T) {
		stubStatfs(t, 300, 1<<40)
		s := &Stage{Dir: t.TempDir(), MinFree: 1e-12}
		if err := put(s, 301); !errors.Is(err, ErrStageFull) {
			t.Fatalf("err = %v, want ErrStageFull", err)
		}
	})

	// statfs cannot see an upload still arriving, so the reservation is what keeps
	// two of them from passing on room for one.
	t.Run("an upload in flight holds its room", func(t *testing.T) {
		stubStatfs(t, 1000, 1200) // room for 400
		s := &Stage{Dir: t.TempDir(), MinFree: 0.5}
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() {
			_, release, err := s.Put(pr, 300, sumOf(strings.Repeat("x", 300)))
			if err == nil {
				release()
			}
			done <- err
		}()
		// The write returns once Put is copying, which is after it reserved.
		if _, err := pw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := put(s, 200); !errors.Is(err, ErrStageFull) {
			t.Fatalf("second upload beside one in flight: err = %v, want ErrStageFull", err)
		}
		if _, err := pw.Write([]byte(strings.Repeat("x", 299))); err != nil {
			t.Fatal(err)
		}
		pw.Close()
		if err := <-done; err != nil {
			t.Fatalf("the upload in flight: %v", err)
		}
		if err := put(s, 200); err != nil {
			t.Fatalf("after the first finished: %v", err)
		}
	})
}

func TestStageSweep(t *testing.T) {
	s := roomyStage(t)
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "upload-left-by-a-crash"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Sweep(); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if names := stagedNames(t, s); len(names) != 0 {
		t.Fatalf("after Sweep: %v", names)
	}
	if _, release, err := s.Put(strings.NewReader("x"), 1, sumOf("x")); err != nil {
		t.Fatalf("Put after Sweep: %v", err)
	} else {
		release()
	}
}

func TestReceiveSealsAndCleansAStream(t *testing.T) {
	for _, size := range []int64{-1, 3} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s := roomyStage(t)
			st, drop, err := s.Receive("owner", "survival", "plugins/a.jar", strings.NewReader("jar"), size)
			if err != nil {
				t.Fatal(err)
			}
			defer drop()
			if st.Size != 3 || st.SHA256 != digest([]byte("jar")) {
				t.Fatalf("received %+v", st)
			}
			f, n, err := s.Open(st.ID, st.Token)
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(f)
			f.Close()
			if err != nil || n != 3 || string(b) != "jar" {
				t.Fatalf("received %q (%d): %v", b, n, err)
			}
			if _, _, err := s.Open(st.ID, st.Token); !errors.Is(err, ErrNotStaged) {
				t.Fatalf("token was reusable: %v", err)
			}
			retry, err := s.Seal("owner", "survival", st.ID)
			if err != nil || retry.SHA256 != st.SHA256 {
				t.Fatalf("retry = %+v, %v", retry, err)
			}
			if err := s.Landed(st.ID, retry.Token); err != nil || len(stagedNames(t, s)) != 0 {
				t.Fatalf("landing left staged content: %v", err)
			}
		})
	}
}

func TestReceiveBoundsUnknownLengthAndReleasesItsReservation(t *testing.T) {
	stubStatfs(t, 1000, 1200)
	s := &Stage{Dir: t.TempDir(), MinFree: 0.5}
	for _, tc := range []struct {
		body io.Reader
		err  error
	}{
		{strings.NewReader(strings.Repeat("x", 401)), ErrStageFull},
		{io.MultiReader(strings.NewReader("x"), errReader{io.ErrUnexpectedEOF}), ErrShortUpload},
	} {
		if _, _, err := s.Receive("owner", "survival", "a", tc.body, -1); !errors.Is(err, tc.err) {
			t.Fatalf("received err = %v, want %v", err, tc.err)
		}
		if len(stagedNames(t, s)) != 0 {
			t.Fatal("failed reception retained bytes")
		}
	}
	st, drop, err := s.Receive("owner", "survival", "a", strings.NewReader(strings.Repeat("x", 400)), -1)
	if err != nil || st.Size != 400 {
		t.Fatalf("the full budget after errors: size = %d, err = %v", st.Size, err)
	}
	drop()
	if len(stagedNames(t, s)) != 0 {
		t.Fatal("cancel retained bytes")
	}
}

func TestReceiveReservesSpaceAgainstConcurrentUploads(t *testing.T) {
	stubStatfs(t, 1000, 1200)
	s := &Stage{Dir: t.TempDir(), MinFree: 0.5}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, drop, err := s.Receive("owner", "survival", "a", pr, -1)
		if err == nil {
			drop()
		}
		done <- err
	}()
	if _, err := pw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Put(strings.NewReader("x"), 1, sumOf("x")); !errors.Is(err, ErrStageFull) {
		t.Fatalf("a second upload took the stream's reservation: %v", err)
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_, drop, err := s.Put(strings.NewReader("x"), 1, sumOf("x"))
	if err != nil {
		t.Fatalf("reservation was not released: %v", err)
	}
	drop()
}

func TestReceiveExpiresUnlandedFilesAndSharesTheSessionLimit(t *testing.T) {
	s := roomyStage(t)
	now := time.Now()
	s.Now = func() time.Time { return now }
	for i := 0; i < MaxSessionsPerUser; i++ {
		if _, _, err := s.Receive("owner", "survival", "a", strings.NewReader("x"), -1); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Receive("owner", "survival", "a", strings.NewReader("x"), -1); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("session limit = %v", err)
	}
	if len(stagedNames(t, s)) != MaxSessionsPerUser {
		t.Fatal("the refused reception retained a file")
	}
	now = now.Add(SessionIdle + time.Second)
	if n := s.Expire(); n != MaxSessionsPerUser || len(stagedNames(t, s)) != 0 {
		t.Fatalf("expired %d files", n)
	}
}

func TestReceiveCountsInFlightSessionsBeforeReading(t *testing.T) {
	s := roomyStage(t)
	for i := 0; i < MaxSessionsPerUser-1; i++ {
		if _, err := s.Begin("owner", "survival", "a", 0); err != nil {
			t.Fatal(err)
		}
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, drop, err := s.Receive("owner", "survival", "a", pr, 1)
		if err == nil {
			drop()
		}
		done <- err
	}()
	if _, err := pw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	unread := &eofReader{r: strings.NewReader("x")}
	if _, _, err := s.Receive("owner", "survival", "b", unread, 1); !errors.Is(err, ErrTooManySessions) || unread.read {
		t.Fatalf("full session quota read body=%v, err=%v", unread.read, err)
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, drop, err := s.Receive("owner", "survival", "b", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	} else {
		drop()
	}
}
