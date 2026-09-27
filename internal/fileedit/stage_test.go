package fileedit

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
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

var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)
var hexToken = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TestStageOpensOnce: a staged upload opens once, for the token minted with it,
// and a wrong token does not spend it.
func TestStageOpensOnce(t *testing.T) {
	s := roomyStage(t)
	const body = "PK\x03\x04 staged"
	st, release, err := s.Put(strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !hexID.MatchString(st.ID) || !hexToken.MatchString(st.Token) ||
		st.Size != int64(len(body)) || st.SHA256 != digest([]byte(body)) {
		t.Fatalf("staged = %+v", st)
	}
	other, releaseOther, err := s.Put(strings.NewReader(body), int64(len(body)))
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

// Staged uploads are other people's files, so neither the folder nor the file
// is readable by anyone but felis-api's own uid.
func TestStageIsPrivate(t *testing.T) {
	s := roomyStage(t)
	_, release, err := s.Put(strings.NewReader("x"), 1)
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

// eofReader serves body and records whether it was read to its end.
type eofReader struct {
	r      io.Reader
	hitEOF bool
}

func (e *eofReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		e.hitEOF = true
	}
	return n, err
}

// Put reads the body to its end: net/http's body deadline is lifted only then
// (withBodyDeadline), and a request whose body was not finished would otherwise
// be cut off under the handler still landing it.
func TestStagePutReadsToTheEnd(t *testing.T) {
	s := roomyStage(t)
	body := &eofReader{r: strings.NewReader("abc")}
	_, release, err := s.Put(body, 3)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	defer release()
	if !body.hitEOF {
		t.Fatal("Put stopped at the declared size without reading the end of the body")
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
			_, release, err := s.Put(tc.body, tc.size)
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
	if _, _, err := roomyStage(t).Put(strings.NewReader(""), -1); err == nil {
		t.Fatal("a negative size was accepted")
	}
}

// TestStageKeepsItsFloor: an upload is refused if it would leave less than
// MinFree of the disk free, counting uploads still arriving.
func TestStageKeepsItsFloor(t *testing.T) {
	put := func(s *Stage, size int64) error {
		_, release, err := s.Put(strings.NewReader(strings.Repeat("x", int(size))), size)
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
			_, release, err := s.Put(pr, 300)
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
	if _, release, err := s.Put(strings.NewReader("x"), 1); err != nil {
		t.Fatalf("Put after Sweep: %v", err)
	} else {
		release()
	}
}
