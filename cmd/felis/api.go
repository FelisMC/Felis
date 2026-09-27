package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
	"felis.lolicon.best/internal/retention"
	"felis.lolicon.best/internal/submit"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// mojangSessionServer is the public Mojang hasJoined endpoint the Felis-nano multiplexer
// leads with as its code-owned identity anchor (正版优先). A protocol constant, not a
// deployment domain, so it is hardcoded rather than configured — and it is the ONLY source
// the code marks Identity (UUIDs trusted verbatim); config can never add another.
const mojangSessionServer = "https://sessionserver.mojang.com/session/minecraft/hasJoined"

// passkeyRelyingParty is the one WebAuthn relying party both web faces share: its id
// is the player console host, derived from server.root_domain the way the panel
// handler derives it when auth.panel_hostname is unset, and its origins are that host
// plus the operator host. An empty id means the install names no panel host at all.
func passkeyRelyingParty(cfg *config.Config) (string, []string) {
	rpID := defaultPanelHostname(cfg.Server.RootDomain, cfg.Auth.PanelHostname)
	if rpID == "" {
		return "", nil
	}
	origins := []string{"https://" + rpID}
	if admin := defaultAdminHostname(cfg.Server.RootDomain, cfg.Auth.AdminHostname); admin != "" && admin != rpID {
		origins = append(origins, "https://"+admin)
	}
	return rpID, origins
}

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
// internal face authenticates per-caller service tokens; the external face
// authenticates the local session cookie.
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

	// Load already refused a malformed [audit] retention.
	auditRetention, _ := cfg.Audit.RetentionPeriod()
	if auditRetention == 0 {
		fmt.Fprintln(stdout, "felis api: audit rows are kept forever ([audit] retention = \"forever\")")
	} else {
		fmt.Fprintf(stdout, "felis api: audit rows older than %d days are deleted ([audit] retention; export them first with felis db audit-export)\n", int(auditRetention/(24*time.Hour)))
	}

	ctx := ctrl.SetupSignalHandler()

	// Before anything serves: an api on a schema it was not built for answers with
	// errors, or writes rows the other version cannot read.
	drv, err := openPodStore(ctx, cfg.Database.URL, "api", stderr)
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
	// every door that mails a code answers 503 mail_unavailable: a code that is
	// not mailed is never written anywhere else either.
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
		mailer = smtpRelay(cfg.SMTP, password)
		if !cfg.SMTP.TLSRequired() {
			fmt.Fprintf(stderr, "felis api: warning: [smtp] %s may be sent codes without TLS (require_tls off or a relay on this host)\n", cfg.SMTP.Host)
		}
	} else {
		fmt.Fprintln(stderr, "felis api: [smtp] not configured — email sign-in and verification are off (503 mail_unavailable); sign in with a passkey, or run felis setup to add a relay")
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
		Store:    build.NewPGStore(drv.DB()),
		Jobs:     buildJobs,
		Config:   buildCfg,
		Outcomes: build.NewK8sOutcomes(clientset, buildCfg),
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
	if blobs != nil {
		submissions.Parts = &submit.PartStore{Dir: uploadPartsDir(contextBase)}
	}
	if v := cfg.Registry.UserUploadsMaxBytes; v != "" {
		if n, err := parseByteSize(v); err != nil || n <= 0 {
			fmt.Fprintf(stderr, "felis api: [registry] user_uploads_max_bytes %q is not a positive size such as 4Gi; keeping the default\n", v)
		} else {
			submissions.MaxStoredBytesTotal = n
		}
	}
	if n, err := contextMaxBytes(cfg); err != nil {
		fmt.Fprintf(stderr, "felis api: [registry] context_max_bytes %q is not a positive size such as 512Mi; keeping the default\n", cfg.Registry.ContextMaxBytes)
	} else {
		submissions.MaxContextBytes = n
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
	//
	// Uploads additionally stage their bytes on this pod's disk until the Job
	// fetches them from the internal face; whatever a previous process staged is
	// orphaned (the index is in memory), so the stage starts empty.
	var files api.FileEditor
	var fileStage *fileedit.Stage
	if felisImage != "" {
		files = &fileedit.Editor{
			Runner: fileedit.NewK8sRunner(clientset),
			Config: fileEditConfig(cfg, felisImage),
		}
		fileStage = &fileedit.Stage{Dir: fileStagingDir()}
		if err := fileStage.Sweep(); err != nil {
			fmt.Fprintf(stderr, "felis api: %v — file uploads return 503\n", err)
			fileStage = nil
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

	serverCache, serversSynced, err := startServerCache(ctx, restCfg, scheme, cfg.K8s.Namespace, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "felis api: MinecraftServer cache: %v\n", err)
		return 1
	}
	cluster := api.NewK8sCluster(cl, cfg.K8s.Namespace).WithServerCache(serverCache, serversSynced)
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
		FileStage:     fileStage,
		// The file Job fetches an upload from here; it runs in the minecraft
		// namespace, where the internal face is reachable like it is for the login
		// gate.
		InternalBaseURL: internalAPIBaseURL(),
		Submissions:     submissions,
		Mailer:          mailer,
		Schedules:       repo,
		// The external face authenticates the local session cookie the sign-in doors
		// mint, live once `felis breakGlass` flips local_auth_enabled on. Cloudflare
		// Access, when the install sits behind it, is enforced at the edge only.
		External: api.SessionAuth{
			Repo:          repo,
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
	// party, not two. Without a panel host (neither auth.panel_hostname nor
	// server.root_domain) a.Passkey stays nil and the passkey routes honestly return
	// 503 (the authenticated enrollment boundary is still enforced by the handlers).
	if rpID, origins := passkeyRelyingParty(cfg); rpID == "" {
		fmt.Fprintln(stderr, "felis api: passkey verifier disabled (no panel host: set server.root_domain or auth.panel_hostname) — passkey endpoints return 503")
	} else if pv, err := passkey.New(rpID, "Felis", origins); err != nil {
		fmt.Fprintf(stderr, "felis api: passkey verifier disabled: %v — passkey endpoints return 503\n", err)
	} else {
		a.Passkey = pv
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
	go runSchedules(ctx, a, stderr)
	// A daily restore point of every world played since its last one, taken
	// once the server stops ([archive] scheduled_every; 0s turns it off).
	if backuper != nil && rcfg.ScheduledEvery > 0 {
		go scheduleBackups(ctx, &api.BackupScheduler{API: a, Store: repo, Jobs: jobStatus, Every: rcfg.ScheduledEvery}, stderr)
	} else {
		fmt.Fprintln(stderr, "felis api: scheduled backups off (needs the backup executor and [archive] scheduled_every above 0s)")
	}

	if pruner := registryPruner(cfg, builder.Store, cluster, stderr); pruner != nil {
		go pruner.Loop(ctx, registryPruneInterval)
	}
	go reapRejectedContexts(ctx, submissions, stderr)
	go retention.Loop(ctx, drv.DB(), retention.Policy{Audit: auditRetention}, retentionInterval, slog.Default())

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
		ScanFailOn:            cfg.Registry.ScanFailOn,
		ScanFailUnfixed:       cfg.Registry.ScanFailUnfixed,
		ScanAccept:            cfg.Registry.ScanAccept,
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

// runSchedules runs the servers' scheduled tasks (api.API.RunSchedules). The
// interval is how late a task may start, and how often a restart or backup in
// progress checks whether it can take its next step.
func runSchedules(ctx context.Context, a *api.API, stderr io.Writer) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.RunSchedules(ctx); err != nil {
				fmt.Fprintf(stderr, "felis api: scheduled tasks: %v\n", err)
			}
		}
	}
}

// scheduleBackups starts the scheduled backups (api.BackupScheduler). Each tick
// starts at most one, so the interval also spaces the worlds that stopped at
// the same time: a world that stops waits at most this long for its point to
// start once the Jobs ahead of it are done.
func scheduleBackups(ctx context.Context, s *api.BackupScheduler, stderr io.Writer) {
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Tick(ctx); err != nil {
				fmt.Fprintf(stderr, "felis api: scheduled backups: %v\n", err)
			}
		}
	}
}

