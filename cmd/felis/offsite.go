package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/dbbackup"
	"felis.lolicon.best/internal/offsite"
	"felis.lolicon.best/internal/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const offsiteUsage = `usage:
  felis offsite sync         [-config path] [-archive-dir dir] [-db-dir dir] [-registry host:port|off]
                             [-uploads-dir dir] [-status-file path]
  felis offsite status       [-config path] [-status-file path]
  felis offsite list         [-config path]
  felis offsite fetch-db     [-config path | -endpoint url -bucket name [-region r] [-prefix p]]
                             [-dir dir] latest|<bundle>
  felis offsite fetch-worlds [-config path] [-archive-dir dir]
  felis offsite fetch-images [-config path] [-registry host:port] [-at version]
  felis offsite fetch-uploads [-config path] [-uploads-dir dir] [-at version]
  felis offsite check-key    [-config path]
  felis offsite take-over    [-config path] [-status-file path] [-yes]
  felis offsite keygen

Every verb but keygen reads the bucket credentials and the encryption key from
the variables [offsite] names (default FELIS_OFFSITE_ACCESS_KEY,
FELIS_OFFSITE_SECRET_KEY, FELIS_OFFSITE_KEY), taking any that are unset from
-env-file (default /etc/felis/offsite.env).

check-key tells whether the key is the one the bucket's objects are sealed
with, writing nothing; it exits 3 when they are sealed with another key, and
sync then refuses to write or prune anything in the bucket.

take-over names the host that writes the bucket, writing nothing; it exits 4
when that is another host and this one never wrote it, and 5 when another
host took the bucket over from this one. A host built from another host's
backup (a rehearsal, or a rebuild) copies nothing into that host's bucket
until -yes makes it the writer; the host it replaces then stops copying and
says so.
`

// defaultOffsiteEnvFile is where bootstrap keeps the [offsite] secrets; the
// felis-offsite.service unit loads it as its EnvironmentFile.
const defaultOffsiteEnvFile = "/etc/felis/offsite.env"

// cmdOffsite implements `felis offsite`: the off-site copy of the world
// archives, the database bundles, the registry's user images and the
// submission uploads (internal/offsite). felis-offsite.timer runs `sync`
// hourly on the host; the fetch verbs are the way back after the node is lost
// (docs/troubleshooting.md §16).
func cmdOffsite(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, offsiteUsage)
		return 2
	}
	verb, rest := args[0], args[1:]
	fs := flag.NewFlagSet("offsite "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, offsiteUsage) }
	switch verb {
	case "sync":
		return offsiteSync(fs, rest, stdout, stderr)
	case "status":
		return offsiteStatus(fs, rest, stdout, stderr)
	case "list":
		return offsiteList(fs, rest, stdout, stderr)
	case "fetch-db":
		return offsiteFetchDB(fs, rest, stdout, stderr)
	case "fetch-worlds":
		return offsiteFetchWorlds(fs, rest, stdout, stderr)
	case "fetch-images":
		return offsiteFetchImages(fs, rest, stdout, stderr)
	case "fetch-uploads":
		return offsiteFetchUploads(fs, rest, stdout, stderr)
	case "check-key":
		return offsiteCheckKey(fs, rest, stdout, stderr)
	case "take-over":
		return offsiteTakeOver(fs, rest, stdout, stderr)
	case "keygen":
		k, err := offsite.NewKey()
		if err != nil {
			fmt.Fprintf(stderr, "felis offsite keygen: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, k)
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(stdout, offsiteUsage)
		return 0
	}
	fmt.Fprintf(stderr, "felis offsite: unknown verb %q\n%s", verb, offsiteUsage)
	return 2
}

// offsiteEnv is the resolved [offsite] binding: the bucket and the key.
type offsiteEnv struct {
	cfg    config.OffsiteConfig
	bucket *offsite.S3
	key    []byte
}

// loadEnvFile sets each KEY=VALUE of path that is not already in the
// environment, so a root shell runs a command the same way its unit does.
// A missing file is not an error.
func loadEnvFile(path string) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

// resolveOffsite builds the bucket client and parses the key for c.
func resolveOffsite(c config.OffsiteConfig) (*offsiteEnv, error) {
	if !c.Enabled() {
		return nil, errors.New("no [offsite] bucket is configured (docs/troubleshooting.md §16, \"Keep a copy somewhere else\")")
	}
	need := func(ref, what string) (string, error) {
		v := os.Getenv(ref)
		if v == "" {
			return "", fmt.Errorf("%s: environment variable %s is empty (set it, or put it in %s)", what, ref, defaultOffsiteEnvFile)
		}
		return v, nil
	}
	ak, err := need(c.AccessKeyRef, "access key")
	if err != nil {
		return nil, err
	}
	sk, err := need(c.SecretKeyRef, "secret key")
	if err != nil {
		return nil, err
	}
	rawKey, err := need(c.KeyRef, "encryption key")
	if err != nil {
		return nil, err
	}
	key, err := offsite.ParseKey(rawKey)
	if err != nil {
		return nil, err
	}
	b, err := offsite.NewS3(offsite.S3Config{
		Endpoint: c.Endpoint, Region: c.Region, Bucket: c.Bucket, Prefix: c.Prefix,
		AccessKey: ak, SecretKey: sk,
	})
	if err != nil {
		return nil, err
	}
	return &offsiteEnv{cfg: c, bucket: b, key: key}, nil
}

// loadOffsite loads felis.toml and the env file and resolves [offsite].
func loadOffsite(cfgPath, envFile string) (*config.Config, *offsiteEnv, error) {
	if err := loadEnvFile(envFile); err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", envFile, err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	env, err := resolveOffsite(cfg.Offsite)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, env, nil
}

func offsiteSync(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) int {
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml (the host copy, which reaches PostgreSQL on 127.0.0.1)")
	envFile := fs.String("env-file", defaultOffsiteEnvFile, "file with the [offsite] secrets, for variables not already set")
	archiveDir := fs.String("archive-dir", "", "host directory of the world archive volume (default: resolved from the backup PVC through the cluster)")
	backupPVC := fs.String("backup-pvc", "felis-backups", `the world archive PVC, in the [k8s] namespace ("" when backups are off)`)
	dbDir := fs.String("db-dir", dbbackup.DefaultDir, `database bundle directory ("" copies no bundles)`)
	stateDir := fs.String("state-dir", dbbackup.DefaultStateDir, `host state directory bundled into the database bundle taken after archives are copied ("" for none)`)
	registry := fs.String("registry", "", `host[:port] of the registry whose user images are copied (default: the in-cluster registry's loopback hostPort; "off" copies none)`)
	uploadsDir := fs.String("uploads-dir", "", "host directory of the submission uploads volume (default: resolved from the uploads PVC through the cluster)")
	uploadsPVC := fs.String("uploads-pvc", platform.UploadsPVCName, `the submission uploads PVC, in the control-plane namespace ("" copies no uploads)`)
	statusFile := fs.String("status-file", offsite.DefaultStatusFile, "where the result of this run is recorded for the watchdog and `status`")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, env, err := loadOffsite(*cfgPath, *envFile)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite sync: %v\n", err)
		return 1
	}
	st, lease := startRun(env.cfg, env.key, *statusFile, time.Now())
	res, err := runOffsiteSync(cfg, env, offsiteSources{
		archiveDir: *archiveDir, backupPVC: *backupPVC, dbDir: *dbDir, stateDir: *stateDir,
		registry:   offsiteRegistryEndpoint(*registry, cfg.Registry),
		uploadsDir: *uploadsDir, uploadsPVC: *uploadsPVC,
	}, &lease, stderr)
	recordRun(&st, res, err, lease)
	if werr := offsite.WriteStatus(*statusFile, st); werr != nil {
		fmt.Fprintf(stderr, "felis offsite sync: record status: %v\n", werr)
	}
	return reportRun(res, err, stdout, stderr)
}

