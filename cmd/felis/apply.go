package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// cmdApply creates a MinecraftServer CRD from a JSON form.
//
// It is an operator-only direct CRD create path — it does NOT seed Postgres
// business rows (ownership, alias, image whitelist, audit), so the resulting
// server is claimable only if those rows are seeded separately. For normal
// provisioning prefer the Web form or felis-api.
//
// Usage: felis apply -f server.json [-n minecraft]
//
// This is a DIRECT Kubernetes write — it does not go through felis-api. It
// requires a kubeconfig or in-cluster identity with "create minecraftservers"
// permission in the target namespace. Subdomain duplicates are checked against
// existing CRDs in the target namespace (same check as the web form's
// Cluster.GetBySubdomain).

// applyRequest is the CLI-facing server creation form. It mirrors the Web
// form's field shape (createServerRequest) so the two provisioners stay
// structurally aligned, but validation differs: here we validate the CRD
// resource shape only — image whitelist admission, PG alias seeding, quota, and
// audit are the API's business layer and are NOT performed.
type applyRequest struct {
	Name            string           `json:"name"`
	Subdomain       string           `json:"subdomain"`
	DisplayName     string           `json:"displayName,omitempty"`
	Image           string           `json:"image"`
	Memory          string           `json:"memory"`
	Storage         string           `json:"storage"`
	AutostartPolicy string           `json:"autostartPolicy,omitempty"`
	Resources       *resourceRequest `json:"resources,omitempty"`
}

