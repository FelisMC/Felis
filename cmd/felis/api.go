package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/backupjob"
	"felis.lolicon.best/internal/build"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/imagepin"
	"felis.lolicon.best/internal/mail"
	"felis.lolicon.best/internal/metrics"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/panel"
	"felis.lolicon.best/internal/passkey"
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/reaper"
	"felis.lolicon.best/internal/registryprune"
	"felis.lolicon.best/internal/restore"
	"felis.lolicon.best/internal/submit"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// mojangSessionServer is the public Mojang hasJoined endpoint the Felis-nano multiplexer
// leads with as its code-owned identity anchor (正版优先). A protocol constant, not a
// deployment domain, so it is hardcoded rather than configured — and it is the ONLY source
// the code marks Identity (UUIDs trusted verbatim); config can never add another.
const mojangSessionServer = "https://sessionserver.mojang.com/session/minecraft/hasJoined"

// authSourcesFromConfig builds the multiplexer's priority list from the configured
// [[auth_source]] entries: Mojang leads as the code-owned identity anchor (正版优先, the ONLY
// Identity source — config can only append namespace-rewritten third-party sources, never a
// trusted one), then each configured source in file order. Both `felis api` and `felis nano`
// call it, so the "Mojang is prepended in code" invariant lives in exactly one place.
func authSourcesFromConfig(configured []config.AuthSourceConfig) []api.AuthSource {
	sources := make([]api.AuthSource, 0, len(configured)+1)
	sources = append(sources, api.AuthSource{Tag: "mojang", URL: mojangSessionServer, Identity: true})
	for _, s := range configured {
		sources = append(sources, api.AuthSource{Tag: s.Tag, Prefix: s.Prefix, URL: s.URL})
	}
	return sources
}

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
	httpsAddr := fs.String("https-addr", "", "external HTTPS listen address (disabled unless --tls-cert and --tls-key are also set)")
	tlsCert := fs.String("tls-cert", "", "TLS certificate path for --https-addr")
	tlsKey := fs.String("tls-key", "", "TLS private key path for --https-addr")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*httpsAddr == "") != (*tlsCert == "" || *tlsKey == "") {
		fmt.Fprintln(stderr, "felis api: --https-addr requires both --tls-cert and --tls-key")
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "felis api: %v\n", err)
		return 1
	}

	ctx := ctrl.SetupSignalHandler()

	// Before anything serves: an api on a schema it was not built for answers with
	// errors, or writes rows the other version cannot read.
	drv, err := openStore(ctx, cfg.Database.URL, false)
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

	metrics.SetBuildInfo("api", resolvedVersion())

	internalAuth, err := internalCallerTokens(os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "felis api: internal face tokens: %v\n", err)
		return 1
	}
	for _, ct := range naming.CallerTokens {
		if internalAuth[api.Caller(ct.Caller)] == "" {
			fmt.Fprintf(stderr, "felis api: warning: %s unset — the internal face turns the %s caller away\n", ct.APIEnv, ct.Caller)
		}
	}

	// Email one-time codes go through the [smtp] relay when one is configured; the
	// password is read from the env var password_ref names (default SMTPPasswordEnv,
	// injected from the felis-smtp Secret). No [smtp] host ⇒ mailer stays nil and
	// deliverOTP logs each code server-side (the pre-SMTP bootstrap posture).
	var mailer api.OTPMailer
	if cfg.SMTP.Host != "" {
		passRef := cfg.SMTP.PasswordRef
		if passRef == "" {
			passRef = platform.SMTPPasswordEnv
		}
		password := os.Getenv(passRef)
		if cfg.SMTP.Username != "" && password == "" {
			fmt.Fprintf(stderr, "felis api: warning: [smtp] username is set but credentials env %s is empty — OTP sends will fail AUTH\n", passRef)
		}
		mailer = &mail.SMTP{
			Host:     cfg.SMTP.Host,
			Port:     cfg.SMTP.Port,
			From:     cfg.SMTP.From,
			Username: cfg.SMTP.Username,
			Password: password,
		}
	} else {
		fmt.Fprintln(stderr, "felis api: [smtp] not configured — email one-time codes are logged, not mailed")
	}

	// Build subsystem (spec §16): the weak-SA build Job runs in the configured
	// build namespace and pushes to the internal registry. The build Pod never
	// holds DB credentials — felis-api owns the PG store and admits scanned
	// images, so the Builder is constructed here with both bindings.
	buildCfg := buildConfig(cfg)
	// The fetch initContainer runs THIS image's fetch-context entrypoint, so the
	// build config carries the api's own image (the platform sets FELIS_IMAGE).
	buildCfg.FelisImage = os.Getenv("FELIS_IMAGE")
	buildJobs := build.NewK8sJobs(cl, buildCfg)
	builder := &build.Builder{
		Store:  build.NewPGStore(drv.DB()),
		Jobs:   buildJobs,
		Config: buildCfg,
	}
	go probeBuildUserNamespaces(ctx, buildJobs, buildCfg, stderr)

	// User-modpack approval lane (user-directed extension over §16; see
	// internal/submit). An ordinary user may only SUBMIT a
	// modpack; an admin must approve it before anything is built, at which point
	// the SAME Trivy-gated Builder runs as for an admin's direct build. Registry
	// MUST match the Builder's RegistryURL (cfg.Registry.URL) — both are wired from
	// the one field here so the lane's pre-CAS validate and the Builder's Submit
	// can never disagree about the push target.
	//
	// The blob upload transport is selected by the shape of user_uploads_context —
	// the two backends the setup wizard chooses between. A local path wires
	// LocalContextStore (the mounted uploads PVC); an s3:// base wires
	// S3ContextStore when its credentials resolve. Anything else — or an s3:// base
	// with no credentials configured — leaves Blobs nil so POST
	// /me/submissions/{id}/context returns 503, honest like the restore executor
	// when its PVC is not supplied.
	//
	// Reading the blob back is the API's job, not Kaniko's: the build Pod runs in
	// another namespace and can neither mount the uploads PVC (a PVC does not cross
	// namespaces) nor hold object-store credentials, so ContextBaseURL makes the
	// derived context ref an internal-face URL that the build Job's fetch
	// initContainer streams (cmd/felis fetch-context). The platform renders this
	// address into the api Deployment (felis API base URL env); the fallback keeps
	// a hand-rolled deployment working under the platform's default control
	// namespace.
	contextBase := cfg.Registry.UserUploadsContext
	var blobs submit.Blobs
	switch {
	case isLocalUploadsPath(contextBase):
		// Normalize a file:// URL to the plain path ONCE and feed it to BOTH the
		// derived ref (ContextStore) and the store (Base), so the recorded
		// context_ref and the on-disk write location can never diverge.
		contextBase = strings.TrimPrefix(contextBase, "file://")
		blobs = &submit.LocalContextStore{Base: contextBase}
	case strings.HasPrefix(strings.ToLower(contextBase), "s3://"):
		if s3, err := newS3UploadsStore(cfg.Registry); err != nil {
			fmt.Fprintf(stderr, "felis api: S3 user-uploads store not configured (%v) — modpack upload transport disabled (POST /api/v1/me/submissions/{id}/context returns 503)\n", err)
		} else {
			blobs = s3
		}
	default:
		fmt.Fprintf(stderr, "felis api: user-uploads context %q is neither a local path nor an s3:// base — modpack upload transport disabled (POST /api/v1/me/submissions/{id}/context returns 503)\n", contextBase)
	}
	submissions := &submit.Manager{
		Store:          submit.NewPGStore(drv.DB()),
		Builds:         builder,
		Registry:       cfg.Registry.URL,
		ContextStore:   contextBase,
		ContextBaseURL: internalAPIBaseURL(),
		Blobs:          blobs,
	}
	if v := cfg.Registry.UserUploadsMaxBytes; v != "" {
		if n, err := parseByteSize(v); err != nil || n <= 0 {
			fmt.Fprintf(stderr, "felis api: [registry] user_uploads_max_bytes %q is not a positive size such as 4Gi; keeping the default\n", v)
		} else {
			submissions.MaxStoredBytesTotal = n
		}
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

	// On-demand backup subsystem (spec §18/§19 WorldArchiver, run on demand). Its
	// backup Job mirrors the restore Job's weak-SA isolation but additionally mounts
	// the config Secret so it self-records the world_backups row (see internal/
	// backupjob). It needs the same deployment-specific values as restore, so it is
	// wired under the same gate; otherwise the Backuper is left nil and the backup
	// endpoint honestly returns 503.
	var backuper api.Backuper
	if felisImage != "" && backupPVC != "" {
		backuper = &backupjob.Backuper{Jobs: backupjob.NewK8sJobs(cl), Config: backupConfig(cfg, felisImage, backupPVC)}
	} else {
		fmt.Fprintln(stderr, "felis api: backup executor disabled (needs FELIS_IMAGE and FELIS_BACKUP_PVC) — backup endpoint returns 503")
	}

	// Server file editor: a weak-SA Job mounts ONLY the target world PVC and runs
	// `felis files`, printing its result for felis-api to read back through
	// pods/log (see internal/fileedit). It needs FELIS_IMAGE but — unlike restore
	// and backup — no backup PVC, since it never touches the archive store, so it
	// is wired on the image alone; otherwise the editor is left nil and the file
	// endpoints honestly return 503. It takes the typed clientset rather than the
	// controller-runtime client because the log subresource lives only on the typed
	// CoreV1 client, and one client covers its Job create, Pod list, and log read.
	var files api.FileEditor
	if felisImage != "" {
		files = &fileedit.Editor{
			Runner: fileedit.NewK8sRunner(clientset),
			Config: fileEditConfig(cfg, felisImage),
		}
	} else {
		fmt.Fprintln(stderr, "felis api: file editor disabled (needs FELIS_IMAGE) — file endpoints return 503")
	}

	// One PGRepo instance backs both the handlers and the session verifier: the
	// SessionAuth that fronts the external face reads sessions/users/settings from
	// the same store the auth handlers write to, so a login and the next request
	// agree on what local auth knows.
	repo := api.NewPGRepo(drv.DB())
	if err := api.RegisterStorePool(drv.DB()); err != nil {
		fmt.Fprintf(stderr, "felis api: store pool metrics unavailable: %v\n", err)
	}

	// The owner's on-demand backup levers come from [archive], the same keys the
	// backup Job and the reaper read. A malformed key leaves the defaults in
	// place here; the reaper Job fails on it and names it.
	rcfg, err := reaperConfig(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "felis api: %v; using the default backup limits\n", err)
		rcfg = reaper.DefaultConfig()
	}

	cluster := api.NewK8sCluster(cl, cfg.K8s.Namespace)
	jobStatus := api.NewK8sJobStatus(cl, cfg.K8s.Namespace)
	a := &api.API{
		Repo:    repo,
		Cluster: cluster,
		Console: api.NewK8sConsole(cl, cfg.K8s.Namespace),
		Logs:    api.NewK8sLogStreamer(clientset, cfg.K8s.Namespace),
		// Build-log stream (spec §16) is scoped to the BUILD namespace — the same
		// value the Builder renders Jobs into — so it follows where build Pods run.
		BuildLogs: api.NewK8sBuildLogStreamer(clientset, cfg.Registry.BuildNamespace),
		Internal:  internalAuth,
		Builder:   builder,
		Images:    imagePinner(cfg.Registry.URL),
		Restorer:  restorer,
		Backuper:  backuper,
		JobStatus: jobStatus,
		// A restore starts with a safety snapshot; settleRestoreChains starts the
		// restore behind each one.
		RestoreChains: jobStatus,
		Files:         files,
		Submissions:   submissions,
		Mailer:        mailer,
		// The external face is fronted by SessionAuth: it prefers a local session
		// cookie (minted by the passwordless doors) and otherwise delegates to the
		// Cloudflare-Access JWT verifier, so both auth models coexist on one face. The
		// delegate's Keyfunc is intentionally nil — the JWT path fails closed until a
		// JWKS-backed key function is wired (deployment integration point) — while the
		// local session path is live the moment `felis breakGlass` flips
		// local_auth_enabled on.
		External: api.SessionAuth{
			Repo:          repo,
			Delegate:      api.AccessVerifier{Audience: cfg.Auth.AccessJWTAud},
			RootDomain:    cfg.Server.RootDomain,
			AdminHostname: cfg.Auth.AdminHostname,
		},
		RootDomain:    cfg.Server.RootDomain,
		AdminHostname: cfg.Auth.AdminHostname,
		PanelHostname: cfg.Auth.PanelHostname,
		WakeCooldown:  30 * time.Second,
		// An owner may start one backup per server per manual_cooldown, and none
		// while the store is at max_local_bytes (data-durability-9).
		BackupCooldown: rcfg.ManualCooldown,
		BackupStoreCap: rcfg.MaxLocalBytes,
		// The user-modpack lane's per-user throttles: a create spaces out
		// review-queue rows, an upload spaces out (up to 1 GiB) context streams.
		// Separate keys, so the normal create→upload sequence stays immediate.
		SubmitCreateCooldown: 30 * time.Second,
		SubmitUploadCooldown: 15 * time.Second,
		// Bound concurrent console/build-log SSE streams per principal. Generous enough
		// for legitimate multi-tab / multi-server watching, while capping how many
		// upstream follow connections a single caller can tie up if their streams stall.
		MaxStreamsPerPrincipal: 16,
		// Public sign-in doors, per client address: a person signing in makes a
		// handful of calls, so 20 at once refilled at 20 a minute never bites a
		// real user and still turns a spray into a trickle. The client address
		// is the edge's header when the install names one (config.AuthConfig).
		AuthDoorLimit:  api.RateLimit{Burst: 20, PerMinute: 20},
		ClientIPHeader: cfg.Auth.EffectiveClientIPHeader(),
		MailLimit:      mailLimit(cfg.SMTP.MaxPerHour),
	}
	fmt.Fprintln(stderr, "felis api: external face fails closed (Access JWKS key function not configured)")
	if a.ClientIPHeader != "" {
		fmt.Fprintf(stderr, "felis api: sign-in rate limit keys on the %s header\n", a.ClientIPHeader)
	} else {
		fmt.Fprintln(stderr, "felis api: sign-in rate limit keys on the TCP peer ([auth] client_ip_header unset)")
	}

	// Felis-nano: the multi-source hasJoined multiplexer. Mojang leads as the code-owned
	// identity anchor (正版优先); config can only append namespace-rewritten third-party
	// sources, never a trusted one, so a misconfig cannot reopen the impersonation hole.
	// Wired unconditionally: the installer points Velocity at this route whether or not any
	// [[auth_source]] is configured, so an empty list has to mean a Mojang-only relay, the
	// same as under `felis nano`. A nil list would 204 every login, premium ones included.
	a.AuthSources = authSourcesFromConfig(cfg.AuthSources)
	fmt.Fprintf(stderr, "felis api: hasJoined multiplexer active — Mojang + %d third-party source(s)\n", len(cfg.AuthSources))

	// Passkey (WebAuthn) verifier (spec §14, Phase 6). One relying party spans BOTH
	// web faces: the RP id is the panel hostname (console.<root>), and because that is
	// a domain suffix of the operator host (op.console.<root>), a single credential
	// enrolled once asserts on either face — one binding, usable on the player console
	// AND the operator console. Both hosts are therefore listed as permitted origins,
	// while the RP id stays the panel host so the credential's scope is ONE relying
	// party, not two. Wired only when auth.panel_hostname is configured; otherwise
	// a.Passkey stays nil and the passkey routes honestly return 503 (the authenticated
	// enrollment boundary is still enforced by the handlers).
	if cfg.Auth.PanelHostname != "" {
		origins := []string{"https://" + cfg.Auth.PanelHostname}
		if admin := defaultAdminHostname(cfg.Server.RootDomain, cfg.Auth.AdminHostname); admin != "" && admin != cfg.Auth.PanelHostname {
			origins = append(origins, "https://"+admin)
		}
		pv, err := passkey.New(cfg.Auth.PanelHostname, "Felis", origins)
		if err != nil {
			fmt.Fprintf(stderr, "felis api: passkey verifier disabled: %v — passkey endpoints return 503\n", err)
		} else {
			a.Passkey = pv
		}
	} else {
		fmt.Fprintln(stderr, "felis api: passkey verifier disabled (auth.panel_hostname unset) — passkey endpoints return 503")
	}

	// Derive the console hostnames when felis.toml leaves them unset, exactly as the
	// setup/breakGlass paths do — otherwise the SPA cannot tell which face it is
	// serving and falls back to the player console on op.console.<root>.
	externalHandler := panel.Handler(a.ExternalHandler(), cfg.Server.RootDomain,
		defaultPanelHostname(cfg.Server.RootDomain, cfg.Auth.PanelHostname),
		defaultAdminHostname(cfg.Server.RootDomain, cfg.Auth.AdminHostname),
		cfg.Velocity.GamePort, resolvedVersion())
	internalSrv := newAPIServer(*internalAddr, a.InternalHandler())
	externalSrv := newAPIServer(cfg.Server.Listen, externalHandler)

	errc := make(chan error, 3)
	go func() { errc <- internalSrv.ListenAndServe() }()
	go func() { errc <- externalSrv.ListenAndServe() }()
	var httpsSrv *http.Server
	if *httpsAddr != "" {
		httpsSrv = newAPIServer(*httpsAddr, externalHandler)
		go func() { errc <- httpsSrv.ListenAndServeTLS(*tlsCert, *tlsKey) }()
	}
	if httpsSrv != nil {
		fmt.Fprintf(stdout, "felis api: internal=%s external=%s https=%s\n", *internalAddr, cfg.Server.Listen, *httpsAddr)
	} else {
		fmt.Fprintf(stdout, "felis api: internal=%s external=%s\n", *internalAddr, cfg.Server.Listen)
	}

	// reconcileBuilds drives the scan-gate translation: poll unfinished builds
	// and advance any whose Job has reached a terminal phase. GET on a build also
	// reconciles it, but this loop converges builds nobody is polling.
	go reconcileBuilds(ctx, builder, stderr)
	go settleRestoreChains(ctx, a, stderr)

	if pruner := registryPruner(cfg, builder.Store, cluster, stderr); pruner != nil {
		go pruner.Loop(ctx, registryPruneInterval)
	}
	go reapRejectedContexts(ctx, submissions, stderr)

	servers := []*http.Server{internalSrv, externalSrv}
	if httpsSrv != nil {
		servers = append(servers, httpsSrv)
	}
	for _, srv := range servers {
		srv.RegisterOnShutdown(a.CloseStreams)
	}

	select {
	case <-ctx.Done():
		shutdownServers(servers, apiShutdownGrace, stderr)
		return 0
	case err := <-errc:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(stderr, "felis api: listener exited: %v\n", err)
			return 1
		}
		return 0
	}
}

