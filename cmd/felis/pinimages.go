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
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/platform"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	system := fs.String("system", "", "pin this system server ("+naming.SystemLoginServer+" or "+naming.SystemLobbyServer+") to the build its tag names now, instead of the user servers")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *system != "" && *system != naming.SystemLoginServer && *system != naming.SystemLobbyServer {
		fmt.Fprintf(stderr, "felis pin-images: --system takes %s or %s, not %q\n", naming.SystemLoginServer, naming.SystemLobbyServer, *system)
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
	if *system != "" {
		return reportSystemPin(ctx, cl, *namespace, *system, imagepin.Resolver{Registry: *registry, Endpoint: *endpoint}, stdout, stderr)
	}
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

// reportSystemPin runs pinSystemServerImage for `felis pin-images --system` and
// prints what it did. Only a failed pin exits non-zero: the installer falls back
// to restarting the pod on its tag then.
func reportSystemPin(ctx context.Context, cl client.Client, namespace, name string, r imagepin.Resolver, stdout, stderr io.Writer) int {
	o, err := pinSystemServerImage(ctx, cl, namespace, name, r)
	switch {
	case meta.IsNoMatchError(err):
		fmt.Fprintln(stdout, "felis pin-images: no MinecraftServer CRD yet, so no server to pin")
		return 0
	case err == nil && o.err != nil:
		err = o.err
	}
	if err != nil {
		fmt.Fprintf(stderr, "felis pin-images: %s: %v\n", name, err)
		return 1
	}
	switch {
	case o.updated:
		fmt.Fprintf(stdout, "felis pin-images: %s: %s; the operator rolls it onto that build\n", name, strings.Join(o.changes, ", "))
	default:
		fmt.Fprintf(stdout, "felis pin-images: %s: %s\n", name, o.skipped)
	}
	return 0
}

// pinSystemServerImage fixes a system server to the build its image tag names now,
// replacing the digest of an earlier build. The installer runs it after pushing a
// rebuilt login or lobby image, and the operator rolls the StatefulSet onto the new
// ref, so the build a system server runs is written in its spec and moves only when
// a build did. An image outside the platform registry, or one naming no tag to
// follow, is the admin's choice and is left alone; so is a server whose image an
// admin retargets while this runs.
func pinSystemServerImage(ctx context.Context, cl client.Client, namespace, name string, r imagepin.Resolver) (systemServerOutcome, error) {
	var ms v1alpha1.MinecraftServer
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &ms); err != nil {
		if apierrors.IsNotFound(err) {
			return systemServerOutcome{name: name, skipped: "not present yet; sudo felis setup creates it"}, nil
		}
		return systemServerOutcome{}, err
	}
	if ms.Labels[v1alpha1.LabelSystemRole] != name {
		return systemServerOutcome{name: name, err: fmt.Errorf(
			"MinecraftServer %s/%s is not marked as the Felis %q system server; left alone", namespace, name, name)}, nil
	}
	tagged := withoutDigest(ms.Spec.Image)
	if !r.Covers(tagged) || !strings.Contains(tagged[strings.LastIndex(tagged, "/")+1:], ":") {
		return systemServerOutcome{name: name, available: true, skipped: "runs " + ms.Spec.Image +
			", which names no platform registry tag to follow; left alone"}, nil
	}
	pinned, err := r.Pin(ctx, tagged)
	if err != nil {
		return systemServerOutcome{name: name, err: fmt.Errorf("resolve %s: %w", tagged, err)}, nil
	}
	changed, err := patchOnConflictRetry(ctx, cl, &ms, func() bool {
		if withoutDigest(ms.Spec.Image) != tagged || ms.Spec.Image == pinned {
			return false
		}
		ms.Spec.Image = pinned
		return true
	})
	if err != nil {
		return systemServerOutcome{name: name, err: fmt.Errorf("patch %s: %w", name, err)}, nil
	}
	if !changed {
		return systemServerOutcome{name: name, available: true, skipped: "already runs " + ms.Spec.Image}, nil
	}
	return systemServerOutcome{name: name, available: true, updated: true,
		changes: []string{"spec.image pinned to " + pinned}}, nil
}

// withoutDigest drops the @sha256:… of a pinned ref, leaving the tag it came from.
func withoutDigest(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i]
	}
	return ref
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
// resolver covers and is not yet pinned. System servers are the installer's to move:
// it re-pins them with --system when it rolls them onto a new build
// (restart_existing_system_servers).
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