// resourceRequest mirrors the API's resourceRequest.
type resourceRequest struct {
	CPU           string `json:"cpu,omitempty"`
	CPURequest    string `json:"cpuRequest,omitempty"`
	Memory        string `json:"memory,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
}

func cmdApply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("f", "", "path to JSON server form (required; use - for stdin)")
	namespace := fs.String("n", "minecraft", "Kubernetes namespace")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "" {
		fmt.Fprintln(stderr, "felis apply: missing required flag -f; use -f server.json or -f - for stdin")
		return 2
	}

	var raw []byte
	var err error
	if *file == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(*file)
	}
	if err != nil {
		fmt.Fprintf(stderr, "felis apply: read: %v\n", err)
		return 1
	}

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var req applyRequest
	if err := dec.Decode(&req); err != nil {
		fmt.Fprintf(stderr, "felis apply: invalid JSON: %v\n", err)
		return 1
	}
	// Drain the decoder: a second Decode must hit io.EOF — anything else
	// (another value, trailing garbage like ] or }) means the input is not
	// exactly one valid form.
	if err := dec.Decode(&struct{}{}); err == nil || !errors.Is(err, io.EOF) {
		fmt.Fprintln(stderr, "felis apply: invalid JSON: unexpected data after the server form")
		return 1
	}

	// Build the CRD from the form. This validates the resource shape but does
	// NOT perform image whitelist admission or PG seeding (API business layer).
	ms, err := buildMinecraftServerFromApplyRequest(req, *namespace)
	if err != nil {
		fmt.Fprintf(stderr, "felis apply: %v\n", err)
		return 1
	}

	// ------- K8s client (one context, one client) -------
	// The operator runs this on the node, where the kubeconfig is k3s's own file and
	// neither $KUBECONFIG nor ~/.kube is set. buildSystemServerClient falls back to that
	// file and names what it tried; ctrl.GetConfigOrDie exited 1 there without a word,
	// because controller-runtime's logger is never set up in a CLI command.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cl, err := buildSystemServerClient()
	if err != nil {
		fmt.Fprintf(stderr, "felis apply: %v\n", err)
		return 1
	}

	// Subdomain duplicate check: the web form queries GetBySubdomain before
	// creating; we list all CRDs in the namespace and check spec.subdomain.
	// metadata.name already receives K8s AlreadyExists enforcement on create,
	// so a name collision surfaces cleanly — but subdomain has no such native
	// uniqueness, so it must be checked explicitly.
	if err := checkSubdomainUnique(ctx, cl, *namespace, req.Subdomain); err != nil {
		fmt.Fprintf(stderr, "felis apply: %v\n", err)
		return 1
	}

	if err := cl.Create(ctx, ms); err != nil {
		if apierrors.IsAlreadyExists(err) {
			fmt.Fprintf(stderr, "felis apply: server %q already exists\n", req.Name)
			return 1
		}
		fmt.Fprintf(stderr, "felis apply: create: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "created MinecraftServer %s/%s (subdomain=%s, image=%s, memory=%s, storage=%s, policy=%s)\n",
		*namespace, req.Name, req.Subdomain, req.Image, resourcesMemoryString(ms.Spec.Resources), ms.Spec.Storage.Size, ms.Spec.AutostartPolicy)
	fmt.Fprintln(stderr, "note: Postgres business rows (ownership / alias / whitelist / audit) were NOT seeded — prefer Web/API for normal provisioning")
	return 0
}

// buildMinecraftServerFromApplyRequest validates the request and constructs a
// MinecraftServer CRD. It is a pure function (no K8s, no I/O) so it can be
// tested without a cluster. It validates: name/subdomain format, required
// fields, policy enum, positive K8s quantities, request ≤ limit, and the §22
// memory ceiling. The returned CRD is always DesiredState=Stopped and unowned
// (ownership is established by a later claim).
func buildMinecraftServerFromApplyRequest(req applyRequest, namespace string) (*v1alpha1.MinecraftServer, error) {
	// ---- name & subdomain ----
	if err := naming.ValidateServerName(req.Name); err != nil {
		return nil, fmt.Errorf("invalid name: %w", err)
	}
	if err := naming.ValidateServerName(req.Subdomain); err != nil {
		return nil, fmt.Errorf("invalid subdomain: %w", err)
	}
	if strings.TrimSpace(req.Image) == "" {
		return nil, fmt.Errorf("image is required")
	}

	// ---- autostart policy ----
	policy, err := parseApplyAutostartPolicy(req.AutostartPolicy)
	if err != nil {
		return nil, err
	}

	// ---- resources (§22 ceiling) ----
	memQ, err := parseApplyPositiveQuantity(req.Memory, "memory")
	if err != nil {
		return nil, err
	}
	limits := corev1.ResourceList{corev1.ResourceMemory: memQ}
	requests := corev1.ResourceList{corev1.ResourceMemory: memQ}

	if req.Resources != nil {
		if req.Resources.Memory != "" {
			q, err := parseApplyPositiveQuantity(req.Resources.Memory, "resources.memory")
			if err != nil {
				return nil, err
			}
			limits[corev1.ResourceMemory] = q
		}
		if req.Resources.MemoryRequest != "" {
			q, err := parseApplyPositiveQuantity(req.Resources.MemoryRequest, "resources.memoryRequest")
			if err != nil {
				return nil, err
			}
			requests[corev1.ResourceMemory] = q
		}
		if req.Resources.CPU != "" {
			q, err := parseApplyPositiveQuantity(req.Resources.CPU, "resources.cpu")
			if err != nil {
				return nil, err
			}
			limits[corev1.ResourceCPU] = q
		}
		if req.Resources.CPURequest != "" {
			q, err := parseApplyPositiveQuantity(req.Resources.CPURequest, "resources.cpuRequest")
			if err != nil {
				return nil, err
			}
			requests[corev1.ResourceCPU] = q
		}
	}
	if err := validateResourceCeilings(requests, limits); err != nil {
		return nil, err
	}
	memLim, ok := limits[corev1.ResourceMemory]
	if !ok || memLim.IsZero() {
		return nil, fmt.Errorf("internal error: refusing to create a server without a memory ceiling")
	}

	// ---- storage ----
	storageQ, err := parseApplyPositiveQuantity(req.Storage, "storage")
	if err != nil {
		return nil, err
	}

	return &v1alpha1.MinecraftServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
		},
		Spec: v1alpha1.MinecraftServerSpec{
			Subdomain:       req.Subdomain,
			DisplayName:     req.DisplayName,
			Image:           req.Image,
			JavaMemory:      deriveApplyJavaHeap(memLim),
			DesiredState:    v1alpha1.DesiredStopped,
			AutostartPolicy: policy,
			Storage:         v1alpha1.StorageSpec{Size: storageQ.String()},
			Resources:       corev1.ResourceRequirements{Limits: limits, Requests: requests},
			// The rest matches what felis-api's create writes (K8sCluster.CreateServer):
			// fall back to the login gate while stopped, RCON on (readiness, the
			// player count and the console all ride it; the operator mints the
			// password), and the default idle stop.
			FallbackServer: naming.SystemLoginServer,
			Rcon: v1alpha1.RconSpec{
				Enabled: true,
				SecretRef: v1alpha1.SecretKeyRef{
					Name: naming.RconSecretName(req.Name),
					Key:  naming.RconSecretKey,
				},
			},
			Idle: v1alpha1.DefaultIdle(),
		},
	}, nil
}

// checkSubdomainUnique lists all MinecraftServers in namespace and rejects the
// request if any CRD already carries the given spec.subdomain. metadata.name
// uniqueness is enforced by K8s on Create, but spec.subdomain must be checked
// here because two CRDs with different names could otherwise share a subdomain.
// It reuses the caller's context and K8s client.
func checkSubdomainUnique(ctx context.Context, cl client.Client, namespace, subdomain string) error {
	var list v1alpha1.MinecraftServerList
	if err := cl.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list servers: %w", err)
	}
	for i := range list.Items {
		if list.Items[i].Spec.Subdomain == subdomain {
			return fmt.Errorf("subdomain %q is already in use by server %q", subdomain, list.Items[i].Name)
		}
	}
	return nil
}

// validateResourceCeilings checks that every resource request is ≤ its limit;
// Kubernetes would reject the CRD anyway, but we fail fast with a clear message.
func validateResourceCeilings(requests, limits corev1.ResourceList) error {
	for name, lim := range limits {
		req, ok := requests[name]
		if !ok {
			continue
		}
		if req.Cmp(lim) > 0 {
			return fmt.Errorf("%s request %s exceeds limit %s", name, req.String(), lim.String())
		}
	}
	return nil
}

// resourcesMemoryString returns the memory limit as a human-readable string
// for the success log.
func resourcesMemoryString(rr corev1.ResourceRequirements) string {
	if m, ok := rr.Limits[corev1.ResourceMemory]; ok {
		return m.String()
	}
	return "?"
}

// ---- pure helpers (K8s-free, testable) ----

func parseApplyAutostartPolicy(s string) (v1alpha1.AutostartPolicy, error) {
	switch s {
	case "":
		return v1alpha1.AutostartOwnerOnly, nil
	case string(v1alpha1.AutostartOwnerOnly):
		return v1alpha1.AutostartOwnerOnly, nil
	case string(v1alpha1.AutostartPublic):
		return v1alpha1.AutostartPublic, nil
	case string(v1alpha1.AutostartAllowlist):
		return v1alpha1.AutostartAllowlist, nil
	default:
		return "", fmt.Errorf("invalid autostartPolicy %q (want ownerOnly, public, or allowlist)", s)
	}
}

func parseApplyPositiveQuantity(s, field string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("invalid %s quantity %q: %v", field, s, err)
	}
	if q.Sign() <= 0 {
		return resource.Quantity{}, fmt.Errorf("%s must be a positive quantity", field)
	}
	return q, nil
}

func deriveApplyJavaHeap(limit resource.Quantity) string {
	const mib = int64(1024 * 1024)
	bytes := limit.Value()

	reserve := bytes / 4
	if floor := 512 * mib; reserve < floor {
		reserve = floor
	}
	if half := bytes / 2; reserve > half {
		reserve = half
	}

	heapMiB := (bytes - reserve) / mib
	if heapMiB < 1 {
		heapMiB = 1
	}
	return fmt.Sprintf("%dM", heapMiB)
}