const (
	// apiReadHeaderTimeout caps how long a client may take to send its request
	// headers, defeating a Slowloris that trickles a header line forever to pin a
	// connection open. It bounds only the header phase, so it is safe on every face —
	// including the SSE streaming one, whose response, not its request, is long-lived.
	apiReadHeaderTimeout = 10 * time.Second
	// apiIdleTimeout caps how long a kept-alive connection may sit idle between
	// requests before the server closes it, bounding idle-connection exhaustion.
	apiIdleTimeout = 120 * time.Second
	// apiShutdownGrace is how long the listeners drain after SIGTERM. The pod gets
	// the Kubernetes default of 30s before SIGKILL; this leaves the rest for the
	// process to exit.
	apiShutdownGrace = 20 * time.Second
)

// shutdownServers drains every listener at once under one deadline: in turn, a
// slow first listener would spend the time the others needed. Log streams end
// through RegisterOnShutdown (API.CloseStreams); what is still running when the
// deadline passes is cut off with the process.
func shutdownServers(servers []*http.Server, grace time.Duration, stderr io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Go(func() {
			if err := srv.Shutdown(ctx); err != nil {
				fmt.Fprintf(stderr, "felis api: shutdown %s: %v\n", srv.Addr, err)
			}
		})
	}
	wg.Wait()
}

