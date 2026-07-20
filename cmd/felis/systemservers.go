package main

import (
	"context"
	"fmt"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// System services are the always-on backends Felis provisions for itself after
// setup: the login limbo (LOOHP/Limbo auth gate) and the lobby (Paper + the
// felis-paper /menu hub). Unlike a user server they are created Running, are
// exempt from the world reaper, and carry reserved names — so they take the
// ValidateSystemServerName admission path rather than the user ValidateServerName.
//
// The routing topology and the one security invariant they encode:
//
//	connect → login (auth gate, front door) → lobby (/menu hub) → target backend
//
// A stopped/starting server's fallback must land on the LOGIN gate, never on the
// lobby: falling back to the lobby would drop an unauthenticated player past the
// gate. So login has NO fallback (if it is down we refuse the connection rather
// than route onward) and everything else — the lobby included — falls back to
// login. "Rather have login unreachable than abandon authentication."

// systemServerSpec is the small, explicit shape a system service is built from.
// It is intentionally narrower than the user applyRequest: no autostart choice
// (always public), no RCON, no resource overrides — a system service is uniform
// by construction so the invariants above cannot be configured away.
type systemServerSpec struct {
	name           string
	subdomain      string
	displayName    string
	image          string
	memory         string // container memory limit == request (§22 ceiling)
	storage        string // world PVC size
	fallbackServer string // "" = none (refuse when down); never the lobby
	healthHTTPPort int32  // > 0 → gate readiness on an HTTP health endpoint
	// rcon opts a system service into the RCON write channel. It is per-service and
	// NOT a default, because enabling it on a backend that runs no RCON listener is
	// actively destructive rather than merely useless: the operator gates readiness
	// on the probe, so the server would never leave Starting and would eventually be
	// marked Failed. The login limbo is exactly that case (LOOHP/Limbo has no RCON),
	// and it is the front door — taking it down locks everyone out.
	rcon bool
	// env are extra plain (non-secret) environment variables baked into the pod.
	// System-service configuration derived from the deployment (the internal API
	// URL, root domain, lobby name) rides here; secrets never do — the service
	// token is injected by the operator via secretKeyRef, not as a literal value.
	env []v1alpha1.EnvVar
}

// systemRcon renders the RCON block for a system service. The secret name comes
// from naming.RconSecretName — the same convention felis-api writes for user
// servers and the operator provisions against — so a system service is not a
// second, parallel way of doing this. Port is left 0 so the operator's default is
// the only place the number lives.
func systemRcon(in systemServerSpec) v1alpha1.RconSpec {
	if !in.rcon {
		return v1alpha1.RconSpec{Enabled: false}
	}
	return v1alpha1.RconSpec{
		Enabled: true,
		SecretRef: v1alpha1.SecretKeyRef{
			Name: naming.RconSecretName(in.name),
			Key:  naming.RconSecretKey,
		},
	}
}

// felisLimboHealthPort is the port the felis-limbo readiness plugin serves its
// HTTP health endpoint on. The login system service gates pod readiness on it so
// "the limbo has finished starting" — not merely "the game socket is bound" —
// is what marks it Ready.
const felisLimboHealthPort int32 = 8080

// The felis-limbo login plugin reads its deployment configuration from these
// environment variables (env wins over its felis-link.properties template). The
// non-secret four are baked into the login pod's Spec.Env here at provision time
// (they derive from the deployment: the internal API URL, the root domain, the
// resolved panel host, the lobby server name); the service-token secret is injected
// separately by the operator via secretKeyRef. Without the token the plugin
// fail-safes to readiness-only, so a login pod that has the URL/domain but not yet
// the token is safe (it simply does not authenticate) rather than broken.
const (
	envAPIBaseURL    = "FELIS_API_BASE_URL"
	envRootDomain    = "FELIS_ROOT_DOMAIN"
	envPanelHostname = "FELIS_PANEL_HOSTNAME"
	envLobbyServer   = "FELIS_LOBBY_SERVER"
)

// buildSystemServer constructs an always-on, reaper-exempt MinecraftServer from
// a systemServerSpec. It is a pure function (no K8s, no I/O) so it is unit
// testable without a cluster. Unlike buildMinecraftServerFromApplyRequest it:
//   - permits reserved names (login/lobby) via ValidateSystemServerName,
//   - sets DesiredState=Running (the service is up the moment it exists),
//   - sets ReaperExempt=true and AutostartPolicy=public,
//   - enables RCON only where the image actually serves it (in.rcon): the lobby
//     is Paper and needs the write channel like any user server, while the login
//     limbo has no RCON listener at all and gates readiness on pod TCP/HTTP
//     health instead — see the operator reconciler.
func buildSystemServer(in systemServerSpec, namespace string) (*v1alpha1.MinecraftServer, error) {
	if err := naming.ValidateSystemServerName(in.name); err != nil {
		return nil, fmt.Errorf("invalid name: %w", err)
	}
	if err := naming.ValidateSystemServerName(in.subdomain); err != nil {
		return nil, fmt.Errorf("invalid subdomain: %w", err)
	}
	if in.image == "" {
		return nil, fmt.Errorf("image is required for system server %q", in.name)
	}
	// A system service must never fall back onto the lobby: that would route an
	// unauthenticated player past the login gate. Refuse to build one that does,
	// rather than silently ship the bypass.
	if in.fallbackServer == naming.SystemLobbyServer {
		return nil, fmt.Errorf("system server %q must not fall back to the lobby (%q) — it would bypass the login gate; fall back to %q or leave it empty",
			in.name, naming.SystemLobbyServer, naming.SystemLoginServer)
	}

	memQ, err := resource.ParseQuantity(in.memory)
	if err != nil {
		return nil, fmt.Errorf("invalid memory %q for %q: %w", in.memory, in.name, err)
	}
	if memQ.Sign() <= 0 {
		return nil, fmt.Errorf("memory must be positive for %q", in.name)
	}
	storageQ, err := resource.ParseQuantity(in.storage)
	if err != nil {
		return nil, fmt.Errorf("invalid storage %q for %q: %w", in.storage, in.name, err)
	}
	if storageQ.Sign() <= 0 {
		return nil, fmt.Errorf("storage must be positive for %q", in.name)
	}

	limits := corev1.ResourceList{corev1.ResourceMemory: memQ}
	requests := corev1.ResourceList{corev1.ResourceMemory: memQ}

	return &v1alpha1.MinecraftServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      in.name,
			Namespace: namespace,
			Labels: map[string]string{
				v1alpha1.LabelSystemRole: in.name,
			},
		},
		Spec: v1alpha1.MinecraftServerSpec{
			Subdomain:       in.subdomain,
			DisplayName:     in.displayName,
			Image:           in.image,
			JavaMemory:      deriveApplyJavaHeap(memQ),
			DesiredState:    v1alpha1.DesiredRunning,
			AutostartPolicy: v1alpha1.AutostartPublic,
			ReaperExempt:    true,
			FallbackServer:  in.fallbackServer,
			// Behind the Velocity proxy (which enforces online-mode and modern
			// forwarding), backends run offline-mode; the proxy is the one place
			// online-mode is true (spec §8, §11).
			OnlineMode: false,
			Rcon:       systemRcon(in),
			Storage:    v1alpha1.StorageSpec{Size: storageQ.String()},
			Resources:  corev1.ResourceRequirements{Limits: limits, Requests: requests},
			Startup:    v1alpha1.StartupSpec{HealthHTTPPort: in.healthHTTPPort},
			Env:        in.env,
		},
	}, nil
}

