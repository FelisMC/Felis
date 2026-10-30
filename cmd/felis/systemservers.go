package main

import (
	"bytes"
	"context"
	"fmt"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/naming"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// System services (login limbo and lobby) are created Running, exempt from the reaper, and use reserved names.

type systemServerSpec struct {
	name           string
	subdomain      string
	displayName    string
	image          string
	memory         string            // container memory limit and request
	storage        string            // world PVC size
	fallbackServer string            // "" = none (refuse when down); never the lobby
	healthHTTPPort int32             // > 0 gates readiness on an HTTP health endpoint
	rcon           bool              // RCON write channel; off for the limbo, which has no RCON listener and would never become ready
	env            []v1alpha1.EnvVar // plain, non-secret variables; the service token is injected via secretKeyRef
}

// systemRcon uses naming.RconSecretName and leaves Port 0 so the operator default applies.
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

const felisLimboHealthPort int32 = 8080 // gates the login pod's readiness on the limbo having finished starting

// Env vars read by the felis-limbo plugin (env wins over felis-link.properties); the service token is injected separately.
const (
	envAPIBaseURL    = naming.EnvAPIBaseURL
	envRootDomain    = "FELIS_ROOT_DOMAIN"
	envPanelHostname = "FELIS_PANEL_HOSTNAME"
	envLobbyServer   = "FELIS_LOBBY_SERVER"
)

// buildSystemServer builds a Running, reaper-exempt, public-autostart server; RCON is enabled only when in.rcon.
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
	// A lobby fallback would route unauthenticated players past the login gate.
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
			// Backends run offline-mode behind Velocity, the only online-mode component.
			OnlineMode: false,
			Rcon:       systemRcon(in),
			Storage:    v1alpha1.StorageSpec{Size: storageQ.String()},
			Resources:  corev1.ResourceRequirements{Limits: limits, Requests: requests},
			Startup:    v1alpha1.StartupSpec{HealthHTTPPort: in.healthHTTPPort},
			Env:        in.env,
		},
	}, nil
}

// systemImages names the system servers' images, an empty one skipping its server; without a lobby the login space is the only exit.
type systemImages struct {
	login, lobby string
	// loginMemory sizes the login space; empty keeps defaultLoginMemory.
	loginMemory string
}

func systemImagesFrom(v config.VelocityConfig) systemImages {
	return systemImages{login: v.LoginImage, lobby: v.LobbyImage, loginMemory: v.LoginMemory}
}

const defaultLoginMemory = "512Mi"

// The login gate has no fallback, so its failure cannot bypass authentication.
func loginSystemServer(image, namespace, apiBaseURL, rootDomain, panelHostname string) (*v1alpha1.MinecraftServer, error) {
	return loginSpace(image, namespace, apiBaseURL, rootDomain, panelHostname, defaultLoginMemory, naming.SystemLobbyServer)
}

// loginSpace builds the login gate; an empty lobby tells it the install has none.
func loginSpace(image, namespace, apiBaseURL, rootDomain, panelHostname, memory, lobby string) (*v1alpha1.MinecraftServer, error) {
	return buildSystemServer(systemServerSpec{
		name:           naming.SystemLoginServer,
		subdomain:      naming.SystemLoginServer,
		displayName:    "Login",
		image:          image,
		memory:         memory,
		storage:        "1Gi",
		fallbackServer: "",
		healthHTTPPort: felisLimboHealthPort,
		env: []v1alpha1.EnvVar{
			{Name: envAPIBaseURL, Value: apiBaseURL},
			{Name: envRootDomain, Value: rootDomain},
			{Name: envPanelHostname, Value: panelHostname},
			{Name: envLobbyServer, Value: lobby},
		},
	}, namespace)
}