// newAPIServer builds an http.Server with hardened header/idle timeouts (gosec
// G112) shared by all three felis-api listeners (internal, external, https).
// WriteTimeout and ReadTimeout are deliberately LEFT UNSET: the external and https
// faces stream Server-Sent Events (console / build logs, spec §8) for the lifetime
// of a client's attachment, and a WriteTimeout would sever a healthy long-lived
// stream mid-flight. Slowloris is closed by ReadHeaderTimeout, which bounds only the
// header phase and never touches the response.
func newAPIServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: apiReadHeaderTimeout,
		IdleTimeout:       apiIdleTimeout,
	}
}

// buildConfig projects felis.toml onto the build subsystem config (spec §16,
// §24). Unset fields fall back to the build package's hardened defaults
// (felis-build namespace + weak SA, 30m deadline, resource limits).
func buildConfig(cfg *config.Config) build.Config {
	return build.Config{
		Namespace:   cfg.Registry.BuildNamespace,
		RegistryURL: cfg.Registry.URL,
		// Empty overrides fall back to the registry's copies of the tools
		// (build.Tools), which felis mirror-build-tools keeps current.
		KanikoImage: cfg.Registry.KanikoImage,
		TrivyImage:  cfg.Registry.TrivyImage,
		CPULimit:    cfg.Registry.BuildCPULimit,
		MemLimit:    cfg.Registry.BuildMemLimit,
		DiskLimit:   cfg.Registry.BuildDiskLimit,
		// "auto" follows the startup probe (see probeBuildUserNamespaces).
		UserNamespaces:        cfg.Registry.BuildUserNamespaces,
		UserNamespacesProbe:   new(atomic.Bool),
		RuntimeClass:          cfg.Registry.BuildRuntimeClass,
		MaxConcurrent:         cfg.Registry.MaxConcurrentBuilds,
		TrivyDBRepository:     cfg.Registry.TrivyDBRepository,
		TrivyJavaDBRepository: cfg.Registry.TrivyJavaDBRepository,
		// The submit lane's derived context URLs live here; the fetch step's
		// service token goes nowhere else.
		ContextOrigin: internalAPIBaseURL(),
	}
}