// reapRejectedContexts deletes, once an hour, the uploaded contexts of
// submissions rejected more than submit.RejectedContextRetention ago, and the
// chunked uploads left untouched for submit.StalePartRetention. Without it a
// rejected modpack or an abandoned upload keeps its bytes on the uploads store
// (and against its submitter's budget) until an admin deletes the row.
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
		n, err = m.ReapStaleParts(submit.StalePartRetention)
		if err != nil {
			fmt.Fprintf(stderr, "felis api: reap abandoned uploads: %v\n", err)
		}
		if n > 0 {
			fmt.Fprintf(stderr, "felis api: deleted %d abandoned chunked upload(s)\n", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// retentionInterval spaces the runs that delete spent sign-in rows and audit rows
// past [audit] retention. The rows are spent for weeks before they go, so a few
// runs a day keep the tables flat.
const retentionInterval = 6 * time.Hour

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

// smtpRelay is the relay [smtp] names, with the resolved password and the TLS
// posture config.SMTPConfig.TLSRequired picks. felis api, the reaper and the
// watchdog all send through it, so none can drift to a weaker posture.
func smtpRelay(c config.SMTPConfig, password string) *mail.SMTP {
	return &mail.SMTP{
		Host:       c.Host,
		Port:       c.Port,
		From:       c.From,
		Username:   c.Username,
		Password:   password,
		RequireTLS: c.TLSRequired(),
	}
}

// startServerCache starts the informer that serves the api's fleet-wide
// MinecraftServer reads (api.K8sCluster.WithServerCache): one watch on the
// namespace instead of a full List per velocity pull, fleet page and wake. It
// caches MinecraftServers only — ReaderFailOnMissingInformer turns any other read
// through it into an error rather than a new informer the api's Role cannot back —
// indexes spec.subdomain for GetBySubdomain, and drops managedFields to keep the
// copy small. It returns without waiting: the reads block until the first list
// lands and /readyz reports not-ready until then.
func startServerCache(ctx context.Context, cfg *rest.Config, scheme *runtime.Scheme, namespace string, stderr io.Writer) (cache.Cache, func() bool, error) {
	c, err := cache.New(cfg, cache.Options{
		Scheme:                      scheme,
		DefaultNamespaces:           map[string]cache.Config{namespace: {}},
		DefaultTransform:            cache.TransformStripManagedFields(),
		ReaderFailOnMissingInformer: true,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := c.IndexField(ctx, &v1alpha1.MinecraftServer{}, api.SubdomainIndex, api.SubdomainOf); err != nil {
		return nil, nil, fmt.Errorf("index %s: %w", api.SubdomainIndex, err)
	}
	inf, err := c.GetInformer(ctx, &v1alpha1.MinecraftServer{})
	if err != nil {
		return nil, nil, err
	}
	go func() {
		if err := c.Start(ctx); err != nil {
			fmt.Fprintf(stderr, "felis api: MinecraftServer cache stopped: %v\n", err)
		}
	}()
	return c, inf.HasSynced, nil
}

// uploadPartsDir is where chunked uploads are staged: beside a local store's
// contexts, so the room check and the budget see one disk and a staged upload
// survives an API restart; for an s3:// store, on the uploads volume the
// platform mounts either way, or the pod's /tmp when run by hand without it.
func uploadPartsDir(contextBase string) string {
	if isLocalUploadsPath(contextBase) {
		return filepath.Join(strings.TrimPrefix(contextBase, "file://"), ".parts")
	}
	if fi, err := os.Stat(platform.UploadsLocalPath); err == nil && fi.IsDir() {
		return filepath.Join(platform.UploadsLocalPath, ".parts")
	}
	return filepath.Join(os.TempDir(), "felis-upload-parts")
}

// fileStagingDir is where file uploads wait for their Job: on the uploads
// volume, whose capacity is its own, or the pod's /tmp when run by hand without
// it — /tmp is the node's disk, which a burst of uploads should not fill.
func fileStagingDir() string {
	if fi, err := os.Stat(platform.UploadsLocalPath); err == nil && fi.IsDir() {
		return filepath.Join(platform.UploadsLocalPath, ".file-staging")
	}
	return filepath.Join(os.TempDir(), "felis-file-staging")
}

// contextMaxBytes resolves [registry] context_max_bytes. 0 keeps the submit
// package's own default (1 GiB). The Cloudflare edge refuses a single request
// body over 100 MB, which the panel's chunked upload stays under, so the edge
// does not lower the cap.
func contextMaxBytes(cfg *config.Config) (int64, error) {
	v := cfg.Registry.ContextMaxBytes
	if v == "" {
		return 0, nil
	}
	n, err := parseByteSize(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("not a positive size: %q", v)
	}
	return n, nil
}