// lobbySystemServer is the post-auth /menu hub; it falls back to the login gate.
func lobbySystemServer(image, namespace string) (*v1alpha1.MinecraftServer, error) {
	return buildSystemServer(systemServerSpec{
		name:           naming.SystemLobbyServer,
		subdomain:      naming.SystemLobbyServer,
		displayName:    "Lobby",
		image:          image,
		memory:         "1Gi",
		storage:        "2Gi",
		fallbackServer: naming.SystemLoginServer,
		rcon:           true,
	}, namespace)
}

// buildSystemServerClient resolves a kubeconfig the standard way, then falls back to the k3s admin kubeconfig at
// hostBootstrapKubeconfigPath; an error is not fatal to setup.
func buildSystemServerClient() (client.Client, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	cfg, err := hostRESTConfig()
	if err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

func hostRESTConfig() (*rest.Config, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		cfg, err = clientcmd.BuildConfigFromFlags("", hostBootstrapKubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("no reachable kubeconfig (tried in-cluster/$KUBECONFIG/~/.kube and %s): %w", hostBootstrapKubeconfigPath, err)
		}
	}
	return cfg, nil
}

type systemServerOutcome struct {
	name      string
	created   bool
	updated   bool
	available bool
	skipped   string // why the service was skipped (image unset / already exists)
	err       error
	changes   []string // converge only: the fields this pass filled
}

type systemServerPlan struct {
	name  string
	image string
	build func(image, namespace string) (*v1alpha1.MinecraftServer, error)
}

func systemServerPlans(images systemImages, apiBaseURL, rootDomain, panelHostname string) []systemServerPlan {
	memory, lobby := images.loginMemory, naming.SystemLobbyServer
	if memory == "" {
		memory = defaultLoginMemory
	}
	if images.lobby == "" {
		lobby = ""
	}
	return []systemServerPlan{
		{name: naming.SystemLoginServer, image: images.login, build: func(image, ns string) (*v1alpha1.MinecraftServer, error) {
			return loginSpace(image, ns, apiBaseURL, rootDomain, panelHostname, memory, lobby)
		}},
		{name: naming.SystemLobbyServer, image: images.lobby, build: lobbySystemServer},
	}
}