// probeBuildUserNamespaces settles build_user_namespaces = "auto": one probe
// pod with hostUsers: false tells whether this node's kernel and runtime can run
// build pods in a user namespace. Builds submitted before it answers run without.
func probeBuildUserNamespaces(ctx context.Context, jobs *build.K8sJobs, cfg build.Config, stderr io.Writer) {
	if mode := cfg.UserNamespaces; mode != "" && mode != build.UserNamespacesAuto {
		return
	}
	if cfg.FelisImage == "" {
		fmt.Fprintln(stderr, "felis api: FELIS_IMAGE unset — build pods run without a user namespace")
		return
	}
	ok, err := jobs.ProbeUserNamespaces(ctx, cfg.FelisImage)
	cfg.UserNamespacesProbe.Store(ok)
	switch {
	case ok:
		fmt.Fprintln(stderr, "felis api: build pods run in a user namespace (hostUsers: false)")
	case err != nil:
		fmt.Fprintf(stderr, "felis api: build pods run without a user namespace: the probe failed: %v\n", err)
	default:
		fmt.Fprintln(stderr, "felis api: build pods run without a user namespace: this node cannot start a pod with hostUsers: false")
	}
}

// internalAPIBaseURL resolves the platform's internal-face base URL: the address
// the platform rendered into this pod (felis API base URL env), or — for a
// hand-rolled deployment that set none — the platform default control namespace,
// the same fallback setup.go uses to hand the login gate its address.
func internalAPIBaseURL() string {
	if base := os.Getenv(naming.EnvAPIBaseURL); base != "" {
		return base
	}
	return platform.InternalAPIBaseURL(platform.DefaultControlNamespace)
}