// reportRun prints one pass's outcome and its exit code. A run stopped before
// it copied anything (a bucket that did not answer, another key's objects,
// another host writing the bucket) prints no counts: its zeros would read as
// an empty bucket.
func reportRun(res offsite.Result, err error, stdout, stderr io.Writer) int {
	if err == nil || len(res.Errors) > 0 {
		fmt.Fprintf(stdout, "felis offsite sync: worlds copied=%d pending=%d missing=%d expired=%d; bundles copied=%d pruned=%d; images copied=%d blobs=%d pruned=%d; uploads copied=%d pruned=%d; bucket holds %d worlds (%s), %d bundles, %d images in %d repositories (%s), %d uploads (%s)\n",
			res.WorldsUploaded, res.WorldsPending, len(res.WorldsMissing), res.WorldsExpired,
			res.DBUploaded, res.DBPruned, res.ImagesUploaded, res.ImageBlobsUploaded, res.ImageObjectsPruned,
			res.UploadsUploaded, res.UploadObjectsPruned,
			res.RemoteWorlds, offsite.HumanBytes(res.RemoteBytes), res.RemoteDB, res.Images, res.ImageRepos, offsite.HumanBytes(res.RemoteImageBytes),
			res.Uploads, offsite.HumanBytes(res.RemoteUploadBytes))
	}
	for _, m := range res.WorldsMissing {
		fmt.Fprintf(stderr, "felis offsite sync: recorded archive not on the volume, nothing to copy: %s\n", m)
	}
	for _, m := range res.ImagesIncomplete {
		fmt.Fprintf(stderr, "felis offsite sync: registry image not whole: %s\n", m)
	}
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite sync: %v\n", err)
		return 1
	}
	return 0
}