// ensureSystemServers creates missing login and lobby servers; existing ones are never overwritten and a service with no image is skipped.
func ensureSystemServers(ctx context.Context, cl client.Client, namespace string, images systemImages, apiBaseURL, rootDomain, panelHostname string) []systemServerOutcome {
	plans := systemServerPlans(images, apiBaseURL, rootDomain, panelHostname)

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
		// An existing service is a skip, not an AlreadyExists error; a user server on a reserved name is never adopted.
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

// derivedSystemEnv lists env vars computed from config. Unlike the rest of an existing system server they are
// refreshed on every setup re-run, so a root-domain change reaches the login gate.
var derivedSystemEnv = map[string]bool{
	envAPIBaseURL:    true,
	envRootDomain:    true,
	envPanelHostname: true,
}

// refreshDerivedEnv overwrites only derived keys already present with a different value; keys the operator added or removed are left alone.
func refreshDerivedEnv(ctx context.Context, cl client.Client, existing, desired *v1alpha1.MinecraftServer) (bool, error) {
	want := derivedEnvWanted(desired)
	changed, err := patchOnConflictRetry(ctx, cl, existing, func() bool {
		changed := false
		for i, e := range existing.Spec.Env {
			if v, ok := want[e.Name]; ok && v != e.Value {
				existing.Spec.Env[i].Value = v
				changed = true
			}
		}
		return changed
	})
	if err != nil {
		return false, fmt.Errorf("refresh %s env: %w", existing.Name, err)
	}
	return changed, nil
}

// Pin merge patches to resourceVersion and retry conflicts to preserve concurrent list updates.
func patchOnConflictRetry(ctx context.Context, cl client.Client, obj client.Object, mutate func() bool) (bool, error) {
	key := client.ObjectKeyFromObject(obj)
	changed, reread := false, false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if reread {
			if err := cl.Get(ctx, key, obj); err != nil {
				return err
			}
		}
		reread = true
		base := obj.DeepCopyObject().(client.Object)
		if changed = mutate(); !changed {
			return nil
		}
		return cl.Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
	return changed, err
}

func derivedEnvWanted(desired *v1alpha1.MinecraftServer) map[string]string {
	want := make(map[string]string, len(derivedSystemEnv))
	for _, e := range desired.Spec.Env {
		if derivedSystemEnv[e.Name] {
			want[e.Name] = e.Value
		}
	}
	return want
}

// Convergence fills unset fields only; update images before enabling their new readiness requirements.
func convergeSystemServers(ctx context.Context, cl client.Client, namespace string, images systemImages, apiBaseURL, rootDomain, panelHostname string) []systemServerOutcome {
	outcomes := make([]systemServerOutcome, 0, 2)
	for _, p := range systemServerPlans(images, apiBaseURL, rootDomain, panelHostname) {
		if p.image == "" {
			outcomes = append(outcomes, systemServerOutcome{name: p.name, skipped: "image not configured"})
			continue
		}
		desired, err := p.build(p.image, namespace)
		if err != nil {
			outcomes = append(outcomes, systemServerOutcome{name: p.name, err: err})
			continue
		}

		var existing v1alpha1.MinecraftServer
		switch err := cl.Get(ctx, client.ObjectKeyFromObject(desired), &existing); {
		case apierrors.IsNotFound(err):
			// A missing login or lobby is recreated, as setup does.
			if err := cl.Create(ctx, desired); err != nil {
				outcomes = append(outcomes, systemServerOutcome{name: p.name, err: err})
				continue
			}
			outcomes = append(outcomes, systemServerOutcome{name: p.name, created: true, available: true})
			continue
		case err != nil:
			outcomes = append(outcomes, systemServerOutcome{name: p.name, err: err})
			continue
		}
		if existing.Labels[v1alpha1.LabelSystemRole] != p.name {
			outcomes = append(outcomes, systemServerOutcome{name: p.name, err: fmt.Errorf(
				"existing MinecraftServer %s/%s is not marked as the Felis %q system role; refusing to converge it",
				namespace, p.name, p.name,
			)})
			continue
		}

		var changes []string
		changed, err := patchOnConflictRetry(ctx, cl, &existing, func() bool {
			changes = nil
			if existing.Spec.Rcon == (v1alpha1.RconSpec{}) && desired.Spec.Rcon != (v1alpha1.RconSpec{}) {
				existing.Spec.Rcon = desired.Spec.Rcon
				changes = append(changes, "spec.rcon")
			}
			if existing.Spec.Startup.HealthHTTPPort == 0 && desired.Spec.Startup.HealthHTTPPort != 0 {
				existing.Spec.Startup.HealthHTTPPort = desired.Spec.Startup.HealthHTTPPort
				changes = append(changes, "spec.startup.healthHTTPPort")
			}
			changes = append(changes, convergeDerivedEnv(&existing, desired)...)
			return len(changes) > 0
		})
		if err != nil {
			outcomes = append(outcomes, systemServerOutcome{name: p.name, err: fmt.Errorf("converge %s: %w", p.name, err)})
			continue
		}
		if !changed {
			outcomes = append(outcomes, systemServerOutcome{name: p.name, available: true, skipped: "already converged"})
			continue
		}
		outcomes = append(outcomes, systemServerOutcome{name: p.name, available: true, updated: true, changes: changes})
	}
	return outcomes
}

// convergeDerivedEnv overwrites drifted derived keys and adds missing ones, unlike refreshDerivedEnv.
func convergeDerivedEnv(existing, desired *v1alpha1.MinecraftServer) []string {
	want := derivedEnvWanted(desired)
	var changes []string
	present := make(map[string]bool, len(existing.Spec.Env))
	for i := range existing.Spec.Env {
		e := &existing.Spec.Env[i]
		present[e.Name] = true
		if v, ok := want[e.Name]; ok && v != e.Value {
			e.Value = v
			changes = append(changes, "env "+e.Name)
		}
	}
	for _, e := range desired.Spec.Env {
		if !derivedSystemEnv[e.Name] || present[e.Name] {
			continue
		}
		existing.Spec.Env = append(existing.Spec.Env, e)
		changes = append(changes, "env "+e.Name)
	}
	return changes
}

// provisionSecretReplicas mirrors workload Secrets into their namespaces. felis-service-token is excluded:
// a copy elsewhere would open every game route to whoever reads that namespace.
func provisionSecretReplicas(ctx context.Context, cl client.Client, controlNS, minecraftNS, buildNS string) []systemServerOutcome {
	return []systemServerOutcome{
		// refresh: `felis rotate-token` replaces the source, so a differing replica is stale.
		ensureSecretReplica(ctx, cl, controlNS, minecraftNS,
			naming.LimboTokenSecretName, naming.ServiceTokenSecretKey, "limbo-token", "minecraft ns", true),
		ensureSecretReplica(ctx, cl, controlNS, minecraftNS,
			naming.ForwardingSecretName, naming.ForwardingSecretKey, "forwarding-secret", "minecraft ns", false),
		// refresh: the rendered config goes stale (e.g. after a database credential rotation) for the Jobs and reaper that mount it.
		ensureSecretReplica(ctx, cl, controlNS, minecraftNS,
			"felis-config", "felis.toml", "config", "minecraft ns", true),
		// The reaper pod needs a local copy of the SMTP relay password; skipped until the relay is configured.
		ensureSecretReplica(ctx, cl, controlNS, minecraftNS,
			"felis-smtp", "password", "smtp", "minecraft ns", false),
		// The build fetch initContainer reads the submission context with this token; best-effort.
		ensureSecretReplica(ctx, cl, controlNS, buildNS,
			naming.BuildTokenSecretName, naming.ServiceTokenSecretKey, "build-token", "felis-build ns", true),
	}
}

// Replicate only Type and Data; preserve existing values unless refreshExisting requests replacement.
func ensureSecretReplica(ctx context.Context, cl client.Client, controlNamespace, minecraftNamespace, secretName, secretKey, label, where string, refreshExisting bool) systemServerOutcome {
	name := label + " (" + where + ")"
	validate := func(secret *corev1.Secret, location, skipped string) systemServerOutcome {
		if len(secret.Data[secretKey]) == 0 {
			return systemServerOutcome{name: name, skipped: fmt.Sprintf(
				"Secret %s/%s has no non-empty %q key", location, secretName, secretKey)}
		}
		return systemServerOutcome{name: name, available: true, skipped: skipped}
	}
	refreshFromControl := func(existing *corev1.Secret) systemServerOutcome {
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
		changed, err := patchOnConflictRetry(ctx, cl, existing, func() bool {
			if bytes.Equal(existing.Data[secretKey], src.Data[secretKey]) {
				return false
			}
			if existing.Data == nil {
				existing.Data = map[string][]byte{}
			}
			existing.Data[secretKey] = src.Data[secretKey]
			return true
		})
		if err != nil {
			return systemServerOutcome{name: name, err: err}
		}
		if !changed {
			return validate(existing, minecraftNamespace, "already current")
		}
		return systemServerOutcome{name: name, updated: true, available: true}
	}
	if controlNamespace == minecraftNamespace {
		// Same namespace needs no replica, but the source must exist.
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
		if refreshExisting {
			return refreshFromControl(&existing)
		}
		return validate(&existing, minecraftNamespace, "already exists")
	}
	if !apierrors.IsNotFound(getErr) {
		return systemServerOutcome{name: name, err: getErr}
	}
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
			if refreshExisting {
				return refreshFromControl(&existing)
			}
			return validate(&existing, minecraftNamespace, "already exists")
		}
		return systemServerOutcome{name: name, err: err}
	}
	return systemServerOutcome{name: name, created: true, available: true}
}
