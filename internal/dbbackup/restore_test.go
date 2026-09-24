package dbbackup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func takeBackup(t *testing.T, pg *fakePG, dir string, when time.Time) string {
	t.Helper()
	path, err := Backup(context.Background(), BackupOptions{DatabaseURL: testURL, Dir: dir, Label: LabelManual, Tools: pg.tools, Now: at(when)})
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func restore(pg *fakePG, dir, bundle string, mut func(*RestoreOptions)) (string, error) {
	o := RestoreOptions{DatabaseURL: testURL, Bundle: bundle, Dir: dir, Tools: pg.tools,
		Safety: BackupOptions{Now: at(t0.Add(time.Hour))}}
	if mut != nil {
		mut(&o)
	}
	_, safety, err := Restore(context.Background(), o)
	return safety, err
}

func TestRestoreReplacesTheDatabase(t *testing.T) {
	pg := newFakePG(t, "alice\n")
	dir := t.TempDir()
	bundle := takeBackup(t, pg, dir, t0)
	pg.setDB(t, "alice\nbob\n")

	safety, err := restore(pg, dir, bundle, nil)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := pg.db(t); got != "alice\n" {
		t.Fatalf("db after restore = %q", got)
	}
	// The replay dropped what the role owns inside the same transaction.
	stdin, _ := os.ReadFile(filepath.Join(pg.dir, "psql.stdin"))
	if !strings.HasPrefix(string(stdin), "BEGIN;\n"+dropOwned) || !strings.HasSuffix(string(stdin), "COMMIT;\n") {
		t.Fatalf("psql input = %q", stdin)
	}

	// The safety bundle holds what was replaced, and restores it.
	if filepath.Base(safety) != "felis-db-20260924T043000Z-pre-restore.tar" {
		t.Fatalf("safety = %s", safety)
	}
	// The dump brought back its own, older freshness record; the restore
	// points it at the newest bundle on disk again.
	if st := pg.recorded(t); st.Name != filepath.Base(safety) || st.Label != LabelPreRestore || st.SchemaVersion != 21 || st.Dir != dir {
		t.Fatalf("recorded after restore = %+v", st)
	}
	if _, err := restore(pg, dir, safety, func(o *RestoreOptions) { o.SkipSafetyBackup = true }); err != nil {
		t.Fatal(err)
	}
	if got := pg.db(t); got != "alice\nbob\n" {
		t.Fatalf("db after undo = %q", got)
	}
}

func TestRestoreRefusesLiveClients(t *testing.T) {
	pg := newFakePG(t, "alice\n")
	dir := t.TempDir()
	bundle := takeBackup(t, pg, dir, t0)
	pg.setDB(t, "changed\n")
	pg.flag(t, "clients", "2\n")

	if _, err := restore(pg, dir, bundle, nil); !errors.Is(err, ErrClientsConnected) {
		t.Fatalf("err = %v, want ErrClientsConnected", err)
	}
	if pg.db(t) != "changed\n" {
		t.Fatal("database touched despite the refusal")
	}
	if _, err := restore(pg, dir, bundle, func(o *RestoreOptions) { o.Force = true }); err != nil {
		t.Fatalf("forced: %v", err)
	}
	if pg.db(t) != "alice\n" {
		t.Fatal("forced restore did not apply")
	}
}

func TestRestoreFailureLeavesTheDatabaseAlone(t *testing.T) {
	for _, tc := range []struct {
		flag, want string
	}{
		// The replay fails in the server: psql stops, the transaction dies with it.
		{"psql_fail", "already exists"},
		// The dump reader dies after streaming everything: psql never gets the
		// COMMIT that only a clean pg_restore exit releases.
		{"restore_fail", "could not read input"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			pg := newFakePG(t, "alice\n")
			dir := t.TempDir()
			bundle := takeBackup(t, pg, dir, t0)
			pg.setDB(t, "current\n")
			pg.flag(t, tc.flag, "")
			_, err := restore(pg, dir, bundle, func(o *RestoreOptions) { o.SkipSafetyBackup = true })
			if err == nil || !strings.Contains(err.Error(), "rolled back") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want rolled back + %q", err, tc.want)
			}
			if got := pg.db(t); got != "current\n" {
				t.Fatalf("db = %q, want it untouched", got)
			}
		})
	}
}

func TestRestoreRefusesACorruptBundle(t *testing.T) {
	pg := newFakePG(t, "alice\n")
	dir := t.TempDir()
	bundle := takeBackup(t, pg, dir, t0)
	if err := os.WriteFile(bundle+".sha256", []byte("0000  x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pg.setDB(t, "current\n")
	if _, err := restore(pg, dir, bundle, nil); err == nil {
		t.Fatal("restored a bundle its checksum disowns")
	}
	if pg.db(t) != "current\n" {
		t.Fatal("database touched")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".restore-") {
			t.Errorf("scratch dump %s left behind", e.Name())
		}
		if strings.Contains(e.Name(), LabelPreRestore) {
			t.Errorf("safety bundle %s taken before the bundle was verified", e.Name())
		}
	}
}