// startRun begins a pass: its status record, in this release's format and
// carrying the last success over, and this host's lease, read from the record
// the last pass left.
func startRun(cfg config.OffsiteConfig, key []byte, statusFile string, now time.Time) (offsite.Status, offsite.Lease) {
	st := offsite.Status{
		LastAttempt: now.UTC(), Endpoint: cfg.Endpoint, Bucket: cfg.Bucket,
		Prefix: cfg.Prefix, KeyID: offsite.KeyID(key), Format: offsite.StatusFormat,
	}
	if prev, _ := offsite.ReadStatus(statusFile); prev != nil {
		st.LastSuccess = prev.LastSuccess
	}
	return st, offsite.HostLease(statusFile)
}

// recordRun puts one pass's outcome into its status record. A host that
// inherited the bucket from an older release keeps that until it has an id.
func recordRun(st *offsite.Status, res offsite.Result, err error, lease offsite.Lease) {
	st.Result = res
	if err != nil {
		st.LastError = err.Error()
		st.KeyMismatch = errors.Is(err, offsite.ErrKeyMismatch)
		var we *offsite.WriterError
		if errors.As(err, &we) {
			st.Standby = errors.Is(err, offsite.ErrStandby)
			st.Displaced = errors.Is(err, offsite.ErrDisplaced)
			st.Writer = we.Writer
		}
	} else {
		st.LastSuccess = st.LastAttempt
	}
	if lease.Inherited {
		id, _ := lease.ID()
		st.Inherited = id == ""
	}
}

// offsiteSources is where one sync pass reads from: the world archive volume
// (archiveDir, or the backupPVC's directory), the bundle directory (with the
// host state the pass bundles, stateDir), the registry's loopback endpoint and
// the uploads volume (uploadsDir, or the uploadsPVC's directory). An empty
// source is skipped.
type offsiteSources struct {
	archiveDir, backupPVC  string
	dbDir, stateDir        string
	registry               string
	uploadsDir, uploadsPVC string
}

// offsiteRunLimit backstops one sync pass. Each upload has its own deadline,
// scaled to its size (internal/offsite), so a pass over a big archive may run
// for hours; the timer starts no second pass while one runs, and the unit's
// TimeoutStartSec sits above this.
const offsiteRunLimit = 23 * time.Hour

func runOffsiteSync(cfg *config.Config, env *offsiteEnv, src offsiteSources, lease *offsite.Lease, log io.Writer) (offsite.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), offsiteRunLimit)
	defer cancel()
	checkCtx, checkCancel := context.WithTimeout(ctx, 30*time.Second)
	err := env.bucket.Check(checkCtx)
	checkCancel()
	if err != nil {
		return offsite.Result{}, err
	}
	archiveDir, uploadsDir := src.archiveDir, src.uploadsDir
	if archiveDir == "" && src.backupPVC != "" {
		dir, err := resolveVolumeDir(ctx, cfg.K8s.Namespace, src.backupPVC, archiveVolume, false, log)
		if err != nil {
			return offsite.Result{}, err
		}
		archiveDir = dir
	}
	// An s3:// uploads store is off the host already; only a local one, on
	// the uploads PVC, needs the copy.
	if uploadsDir == "" && src.uploadsPVC != "" && isLocalUploadsPath(cfg.Registry.UserUploadsContext) {
		dir, err := resolveVolumeDir(ctx, platform.DefaultControlNamespace, src.uploadsPVC, uploadsVolume, false, log)
		if err != nil {
			return offsite.Result{}, err
		}
		uploadsDir = dir
	}
	drv, err := openStore(ctx, cfg.Database.URL, false)
	if err != nil {
		return offsite.Result{}, fmt.Errorf("open database: %w", err)
	}
	defer drv.Close()
	s := offsiteSyncer(cfg, env, src, archiveDir, uploadsDir, lease, log)
	s.Catalog = offsite.PGCatalog{DB: drv.DB()}
	if src.registry != "" {
		s.Images = newRegistryImages(src.registry)
		s.ImagePins = imagePins(drv.DB(), cfg.Registry.URL)
	}
	return s.Run(ctx)
}

// offsiteSyncer is the pass runOffsiteSync runs over the resolved archive and
// uploads directories, before its catalog and registry are attached. It
// snapshots the database into the bundle directory after copying archives, and
// sweeps world objects no backup records once they outlive every retention in
// [archive]; a retention that does not parse sweeps none.
func offsiteSyncer(cfg *config.Config, env *offsiteEnv, src offsiteSources, archiveDir, uploadsDir string, lease *offsite.Lease, log io.Writer) *offsite.Syncer {
	s := &offsite.Syncer{
		Bucket: env.bucket, Key: env.key,
		ArchiveDir: archiveDir, DBDir: src.dbDir, DBKeep: env.cfg.DBKeep, UploadsDir: uploadsDir, Lease: lease, Log: log,
	}
	if src.dbDir != "" {
		s.Snapshot = offsiteSnapshot(cfg.Database, src.dbDir, src.stateDir, log)
	}
	if rc, err := reaperConfig(cfg); err != nil {
		fmt.Fprintf(log, "felis offsite: world objects no backup records are kept: %v\n", err)
	} else {
		s.OrphanAfter = max(rc.Retention, rc.ManualRetention, rc.ScheduledRetention)
	}
	return s
}