// uploadsSchemeRE matches a leading URL scheme like "s3://" or "gs://".
var uploadsSchemeRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)

// isLocalUploadsPath reports whether the user-uploads context base is a local
// filesystem path (a bare path or a file:// URL), i.e. one LocalContextStore can
// write to. An s3:// base routes to newS3UploadsStore instead; any other scheme
// has no implemented transport, so its uploads are left disabled (503).
func isLocalUploadsPath(base string) bool {
	if strings.HasPrefix(base, "file://") {
		return true
	}
	return !uploadsSchemeRE.MatchString(base)
}

// newS3UploadsStore builds the S3 blob transport for an s3:// user_uploads_context.
// The bucket + key prefix come from the base itself; the endpoint/region come from
// [registry.s3]; and the credentials are read from the environment variables named
// by access_key_ref / secret_key_ref (defaulting to the fixed env names the
// felis-api Deployment injects from the felis-uploads-s3 Secret). Any missing piece
// is an error, so the caller leaves Blobs nil and the upload endpoint returns 503
// rather than pretending it can persist a file.
func newS3UploadsStore(reg config.RegistryConfig) (submit.Blobs, error) {
	accessRef, secretRef := reg.S3.AccessKeyRef, reg.S3.SecretKeyRef
	if accessRef == "" {
		accessRef = platform.UploadsS3AccessKeyEnv
	}
	if secretRef == "" {
		secretRef = platform.UploadsS3SecretKeyEnv
	}
	accessKey, secretKey := os.Getenv(accessRef), os.Getenv(secretRef)
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("credentials env %s/%s are empty", accessRef, secretRef)
	}
	return submit.NewS3ContextStore(submit.S3StoreConfig{
		Base:      reg.UserUploadsContext,
		Endpoint:  reg.S3.Endpoint,
		Region:    reg.S3.Region,
		AccessKey: accessKey,
		SecretKey: secretKey,
	})
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

