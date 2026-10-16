package software

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Fetcher downloads one File to dest and checks it against its digest; a
// returned nil error means dest holds exactly that content.
type Fetcher interface {
	Fetch(ctx context.Context, f File, dest string) error
}

// RecordPath is the installation record, relative to the data directory.
const RecordPath = ManagedDir + "/installed.json"

// Record is what the installer leaves on the volume: the plan it carried out
// and how to launch the result. `felis launch` reads Launch from it.
type Record struct {
	Format      int       `json:"format"`
	Plan        Plan      `json:"plan"`
	Launch      Launch    `json:"launch"`
	InstalledAt time.Time `json:"installedAt"`
}

// Installer installs server software into a data directory.
type Installer struct {
	DataDir  string
	Resolver *Resolver
	Fetch    Fetcher
	// Java is the java binary that runs a Forge or NeoForge installer.
	Java string
	Log  io.Writer
	// InstallerTimeout bounds a Forge or NeoForge installer run (default 20
	// minutes; it downloads and patches several hundred megabytes on a slow
	// line).
	InstallerTimeout time.Duration
}

func (in *Installer) logf(format string, a ...any) {
	if in.Log != nil {
		fmt.Fprintf(in.Log, format+"\n", a...)
	}
}

func (in *Installer) abs(rel string) string {
	return filepath.Join(in.DataDir, filepath.FromSlash(rel))
}

// ReadRecord reads the installation record; a missing one is (nil, nil).
func ReadRecord(dataDir string) (*Record, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(RecordPath)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("software: parse %s: %w", RecordPath, err)
	}
	return &rec, nil
}

// Install brings the data directory to spec s and returns how to launch it. A
// record of the same spec is carried out again without reading any metadata:
// every file is re-checked against its digest and fetched only when missing or
// changed, so a server whose volume is intact starts while upstream is down. A
// changed spec is planned afresh, and the files the old plan placed that the new
// one does not use are removed.
func (in *Installer) Install(ctx context.Context, s Spec) (*Launch, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if s.Type == Custom {
		return in.installCustom(s)
	}
	prev, err := ReadRecord(in.DataDir)
	if err != nil {
		in.logf("felis install-server: ignoring an unreadable installation record: %v", err)
		prev = nil
	}
	var plan *Plan
	reuse := prev != nil && prev.Format == PlanFormat && prev.Plan.Format == PlanFormat && prev.Plan.Spec.Key() == s.Key()
	if reuse {
		plan = &prev.Plan
		in.logf("felis install-server: %s %s %s is installed; checking its files", s.Type, s.MinecraftVersion, s.Build)
	} else {
		in.logf("felis install-server: planning %s %s %s", s.Type, s.MinecraftVersion, s.Build)
		if plan, err = in.Resolver.Plan(ctx, s); err != nil {
			return nil, err
		}
	}

	for _, f := range plan.Files {
		if err := in.ensure(ctx, f); err != nil {
			return nil, err
		}
	}
	if plan.Paperclip && len(plan.Files) > 0 {
		in.prefetchPaperclip(ctx, plan.Files[0])
	}

	launch := plan.Launch
	if plan.Installer != nil {
		var installed *Launch
		if reuse {
			installed = &prev.Launch
		}
		l, err := in.runForgeInstaller(ctx, plan, installed)
		if err != nil {
			return nil, err
		}
		launch = l
	}
	if err := in.placeMods(ctx, plan.Mods); err != nil {
		return nil, err
	}
	if prev != nil && !reuse {
		in.removeStale(prev.Plan, *plan)
	}

	rec := Record{Format: PlanFormat, Plan: *plan, Launch: launch, InstalledAt: time.Now().UTC()}
	if err := in.writeRecord(rec); err != nil {
		return nil, err
	}
	return &launch, nil
}

func (in *Installer) installCustom(s Spec) (*Launch, error) {
	st, err := os.Stat(in.abs(s.Jar))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("software: the custom jar %s is not in the server's files yet; upload it, then start the server", s.Jar)
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("software: the custom jar %s is not a regular file", s.Jar)
	}
	launch := Launch{Jar: s.Jar, GameArgs: []string{"nogui"}}
	rec := Record{Format: PlanFormat, Plan: Plan{Format: PlanFormat, Spec: s, Launch: launch}, Launch: launch, InstalledAt: time.Now().UTC()}
	if err := in.writeRecord(rec); err != nil {
		return nil, err
	}
	return &launch, nil
}

// ensure makes f present at its path with its digest.
func (in *Installer) ensure(ctx context.Context, f File) error {
	if err := safeRel(f.Path); err != nil {
		return err
	}
	dest := in.abs(f.Path)
	ok, err := FileMatches(dest, f.Digest)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in.logf("felis install-server: fetching %s", f.Path)
	if err := in.Fetch.Fetch(ctx, f, dest); err != nil {
		return fmt.Errorf("software: fetch %s: %w", f.Path, err)
	}
	return nil
}

// safeRel refuses a path that leaves the data directory.
func safeRel(p string) error {
	clean := path.Clean(p)
	if p == "" || strings.HasPrefix(p, "/") || clean == ".." || strings.HasPrefix(clean, "../") || clean != p {
		return fmt.Errorf("software: refusing path %q outside the server directory", p)
	}
	return nil
}