// offsiteSnapshot takes the bundle a pass sends after copying world archives
// (offsite.Syncer.Snapshot): what `felis db backup` takes, labelled offsite,
// with the newest one kept in dir. It is not recorded for the panel, whose
// backup card watches felis-db-backup.timer: snapshots come only when archives
// are copied, and would hide a daily timer that stopped.
func offsiteSnapshot(db config.DatabaseConfig, dir, stateDir string, log io.Writer) func(context.Context) error {
	return func(ctx context.Context) error {
		tools, err := dbTools(db)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		path, err := dbbackup.Backup(ctx, dbbackup.BackupOptions{
			DatabaseURL: db.URL, Tools: tools, Dir: dir, Label: dbbackup.LabelOffsite,
			Keep: defaultKeep[dbbackup.LabelOffsite], StateDir: stateDir, Version: resolvedVersion(),
			ExportServers: exportMinecraftServers, Log: log,
		})
		if err == nil {
			fmt.Fprintf(log, "felis offsite: took database bundle %s, which lists the archives just copied\n", filepath.Base(path))
		}
		return err
	}
}

// volumeKind names a PVC the off-site copy reads or restores, for messages,
// with the flag that bypasses finding it through the cluster.
type volumeKind struct{ what, dirFlag, empty string }

var (
	archiveVolume = volumeKind{"archive volume", "-archive-dir", "no world has been archived"}
	uploadsVolume = volumeKind{"uploads volume", "-uploads-dir", "no modpack has been uploaded"}
)

// resolveVolumeDir finds the host directory behind a PVC: a local-path volume
// is a directory on this node. A PVC still waiting for its first consumer
// holds nothing yet: without bind that is "" (nothing to copy), with bind it
// is bound first, for a fetch to write into.
func resolveVolumeDir(ctx context.Context, ns, pvcName string, kind volumeKind, bind bool, log io.Writer) (string, error) {
	if ns == "" {
		ns = platform.DefaultMinecraftNamespace
	}
	cl, err := buildSystemServerClient()
	if err != nil {
		return "", fmt.Errorf("reach the cluster to find the %s (or pass %s): %w", kind.what, kind.dirFlag, err)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: pvcName}, &pvc); err != nil {
		return "", fmt.Errorf("%s %s/%s: %w", kind.what, ns, pvcName, err)
	}
	if pvc.Spec.VolumeName == "" {
		if !bind {
			fmt.Fprintf(log, "felis offsite: %s %s/%s is not bound yet; %s\n", kind.what, ns, pvcName, kind.empty)
			return "", nil
		}
		if err := bindVolume(ctx, cl, ns, pvcName, kind, log); err != nil {
			return "", err
		}
		if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: pvcName}, &pvc); err != nil {
			return "", err
		}
	}
	var pv corev1.PersistentVolume
	if err := cl.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err != nil {
		return "", fmt.Errorf("%s %s: %w", kind.what, pvc.Spec.VolumeName, err)
	}
	var dir string
	switch {
	case pv.Spec.Local != nil:
		dir = pv.Spec.Local.Path
	case pv.Spec.HostPath != nil:
		dir = pv.Spec.HostPath.Path
	default:
		return "", fmt.Errorf("%s %s is not a directory on a node (local or hostPath); pass %s with where it is mounted on this host", kind.what, pv.Name, kind.dirFlag)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s %s is %s on its node, which is not a directory here; run this on the node that holds it, or pass %s", kind.what, pv.Name, dir, kind.dirFlag)
	}
	return dir, nil
}

// bindVolume runs a pod that mounts the PVC and exits, which is what makes a
// WaitForFirstConsumer volume (k3s local-path) get provisioned. The pod uses
// the control plane's own image, which every install already has.
func bindVolume(ctx context.Context, cl client.Client, ns, pvcName string, kind volumeKind, log io.Writer) error {
	var api appsv1.Deployment
	if err := cl.Get(ctx, types.NamespacedName{Namespace: platform.DefaultControlNamespace, Name: "felis-api"}, &api); err != nil {
		return fmt.Errorf("find the felis image to bind the %s with: %w", kind.what, err)
	}
	if len(api.Spec.Template.Spec.Containers) == 0 {
		return errors.New("felis-api has no container to take the image from")
	}
	image := api.Spec.Template.Spec.Containers[0].Image
	pod := platform.VolumeBinderPod(ns, pvcName, image)
	if err := cl.Create(ctx, pod); err != nil {
		return fmt.Errorf("start a pod to bind the %s: %w", kind.what, err)
	}
	fmt.Fprintf(log, "felis offsite: binding the %s %s/%s (pod %s)\n", kind.what, ns, pvcName, pod.Name)
	defer func() {
		_ = cl.Delete(context.Background(), pod, client.PropagationPolicy(metav1.DeletePropagationBackground))
	}()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		var pvc corev1.PersistentVolumeClaim
		if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: pvcName}, &pvc); err == nil && pvc.Spec.VolumeName != "" && pvc.Status.Phase == corev1.ClaimBound {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("the %s %s/%s did not bind within 3 minutes; see kubectl -n %s describe pod %s", kind.what, ns, pvcName, ns, pod.Name)
}

