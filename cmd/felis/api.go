package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/build"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/restore"
	"felis.lolicon.best/internal/store"
	"felis.lolicon.best/internal/submit"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// cmdAPI runs felis-api: two listeners, two middleware chains (spec §7). The
// internal face (service token) is fully wired. The external face is wired but
// fails closed until an Access JWKS key function is configured — the verifier's
// audience logic is unit-tested (internal/api), the JWKS source is a deployment
// integration point.
func cmdAPI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("api", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/felis/felis.toml", "path to felis.toml")
	internalAddr := fs.String("internal-addr", ":8081", "internal-face listen address (service token, no Zero Trust)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis api: %v\n", err)
		return 1
	}

	ctx := ctrl.SetupSignalHandler()

	drv, err := store.Open(ctx, cfg.Database.URL)
	if err != nil {
		fmt.Fprintf(stderr, "felis api: open database: %v\n", err)
		return 1
	}
	defer drv.Close()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	// Both clients are built from the SAME rest.Config. The controller-runtime
	// client.Client drives CRDs/Secrets/Jobs (cluster, console-write, restore); the
	// typed clientset is needed solely for the read-side console, because the
	// pods/log subresource (GetLogs(...).Stream) lives only on the typed CoreV1
	// client, not on client.Client (spec §8 读=pods/log follow).
	restCfg := ctrl.GetConfigOrDie()
	cl, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(stderr, "felis api: build k8s client: %v\n", err)
		return 1
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		fmt.Fprintf(stderr, "felis api: build k8s clientset: %v\n", err)
		return 1
	}

	token := os.Getenv("FELIS_SERVICE_TOKEN")
	if token == "" {
		fmt.Fprintln(stderr, "felis api: warning: FELIS_SERVICE_TOKEN unset — internal face will reject all callers")
	}

	// Build subsystem (spec §16): the weak-SA build Job runs in the configured
	// build namespace and pushes to the internal registry. The build Pod never
	// holds DB credentials — felis-api owns the PG store and admits scanned
	// images, so the Builder is constructed here with both bindings.
	builder := &build.Builder{
		Store:  build.NewPGStore(drv.DB()),
		Jobs:   build.NewK8sJobs(cl, buildConfig(cfg)),
		Config: buildConfig(cfg),
	}

	// User-modpack approval lane (user-directed extension over §16; see
	// internal/submit). An ordinary user may only SUBMIT a
	// modpack; an admin must approve it before anything is built, at which point
	// the SAME Trivy-gated Builder runs as for an admin's direct build. Registry
	// MUST match the Builder's RegistryURL (cfg.Registry.URL) — both are wired from
	// the one field here so the lane's pre-CAS validate and the Builder's Submit
	// can never disagree about the push target. The blob upload transport that
	// populates the derived context ref is deferred (INTEGRATION-ONLY): the
	// create→approve→reject state machine is real Postgres truth, but a real
	// Kaniko context pull needs that transport in place.
	submissions := &submit.Manager{
		Store:        submit.NewPGStore(drv.DB()),
		Builds:       builder,
		Registry:     cfg.Registry.URL,
		ContextStore: cfg.Registry.UserUploadsContext,
	}

	// Restore subsystem (spec §7): the weak-SA restore Job mounts the target
	// world PVC + the backup PVC and runs `felis restore`. It needs deployment-
	// specific values that have no safe default — the felis image to run and the
	// backup PVC to mount — so it is wired only when both are supplied. Otherwise
	// the Restorer is left nil and the restore endpoint honestly returns 503
	// rather than enqueuing a Job that cannot run. (The archive store no longer
	// gates wiring here: config.Validate rejects any recognized-but-unimplemented
	// store at load, so by this point cfg.Archive.Store is guaranteed tarLocal.)
	var restorer api.Restorer
	felisImage, backupPVC := os.Getenv("FELIS_IMAGE"), os.Getenv("FELIS_BACKUP_PVC")
	if felisImage != "" && backupPVC != "" {
		rcfg := restoreConfig(cfg, felisImage, backupPVC)
		restorer = &restore.Restorer{Jobs: restore.NewK8sJobs(cl), Config: rcfg}
	} else {
		fmt.Fprintln(stderr, "felis api: restore executor disabled (needs FELIS_IMAGE and FELIS_BACKUP_PVC) — restore endpoint returns 503")
	}

	// One PGRepo instance backs both the handlers and the session verifier: the
	// SessionAuth that fronts the external face reads sessions/users/settings from
	// the same store the auth handlers write to, so a login and the next request
	// agree on what local auth knows.
	repo := api.NewPGRepo(drv.DB())

	a := &api.API{
		Repo:    repo,
		Cluster: api.NewK8sCluster(cl, cfg.K8s.Namespace),
		Console: api.NewK8sConsole(cl, cfg.K8s.Namespace),
		Logs:    api.NewK8sLogStreamer(clientset, cfg.K8s.Namespace),
		// Build-log stream (spec §16) is scoped to the BUILD namespace — the same
		// value the Builder renders Jobs into — so it follows where build Pods run.
		BuildLogs:   api.NewK8sBuildLogStreamer(clientset, cfg.Registry.BuildNamespace),
		Internal:    api.BearerTokenAuth{Token: token},
		Builder:     builder,
		Restorer:    restorer,
		Submissions: submissions,
		// The external face is fronted by SessionAuth: it prefers a local-password
		// session cookie and otherwise delegates to the Cloudflare-Access JWT verifier,
		// so both auth models coexist on one face. The delegate's Keyfunc is
		// intentionally nil — the JWT path fails closed until a JWKS-backed key function
		// is wired (deployment integration point) — while the local-password path is
		// live the moment `felis breakGlass` flips local_auth_enabled on.
		External: api.SessionAuth{
			Repo:          repo,
			Delegate:      api.AccessVerifier{Audience: cfg.Auth.AccessJWTAud},
			RootDomain:    cfg.Server.RootDomain,
			AdminHostname: cfg.Auth.AdminHostname,
		},
		RootDomain:   cfg.Server.RootDomain,
		WakeCooldown: 30 * time.Second,
	}
	fmt.Fprintln(stderr, "felis api: external face fails closed (Access JWKS key function not configured)")

	internalSrv := &http.Server{Addr: *internalAddr, Handler: a.InternalHandler()}
	externalSrv := &http.Server{Addr: cfg.Server.Listen, Handler: a.ExternalHandler()}

	errc := make(chan error, 2)
	go func() { errc <- internalSrv.ListenAndServe() }()
	go func() { errc <- externalSrv.ListenAndServe() }()
	fmt.Fprintf(stdout, "felis api: internal=%s external=%s\n", *internalAddr, cfg.Server.Listen)

	// reconcileBuilds drives the scan-gate translation: poll unfinished builds
	// and advance any whose Job has reached a terminal phase. GET on a build also
	// reconciles it, but this loop converges builds nobody is polling.
	go reconcileBuilds(ctx, builder, stderr)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = internalSrv.Shutdown(shutdownCtx)
		_ = externalSrv.Shutdown(shutdownCtx)
		return 0
	case err := <-errc:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(stderr, "felis api: listener exited: %v\n", err)
			return 1
		}
		return 0
	}
}

