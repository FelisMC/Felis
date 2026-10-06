package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"path/filepath"
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
//
// --only postgres renders just the control-plane database (platform.PostgresObjects),
// which the installer brings up before migrations, before it has anything else
// to render the full bundle with; it needs neither --felis-image nor
// --velocity-cidr.
func cmdManifests(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("manifests", flag.ContinueOnError)
	fs.SetOutput(stderr)
	distributed := fs.Bool("distributed", false, "enable approved workers and archive transport")
	controller := fs.String("controller-node", "", "protected controller identity for A")
	probe := fs.String("egress-probe", "", "reachable controller host:port denied to game Pods")
	var registryNodes multiFlag
	fs.Var(&registryNodes, "registry-node-cidr", "exact node pull source for the registry (repeatable)")
	socket := fs.String("node-control-socket", "", "host node-control Unix socket (optional, API only)")
	nodeControlNode := fs.String("node-control-node", "", "controller hostname hosting the socket")
	controlNS := fs.String("control-namespace", platform.DefaultControlNamespace, "namespace the control plane (api/operator/reaper) runs in")
	minecraftNS := fs.String("minecraft-namespace", platform.DefaultMinecraftNamespace, "namespace MinecraftServer workloads run in")
	buildNS := fs.String("build-namespace", platform.DefaultBuildNamespace, "namespace image-build Jobs run in")
	registryNS := fs.String("registry-namespace", "", "namespace of the in-cluster registry (default: control namespace)")
	registryPort := fs.Int("registry-port", 5000, "port the in-cluster registry listens on")
	panelNodePort := fs.Int("panel-node-port", int(platform.DefaultPanelNodePort), "NodePort that exposes the built-in HTTPS panel/API origin")
	felisImage := fs.String("felis-image", "", "container image the felis-api/operator Deployments run, also passed through as FELIS_IMAGE (REQUIRED)")
	registryImage := fs.String("registry-image", "", "in-cluster registry image (default: registry 2.8.3, pinned by digest)")
	postgresImage := fs.String("postgres-image", "", "control-plane database image (default: PostgreSQL 18.6, pinned by digest)")
	only := fs.String("only", "", `render one part of the bundle instead of all of it; "postgres" is the control-plane database`)
	backupPVC := fs.String("backup-pvc", "felis-backups", "name of the world-archive PVC this bundle renders in the Minecraft namespace and advertises to the backup/restore executors via FELIS_BACKUP_PVC (default: felis-backups; pass an empty value to render none, leaving backup/restore answering 503)")
	worldsHostPath := fs.String("worlds-host-path", "", "node directory the reaper reads worlds from: each world PVC resolves as <path>/<pvc>, or as the stock local-path directory <path>/<pv-name>_<ns>_<pvc-name> (k3s storage root: /var/lib/rancher/k3s/storage); enables the reaper CronJob (requires --archive-local-path and a non-empty --backup-pvc)")
	archiveLocalPath := fs.String("archive-local-path", "", "path the backup PVC is mounted at in the reaper CronJob; MUST equal felis.toml [archive] local_path. With the backup PVC alone it renders the retention-only CronJob, which deletes backups past their expiry and never touches a world")
	registryStorage := fs.String("registry-storage", "", "capacity the registry PVC requests (default 10Gi; k3s local-path does not enforce it)")
	uploadsStorage := fs.String("uploads-storage", "", "capacity the uploads PVC requests (default 5Gi; k3s local-path does not enforce it)")
	backupStorage := fs.String("backup-storage", "", "capacity the world-archive PVC requests (default 10Gi; k3s local-path does not enforce it)")
	reaperNode := fs.String("reaper-node", "", "node that holds --worlds-host-path: pins the reaper CronJob's pod there via nodeSelector kubernetes.io/hostname (multi-node clusters need this, or the reaper may schedule where the hostPath is empty)")
	var velocityCIDRs multiFlag
	fs.Var(&velocityCIDRs, "velocity-cidr", "CIDR of a Velocity proxy host allowed to reach game port 25565 (repeatable, REQUIRED)")
	var packageCIDRs multiFlag
	fs.Var(&packageCIDRs, "package-cidr", "CIDR of a package mirror build Pods may reach (repeatable; default none = no internet egress)")
	var serverDenyCIDRs multiFlag
	fs.Var(&serverDenyCIDRs, "server-egress-deny-cidr", "extra CIDR game server pods may never reach, e.g. the node's public address (repeatable)")
	var serverAllowCIDRs multiFlag
	fs.Var(&serverAllowCIDRs, "server-egress-allow-cidr", "private CIDR game server pods may reach despite the private-range block, e.g. a LAN database (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch *only {
	case "":
	case "postgres":
		return renderManifests(stdout, stderr, platform.PostgresObjects(platform.Params{
			ControllerNode:     *controller,
			ControlNamespace:   *controlNS,
			MinecraftNamespace: *minecraftNS,
			PostgresImage:      *postgresImage,
		}))
	default:
		fmt.Fprintf(stderr, "felis manifests: --only %q: the one part that renders alone is \"postgres\"\n", *only)
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
	allCIDRs := append(append([]string{}, velocityCIDRs...), packageCIDRs...)
	allCIDRs = append(append(allCIDRs, serverDenyCIDRs...), serverAllowCIDRs...)
	for _, cidr := range allCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			fmt.Fprintf(stderr, "felis manifests: invalid CIDR %q: %v\n", cidr, err)
			return 2
		}
	}
	if *panelNodePort < 30000 || *panelNodePort > 32767 {
		fmt.Fprintf(stderr, "felis manifests: --panel-node-port must be in Kubernetes NodePort range 30000-32767 (got %d)\n", *panelNodePort)
		return 2
	}
	// The node pin exists only for the reaper's hostPath: naming a node without the
	// worlds root would be silently dropped (no CronJob renders), so fail loud like
	// the storage-trio check below.
	if *reaperNode != "" && *worldsHostPath == "" && !*distributed {
		fmt.Fprintln(stderr, "felis manifests: --reaper-node requires --worlds-host-path "+
			"(it pins the reaper CronJob, which renders only with the retention storage trio)")
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
		// The reaper WILL render. Two deployment facts this generator cannot check
		// would silently turn retention into a no-op if unmet — surface them as
		// loudly as the fail-closed cases above, so an operator is never left with a
		// reaper that reaps nothing. (Both are also in the WorldsHostPath flag/field
		// docs, but nobody deploying from stdout reads those.)
		pin := "the CronJob sets NO nodeSelector: a single-node starter pins it to the worlds implicitly, but on a " +
			"multi-node cluster you MUST pass --reaper-node <name> (or add a nodeSelector) for the node holding the " +
			"worlds, or the reaper may schedule where the hostPath is empty"
		if *reaperNode != "" {
			pin = fmt.Sprintf("the CronJob and its worlds-root PV are pinned to node %q via kubernetes.io/hostname — "+
				"keep this pointed at the node that actually holds the world volumes", *reaperNode)
		}
		fmt.Fprintf(stderr, "felis manifests: note: rendering the retention reaper CronJob (worlds hostPath %q). "+
			"These points are NOT verified here:\n"+
			"  - the node's world volumes must actually live below %s: the reaper resolves a world as "+
			"%s/<pvc>, then as the stock local-path directory <path>/<pv-name>_<ns>_<pvc-name> (what k3s "+
			"writes under /var/lib/rancher/k3s/storage). Any other provisioner needs its volumes exposed as "+
			"<path>/<pvc>, or each candidate's archive fails and the world is preserved;\n"+
			"  - %s.\n", *worldsHostPath, *worldsHostPath, *worldsHostPath, pin)
	} else if !*distributed {
		switch {
		case *archiveLocalPath != "" && *backupPVC == "":
			fmt.Fprintln(stderr, "felis manifests: --archive-local-path names where the backup PVC is mounted, "+
				"but --backup-pvc is empty (no archive store renders); drop one or the other")
			return 2
		case *archiveLocalPath != "":
			fmt.Fprintln(stderr, "felis manifests: note: rendering the reaper CronJob retention-only: backups past "+
				"their expiry are deleted daily, idle worlds are never archived or deleted "+
				"(pass --worlds-host-path to reap them too)")
		case *backupPVC != "":
			fmt.Fprintln(stderr, "felis manifests: note: reaper CronJob not rendered: backups are never expired, so "+
				"the archive store only grows until the disk fills (pass --archive-local-path, equal to felis.toml "+
				"[archive] local_path, for the retention-only CronJob, and --worlds-host-path as well to reap idle worlds)")
		default:
			fmt.Fprintln(stderr, "felis manifests: note: reaper CronJob not rendered (backups are disabled)")
		}
	}

	if *socket != "" && (!filepath.IsAbs(*socket) || *nodeControlNode == "") {
		fmt.Fprintln(stderr, "node-control requires an absolute socket and its controller hostname")
		return 2
	}
	params := platform.Params{
		NodeControlSocket: *socket, NodeControlNode: *nodeControlNode,
		Distributed: *distributed, ControllerNode: *controller, EgressProbe: *probe, RegistryNodeCIDRs: registryNodes,
		ControlNamespace:   *controlNS,
		MinecraftNamespace: *minecraftNS,
		BuildNamespace:     *buildNS,
		RegistryNamespace:  *registryNS,
		RegistryPort:       int32(*registryPort),
		PanelNodePort:      int32(*panelNodePort),
		FelisImage:         *felisImage,
		RegistryImage:      *registryImage,
		PostgresImage:      *postgresImage,
		BackupPVC:          *backupPVC,
		WorldsHostPath:     *worldsHostPath,
		ReaperNode:         *reaperNode,
		ArchiveLocalPath:   *archiveLocalPath,
		VelocityCIDRs:      []string(velocityCIDRs),
		PackageSourceCIDRs: []string(packageCIDRs),

		ServerEgressDenyCIDRs:  []string(serverDenyCIDRs),
		ServerEgressAllowCIDRs: []string(serverAllowCIDRs),

		RegistryStorage: *registryStorage,
		UploadsStorage:  *uploadsStorage,
		BackupStorage:   *backupStorage,
	}
	if err := params.Validate(); err != nil {
		fmt.Fprintf(stderr, "felis manifests: %v\n", err)
		return 2
	}
	return renderManifests(stdout, stderr, platform.Objects(params))
}

func renderManifests(stdout, stderr io.Writer, objs []platform.Object) int {
	out, err := platform.RenderObjects(objs)
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
