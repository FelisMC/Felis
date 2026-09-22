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
// Deployments + the in-cluster registry Deployment/Service/PVC + the
// world-archive PVC that backs backup/restore, unless --backup-pvc is emptied)
// — as a single multi-document YAML stream on stdout, ready for
// `kubectl apply -f -`.
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
	backupPVC := fs.String("backup-pvc", "felis-backups", "name of the world-archive PVC this bundle renders in the Minecraft namespace and advertises to the backup/restore executors via FELIS_BACKUP_PVC (default: felis-backups; pass an empty value to render none, leaving backup/restore answering 503)")
	worldsHostPath := fs.String("worlds-host-path", "", "node directory the reaper reads worlds from: each world PVC resolves as <path>/<pvc>, or as the stock local-path directory <path>/<pv-name>_<ns>_<pvc-name> (k3s storage root: /var/lib/rancher/k3s/storage); enables the reaper CronJob (requires --archive-local-path and a non-empty --backup-pvc)")
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

	// Retention/reaper rendering is opt-in and needs a storage topology together:
	// where worlds live (to read+archive them), a backup PVC (to write archives
	// into — rendered from --backup-pvc), and the path it is mounted at (which MUST
	// equal felis.toml [archive] local_path so tarLocal's absolute archive refs
	// resolve). A partial configuration is almost certainly an operator mistake, so
	// fail loud rather than silently drop retention or render a reaper with nowhere
	// to write. The backup PVC itself defaults to felis-backups (it is what makes a
	// default install's backup endpoint work at all); retention additionally needs
	// --worlds-host-path.
	if *worldsHostPath != "" {
		if *backupPVC == "" || *archiveLocalPath == "" {
			fmt.Fprintln(stderr, "felis manifests: --worlds-host-path enables the reaper CronJob and requires "+
				"--archive-local-path (must equal felis.toml [archive] local_path) and a non-empty --backup-pvc "+
				"(the archive store; default felis-backups)")
			return 2
		}
		// The reaper WILL render. Three deployment preconditions this generator cannot
		// check would silently turn retention into a no-op (or a permission-denied
		// loop) if unmet — surface them as loudly as the fail-closed cases above, so
		// an operator is never left with a reaper that reaps nothing. (All three are
		// also in the WorldsHostPath flag/field docs, but nobody deploying from stdout
		// reads those.)
		fmt.Fprintf(stderr, "felis manifests: note: rendering the retention reaper CronJob (worlds hostPath %q). "+
			"Three preconditions are NOT verified here:\n"+
			"  - the node's world volumes must actually live below %s: the reaper resolves a world as "+
			"%s/<pvc>, then as the stock local-path directory <path>/<pv-name>_<ns>_<pvc-name> (what k3s "+
			"writes under /var/lib/rancher/k3s/storage). Any other provisioner needs its volumes exposed as "+
			"<path>/<pvc>, or each candidate's archive fails and the world is preserved;\n"+
			"  - the reaper pod runs as uid 1000 and must be able to traverse %s (k3s ships its storage root "+
			"0700 root:root — the installer grants `setfacl -m u:1000:x` or o+x; a manual install must do the "+
			"same or every archive fails with permission denied and the world is preserved);\n"+
			"  - the CronJob sets NO nodeSelector: a single-node starter pins it to the worlds implicitly, but "+
			"on a multi-node cluster you MUST add a nodeSelector for the node holding the worlds, or the reaper "+
			"may schedule where the hostPath is empty.\n", *worldsHostPath, *worldsHostPath, *worldsHostPath, *worldsHostPath)
	} else {
		fmt.Fprintln(stderr, "felis manifests: note: retention reaper CronJob not rendered "+
			"(pass --worlds-host-path and --archive-local-path — the archive PVC defaults to felis-backups — to enable it)")
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
