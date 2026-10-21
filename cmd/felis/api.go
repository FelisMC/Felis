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
	"felis.lolicon.best/internal/distributed"
	"felis.lolicon.best/internal/fileedit"
	"felis.lolicon.best/internal/imagepin"
	"felis.lolicon.best/internal/mail"
	"felis.lolicon.best/internal/mailsettings"
	"felis.lolicon.best/internal/metrics"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/nodecontrol"
	"felis.lolicon.best/internal/panel"
	"felis.lolicon.best/internal/passkey"
	"felis.lolicon.best/internal/placement"
	"felis.lolicon.best/internal/platform"
	"felis.lolicon.best/internal/reaper"
	"felis.lolicon.best/internal/registryprune"
	"felis.lolicon.best/internal/restore"
	"felis.lolicon.best/internal/retention"
	"felis.lolicon.best/internal/software"
	"felis.lolicon.best/internal/submit"
	"felis.lolicon.best/internal/worldexport"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// mojangSessionServer is the only auth source marked Identity (UUIDs trusted verbatim); config cannot add another.
const mojangSessionServer = "https://sessionserver.mojang.com/session/minecraft/hasJoined"

// passkeyRelyingParty returns the WebAuthn rp id (the player console host) and its allowed origins; empty id means no panel host.
func passkeyRelyingParty(cfg *config.Config) (string, []string) {
	rpID := defaultPanelHostname(cfg.Server.RootDomain, cfg.Auth.PanelHostname)
	if rpID == "" {
		return "", nil
	}
	origins := []string{"https://" + rpID, fmt.Sprintf("https://%s:%d", rpID, setupPanelNodePort())}
	if admin := defaultAdminHostname(cfg.Server.RootDomain, cfg.Auth.AdminHostname); admin != "" && admin != rpID {
		origins = append(origins, "https://"+admin, fmt.Sprintf("https://%s:%d", admin, setupPanelNodePort()))
	}
	return rpID, origins
}

// authSourcesFromConfig prepends Mojang to the configured [[auth_source]] entries; shared by `felis api` and `felis nano`.
func authSourcesFromConfig(configured []config.AuthSourceConfig) []api.AuthSource {
	sources := make([]api.AuthSource, 0, len(configured)+1)
	sources = append(sources, api.AuthSource{Tag: "mojang", URL: mojangSessionServer, Identity: true})
	for _, s := range configured {
		sources = append(sources, api.AuthSource{Tag: s.Tag, Prefix: s.Prefix, URL: s.URL, APIURL: s.APIURL})
	}
	return sources
}