// loginSystemServer is the LOOHP/Limbo auth gate. It is the front door and the
// only safe fallback, so it carries no fallback of its own: if it is down the
// proxy refuses the connection rather than routing onward past authentication.
//
// apiBaseURL is the felis-api internal face the login plugin authenticates to;
// panelHostname is the resolved console/panel host the plugin links players at (the
// single source of truth for that host — see defaultPanelHostname), and rootDomain
// is kept for the plugin's own console.<root> fallback when the panel env is absent
// (an older operator). All three are baked in as plain env. The service token is NOT
// passed here — the operator injects it via secretKeyRef so the credential never
// lands in the CRD.
func loginSystemServer(image, namespace, apiBaseURL, rootDomain, panelHostname string) (*v1alpha1.MinecraftServer, error) {
	return buildSystemServer(systemServerSpec{
		name:           naming.SystemLoginServer,
		subdomain:      naming.SystemLoginServer,
		displayName:    "Login",
		image:          image,
		memory:         "512Mi",
		storage:        "1Gi",
		fallbackServer: "", // none — refuse if the gate is down
		healthHTTPPort: felisLimboHealthPort,
		env: []v1alpha1.EnvVar{
			{Name: envAPIBaseURL, Value: apiBaseURL},
			{Name: envRootDomain, Value: rootDomain},
			{Name: envPanelHostname, Value: panelHostname},
			{Name: envLobbyServer, Value: naming.SystemLobbyServer},
		},
	}, namespace)
}

