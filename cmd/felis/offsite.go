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
  felis offsite keygen

Every verb but keygen reads the bucket credentials and the encryption key from
the variables [offsite] names (default FELIS_OFFSITE_ACCESS_KEY,
FELIS_OFFSITE_SECRET_KEY, FELIS_OFFSITE_KEY), taking any that are unset from
-env-file (default /etc/felis/offsite.env).
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
	st := offsite.Status{
		LastAttempt: time.Now().UTC(), Endpoint: env.cfg.Endpoint, Bucket: env.cfg.Bucket,
		Prefix: env.cfg.Prefix, KeyID: offsite.KeyID(env.key),
	}
	if prev, _ := offsite.ReadStatus(*statusFile); prev != nil {
		st.LastSuccess = prev.LastSuccess
	}
	res, err := runOffsiteSync(cfg, env, offsiteSources{
		archiveDir: *archiveDir, backupPVC: *backupPVC, dbDir: *dbDir,
		registry:   offsiteRegistryEndpoint(*registry, cfg.Registry),
		uploadsDir: *uploadsDir, uploadsPVC: *uploadsPVC,
	}, stderr)
	st.Result = res
	if err != nil {
		st.LastError = err.Error()
	} else {
		st.LastSuccess = st.LastAttempt
	}
	if werr := offsite.WriteStatus(*statusFile, st); werr != nil {
		fmt.Fprintf(stderr, "felis offsite sync: record status: %v\n", werr)
	}
	fmt.Fprintf(stdout, "felis offsite sync: worlds copied=%d pending=%d missing=%d expired=%d; bundles copied=%d pruned=%d; images copied=%d blobs=%d pruned=%d; uploads copied=%d pruned=%d; bucket holds %d worlds (%s), %d bundles, %d images in %d repositories (%s), %d uploads (%s)\n",
		res.WorldsUploaded, res.WorldsPending, len(res.WorldsMissing), res.WorldsExpired,
		res.DBUploaded, res.DBPruned, res.ImagesUploaded, res.ImageBlobsUploaded, res.ImageObjectsPruned,
		res.UploadsUploaded, res.UploadObjectsPruned,
		res.RemoteWorlds, offsite.HumanBytes(res.RemoteBytes), res.RemoteDB, res.Images, res.ImageRepos, offsite.HumanBytes(res.RemoteImageBytes),
		res.Uploads, offsite.HumanBytes(res.RemoteUploadBytes))
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

// offsiteSources is where one sync pass reads from: the world archive volume
// (archiveDir, or the backupPVC's directory), the bundle directory, the
// registry's loopback endpoint and the uploads volume (uploadsDir, or the
// uploadsPVC's directory). An empty source is skipped.
type offsiteSources struct {
	archiveDir, backupPVC  string
	dbDir                  string
	registry               string
	uploadsDir, uploadsPVC string
}

func runOffsiteSync(cfg *config.Config, env *offsiteEnv, src offsiteSources, log io.Writer) (offsite.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
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
	s := &offsite.Syncer{
		Bucket: env.bucket, Catalog: offsite.PGCatalog{DB: drv.DB()}, Key: env.key,
		ArchiveDir: archiveDir, DBDir: src.dbDir, DBKeep: env.cfg.DBKeep, UploadsDir: uploadsDir, Log: log,
	}
	if src.registry != "" {
		s.Images = newRegistryImages(src.registry)
	}
	return s.Run(ctx)
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
	fmt.Fprintf(stdout, "database bundles (%d, newest first):\n", len(bundles))
	for _, b := range bundles {
		fmt.Fprintf(stdout, "  %s  %s\n", b.Key, offsite.HumanBytes(b.Size))
	}
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
	name := arg
	if name == "latest" {
		bundles, err := offsite.ListDB(ctx, env.bucket)
		if err != nil {
			fmt.Fprintf(stderr, "felis offsite fetch-db: %v\n", err)
			return 1
		}
		if len(bundles) == 0 {
			fmt.Fprintln(stderr, "felis offsite fetch-db: the bucket holds no database bundle")
			return 1
		}
		name = bundles[0].Key
	}
	if _, _, ok := dbbackup.ParseBundleName(name); !ok {
		fmt.Fprintf(stderr, "felis offsite fetch-db: %q is not a bundle name (felis-db-<stamp>-<label>.tar); see `felis offsite list`\n", name)
		return 2
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-db: %v\n", err)
		return 1
	}
	dst := filepath.Join(*dir, name)
	if err := offsite.FetchObject(ctx, env.bucket, env.key, offsite.DBKey(name), dst, 0o600); err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-db: %v\n", err)
		return 1
	}
	if _, err := dbbackup.Verify(dst); err != nil {
		fmt.Fprintf(stderr, "felis offsite fetch-db: fetched %s but it does not verify: %v\n", dst, err)
		return 1
	}
	fmt.Fprintf(stdout, "felis offsite fetch-db: wrote %s (verified)\n", dst)
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
