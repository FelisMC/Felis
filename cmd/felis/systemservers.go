package main

import (
	"context"
	"fmt"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	// env are extra plain (non-secret) environment variables baked into the pod.
	// System-service configuration derived from the deployment (the internal API
	// URL, root domain, lobby name) rides here; secrets never do — the service
	// token is injected by the operator via secretKeyRef, not as a literal value.
	env []v1alpha1.EnvVar
}

// felisLimboHealthPort is the port the felis-limbo readiness plugin serves its
// HTTP health endpoint on. The login system service gates pod readiness on it so
// "the limbo has finished starting" — not merely "the game socket is bound" —
// is what marks it Ready.
const felisLimboHealthPort int32 = 8080

// The felis-limbo login plugin reads its deployment configuration from these
// environment variables (env wins over its felis-link.properties template). The
// non-secret three are baked into the login pod's Spec.Env here at provision time
// (they derive from the deployment: the internal API URL, the root domain, the
// lobby server name); the service-token secret is injected separately by the
// operator via secretKeyRef. Without the token the plugin fail-safes to
// readiness-only, so a login pod that has the URL/domain but not yet the token is
// safe (it simply does not authenticate) rather than broken.
const (
	envAPIBaseURL  = "FELIS_API_BASE_URL"
	envRootDomain  = "FELIS_ROOT_DOMAIN"
	envLobbyServer = "FELIS_LOBBY_SERVER"
)

// buildSystemServer constructs an always-on, reaper-exempt MinecraftServer from
// a systemServerSpec. It is a pure function (no K8s, no I/O) so it is unit
// testable without a cluster. Unlike buildMinecraftServerFromApplyRequest it:
//   - permits reserved names (login/lobby) via ValidateSystemServerName,
//   - sets DesiredState=Running (the service is up the moment it exists),
//   - sets ReaperExempt=true and AutostartPolicy=public,
//   - leaves RCON disabled (LOOHP/Limbo has none; readiness is gated on pod
//     TCP/HTTP health, not an RCON probe — see the operator reconciler).
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
			Rcon:       v1alpha1.RconSpec{Enabled: false},
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
// apiBaseURL is the felis-api internal face the login plugin authenticates to and
// rootDomain builds the console URL the plugin links players at; both are baked in
// as plain env. The service token is NOT passed here — the operator injects it via
// secretKeyRef so the credential never lands in the CRD.
func loginSystemServer(image, namespace, apiBaseURL, rootDomain string) (*v1alpha1.MinecraftServer, error) {
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
	name    string
	created bool   // true = we created it this run
	skipped string // non-empty = why it was skipped (image unset / already exists)
	err     error  // non-nil = create failed
}

// ensureSystemServers idempotently creates the login and lobby system services.
// It create-if-absent per service: an existing CRD is left untouched (so an
// operator's later edits to a system service survive re-runs of setup), a
// service whose image is unset in config is skipped with a reason, and any other
// service is created. It never deletes or overwrites. The caller supplies the
// K8s client and namespace; this function performs no signal-handler or client
// setup of its own.
func ensureSystemServers(ctx context.Context, cl client.Client, namespace, loginImage, lobbyImage, apiBaseURL, rootDomain string) []systemServerOutcome {
	type plan struct {
		name  string
		image string
		build func(image, namespace string) (*v1alpha1.MinecraftServer, error)
	}
	plans := []plan{
		{name: naming.SystemLoginServer, image: loginImage, build: func(image, ns string) (*v1alpha1.MinecraftServer, error) {
			return loginSystemServer(image, ns, apiBaseURL, rootDomain)
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
		// deliberate skip rather than an AlreadyExists error.
		var existing v1alpha1.MinecraftServer
		getErr := cl.Get(ctx, client.ObjectKeyFromObject(ms), &existing)
		if getErr == nil {
			outcomes = append(outcomes, systemServerOutcome{name: p.name, skipped: "already exists"})
			continue
		}
		if !apierrors.IsNotFound(getErr) {
			outcomes = append(outcomes, systemServerOutcome{name: p.name, err: getErr})
			continue
		}
		if err := cl.Create(ctx, ms); err != nil {
			if apierrors.IsAlreadyExists(err) {
				outcomes = append(outcomes, systemServerOutcome{name: p.name, skipped: "already exists"})
				continue
			}
			outcomes = append(outcomes, systemServerOutcome{name: p.name, err: err})
			continue
		}
		outcomes = append(outcomes, systemServerOutcome{name: p.name, created: true})
	}
	return outcomes
}

// ensureServiceTokenReplica copies the internal-API service-token Secret from the
// control namespace into the minecraft namespace so the login system server's pod
// can mount it via secretKeyRef. A secretKeyRef is namespace-local, but the login
// pod runs in the minecraft namespace while the source Secret lives beside the
// control plane — so without this replica the operator's injected secretKeyRef
// would dangle and wedge the login pod in CreateContainerConfigError. It is
// create-if-absent: an existing replica is left untouched so a hand-rotated token
// in the minecraft namespace is never clobbered (to rotate, delete the replica and
// re-run setup). Best-effort like the rest of the provisioner: a missing source or
// a create failure degrades to a reported outcome, never a hard setup failure. It
// copies only Type and Data — never labels/annotations/ownerRefs — so the replica
// carries no accidental GC owner or managed-by lineage.
func ensureServiceTokenReplica(ctx context.Context, cl client.Client, controlNamespace, minecraftNamespace string) systemServerOutcome {
	const name = "service-token (minecraft ns)"
	if controlNamespace == minecraftNamespace {
		// Same namespace — the operator's secretKeyRef already resolves in place.
		return systemServerOutcome{name: name, skipped: "control and minecraft namespaces coincide"}
	}
	// Never overwrite an existing replica (it may hold a rotated token).
	var existing corev1.Secret
	getErr := cl.Get(ctx, client.ObjectKey{Namespace: minecraftNamespace, Name: naming.ServiceTokenSecretName}, &existing)
	if getErr == nil {
		return systemServerOutcome{name: name, skipped: "already exists"}
	}
	if !apierrors.IsNotFound(getErr) {
		return systemServerOutcome{name: name, err: getErr}
	}
	// Read the source of truth from the control namespace.
	var src corev1.Secret
	if err := cl.Get(ctx, client.ObjectKey{Namespace: controlNamespace, Name: naming.ServiceTokenSecretName}, &src); err != nil {
		if apierrors.IsNotFound(err) {
			return systemServerOutcome{name: name, skipped: fmt.Sprintf(
				"source Secret %s/%s not found — provision it (deploy/bootstrap.sh), then re-run setup",
				controlNamespace, naming.ServiceTokenSecretName)}
		}
		return systemServerOutcome{name: name, err: err}
	}
	replica := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: naming.ServiceTokenSecretName, Namespace: minecraftNamespace},
		Type:       src.Type,
		Data:       src.Data,
	}
	if err := cl.Create(ctx, replica); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return systemServerOutcome{name: name, skipped: "already exists"}
		}
		return systemServerOutcome{name: name, err: err}
	}
	return systemServerOutcome{name: name, created: true}
}