// buildConfig projects felis.toml onto the build subsystem config (spec §16,
// §24). Unset fields fall back to the build package's hardened defaults
// (felis-build namespace + weak SA, 30m deadline, resource limits).
func buildConfig(cfg *config.Config) build.Config {
	return build.Config{
		Namespace:   cfg.Registry.BuildNamespace,
		RegistryURL: cfg.Registry.URL,
	}
}

// restoreConfig projects felis.toml + the deployment-supplied image and backup
// PVC onto the restore subsystem config (spec §7). The runtime identity, mount
// roots, resource limits, and weak SA fall back to the restore package's
// hardened defaults. BackupRoot tracks cfg.Archive.LocalPath because tarLocal
// archive refs are absolute: the restore Pod must mount the backup PVC at the
// same path the reaper wrote archives under, or the stored ref won't resolve.
func restoreConfig(cfg *config.Config, image, backupPVC string) restore.Config {
	return restore.Config{
		Namespace:    cfg.K8s.Namespace,
		Image:        image,
		BackupPVC:    backupPVC,
		ArchiveStore: cfg.Archive.Store,
		BackupRoot:   cfg.Archive.LocalPath,
	}
}

// reconcileBuilds polls unfinished builds on an interval and advances any whose
// Job has reached a terminal phase. It exits when ctx is cancelled.
func reconcileBuilds(ctx context.Context, b *build.Builder, stderr io.Writer) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := b.SyncAll(ctx); err != nil {
				fmt.Fprintf(stderr, "felis api: build reconcile: %v\n", err)
			}
		}
	}
}