// lobbySystemServer is the post-auth /menu hub (Paper + felis-paper). It falls
// back to the login gate — never to itself and never onward — so a lobby that is
// briefly down still routes players through authentication first.
func lobbySystemServer(image, namespace string) (*v1alpha1.MinecraftServer, error) {
	return buildSystemServer(systemServerSpec{
		name:           naming.SystemLobbyServer,
		subdomain:      naming.SystemLobbyServer,
		displayName:    "Lobby",
		image:          image,
		memory:         "1Gi",
		storage:        "2Gi",
		fallbackServer: naming.SystemLoginServer,
		// Paper serves RCON, and the lobby is administered through the panel like any
		// other server — online players, console, permissions all ride this channel.
		rcon: true,
	}, namespace)
}

// buildSystemServerClient builds a controller-runtime client for the setup-time
// system-service provisioner. It is deliberately best-effort and never calls
// ctrl.SetupSignalHandler (setup owns its own context): it first honours the
// standard resolution (in-cluster, then $KUBECONFIG / --kubeconfig, then
// ~/.kube/config) and, failing that, falls back to the k3s admin kubeconfig the
// host bootstrap writes at hostBootstrapKubeconfigPath — the common case when
// setup runs as root directly on a single-node control-plane host. A returned
// error is not fatal to setup; the caller degrades to printed guidance.
func buildSystemServerClient() (client.Client, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		cfg, err = clientcmd.BuildConfigFromFlags("", hostBootstrapKubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("no reachable kubeconfig (tried in-cluster/$KUBECONFIG/~/.kube and %s): %w", hostBootstrapKubeconfigPath, err)
		}
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

// systemServerOutcome records what ensureSystemServers did with one service so
// setup can report it without the provisioner deciding on the output format.
type systemServerOutcome struct {
	name      string
	created   bool   // true = we created it this run
	available bool   // true = the required object now exists
	skipped   string // non-empty = why it was skipped (image unset / already exists)
	err       error  // non-nil = create failed
}

// ensureSystemServers idempotently creates the login and lobby system services.
// It create-if-absent per service: an existing CRD is left untouched (so an
// operator's later edits to a system service survive re-runs of setup), a
// service whose image is unset in config is skipped with a reason, and any other
// service is created. It never deletes or overwrites. The caller supplies the
// K8s client and namespace; this function performs no signal-handler or client
// setup of its own.
func ensureSystemServers(ctx context.Context, cl client.Client, namespace, loginImage, lobbyImage, apiBaseURL, rootDomain, panelHostname string) []systemServerOutcome {
	type plan struct {
		name  string
		image string
		build func(image, namespace string) (*v1alpha1.MinecraftServer, error)
	}
	plans := []plan{
		{name: naming.SystemLoginServer, image: loginImage, build: func(image, ns string) (*v1alpha1.MinecraftServer, error) {
			return loginSystemServer(image, ns, apiBaseURL, rootDomain, panelHostname)
		}},
		{name: naming.SystemLobbyServer, image: lobbyImage, build: lobbySystemServer},
	}

	outcomes := make([]systemServerOutcome, 0, len(plans))
	for _, p := range plans {
		if p.image == "" {
			outcomes = append(outcomes, systemServerOutcome{name: p.name, skipped: "image not configured"})
			continue
		}
		ms, err := p.build(p.image, namespace)
		if err != nil {
			outcomes = append(outcomes, systemServerOutcome{name: p.name, err: err})
			continue
		}
		// Create-if-absent: check first so an existing service is reported as a
		// deliberate skip rather than an AlreadyExists error. Never adopt a
		// legacy user server that happens to occupy a reserved system name.
		var existing v1alpha1.MinecraftServer
		getErr := cl.Get(ctx, client.ObjectKeyFromObject(ms), &existing)
		if getErr == nil {
			if existing.Labels[v1alpha1.LabelSystemRole] != p.name {
				outcomes = append(outcomes, systemServerOutcome{
					name: p.name,
					err: fmt.Errorf(
						"existing MinecraftServer %s/%s is not marked as the Felis %q system role; remove or rename it, then rerun setup",
						namespace, p.name, p.name,
					),
				})
				continue
			}
			refreshed, err := refreshDerivedEnv(ctx, cl, &existing, ms)
			if err != nil {
				outcomes = append(outcomes, systemServerOutcome{name: p.name, err: err})
				continue
			}
			skipped := "already exists"
			if refreshed {
				skipped = "already exists; refreshed the console hostnames it points players at"
			}
			outcomes = append(outcomes, systemServerOutcome{name: p.name, available: true, skipped: skipped})
			continue
		}
		if !apierrors.IsNotFound(getErr) {
			outcomes = append(outcomes, systemServerOutcome{name: p.name, err: getErr})
			continue
		}
		if err := cl.Create(ctx, ms); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Close the Get/Create race without trusting the object that won it.
				var raced v1alpha1.MinecraftServer
				if getErr := cl.Get(ctx, client.ObjectKeyFromObject(ms), &raced); getErr != nil {
					outcomes = append(outcomes, systemServerOutcome{name: p.name, err: getErr})
					continue
				}
				if raced.Labels[v1alpha1.LabelSystemRole] != p.name {
					outcomes = append(outcomes, systemServerOutcome{
						name: p.name,
						err: fmt.Errorf(
							"concurrent MinecraftServer %s/%s is not marked as the Felis %q system role; refusing to adopt it",
							namespace, p.name, p.name,
						),
					})
					continue
				}
				outcomes = append(outcomes, systemServerOutcome{name: p.name, available: true, skipped: "already exists"})
				continue
			}
			outcomes = append(outcomes, systemServerOutcome{name: p.name, err: err})
			continue
		}
		outcomes = append(outcomes, systemServerOutcome{name: p.name, created: true, available: true})
	}
	return outcomes
}