// cmdAPI runs felis-api: an internal face (per-caller service tokens) and an external face (session cookie).
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

	auditRetention, _ := cfg.Audit.RetentionPeriod()
	if auditRetention == 0 {
		fmt.Fprintln(stdout, "felis api: audit rows are kept forever ([audit] retention = \"forever\")")
	} else {
		fmt.Fprintf(stdout, "felis api: audit rows older than %d days are deleted ([audit] retention; export them first with felis db audit-export)\n", int(auditRetention/(24*time.Hour)))
	}

	ctx := ctrl.SetupSignalHandler()

	drv, err := openPodStore(ctx, cfg.Database.URL, "api", stderr)
	if err != nil {
		fmt.Fprintf(stderr, "felis api: open database: %v\n", err)
		return 1
	}
	defer drv.Close()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	// The typed clientset is needed for pods/log streaming, which client.Client lacks.
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

	// With no [smtp] host the mailer stays nil and code-mailing endpoints answer 503 mail_unavailable.
	var mailer api.OTPMailer
	var smtpPassword string
	if cfg.SMTP.Host != "" {
		passRef := cfg.SMTP.PasswordRef
		if passRef == "" {
			passRef = platform.SMTPPasswordEnv
		}
		password := os.Getenv(passRef)
		smtpPassword = password
		if cfg.SMTP.Username != "" && password == "" {
			fmt.Fprintf(stderr, "felis api: warning: [smtp] username is set but credentials env %s is empty — OTP sends will fail AUTH\n", passRef)
		}
		mailer = smtpRelay(cfg.SMTP, password)
		if !cfg.SMTP.TLSRequired() {
			fmt.Fprintf(stderr, "felis api: warning: [smtp] %s may be sent codes without TLS (require_tls off or a relay on this host)\n", cfg.SMTP.Host)
		}
	} else {
		fmt.Fprintln(stderr, "felis api: no [smtp] in felis.toml — email codes go through the relay saved in the panel (Platform settings → Mail delivery); while there is none, email sign-in and verification answer 503 mail_unavailable")
	}

	buildCfg := buildConfig(cfg)
	buildCfg.FelisImage = os.Getenv("FELIS_IMAGE")
	buildJobs := build.NewK8sJobs(cl, buildCfg)
	builder := &build.Builder{
		Store:    build.NewPGStore(drv.DB()),
		Jobs:     buildJobs,
		Config:   buildCfg,
		Outcomes: build.NewK8sOutcomes(clientset, buildCfg),
	}
	go probeBuildUserNamespaces(ctx, buildJobs, buildCfg, stderr)

	// Build Pods fetch contexts through the internal API without uploads mounts or object-store credentials.
	contextBase := cfg.Registry.UserUploadsContext
	var blobs submit.Blobs
	switch {
	case isLocalUploadsPath(contextBase):
		// ContextStore and Base share one plain path so context_ref matches the write location.
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

	// The restore and backup executors need FELIS_IMAGE and FELIS_BACKUP_PVC; without them the endpoints return 503.
	var restorer api.Restorer
	felisImage, backupPVC := os.Getenv("FELIS_IMAGE"), os.Getenv("FELIS_BACKUP_PVC")
	worldResolver := placement.Resolve(cl, cfg.K8s.Namespace)
	distribution, err := distributionManager(cl, cfg, felisImage)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if distribution != nil {
		worldResolver = distribution.Resolve
	}
	if felisImage != "" && backupPVC != "" {
		rcfg := restoreConfig(cfg, felisImage, backupPVC)
		rcfg.ResolveWorld = worldResolver
		var jobs restore.Jobs = restore.NewK8sJobs(cl)
		if distribution != nil {
			jobs = distribution
		}
		restorer = &restore.Restorer{Jobs: jobs, Config: rcfg}
	} else {
		fmt.Fprintln(stderr, "felis api: restore executor disabled (needs FELIS_IMAGE and FELIS_BACKUP_PVC) — restore endpoint returns 503")
	}

	var backuper api.Backuper
	if felisImage != "" && backupPVC != "" {
		bcfg := backupConfig(cfg, felisImage, backupPVC)
		bcfg.ResolveWorld = worldResolver
		var jobs backupjob.Jobs = backupjob.NewK8sJobs(cl)
		if distribution != nil {
			jobs = distribution
		}
		backuper = &backupjob.Backuper{Jobs: jobs, Config: bcfg}
	} else {
		fmt.Fprintln(stderr, "felis api: backup executor disabled (needs FELIS_IMAGE and FELIS_BACKUP_PVC) — backup endpoint returns 503")
	}

	var exporter api.Exporter
	if felisImage != "" && backupPVC != "" {
		ecfg := exportConfig(cfg, felisImage, backupPVC)
		ecfg.ResolveWorld = worldResolver
		exporter = worldexport.New(clientset, ecfg)
	} else {
		fmt.Fprintln(stderr, "felis api: world export disabled (needs FELIS_IMAGE and FELIS_BACKUP_PVC) — export endpoints return 503")
	}

	// The file editor needs only FELIS_IMAGE; the upload stage starts empty because a previous process's staged files are orphaned.
	var files api.FileEditor
	var fileStage *fileedit.Stage
	var fileBrowser *fileedit.Browser
	if felisImage != "" {
		fcfg := fileEditConfig(cfg, felisImage)
		fcfg.ResolveWorld = worldResolver
		fileBrowser = &fileedit.Browser{BaseURL: internalAPIBaseURL()}
		runner := fileedit.NewK8sRunner(clientset)
		runner.Browser = fileBrowser
		files = &fileedit.Editor{
			Runner: runner,
			Config: fcfg,
		}
		fileStage = &fileedit.Stage{Dir: fileStagingDir()}
		if err := fileStage.Sweep(); err != nil {
			fmt.Fprintf(stderr, "felis api: %v — file uploads return 503\n", err)
			fileStage = nil
		}
	} else {
		fmt.Fprintln(stderr, "felis api: file editor disabled (needs FELIS_IMAGE) — file endpoints return 503")
	}

	repo := api.NewPGRepo(drv.DB())
	if err := api.RegisterStorePool(drv.DB()); err != nil {
		fmt.Fprintf(stderr, "felis api: store pool metrics unavailable: %v\n", err)
	}

	// A malformed [archive] key keeps the defaults here; the reaper Job fails on it and names it.
	rcfg, err := reaperConfig(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "felis api: %v; using the default backup limits\n", err)
		rcfg = reaper.DefaultConfig()
	}

	archive := &api.ArchivePolicy{Repo: repo, Host: rcfg, HostAudit: auditRetention}

	serverCache, serversSynced, err := startServerCache(ctx, restCfg, scheme, cfg.K8s.Namespace, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "felis api: MinecraftServer cache: %v\n", err)
		return 1
	}
	cluster := api.NewK8sCluster(cl, cfg.K8s.Namespace).WithServerCache(serverCache, serversSynced).WithDistributed(distribution != nil, os.Getenv("FELIS_CONTROLLER_NODE"))
	if distribution != nil {
		distribution.Record = func(ctx context.Context, b distributed.Backup) error {
			keep, retention, ok := backupPolicy(b.Reason, archive.Reaper(ctx))
			if !ok {
				return fmt.Errorf("unknown backup reason %s", b.Reason)
			}
			st := reaper.NewPGStore(drv.DB())
			if err := st.InsertBackup(ctx, reaper.BackupRecord{ID: "bk-" + b.ID, ServerName: b.Server, FormerOwner: b.Owner, BackupRef: b.Receipt.Ref, SizeBytes: b.Receipt.Size, SHA256: b.Receipt.SHA256, Reason: b.Reason, ExpiresAt: time.Now().Add(retention)}); err != nil {
				return err
			}
			pruneBackups(ctx, st, distribution.Archive, b.Server, b.Owner, b.Reason, keep, b.Protect, stdout, stderr)
			return nil
		}
	}
	jobStatus := api.NewK8sJobStatus(cl, cfg.K8s.Namespace)
	a := &api.API{
		Repo:            repo,
		Cluster:         cluster,
		Console:         api.NewK8sConsole(cl, cfg.K8s.Namespace),
		Logs:            api.NewK8sLogStreamer(clientset, cfg.K8s.Namespace),
		BuildLogs:       api.NewK8sBuildLogStreamer(clientset, cfg.Registry.BuildNamespace),
		Internal:        internalAuth,
		Builder:         builder,
		Images:          imagePinner(cfg.Registry.URL),
		Software:        &software.Resolver{CacheTTL: 10 * time.Minute},
		RuntimeRegistry: cfg.Registry.URL,
		Restorer:        restorer,
		Backuper:        backuper,
		JobStatus:       jobStatus,
		RestoreChains:   jobStatus,
		Files:           files,
		FileStage:       fileStage,
		FileBrowser:     fileBrowser,
		InternalBaseURL: internalAPIBaseURL(),
		Exporter:        exporter,
		Submissions:     submissions,
		Mailer:          mailer,
		Schedules:       repo,
		External: api.SessionAuth{
			Repo:          repo,
			RootDomain:    cfg.Server.RootDomain,
			AdminHostname: cfg.Auth.AdminHostname,
		},
		RootDomain:     cfg.Server.RootDomain,
		AdminHostname:  cfg.Auth.AdminHostname,
		PanelHostname:  cfg.Auth.PanelHostname,
		WakeCooldown:   30 * time.Second,
		BackupCooldown: rcfg.ManualCooldown,
		BackupStoreCap: rcfg.MaxLocalBytes,
		ArchivePolicy:  archive,
		AuditExport: func(ctx context.Context, since, until time.Time, w io.Writer) (int, error) {
			return retention.ExportAudit(ctx, drv.DB(), since, until, w)
		},
		SubmitCreateCooldown:   30 * time.Second,
		SubmitUploadCooldown:   15 * time.Second,
		MaxStreamsPerPrincipal: 16,
		AuthDoorLimit:          api.RateLimit{Burst: 20, PerMinute: 20},
		ClientIPHeader:         cfg.Auth.EffectiveClientIPHeader(),
		MailLimit:              api.MailRateLimit(cfg.SMTP.MaxPerHour),
	}
	if a.ClientIPHeader != "" {
		fmt.Fprintf(stderr, "felis api: sign-in rate limit keys on the %s header\n", a.ClientIPHeader)
	} else {
		fmt.Fprintln(stderr, "felis api: sign-in rate limit keys on the TCP peer ([auth] client_ip_header unset)")
	}

	// An empty [[auth_source]] list still relays to Mojang; a nil list would 204 every login.
	a.AuthSources = authSourcesFromConfig(cfg.AuthSources)
	a.AuthSourceSettings = &api.AuthSourceSettings{Repo: repo, Defaults: a.AuthSources}
	a.MailSettings = &api.MailSettings{Repo: repo, Host: hostMailSettings(cfg.SMTP, smtpPassword), NewMailer: func(s mailsettings.Settings) api.OTPMailer { return s.SMTP() }}
	fmt.Fprintf(stderr, "felis api: hasJoined multiplexer active — Mojang + %d default third-party source(s); panel settings take precedence\n", len(cfg.AuthSources))

	// One relying party spans both web faces: the panel host is a domain suffix of the operator host.
	if rpID, origins := passkeyRelyingParty(cfg); rpID == "" {
		fmt.Fprintln(stderr, "felis api: passkey verifier disabled (no panel host: set server.root_domain or auth.panel_hostname) — passkey endpoints return 503")
	} else if pv, err := passkey.New(rpID, "Felis", origins); err != nil {
		fmt.Fprintf(stderr, "felis api: passkey verifier disabled: %v — passkey endpoints return 503\n", err)
	} else {
		a.Passkey = pv
	}

	externalHandler := panel.Handler(a.ExternalHandler(), cfg.Server.RootDomain,
		defaultPanelHostname(cfg.Server.RootDomain, cfg.Auth.PanelHostname),
		defaultAdminHostname(cfg.Server.RootDomain, cfg.Auth.AdminHostname),
		cfg.Velocity.GamePort, cfg.Velocity.GameVersion, resolvedVersion(), panel.Topology{Distributed: distribution != nil, ControllerHA: distribution != nil && os.Getenv("FELIS_CONTROLLER_HA") == "true"})
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

	if distribution != nil {
		a.Distribution = distribution
	}
	if socket := os.Getenv("FELIS_NODE_CONTROL_SOCKET"); socket != "" {
		a.NodeControl = nodecontrol.NewClient(socket)
		cluster.NodeMaintenanceGuard = api.NodeMaintenanceGuard(a.NodeControl)
	}
	if fileStage != nil {
		go expireFileSessions(ctx, fileStage, fileSessionSweep, stderr)
	}
	if exporter != nil {
		go expireExports(ctx, a, exportSweep)
	}
	// These loops run only in the felis-api instance holding the lease (leadLoops).
	loops := func(ctx context.Context) {
		go reconcileBuilds(ctx, builder, stderr)
		go settleRestoreChains(ctx, a, stderr)
		go settleLiveBackups(ctx, a, stderr)
		go runSchedules(ctx, a, stderr)
		go notifyServerEvents(ctx, a, stderr)
		if backuper != nil {
			go scheduleBackups(ctx, &api.BackupScheduler{API: a, Store: repo, Jobs: jobStatus, Every: rcfg.ScheduledEvery}, stderr)
		} else {
			fmt.Fprintln(stderr, "felis api: scheduled backups off (needs the backup executor)")
		}
		if pruner := registryPruner(cfg, builder.Store, cluster, stderr); pruner != nil {
			go pruner.Loop(ctx, registryPruneInterval)
		}
		go reapRejectedContexts(ctx, submissions, stderr)
		go retention.Loop(ctx, drv.DB(), func(ctx context.Context) retention.Policy {
			return retention.Policy{Audit: archive.Audit(ctx)}
		}, retentionInterval, slog.Default())
		if distribution != nil {
			go reconcileDistribution(ctx, distribution, stderr)
			go runFailover(ctx, a, stderr)
		}
	}
	go leadLoops(ctx, clientset, podNamespace(), loops, stderr)
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
	apiReadHeaderTimeout = 10 * time.Second // header phase only, so SSE streams are unaffected (Slowloris guard)
	apiIdleTimeout       = 120 * time.Second
	apiShutdownGrace     = 20 * time.Second // listener drain after SIGTERM; the pod gets 30s before SIGKILL
)

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

