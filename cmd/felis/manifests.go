package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"strings"

	"felis.lolicon.best/internal/platform"
)

// multiFlag collects a repeatable string flag (e.g. --velocity-cidr a --velocity-cidr b).
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// cmdManifests renders the control-plane install bundle (spec §21, §22) —
// namespaces, the control-plane identities (SAs + namespaced Roles +
// RoleBindings — felis-api and felis-operator always, plus the destructive
// felis-reaper identity only when the retention reaper is enabled, gated with
// its CronJob), the weak build/restore Job SAs, the build/minecraft
// NetworkPolicies, and the running control-plane workloads (felis-api/operator
// Deployments + the in-cluster registry Deployment/Service/PVC) — as a single
// multi-document YAML stream on stdout, ready for `kubectl apply -f -`.
//
// It is a pure renderer: it never contacts a cluster and holds no credentials.
// --velocity-cidr records the proxy host addresses allowed by the game NetworkPolicy.
// Kubernetes permits resident-node traffic regardless, but remote proxy deployments
// need an explicit CIDR, so the renderer refuses to guess.
func cmdManifests(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("manifests", flag.ContinueOnError)
	fs.SetOutput(stderr)
	controlNS := fs.String("control-namespace", platform.DefaultControlNamespace, "namespace the control plane (api/operator/reaper) runs in")
	minecraftNS := fs.String("minecraft-namespace", platform.DefaultMinecraftNamespace, "namespace MinecraftServer workloads run in")
	buildNS := fs.String("build-namespace", platform.DefaultBuildNamespace, "namespace image-build Jobs run in")
	registryNS := fs.String("registry-namespace", "", "namespace of the in-cluster registry (default: control namespace)")
	registryPort := fs.Int("registry-port", 5000, "port the in-cluster registry listens on")
	panelNodePort := fs.Int("panel-node-port", int(platform.DefaultPanelNodePort), "NodePort that exposes the built-in HTTPS panel/API origin")
	felisImage := fs.String("felis-image", "", "container image the felis-api/operator Deployments run, also passed through as FELIS_IMAGE (REQUIRED)")
	registryImage := fs.String("registry-image", "", "in-cluster registry image (default: registry:2)")
	backupPVC := fs.String("backup-pvc", "", "name of the backup PVC advertised to the restore executor via FELIS_BACKUP_PVC (default none = restore endpoint returns 503)")
	worldsHostPath := fs.String("worlds-host-path", "", "node directory under which each world PVC is visible as <path>/<pvc>; enables the reaper CronJob (requires --backup-pvc and --archive-local-path)")
	archiveLocalPath := fs.String("archive-local-path", "", "path the backup PVC is mounted at in the reaper CronJob; MUST equal felis.toml [archive] local_path")
	var velocityCIDRs multiFlag
	fs.Var(&velocityCIDRs, "velocity-cidr", "CIDR of a Velocity proxy host allowed to reach game port 25565 (repeatable, REQUIRED)")
	var packageCIDRs multiFlag
	fs.Var(&packageCIDRs, "package-cidr", "CIDR of a package mirror build Pods may reach (repeatable; default none = no internet egress)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Keep proxy placement explicit. This matters for remote proxies and documents
	// the expected source even when Velocity runs on the resident node.
	if len(velocityCIDRs) == 0 {
		fmt.Fprintln(stderr, "felis manifests: at least one --velocity-cidr is required "+
			"(pass the Velocity proxy host CIDR, e.g. --velocity-cidr 10.0.0.5/32)")
		return 2
	}

	// --felis-image is mandatory: the api/operator Deployments and the FELIS_IMAGE
	// passthrough (used to launch the restore Job) have no safe default image. Same
	// fail-loud contract as --velocity-cidr.
	if *felisImage == "" {
		fmt.Fprintln(stderr, "felis manifests: --felis-image is required "+
			"(the felis-api/operator Deployments run it and it is passed through as FELIS_IMAGE, e.g. --felis-image registry.felis.svc:5000/felis:v1)")
		return 2
	}
	for _, cidr := range append(append([]string{}, velocityCIDRs...), packageCIDRs...) {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			fmt.Fprintf(stderr, "felis manifests: invalid CIDR %q: %v\n", cidr, err)
			return 2
		}
	}
	if *panelNodePort < 30000 || *panelNodePort > 32767 {
		fmt.Fprintf(stderr, "felis manifests: --panel-node-port must be in Kubernetes NodePort range 30000-32767 (got %d)\n", *panelNodePort)
		return 2
	}

	// Retention/reaper rendering is opt-in and needs all three storage coordinates
	// together: where worlds live (to read+archive them), the backup PVC (to write
	// archives into), and the path it is mounted at (which MUST equal felis.toml
	// [archive] local_path so tarLocal's absolute archive refs resolve). A partial
	// configuration is almost certainly an operator mistake, so fail loud rather than
	// silently drop retention. Asking for it without the other two is rejected; an
	// empty trio renders the bundle WITHOUT the reaper and says so.
	if *worldsHostPath != "" {
		if *backupPVC == "" || *archiveLocalPath == "" {
			fmt.Fprintln(stderr, "felis manifests: --worlds-host-path enables the reaper CronJob and requires "+
				"--backup-pvc and --archive-local-path too (--archive-local-path must equal felis.toml [archive] local_path)")
			return 2
		}
		// The reaper WILL render. Two deployment preconditions this generator cannot
		// check would SILENTLY turn retention into a no-op if unmet — surface them as
		// loudly as the fail-closed cases above, so an operator is never left with a
		// reaper that reaps an empty directory. (Both are also in the WorldsHostPath
		// flag/field docs, but nobody deploying from stdout reads those.)
		fmt.Fprintf(stderr, "felis manifests: note: rendering the retention reaper CronJob (worlds hostPath %q). "+
			"Two preconditions are NOT verified here:\n"+
			"  - each world PVC must be visible at %s/<pvc> on the node: a stock local-path-provisioner lays "+
			"volumes under PV-name paths (.../pvc-<uuid>_<ns>_<pvc>/), so unless the worlds StorageClass is "+
			"arranged to expose <path>/<pvc>, the reaper tars an empty directory;\n"+
			"  - the CronJob sets NO nodeSelector: a single-node starter pins it to the worlds implicitly, but "+
			"on a multi-node cluster you MUST add a nodeSelector for the node holding the worlds, or the reaper "+
			"may schedule where the hostPath is empty.\n", *worldsHostPath, *worldsHostPath)
	} else {
		fmt.Fprintln(stderr, "felis manifests: note: retention reaper CronJob not rendered "+
			"(pass --worlds-host-path, --backup-pvc and --archive-local-path to enable it)")
	}

	out, err := platform.RenderYAML(platform.Params{
		ControlNamespace:   *controlNS,
		MinecraftNamespace: *minecraftNS,
		BuildNamespace:     *buildNS,
		RegistryNamespace:  *registryNS,
		RegistryPort:       int32(*registryPort),
		PanelNodePort:      int32(*panelNodePort),
		FelisImage:         *felisImage,
		RegistryImage:      *registryImage,
		BackupPVC:          *backupPVC,
		WorldsHostPath:     *worldsHostPath,
		ArchiveLocalPath:   *archiveLocalPath,
		VelocityCIDRs:      []string(velocityCIDRs),
		PackageSourceCIDRs: []string(packageCIDRs),
	})
	if err != nil {
		fmt.Fprintf(stderr, "felis manifests: render: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(out); err != nil {
		fmt.Fprintf(stderr, "felis manifests: write: %v\n", err)
		return 1
	}
	return 0
}
