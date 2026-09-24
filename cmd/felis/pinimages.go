package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/imagepin"
	"felis.lolicon.best/internal/platform"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// defaultRegistryURL is the [registry] url every install uses; deploy/bootstrap.sh
// spells the same value as REGISTRY_URL.
const defaultRegistryURL = "registry.felis.svc:5000"

// cmdPinImages pins every user server whose spec.image still names a mutable tag
// in the platform registry to the digest that tag names now (internal/imagepin).
// felis-api pins on create, so this covers the servers created before it did.
//
// deploy/bootstrap.sh runs it before it rebuilds the game images and pushes them
// over the same tags: run after the push, it would pin those servers to the new
// build, which is exactly the silent Minecraft upgrade pinning exists to stop.
// It reaches the registry through the node's loopback hostPort, the same way the
// installer pushes.
func cmdPinImages(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pin-images", flag.ContinueOnError)
	fs.SetOutput(stderr)
	namespace := fs.String("namespace", platform.DefaultMinecraftNamespace, "namespace the MinecraftServers live in")
	registry := fs.String("registry", defaultRegistryURL, "registry host[:port] the image refs spell")
	endpoint := fs.String("endpoint", "", "host[:port] to reach the registry at (default: 127.0.0.1 on the registry's port, its hostPort on this node)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *endpoint == "" {
		*endpoint = loopbackEndpoint(*registry)
	}
	cl, err := buildSystemServerClient()
	if err != nil {
		fmt.Fprintf(stderr, "felis pin-images: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	outcomes, err := pinUserServerImages(ctx, cl, *namespace, imagepin.Resolver{Registry: *registry, Endpoint: *endpoint})
	if meta.IsNoMatchError(err) {
		fmt.Fprintln(stdout, "felis pin-images: no MinecraftServer CRD yet, so no server to pin")
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "felis pin-images: %v\n", err)
		return 1
	}
	if len(outcomes) == 0 {
		fmt.Fprintln(stdout, "felis pin-images: every user server already runs a pinned image")
		return 0
	}
	fmt.Fprintln(stdout, "felis pin-images: pinning user servers to the build their tag names now:")
	exit := 0
	for _, o := range outcomes {
		if o.err != nil {
			fmt.Fprintf(stdout, "  - %s: ERROR %v\n", o.name, o.err)
			exit = 1
			continue
		}
		fmt.Fprintf(stdout, "  - %s: %s\n", o.name, strings.Join(o.changes, ", "))
	}
	return exit
}

// loopbackEndpoint is the registry's port on 127.0.0.1: the registry Deployment
// binds it as a hostPort, and containerd's mirror and the installer's pushes use
// the same address.
func loopbackEndpoint(registry string) string {
	if i := strings.LastIndex(registry, ":"); i >= 0 {
		return "127.0.0.1" + registry[i:]
	}
	return "127.0.0.1"
}

// pinUserServerImages patches spec.image of every user server whose image the
// resolver covers and is not yet pinned. System servers are left on their tags:
// the installer rebuilds and restarts them on purpose (restart_existing_system_servers).
// A server that is already pinned, or runs an image from elsewhere, produces no
// outcome, so a pinned fleet reports nothing. A running server restarts once as
// the operator rolls its StatefulSet onto the pinned ref, which is the build it
// already runs.
func pinUserServerImages(ctx context.Context, cl client.Client, namespace string, r imagepin.Resolver) ([]systemServerOutcome, error) {
	var list v1alpha1.MinecraftServerList
	if err := cl.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	var out []systemServerOutcome
	for i := range list.Items {
		ms := &list.Items[i]
		if ms.Labels[v1alpha1.LabelSystemRole] != "" || imagepin.Pinned(ms.Spec.Image) || !r.Covers(ms.Spec.Image) {
			continue
		}
		pinned, err := r.Pin(ctx, ms.Spec.Image)
		if errors.Is(err, imagepin.ErrNotFound) {
			err = fmt.Errorf("%s is not in the registry, so there is no build to pin it to; left unpinned: %w", ms.Spec.Image, err)
		}
		if err != nil {
			out = append(out, systemServerOutcome{name: ms.Name, err: err})
			continue
		}
		patch := client.MergeFrom(ms.DeepCopy())
		ms.Spec.Image = pinned
		if err := cl.Patch(ctx, ms, patch); err != nil {
			out = append(out, systemServerOutcome{name: ms.Name, err: fmt.Errorf("patch %s: %w", ms.Name, err)})
			continue
		}
		out = append(out, systemServerOutcome{name: ms.Name, available: true, updated: true,
			changes: []string{"spec.image pinned to " + pinned}})
	}
	return out, nil
}