// newAPIServer builds an http.Server with header and idle timeouts. Read and Write timeouts stay unset because the faces stream SSE for the life of a client.
func newAPIServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: apiReadHeaderTimeout,
		IdleTimeout:       apiIdleTimeout,
	}
}

func buildConfig(cfg *config.Config) build.Config {
	return build.Config{
		Namespace:             cfg.Registry.BuildNamespace,
		RegistryURL:           cfg.Registry.URL,
		KanikoImage:           cfg.Registry.KanikoImage,
		TrivyImage:            cfg.Registry.TrivyImage,
		CPULimit:              cfg.Registry.BuildCPULimit,
		MemLimit:              cfg.Registry.BuildMemLimit,
		DiskLimit:             cfg.Registry.BuildDiskLimit,
		UserNamespaces:        cfg.Registry.BuildUserNamespaces,
		UserNamespacesProbe:   new(atomic.Bool),
		RuntimeClass:          cfg.Registry.BuildRuntimeClass,
		MaxConcurrent:         cfg.Registry.MaxConcurrentBuilds,
		TrivyDBRepository:     cfg.Registry.TrivyDBRepository,
		TrivyJavaDBRepository: cfg.Registry.TrivyJavaDBRepository,
		ScanFailOn:            cfg.Registry.ScanFailOn,
		ScanFailUnfixed:       cfg.Registry.ScanFailUnfixed,
		ScanAccept:            cfg.Registry.ScanAccept,
		ContextOrigin:         internalAPIBaseURL(),
	}
}