func (in *Installer) prefetchPaperclip(ctx context.Context, jar File) {
	f, err := PaperclipDownload(in.abs(jar.Path))
	if err != nil {
		in.logf("felis install-server: not prefetching the vanilla jar: %v", err)
		return
	}
	if f == nil {
		return
	}
	if err := in.ensure(ctx, *f); err != nil {
		// Paperclip downloads it itself on first start; the cache only saves
		// the trip.
		in.logf("felis install-server: the vanilla jar could not be prefetched (the server will download it): %v", err)
	}
}

// runForgeInstaller places the installer, the libraries it lists and the
// vanilla jar, then runs it unless an earlier run for the same spec left a
// launch whose files are all still there.
func (in *Installer) runForgeInstaller(ctx context.Context, plan *Plan, installed *Launch) (Launch, error) {
	inst := plan.Installer
	if err := in.ensure(ctx, inst.Jar); err != nil {
		return Launch{}, err
	}
	if installed != nil && in.launchPresent(*installed) {
		return *installed, nil
	}
	libs, serverJar, err := ForgeLibraries(in.abs(inst.Jar.Path), plan.Spec.MinecraftVersion)
	if err != nil {
		return Launch{}, err
	}
	if plan.MojangServer != nil && serverJar != "" {
		mj := *plan.MojangServer
		mj.Path = serverJar
		if err := in.ensure(ctx, mj); err != nil {
			in.logf("felis install-server: the vanilla jar could not be placed ahead (the installer will download it): %v", err)
		}
	}
	for _, f := range libs {
		if err := in.ensure(ctx, f); err != nil {
			in.logf("felis install-server: a library could not be placed ahead (the installer will download it): %v", err)
		}
	}
	timeout := in.InstallerTimeout
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	java := in.Java
	if java == "" {
		java = "java"
	}
	in.logf("felis install-server: running the %s installer %s", inst.Type, inst.Version)
	cmd := exec.CommandContext(runCtx, java, "-jar", in.abs(inst.Jar.Path), "--installServer", in.DataDir)
	cmd.Dir = in.DataDir
	if in.Log != nil {
		cmd.Stdout, cmd.Stderr = in.Log, in.Log
	}
	if err := cmd.Run(); err != nil {
		return Launch{}, fmt.Errorf("software: the %s installer failed: %w", inst.Type, err)
	}
	l, err := ForgeLaunch(in.DataDir, *inst)
	if err != nil {
		return Launch{}, err
	}
	l.JVMArgs = append(append([]string{}, plan.Launch.JVMArgs...), l.JVMArgs...)
	return l, nil
}

// launchPresent reports whether every file a launch names is still on disk.
func (in *Installer) launchPresent(l Launch) bool {
	var need []string
	if l.Jar != "" {
		need = append(need, l.Jar)
	}
	for _, a := range l.Args {
		if strings.HasPrefix(a, "@") {
			need = append(need, strings.TrimPrefix(a, "@"))
		} else if strings.HasSuffix(a, ".jar") {
			need = append(need, a)
		}
	}
	if len(need) == 0 {
		return false
	}
	for _, p := range need {
		if safeRel(p) != nil {
			return false
		}
		if _, err := os.Stat(in.abs(p)); err != nil {
			return false
		}
	}
	return true
}

// placeMods installs the forwarding mods, each skipped when one of the owner's
// own jars already declares its mod id, and removes managed copies the plan no
// longer names or the owner now provides.
func (in *Installer) placeMods(ctx context.Context, mods []Mod) error {
	dir := in.abs(ModsDir)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ownIDs := map[string]bool{}
	want := map[string]bool{}
	for _, m := range mods {
		want[path.Base(m.Path)] = true
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jar") {
			continue
		}
		if strings.HasPrefix(name, ManagedPrefix) {
			if !want[name] {
				_ = os.Remove(filepath.Join(dir, name))
			}
			continue
		}
		for _, id := range JarModIDs(filepath.Join(dir, name)) {
			ownIDs[id] = true
		}
	}
	for _, m := range mods {
		if ownIDs[m.ModID] {
			_ = os.Remove(in.abs(m.Path))
			in.logf("felis install-server: using the server's own %s", m.ModID)
			continue
		}
		if err := in.ensure(ctx, m.File); err != nil {
			return err
		}
	}
	return nil
}

// removeStale deletes the files the old plan placed that the new one does not
// use. Only paths the platform manages are touched: a file the old plan put in
// libraries/ is removed, a world or a config never is, because no plan lists
// one.
func (in *Installer) removeStale(old, cur Plan) {
	keep := map[string]bool{}
	for _, f := range cur.Files {
		keep[f.Path] = true
	}
	if cur.Installer != nil {
		keep[cur.Installer.Jar.Path] = true
	}
	stale := append([]File{}, old.Files...)
	if old.Installer != nil {
		stale = append(stale, old.Installer.Jar)
	}
	for _, f := range stale {
		if keep[f.Path] || safeRel(f.Path) != nil {
			continue
		}
		if strings.HasPrefix(f.Path, ManagedDir+"/") || strings.HasPrefix(f.Path, LibrariesDir+"/") {
			_ = os.Remove(in.abs(f.Path))
		}
	}
}

func (in *Installer) writeRecord(rec Record) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	p := in.abs(RecordPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