// derivedSystemEnv are the system-server env vars whose values setup computes from
// config rather than inventing. They are the exception to create-if-absent, and the
// exception is narrow on purpose.
//
// Everything else on an existing system service is left alone so an operator's edits
// survive a re-run — but these are not the operator's to own, they are a copy of
// config that goes stale the moment config changes. That is not hypothetical: after
// a root-domain change the login gate keeps handing every joining player a console
// link built from the OLD domain, which is the one screen an unauthenticated player
// is guaranteed to see. Nothing else in the install rewrites them, so a re-run of
// setup is the only chance they get to catch up.
var derivedSystemEnv = map[string]bool{
	envAPIBaseURL:    true,
	envRootDomain:    true,
	envPanelHostname: true,
}

// refreshDerivedEnv converges the config-derived env of an existing system server
// onto what setup just computed, and reports whether anything actually changed.
//
// It only ever overwrites a name that is already present with a different value, and
// only for the names above: env the operator added by hand is untouched, and a name
// missing from the live object is left missing rather than added back, since a
// deliberate removal is indistinguishable from drift and re-adding it would fight the
// operator every run.
func refreshDerivedEnv(ctx context.Context, cl client.Client, existing, desired *v1alpha1.MinecraftServer) (bool, error) {
	want := make(map[string]string, len(derivedSystemEnv))
	for _, e := range desired.Spec.Env {
		if derivedSystemEnv[e.Name] {
			want[e.Name] = e.Value
		}
	}

	changed := false
	for i, e := range existing.Spec.Env {
		if v, ok := want[e.Name]; ok && v != e.Value {
			existing.Spec.Env[i].Value = v
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	if err := cl.Update(ctx, existing); err != nil {
		return false, fmt.Errorf("refresh %s env: %w", existing.Name, err)
	}
	return true, nil
}

// The login gate is a hard prerequisite of the Owner bind, so setup waits for it
// rather than racing it. The ceiling covers a cold image pull on a fresh node;
// the poll is fast enough that a warm start feels immediate.
const (
	loginGateReadyTimeout = 5 * time.Minute
	loginGatePollInterval = 3 * time.Second
)

// awaitLoginGateReady blocks until the login system server reports status.ready.
//
// The Owner claims their seat by JOINING the game and running /link, so the gate
// being up is not a nicety — it is the precondition for the very next thing setup
// asks of the operator. progress is called on each phase change so the caller can
// show movement during a cold image pull; it may be nil.
func awaitLoginGateReady(ctx context.Context, cl client.Client, namespace string, timeout, poll time.Duration, progress func(v1alpha1.Phase)) error {
	key := client.ObjectKey{Namespace: namespace, Name: naming.SystemLoginServer}
	deadline := time.Now().Add(timeout)
	last := v1alpha1.Phase("")
	for {
		var ms v1alpha1.MinecraftServer
		switch err := cl.Get(ctx, key, &ms); {
		case err == nil:
			if ms.Status.Ready {
				return nil
			}
			if ms.Status.Phase != last {
				last = ms.Status.Phase
				if progress != nil {
					progress(last)
				}
			}
			// The operator only marks Failed once its OWN startup deadline has already
			// elapsed, so Failed is a settled verdict rather than a transient — sitting
			// out the rest of our timeout on top of it would only hide the reason.
			if ms.Status.Phase == v1alpha1.PhaseFailed {
				return fmt.Errorf("the login gate failed to start: %s", readyConditionMessage(&ms))
			}
		case !apierrors.IsNotFound(err):
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timed out after %s waiting for the login gate to become ready (last phase: %s)", timeout, phaseOrPending(last))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// readyConditionMessage is the operator's own account of why the gate is not
// ready — far more useful to an operator than "phase: Failed".
func readyConditionMessage(ms *v1alpha1.MinecraftServer) string {
	if c := meta.FindStatusCondition(ms.Status.Conditions, v1alpha1.ConditionReady); c != nil && c.Message != "" {
		return c.Message
	}
	return "no Ready condition was reported"
}

// phaseOrPending names the empty phase, which means the operator has not
// reconciled the server yet (commonly: the operator itself is not running).
func phaseOrPending(p v1alpha1.Phase) string {
	if p == "" {
		return "not yet reconciled — is the felis operator running?"
	}
	return string(p)
}

// ensureSecretReplica copies one Secret from the control namespace into the minecraft
// namespace so a backend pod can mount it via secretKeyRef. A secretKeyRef is
// namespace-local, but the backends run in the minecraft namespace while the sources
// of truth live beside the control plane — so without this replica the operator's
// injected secretKeyRef would dangle and wedge the pod in CreateContainerConfigError.
//
// Two Secrets need it, for different reasons: the service token (login only — it
// authenticates the limbo plugin to the felis-api internal face) and the Velocity
// modern-forwarding secret (every backend — it is how a backend knows a login really
// came from the proxy, and so that the player's UUID is Mojang-verified rather than
// offline-derived).
//
// It is create-if-absent: an existing replica is left untouched so a hand-rotated
// value in the minecraft namespace is never clobbered (to rotate, delete the replica
// and re-run setup). Best-effort like the rest of the provisioner: a missing source or
// a create failure degrades to a reported outcome, never a hard setup failure. It
// copies only Type and Data — never labels/annotations/ownerRefs — so the replica
// carries no accidental GC owner or managed-by lineage.
func ensureSecretReplica(ctx context.Context, cl client.Client, controlNamespace, minecraftNamespace, secretName, secretKey, label string) systemServerOutcome {
	name := label + " (minecraft ns)"
	validate := func(secret *corev1.Secret, location, skipped string) systemServerOutcome {
		if len(secret.Data[secretKey]) == 0 {
			return systemServerOutcome{name: name, skipped: fmt.Sprintf(
				"Secret %s/%s has no non-empty %q key", location, secretName, secretKey)}
		}
		return systemServerOutcome{name: name, available: true, skipped: skipped}
	}
	if controlNamespace == minecraftNamespace {
		// Same namespace needs no replica, but the source still has to exist.
		var existing corev1.Secret
		err := cl.Get(ctx, client.ObjectKey{Namespace: minecraftNamespace, Name: secretName}, &existing)
		if err == nil {
			return validate(&existing, minecraftNamespace, "control and minecraft namespaces coincide")
		}
		if apierrors.IsNotFound(err) {
			return systemServerOutcome{name: name, skipped: fmt.Sprintf(
				"source Secret %s/%s not found — provision it (deploy/bootstrap.sh), then re-run setup",
				controlNamespace, secretName)}
		}
		return systemServerOutcome{name: name, err: err}
	}
	// Never overwrite an existing replica (it may hold a rotated value).
	var existing corev1.Secret
	getErr := cl.Get(ctx, client.ObjectKey{Namespace: minecraftNamespace, Name: secretName}, &existing)
	if getErr == nil {
		return validate(&existing, minecraftNamespace, "already exists")
	}
	if !apierrors.IsNotFound(getErr) {
		return systemServerOutcome{name: name, err: getErr}
	}
	// Read the source of truth from the control namespace.
	var src corev1.Secret
	if err := cl.Get(ctx, client.ObjectKey{Namespace: controlNamespace, Name: secretName}, &src); err != nil {
		if apierrors.IsNotFound(err) {
			return systemServerOutcome{name: name, skipped: fmt.Sprintf(
				"source Secret %s/%s not found — provision it (deploy/bootstrap.sh), then re-run setup",
				controlNamespace, secretName)}
		}
		return systemServerOutcome{name: name, err: err}
	}
	if out := validate(&src, controlNamespace, ""); !out.available {
		return out
	}
	replica := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: minecraftNamespace},
		Type:       src.Type,
		Data:       src.Data,
	}
	if err := cl.Create(ctx, replica); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if getErr := cl.Get(ctx, client.ObjectKey{Namespace: minecraftNamespace, Name: secretName}, &existing); getErr != nil {
				return systemServerOutcome{name: name, err: getErr}
			}
			return validate(&existing, minecraftNamespace, "already exists")
		}
		return systemServerOutcome{name: name, err: err}
	}
	return systemServerOutcome{name: name, created: true, available: true}
}

func requiredProvisioningError(outcomes []systemServerOutcome) error {
	required := map[string]struct{}{
		"service-token (minecraft ns)":     {},
		"forwarding-secret (minecraft ns)": {},
		naming.SystemLoginServer:           {},
	}
	for _, o := range outcomes {
		if o.err != nil {
			return fmt.Errorf("%s: %w", o.name, o.err)
		}
		if _, ok := required[o.name]; ok && !o.available {
			reason := o.skipped
			if reason == "" {
				reason = "object was not created"
			}
			return fmt.Errorf("%s unavailable: %s", o.name, reason)
		}
	}
	return nil
}