func offsiteStatus(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) int {
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	statusFile := fs.String("status-file", offsite.DefaultStatusFile, "the record `sync` writes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite status: %v\n", err)
		return 1
	}
	if !cfg.Offsite.Enabled() {
		fmt.Fprintln(stdout, "off-site copy: not configured. World archives, database bundles, user images and uploaded modpacks exist on this machine only.")
		fmt.Fprintln(stdout, "See docs/troubleshooting.md §16, \"Keep a copy somewhere else\".")
		return 1
	}
	o := cfg.Offsite
	fmt.Fprintf(stdout, "bucket:       %s at %s", o.Bucket, o.Endpoint)
	if o.Prefix != "" {
		fmt.Fprintf(stdout, ", prefix %s", o.Prefix)
	}
	fmt.Fprintln(stdout)
	st, err := offsite.ReadStatus(*statusFile)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite status: %v\n", err)
		return 1
	}
	if st == nil {
		fmt.Fprintln(stdout, "last sync:    never (sudo systemctl start felis-offsite.service)")
		return 1
	}
	now := time.Now()
	fmt.Fprintf(stdout, "key id:       %s\n", st.KeyID)
	fmt.Fprintf(stdout, "last attempt: %s (%s ago)\n", st.LastAttempt.Local().Format(time.DateTime), dbbackup.Age(now.Sub(st.LastAttempt)))
	if st.LastSuccess.IsZero() {
		fmt.Fprintln(stdout, "last success: never")
	} else {
		fmt.Fprintf(stdout, "last success: %s (%s ago)\n", st.LastSuccess.Local().Format(time.DateTime), dbbackup.Age(now.Sub(st.LastSuccess)))
	}
	if st.LastError != "" {
		fmt.Fprintf(stdout, "last error:   %s\n", st.LastError)
	}
	if st.KeyMismatch {
		fmt.Fprintf(stdout, "\nThe last run was refused: the bucket's objects are sealed with another key than this host's (key id %s). No sync copies or prunes anything there until FELIS_OFFSITE_KEY in %s is theirs (sudo felis offsite check-key).\n", st.KeyID, defaultOffsiteEnvFile)
		return 1
	}
	if st.Displaced && st.Writer != nil {
		fmt.Fprintf(stdout, "\nThe last run was refused: %s took the bucket over (it last wrote it at %s), and this host copies nothing there any more. If that host is a rehearsal machine, take the bucket back: sudo felis offsite take-over -yes\n",
			st.Writer, st.Writer.At.Local().Format(time.DateTime))
		return 1
	}
	if st.Standby {
		switch w := st.StandsBy(now); {
		case w != nil:
			fmt.Fprintf(stdout, "\nThis host stands by: %s writes the bucket (last at %s). This host was built from its backup, copies nothing into the bucket and, while that host keeps writing, mails no watchdog alert.", w, w.At.Local().Format(time.DateTime))
		case st.Writer != nil:
			fmt.Fprintf(stdout, "\nThis host copies nothing into the bucket: %s wrote it, last at %s, and this host was built from its backup.", st.Writer, st.Writer.At.Local().Format(time.DateTime))
		default:
			fmt.Fprint(stdout, "\nThis host copies nothing into the bucket: it holds copies this host did not write, and names no host writing it.")
		}
		fmt.Fprintln(stdout, " Once this host replaces that one for good: sudo felis offsite take-over -yes")
		return 1
	}
	r := st.Result
	fmt.Fprintf(stdout, "bucket holds: %d world archives (%s), %d database bundles, newest %s\n",
		r.RemoteWorlds, offsite.HumanBytes(r.RemoteBytes), r.RemoteDB, orNone(r.NewestDB))
	if r.ImageIndex != "" {
		fmt.Fprintf(stdout, "images:       %d in %d repositories (%s), registry index %s\n",
			r.Images, r.ImageRepos, offsite.HumanBytes(r.RemoteImageBytes), r.ImageIndex)
	} else {
		fmt.Fprintln(stdout, "images:       not copied (no in-cluster registry, or no sync has reached it yet)")
	}
	if r.UploadIndex != "" {
		fmt.Fprintf(stdout, "uploads:      %d submission contexts (%s), uploads index %s\n",
			r.Uploads, offsite.HumanBytes(r.RemoteUploadBytes), r.UploadIndex)
	} else {
		fmt.Fprintln(stdout, "uploads:      not copied (an s3:// uploads store, or no sync has reached the volume yet)")
	}
	fmt.Fprintf(stdout, "waiting:      %d world archives not yet copied\n", r.WorldsPending)
	for _, m := range r.WorldsMissing {
		fmt.Fprintf(stdout, "missing:      %s is recorded but not on the volume\n", m)
	}
	for _, m := range r.ImagesIncomplete {
		fmt.Fprintf(stdout, "not whole:    %s\n", m)
	}
	if st.LastSuccess.IsZero() || now.Sub(st.LastSuccess) > offsite.StaleAfter {
		fmt.Fprintf(stdout, "\nThe last successful sync is older than %s: journalctl -u felis-offsite -n 50\n", dbbackup.Age(offsite.StaleAfter))
		return 1
	}
	return 0
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func offsiteList(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) int {
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	envFile := fs.String("env-file", defaultOffsiteEnvFile, "file with the [offsite] secrets, for variables not already set")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_, env, err := loadOffsite(*cfgPath, *envFile)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite list: %v\n", err)
		return 1
	}
	return printOffsiteList(env, stdout, stderr)
}