// probeBuildUserNamespaces settles build_user_namespaces = "auto" with one hostUsers: false probe pod; builds submitted before it answers run without.
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

func internalAPIBaseURL() string {
	if base := os.Getenv(naming.EnvAPIBaseURL); base != "" {
		return base
	}
	return platform.InternalAPIBaseURL(platform.DefaultControlNamespace)
}

var uploadsSchemeRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)

func isLocalUploadsPath(base string) bool {
	if strings.HasPrefix(base, "file://") {
		return true
	}
	return !uploadsSchemeRE.MatchString(base)
}

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

// restoreConfig projects felis.toml onto the restore config. BackupRoot must equal [archive] local_path because tarLocal refs are absolute.
func restoreConfig(cfg *config.Config, image, backupPVC string) restore.Config {
	return restore.Config{
		Namespace:    cfg.K8s.Namespace,
		Image:        image,
		BackupPVC:    backupPVC,
		ArchiveStore: cfg.Archive.Store,
		BackupRoot:   cfg.Archive.LocalPath,
	}
}

func backupConfig(cfg *config.Config, image, backupPVC string) backupjob.Config {
	return backupjob.Config{
		Namespace:  cfg.K8s.Namespace,
		Image:      image,
		BackupPVC:  backupPVC,
		BackupRoot: cfg.Archive.LocalPath,
	}
}