// backupConfig builds the on-demand backup executor's config from felis.toml plus
// the deployment-supplied image and backup PVC. BackupRoot mirrors restoreConfig —
// it MUST equal [archive] local_path so the recorded ref resolves the same way a
// later restore Job mounts it. ConfigSecret/ConfigMount are left to backupjob's
// defaults (the control-plane manifest names), which is the Secret this backup Job
// mounts to self-record its world_backups row.
func backupConfig(cfg *config.Config, image, backupPVC string) backupjob.Config {
	return backupjob.Config{
		Namespace:  cfg.K8s.Namespace,
		Image:      image,
		BackupPVC:  backupPVC,
		BackupRoot: cfg.Archive.LocalPath,
	}
}

// fileEditConfig builds the file editor's config from felis.toml plus the
// deployment-supplied image. It is the shortest of the three: the editor mounts
// only the world PVC, so it needs no archive coordinates at all, and everything
// else — the weak SA, the "/data" world root that makes paths match what the
// minecraft server itself sees, the runtime identity, and the size/time ceilings —
// falls back to the fileedit package's hardened defaults.
func fileEditConfig(cfg *config.Config, image string) fileedit.Config {
	return fileedit.Config{
		Namespace: cfg.K8s.Namespace,
		Image:     image,
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

// settleRestoreChains starts the restore behind each safety snapshot that has
// finished (and gives up the one behind a snapshot that failed). The world stays
// locked in between, so the interval is how long a finished snapshot keeps the
// server down before its restore begins.
func settleRestoreChains(ctx context.Context, a *api.API, stderr io.Writer) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.SettleRestoreChains(ctx); err != nil {
				fmt.Fprintf(stderr, "felis api: restore chains: %v\n", err)
			}
		}
	}
}