func printOffsiteList(env *offsiteEnv, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	bundles, err := offsite.ListDB(ctx, env.bucket)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite list: %v\n", err)
		return 1
	}
	worlds, err := env.bucket.List(ctx, "worlds/")
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite list: %v\n", err)
		return 1
	}
	printDBBundles(ctx, env.bucket, env.key, bundles, stdout)
	var total int64
	for _, w := range worlds {
		total += w.Size
	}
	fmt.Fprintf(stdout, "world archives: %d (%s)\n", len(worlds), offsite.HumanBytes(total))
	versions, err := offsite.ImageIndexes(ctx, env.bucket)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite list: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "registry index versions (%d, newest first; restore one with fetch-images -at):\n", len(versions))
	for i := len(versions) - 1; i >= 0; i-- {
		x, err := offsite.LoadImageIndex(ctx, env.bucket, env.key, versions[i])
		if err != nil {
			fmt.Fprintf(stdout, "  %s  unreadable: %v\n", versions[i], err)
			continue
		}
		fmt.Fprintf(stdout, "  %s  %d images in %d repositories\n", versions[i], x.Images(), len(x.Repositories))
	}
	uploads, err := offsite.UploadIndexes(ctx, env.bucket)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite list: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "uploads index versions (%d, newest first; restore one with fetch-uploads -at):\n", len(uploads))
	for i := len(uploads) - 1; i >= 0; i-- {
		x, err := offsite.LoadUploadIndex(ctx, env.bucket, env.key, uploads[i])
		if err != nil {
			fmt.Fprintf(stdout, "  %s  unreadable: %v\n", uploads[i], err)
			continue
		}
		fmt.Fprintf(stdout, "  %s  %d submission contexts (%s)\n", uploads[i], len(x.Contexts), offsite.HumanBytes(x.Bytes()))
	}
	return 0
}

// printDBBundles lists the database bundles with what each one's database
// held, read off the front of each, so a restore can pick one by its contents.
func printDBBundles(ctx context.Context, b offsite.Bucket, key []byte, bundles []offsite.Object, stdout io.Writer) {
	fmt.Fprintf(stdout, "database bundles (%d, newest first; restore one with fetch-db):\n", len(bundles))
	for _, o := range bundles {
		m, err := offsite.PeekDB(ctx, b, key, o.Key)
		if err != nil {
			fmt.Fprintf(stdout, "  %s  %s  unreadable: %v\n", o.Key, offsite.HumanBytes(o.Size), err)
			continue
		}
		fmt.Fprintf(stdout, "  %s  %s  %s\n", o.Key, offsite.HumanBytes(o.Size), m.Counts.String())
	}
}

func offsiteCheckKey(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) int {
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	envFile := fs.String("env-file", defaultOffsiteEnvFile, "file with the [offsite] secrets, for variables not already set")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_, env, err := loadOffsite(*cfgPath, *envFile)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite check-key: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := env.bucket.Check(ctx); err != nil {
		fmt.Fprintf(stderr, "felis offsite check-key: %v\n", err)
		return 1
	}
	return checkKey(ctx, env.bucket, env.key, stdout, stderr)
}

// checkKey is check-key once the bucket is open: 0 when the key fits, 3 when
// the bucket's objects are sealed with another one, 1 when it cannot tell.
func checkKey(ctx context.Context, b offsite.Bucket, key []byte, stdout, stderr io.Writer) int {
	fit, err := offsite.CheckKey(ctx, b, key)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite check-key: %v\n", err)
		if errors.Is(err, offsite.ErrKeyMismatch) {
			return 3
		}
		return 1
	}
	id := offsite.KeyID(key)
	switch fit {
	case offsite.KeyRecorded:
		fmt.Fprintf(stdout, "felis offsite check-key: the bucket records key id %s, this key's\n", id)
	case offsite.KeyOpens:
		fmt.Fprintf(stdout, "felis offsite check-key: the bucket's newest objects open with this key (key id %s); the next sync records it\n", id)
	case offsite.KeyUnused:
		fmt.Fprintf(stdout, "felis offsite check-key: the bucket holds no sealed object yet; the first sync records key id %s\n", id)
	}
	return 0
}