func exportConfig(cfg *config.Config, image, backupPVC string) worldexport.Config {
	return worldexport.Config{
		Namespace:  cfg.K8s.Namespace,
		Image:      image,
		BackupPVC:  backupPVC,
		BackupRoot: cfg.Archive.LocalPath,
	}
}

func fileEditConfig(cfg *config.Config, image string) fileedit.Config {
	return fileedit.Config{
		Namespace: cfg.K8s.Namespace,
		Image:     image,
	}
}

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

func settleLiveBackups(ctx context.Context, a *api.API, stderr io.Writer) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.SettleLiveBackups(ctx); err != nil {
				fmt.Fprintf(stderr, "felis api: live backups: %v\n", err)
			}
		}
	}
}

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

func notifyServerEvents(ctx context.Context, a *api.API, stderr io.Writer) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.NotifyServerEvents(ctx); err != nil {
				fmt.Fprintf(stderr, "felis api: lifecycle mail: %v\n", err)
			}
		}
	}
}

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

const fileSessionSweep = 10 * time.Minute

func expireFileSessions(ctx context.Context, s *fileedit.Stage, every time.Duration, stderr io.Writer) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := s.Expire(); n > 0 {
				fmt.Fprintf(stderr, "felis api: dropped %d upload session(s) left idle for %s\n", n, fileedit.SessionIdle)
			}
		}
	}
}