// reapRejectedContexts deletes, once an hour, the uploaded contexts of
// submissions rejected more than submit.RejectedContextRetention ago. Without it a
// rejected modpack keeps its bytes on the uploads store (and against its
// submitter's budget) until an admin deletes the row.
func reapRejectedContexts(ctx context.Context, m *submit.Manager, stderr io.Writer) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		n, err := m.ReapRejected(ctx, submit.RejectedContextRetention)
		if err != nil {
			fmt.Fprintf(stderr, "felis api: reap rejected uploads: %v\n", err)
		}
		if n > 0 {
			fmt.Fprintf(stderr, "felis api: deleted the uploaded contexts of %d rejected submission(s)\n", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// registryPruneInterval spaces the registry pruner's runs. The registry-gc
// sidecar sweeps once a day, so pruning more often only changes which sweep frees
// a layer.
const registryPruneInterval = 6 * time.Hour

// registryPruner deletes the registry manifests nothing references
// (internal/registryprune); the registry-gc sidecar frees their layers on its next
// sweep. It acts as the gate's prune principal, whose token the api Deployment
// injects from felis-registry-auth. Without the token the registry only grows,
// which is said once here.
func registryPruner(cfg *config.Config, store imageRefStore, servers serverLister, stderr io.Writer) *registryprune.Pruner {
	if cfg.Registry.URL == "" {
		return nil
	}
	token := os.Getenv(platform.RegistryPruneTokenEnv)
	if token == "" {
		fmt.Fprintf(stderr, "felis api: registry pruner disabled (%s unset) — images nothing uses are never deleted from the registry\n", platform.RegistryPruneTokenEnv)
		return nil
	}
	static := append([]string{os.Getenv("FELIS_IMAGE")}, buildConfig(cfg).ToolRefs()...)
	return &registryprune.Pruner{
		Registry: &registryprune.Client{Endpoint: "http://" + cfg.Registry.URL, Token: token},
		Host:     cfg.Registry.URL,
		Refs: func(ctx context.Context) ([]string, error) {
			return inUseImageRefs(ctx, store, servers, static)
		},
		Log: slog.New(slog.NewTextHandler(stderr, nil)),
	}
}

type imageRefStore interface {
	ListImages(ctx context.Context) ([]build.Image, error)
	ListUnfinishedBuilds(ctx context.Context) ([]build.Build, error)
}

type serverLister interface {
	ListServers(ctx context.Context) ([]api.ServerInfo, error)
	PodImages(ctx context.Context) ([]string, error)
}

// inUseImageRefs lists every image reference the platform still depends on: the
// whitelist (disabled rows too, an admin may enable them again), every server's
// spec, the images the game pods run, builds still running, and the images the
// control plane and the build Jobs run. Any source failing fails the whole list,
// so the pruner never decides on a partial view.
//
// The pods matter for the felis image: a running server keeps the one it started
// with across platform upgrades (operator.PodTemplateAnnotation), which after a
// few releases is no longer among the newest tags the pruner keeps anyway, and
// the pod needs it again whenever it is recreated.
func inUseImageRefs(ctx context.Context, store imageRefStore, servers serverLister, static []string) ([]string, error) {
	refs := append([]string(nil), static...)
	images, err := store.ListImages(ctx)
	if err != nil {
		return nil, fmt.Errorf("image whitelist: %w", err)
	}
	for _, img := range images {
		refs = append(refs, img.ImageRef)
	}
	srvs, err := servers.ListServers(ctx)
	if err != nil {
		return nil, fmt.Errorf("servers: %w", err)
	}
	for _, s := range srvs {
		refs = append(refs, s.Image)
	}
	podImages, err := servers.PodImages(ctx)
	if err != nil {
		return nil, fmt.Errorf("game pods: %w", err)
	}
	refs = append(refs, podImages...)
	builds, err := store.ListUnfinishedBuilds(ctx)
	if err != nil {
		return nil, fmt.Errorf("running builds: %w", err)
	}
	for _, b := range builds {
		refs = append(refs, b.ImageRef)
	}
	return refs, nil
}

// mailLimit turns smtp.max_per_hour into the API's install-wide mail bucket:
// the hourly cap as the refill rate, with a quarter of it (at least 5) allowed
// at once so a burst of real sign-ins is not queued behind the average.
func mailLimit(perHour int) api.RateLimit {
	if perHour <= 0 {
		perHour = config.DefaultMailPerHour
	}
	return api.RateLimit{Burst: max(perHour/4, 5), PerMinute: float64(perHour) / 60}
}

// imagePinner resolves a new server's image against the platform registry
// through its in-cluster Service, the address its refs already spell. An install
// without a registry has no platform-built images to pin.
func imagePinner(registry string) api.ImagePinner {
	if registry == "" {
		return nil
	}
	return imagepin.Resolver{Registry: registry}
}

// internalCallerTokens reads each internal caller's token from the env var the
// Deployment feeds it from (naming.CallerTokens). Two callers sharing a value
// would make the caller ambiguous, so that refuses to start.
func internalCallerTokens(getenv func(string) string) (api.CallerTokens, error) {
	tokens := map[api.Caller]string{}
	for _, ct := range naming.CallerTokens {
		tokens[api.Caller(ct.Caller)] = strings.TrimSpace(getenv(ct.APIEnv))
	}
	return api.NewCallerTokens(tokens)
}