func offsiteTakeOver(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) int {
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	envFile := fs.String("env-file", defaultOffsiteEnvFile, "file with the [offsite] secrets, for variables not already set")
	statusFile := fs.String("status-file", offsite.DefaultStatusFile, "the record `sync` writes; this host's id is kept next to it")
	yes := fs.Bool("yes", false, "make this host the one that writes the bucket")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_, env, err := loadOffsite(*cfgPath, *envFile)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite take-over: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := env.bucket.Check(ctx); err != nil {
		fmt.Fprintf(stderr, "felis offsite take-over: %v\n", err)
		return 1
	}
	return takeOver(ctx, env.bucket, env.key, offsite.HostLease(*statusFile), *statusFile, *yes, time.Now(), stdout, stderr)
}

// takeOver is take-over once the bucket is open: without yes it says which
// host writes the bucket, 0 for this one (or none yet), 4 for another and 5
// for one that took the bucket over from this host; with
// yes it records this host as the writer. A key the bucket's objects refuse
// is 3, as in check-key: taking over a bucket this host cannot copy into
// would only stop the host that can.
func takeOver(ctx context.Context, b offsite.Bucket, key []byte, lease offsite.Lease, statusFile string, yes bool, now time.Time, stdout, stderr io.Writer) int {
	fit, err := offsite.CheckKey(ctx, b, key)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite take-over: %v\n", err)
		if errors.Is(err, offsite.ErrKeyMismatch) {
			return 3
		}
		return 1
	}
	role, w, err := lease.Plan(ctx, b, fit == offsite.KeyUnused)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite take-over: %v\n", err)
		return 1
	}
	switch role {
	case offsite.RoleWrites:
		id, _ := lease.ID()
		fmt.Fprintf(stdout, "felis offsite take-over: this host (id %s) writes the bucket; nothing to take over\n", id)
		return 0
	case offsite.RoleClaims:
		fmt.Fprintln(stdout, "felis offsite take-over: the bucket names no host writing it; this host's next sync records itself")
		return 0
	}
	who := "another host"
	if w != nil {
		who = w.String()
		fmt.Fprintf(stdout, "felis offsite take-over: %s writes the bucket, last at %s (%s ago)\n", w, w.At.Local().Format(time.DateTime), dbbackup.Age(now.Sub(w.At)))
	} else {
		fmt.Fprintln(stdout, "felis offsite take-over: the bucket holds copies this host did not write, and names no host writing it")
	}
	if !yes {
		if role == offsite.RoleDisplaced {
			fmt.Fprintf(stdout, "It took the bucket over from this host: this host copies nothing there any more, and its watchdog mails the owners about it. If that host is a rehearsal machine, take the bucket back:\n  sudo felis offsite take-over -yes\n")
			return 5
		}
		fmt.Fprintf(stdout, "This host was built from its backup and copies nothing into the bucket.\n")
		fmt.Fprintf(stdout, "Taking it over makes this host the one that copies into the bucket and prunes it; %s stops at its next copy and mails its owners. Do it once that host is gone for good, or is a rehearsal machine you are done with:\n  sudo felis offsite take-over -yes\n", who)
		return 4
	}
	if _, err := lease.TakeOver(ctx, b, now); err != nil {
		fmt.Fprintf(stderr, "felis offsite take-over: %v\n", err)
		return 1
	}
	// The refusal the last sync recorded is over: the watchdog mails again
	// from now on, and status shows the next run's outcome.
	if st, err := offsite.ReadStatus(statusFile); err == nil && st != nil && (st.Standby || st.Displaced) {
		st.Standby, st.Displaced, st.Writer, st.LastError, st.Inherited = false, false, nil, "", false
		if err := offsite.WriteStatus(statusFile, *st); err != nil {
			fmt.Fprintf(stderr, "felis offsite take-over: record status: %v\n", err)
		}
	}
	id, _ := lease.ID()
	fmt.Fprintf(stdout, "felis offsite take-over: this host (id %s) writes the bucket now; %s stops at its next copy.\nStart the first copy: sudo systemctl start felis-offsite.service\n", id, who)
	return 0
}

// keyHint explains an object the key cannot open when the bucket records
// another key's id, "" otherwise.
func keyHint(ctx context.Context, b offsite.Bucket, key []byte, err error) string {
	if !errors.Is(err, offsite.ErrAuth) {
		return ""
	}
	id, ierr := offsite.BucketKeyID(ctx, b)
	if ierr != nil || id == "" || id == offsite.KeyID(key) {
		return ""
	}
	return fmt.Sprintf("\n  the bucket records key id %s, and this key is %s: set FELIS_OFFSITE_KEY to the key the bucket was written with", id, offsite.KeyID(key))
}

