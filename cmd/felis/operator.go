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
	"felis.lolicon.best/internal/platform"
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

// cmdOperator runs the MinecraftServer controller-manager: it builds the scheme, wires the Reconciler with the production RCON prober, and blocks until a termination signal.
func cmdOperator(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("operator", flag.ContinueOnError)
	fs.SetOutput(stderr)
	metricsAddr := fs.String("metrics-bind-address", ":8080", "address the metric endpoint binds to")
	// Health and metrics listeners must use different addresses.
	healthAddr := fs.String("health-probe-bind-address", ":8081", "address the health probe endpoint binds to")
	// Watch the API's namespace so a namespaced Role suffices.
	namespace := fs.String("namespace", "minecraft", "namespace to watch; must match felis-api's [k8s] namespace")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	// Route controller-runtime logs through slog.
	ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *healthAddr,
		// Use leader election to prevent overlapping reconcilers during controller failover.
		LeaderElection:                true,
		LeaderElectionID:              platform.LeaseOperator,
		LeaderElectionReleaseOnCancel: true,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{*namespace: {}},
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "felis operator: create manager: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "felis operator: watching namespace %q\n", *namespace)

	// Dependency failures alone do not fail liveness; readiness waits for informer cache synchronization.
	watch := &operator.ReconcileWatch{}
	if err := mgr.AddHealthzCheck("reconcile", watch.Check); err != nil {
		fmt.Fprintf(stderr, "felis operator: register healthz check: %v\n", err)
		return 1
	}
	if err := mgr.AddReadyzCheck("informers", cacheSynced(mgr.GetCache())); err != nil {
		fmt.Fprintf(stderr, "felis operator: register readyz check: %v\n", err)
		return 1
	}

	if err := felismetrics.Register(ctrlmetrics.Registry); err != nil {
		fmt.Fprintf(stderr, "felis operator: register metrics: %v\n", err)
		return 1
	}
	felismetrics.SetBuildInfo("operator", resolvedVersion())

	r := &operator.Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Prober: operator.NewRconProber(),
		// The operator's own image, for the forwarding-config initContainer it injects into user servers (passed as FELIS_IMAGE); absent, that injection is skipped.
		FelisImage: os.Getenv("FELIS_IMAGE"),
		// The server-software download cache runtime-image servers install
		// through (platform.ArtifactCacheURL); absent, they download directly.
		ArtifactCache:  os.Getenv("FELIS_ARTIFACT_CACHE"),
		Nodes:          nil,
		EgressProbe:    os.Getenv("FELIS_EGRESS_PROBE"),
		ControllerNode: os.Getenv("FELIS_CONTROLLER_NODE"),
		ControllerHA:   os.Getenv("FELIS_CONTROLLER_HA") == "true",
		// Uncached: the maintenance-lock check lists Jobs only when a server is
		// about to start, which does not justify a namespace-wide Job informer.
		Jobs: mgr.GetAPIReader(),
		// Uncached too: RCON Secrets are read by name, so the Role grants
		// secrets:get without the list/watch an informer would need.
		Secrets: mgr.GetAPIReader(),
		// And pods: pod-0 is read by name, so the Role grants pods:get and no
		// namespace-wide pod informer runs.
		Pods:     mgr.GetAPIReader(),
		Volumes:  mgr.GetAPIReader(),
		Recorder: mgr.GetEventRecorderFor("felis-operator"),
		Watch:    watch,
	}
	if os.Getenv("FELIS_DISTRIBUTED") == "true" {
		r.Nodes = mgr.GetAPIReader()
		r.BootID = operator.NodeBootID(r.Nodes)
	} else if boot, err := operator.LocalBootID(); err == nil {
		r.BootID = boot
	} else {
		fmt.Fprintf(stderr, "felis operator: host restarts will count as crashes: %v\n", err)
	}
	if err := r.SetupWithManager(mgr); err != nil {
		fmt.Fprintf(stderr, "felis operator: setup controller: %v\n", err)
		return 1
	}

	// Rebuild fleet-wide metrics from periodic cached snapshots rather than per-object transitions.
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