const exportSweep = time.Minute

// expireExports runs the export sweep (api.API.ExpireExports) on a ticker; a stalled export Job would otherwise block server start until its deadline.
func expireExports(ctx context.Context, a interface{ ExpireExports() }, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.ExpireExports()
		}
	}
}

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

const retentionInterval = 6 * time.Hour

const registryPruneInterval = 6 * time.Hour

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

// Include running Pods' images; any failed source aborts pruning rather than returning partial references.
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

func imagePinner(registry string) api.ImagePinner {
	if registry == "" {
		return nil
	}
	return imagepin.Resolver{Registry: registry}
}

func internalCallerTokens(getenv func(string) string) (api.CallerTokens, error) {
	tokens := map[api.Caller]string{}
	for _, ct := range naming.CallerTokens {
		tokens[api.Caller(ct.Caller)] = strings.TrimSpace(getenv(ct.APIEnv))
	}
	return api.NewCallerTokens(tokens)
}

// smtpRelay builds the [smtp] relay with the TLS posture from config.SMTPConfig.TLSRequired; shared by api, reaper and watchdog.
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

// startServerCache starts the MinecraftServer informer behind api.K8sCluster.WithServerCache. It returns without
// waiting; reads block until the first list and /readyz stays not-ready until then.
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

// uploadPartsDir is where chunked uploads are staged: beside a local store's contexts, else the uploads volume, else /tmp.
func uploadPartsDir(contextBase string) string {
	if isLocalUploadsPath(contextBase) {
		return filepath.Join(strings.TrimPrefix(contextBase, "file://"), ".parts")
	}
	if fi, err := os.Stat(platform.UploadsLocalPath); err == nil && fi.IsDir() {
		return filepath.Join(platform.UploadsLocalPath, ".parts")
	}
	return filepath.Join(os.TempDir(), "felis-upload-parts")
}

func fileStagingDir() string {
	if fi, err := os.Stat(platform.UploadsLocalPath); err == nil && fi.IsDir() {
		return filepath.Join(platform.UploadsLocalPath, ".file-staging")
	}
	return filepath.Join(os.TempDir(), "felis-file-staging")
}

// contextMaxBytes resolves [registry] context_max_bytes; 0 keeps the submit package default (1 GiB).
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

func hostMailSettings(c config.SMTPConfig, password string) *mailsettings.Settings {
	if c.Host == "" {
		return nil
	}
	return &mailsettings.Settings{Host: c.Host, Port: c.Port, From: c.From, Username: c.Username, Password: password, RequireTLS: c.TLSRequired(), MaxPerHour: c.MaxPerHour}
}