func offsiteFetchDB(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) int {
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml; on a host with no install yet, give -endpoint and -bucket instead")
	envFile := fs.String("env-file", defaultOffsiteEnvFile, "file with the [offsite] secrets, for variables not already set")
	endpoint := fs.String("endpoint", "", "bucket endpoint, when there is no felis.toml")
	bucket := fs.String("bucket", "", "bucket name, when there is no felis.toml")
	region := fs.String("region", "", "bucket region, when there is no felis.toml")
	prefix := fs.String("prefix", "", "key prefix, when there is no felis.toml")
	dir := fs.String("dir", dbbackup.DefaultDir, "directory to write the bundle to")
	arg, ok := parseWithArg(fs, args)
	if !ok {
		return 2
	}
	if arg == "" {
		fmt.Fprint(stderr, offsiteUsage)
		return 2
	}
	if err := loadEnvFile(*envFile); err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-db: read %s: %v\n", *envFile, err)
		return 1
	}
	var oc config.OffsiteConfig
	if *bucket != "" {
		oc = config.OffsiteConfig{
			Endpoint: *endpoint, Bucket: *bucket, Region: *region, Prefix: *prefix,
			AccessKeyRef: config.DefaultOffsiteAccessKeyEnv, SecretKeyRef: config.DefaultOffsiteSecretKeyEnv,
			KeyRef: config.DefaultOffsiteKeyEnv,
		}
	} else {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintf(stderr, "felis offsite fetch-db: %v (on a host with no install yet, pass -endpoint and -bucket)\n", err)
			return 1
		}
		oc = cfg.Offsite
	}
	env, err := resolveOffsite(oc)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-db: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	return fetchDB(ctx, env.bucket, env.key, arg, *dir, time.Now(), stdout, stderr)
}

// fetchDB is fetch-db once the bucket is open: arg is a bundle name or latest.
func fetchDB(ctx context.Context, b offsite.Bucket, key []byte, arg, dir string, now time.Time, stdout, stderr io.Writer) int {
	name := arg
	if name == "latest" {
		var err error
		if name, _, err = offsite.ChooseDB(ctx, b, key); err != nil {
			fmt.Fprintf(stderr, "felis offsite fetch-db: %v%s\n", err, keyHint(ctx, b, key, err))
			return 1
		}
	}
	if _, _, ok := dbbackup.ParseBundleName(name); !ok {
		fmt.Fprintf(stderr, "felis offsite fetch-db: %q is not a bundle name (felis-db-<stamp>-<label>.tar); see `felis offsite list`\n", name)
		return 2
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-db: %v\n", err)
		return 1
	}
	dst := filepath.Join(dir, name)
	if err := offsite.FetchObject(ctx, b, key, offsite.DBKey(name), dst, 0o600); err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-db: %v%s\n", err, keyHint(ctx, b, key, err))
		return 1
	}
	m, err := dbbackup.Verify(dst)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-db: fetched %s but it does not verify: %v\n", dst, err)
		return 1
	}
	fmt.Fprintf(stdout, "felis offsite fetch-db: wrote %s (verified)\n", dst)
	fmt.Fprintf(stdout, "  taken   %s (%s, %s ago)\n  felis   %s, schema %d\n  holds   %s\n",
		m.CreatedAt.Format(time.RFC3339), m.Label, dbbackup.Age(now.Sub(m.CreatedAt)),
		orUnknown(m.FelisVersion), m.SchemaVersion, m.Counts.String())
	if m.Counts.Fresh() {
		fmt.Fprintln(stdout, "  This database holds no servers and at most one account, like a new install's. Check it is the state to restore before `felis db restore`.")
	}
	return 0
}

func offsiteFetchWorlds(fs *flag.FlagSet, args []string, stdout, stderr io.Writer) int {
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml (the host copy)")
	envFile := fs.String("env-file", defaultOffsiteEnvFile, "file with the [offsite] secrets, for variables not already set")
	archiveDir := fs.String("archive-dir", "", "host directory of the world archive volume (default: resolved from the backup PVC, binding it if needed)")
	backupPVC := fs.String("backup-pvc", "felis-backups", "the world archive PVC, in the [k8s] namespace")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, env, err := loadOffsite(*cfgPath, *envFile)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-worlds: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	dir := *archiveDir
	if dir == "" {
		if dir, err = resolveVolumeDir(ctx, cfg.K8s.Namespace, *backupPVC, archiveVolume, true, stderr); err != nil {
			fmt.Fprintf(stderr, "felis offsite fetch-worlds: %v\n", err)
			return 1
		}
	}
	drv, err := openStore(ctx, cfg.Database.URL, false)
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-worlds: open database: %v\n", err)
		return 1
	}
	defer drv.Close()
	res, err := offsite.FetchWorlds(ctx, env.bucket, offsite.PGCatalog{DB: drv.DB()}, env.key, dir, stderr)
	fmt.Fprintf(stdout, "felis offsite fetch-worlds: %d recorded archives, %d fetched into %s, %d with no copy in the bucket\n",
		res.Present, len(res.Fetched), dir, len(res.Missing))
	for _, m := range res.Missing {
		fmt.Fprintf(stdout, "  no off-site copy: %s\n", m)
	}
	if err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-worlds: %v\n", err)
		return 1
	}
	return 0
}
