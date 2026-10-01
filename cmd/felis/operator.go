package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	felismetrics "felis.lolicon.best/internal/metrics"
	"felis.lolicon.best/internal/operator"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// cmdOperator runs the MinecraftServer controller-manager (spec §5). It builds
// the scheme, wires the Reconciler with the production RCON prober, and blocks
// on the manager until the process receives a termination signal.
func cmdOperator(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("operator", flag.ContinueOnError)
	fs.SetOutput(stderr)
	metricsAddr := fs.String("metrics-bind-address", ":8080", "address the metric endpoint binds to")
	// healthAddr serves the manager's health endpoints (/healthz, /readyz) that the
	// Deployment's probes dial. Without it the operator pod would carry no probe at
	// all, and a wedged manager would keep its endpoint forever. It must differ from
	// metricsAddr: the metrics server owns :8080.
	healthAddr := fs.String("health-probe-bind-address", ":8081", "address the health probe endpoint binds to")
	// namespace MUST equal the [k8s] namespace felis-api is configured with, and
	// the deployment manifests (felis manifests) render both from one value. It
	// scopes the manager's cache (informers) to a single namespace so the operator
	// can run under a namespaced Role instead of cluster-admin (spec §21). The
	// default matches config.defaultNamespace, so an unconfigured deployment
	// agrees; a mismatch would silently scope the cache to the wrong namespace and
	// every reconcile would see zero servers — hence the watched namespace is
	// logged at startup so a divergence surfaces immediately rather than silently.
	namespace := fs.String("namespace", "minecraft", "namespace to watch; must match felis-api's [k8s] namespace")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	// controller-runtime logs through its own logr sink; without one, its first
	// reconcile prints "log.SetLogger(...) was never called" ATTACHED TO A FULL
	// GOROUTINE STACK — pure noise, not signal. Route it to slog's default handler
	// so its messages appear as ordinary stderr lines.
	ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *healthAddr,
		// Scope every informer to the single watched namespace. Without this the
		// cached client (mgr.GetClient) would LIST/WATCH cluster-wide, which a
		// namespaced Role cannot grant — the operator would fail closed at runtime
		// or, worse, demand cluster-admin. With it, the platform.OperatorRole
		// (get/list/watch in one namespace) is exactly sufficient.
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{*namespace: {}},
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "felis operator: create manager: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "felis operator: watching namespace %q\n", *namespace)

	// /healthz fails while a reconcile pass has been stuck past its limit, so the
	// liveness probe restarts an operator whose workers are wedged (a Pod whose
	// process answers but no server starts or stops). /readyz waits for the
	// informer caches: until they sync the operator acts on nothing, and one that
	// never syncs (lost RBAC, an unreachable API) never reports Available.
	// A dependency hiccup fails neither: the caches ride through API blips, and
	// each pass is bounded well inside the stuck limit.
	watch := &operator.ReconcileWatch{}
	if err := mgr.AddHealthzCheck("reconcile", watch.Check); err != nil {
		fmt.Fprintf(stderr, "felis operator: register healthz check: %v\n", err)
		return 1
	}
	if err := mgr.AddReadyzCheck("informers", cacheSynced(mgr.GetCache())); err != nil {
		fmt.Fprintf(stderr, "felis operator: register readyz check: %v\n", err)
		return 1
	}

	// Publish the named felis_* metrics (spec §23) on the endpoint the manager
	// already serves (metricsAddr). controller-runtime's metrics server exposes
	// its global Registry, so registering into it is all that is needed for
	// /metrics to carry felis_servers_total and friends. Register is idempotent,
	// so an in-process restart never double-registers fatally.
	if err := felismetrics.Register(ctrlmetrics.Registry); err != nil {
		fmt.Fprintf(stderr, "felis operator: register metrics: %v\n", err)
		return 1
	}
	felismetrics.SetBuildInfo("operator", resolvedVersion())

	r := &operator.Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Prober: operator.RconProber{},
		// The operator's own image, for the forwarding-config initContainer it
		// injects into user servers. The Deployment passes it as FELIS_IMAGE (see
		// platform.OperatorDeployment); absent, that injection is simply skipped.
		FelisImage:     os.Getenv("FELIS_IMAGE"),
		Nodes:          nil,
		EgressProbe:    os.Getenv("FELIS_EGRESS_PROBE"),
		ControllerNode: os.Getenv("FELIS_CONTROLLER_NODE"),
		// Uncached: the maintenance-lock check lists Jobs only when a server is
		// about to start, which does not justify a namespace-wide Job informer.
		Jobs: mgr.GetAPIReader(),
		// Uncached too: RCON Secrets are read by name, so the Role grants
		// secrets:get without the list/watch an informer would need.
		Secrets: mgr.GetAPIReader(),
		// And pods: pod-0 is read by name, so the Role grants pods:get and no
		// namespace-wide pod informer runs.
		Pods:     mgr.GetAPIReader(),
		Recorder: mgr.GetEventRecorderFor("felis-operator"),
		Watch:    watch,
	}
	if os.Getenv("FELIS_DISTRIBUTED") == "true" {
		r.Nodes = mgr.GetAPIReader()
	}
	if err := r.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(stderr, "felis operator: setup controller: %v\n", err)
		return 1
	}

	// Republish felis_servers_total from a periodic full List of the fleet. A
	// per-object reconcile can never maintain a fleet-wide gauge correctly, so a
	// snapshot Runnable owns it; it shares the manager's cached client and stops
	// with the manager.
	if err := mgr.Add(&operator.GaugeSyncer{Client: mgr.GetClient()}); err != nil {
		fmt.Fprintf(stderr, "felis operator: add gauge syncer: %v\n", err)
		return 1
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fmt.Fprintf(stderr, "felis operator: manager exited: %v\n", err)
		return 1
	}
	return 0
}

// cacheSynced is a readyz check that passes once every informer the manager
// started has synced. It waits at most a second, well inside the probe timeout.
func cacheSynced(c interface {
	WaitForCacheSync(ctx context.Context) bool
}) healthz.Checker {
	return func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), time.Second)
		defer cancel()
		if !c.WaitForCacheSync(ctx) {
			return errors.New("informer caches not synced")
		}
		return nil
	}
}
