// Package platform renders the cluster-install objects that bound what every
// Felis identity may do (spec §21, §22): the RBAC for the control-plane
// identities (felis-api and felis-operator always; felis-reaper only when the
// retention reaper is enabled, gated with its CronJob), the weak service
// accounts for the build/restore Jobs, and the minecraft-namespace
// NetworkPolicies that fence server pods. Like internal/build and
// internal/restore, the objects are pure Go-typed values so the security-critical
// shape is unit-tested here, because no cluster runs in this environment.
//
// # Why the RBAC is derived from code, not from the spec prose
//
// The allowlist is built from the actual K8s API call sites in the control
// plane, not transcribed from §21's wording, because the prose is incomplete and
// in places wrong. Concretely, the rendered Roles encode these realities the
// prose misses:
//
//   - felis-api creates the restore Job in the minecraft namespace (see
//     internal/restore) and the build Job in the build namespace (see
//     internal/build), so it needs batch/jobs verbs in BOTH namespaces — not
//     just minecraftservers + secrets.
//   - The RCON port (25575) is reached by BOTH felis-api (console writes, see
//     internal/api.console) AND felis-operator (the readiness prober, see
//     internal/operator.prober). The RCON NetworkPolicy peer is therefore
//     {api, operator}, not api alone.
//   - felis-api ALSO reads pod logs for the read-side console (spec §8 读=pods/log
//     follow, see internal/api.logstream): it gets pods:list (to find a server's
//     running pod by label) + pods/log:get (to follow it), and deliberately NOT
//     pods:get — the streamer never reads a pod's full object. This is the only
//     pods grant in the bundle, and it belongs to the api, not the operator.
//   - felis-operator uses the manager's CACHED client (informers), so even a
//     single Get on a type requires list+watch on it; felis-api and felis-reaper
//     use direct clients, so they need only the verbs they literally call.
//   - The reaper is a distinct, destructive identity: it deletes
//     PersistentVolumeClaims and patches minecraftservers, powers that belong to
//     neither the api nor the operator. It gets its own SA and Role.
//   - The operator never emits Events, sets finalizers, deletes workloads, or
//     touches pods/PVCs, so none of those appear in its Role — and
//     minecraftservers/status carries only `update` (it calls Status().Update),
//     never get/patch.
//
// # Honesty
//
// The typed render and the §22 invariant tests are Oracle-verifiable here. What
// is NOT verifiable in this toolchain, and is therefore code-complete-but-
// unverified (the same bucket as the K8s E2E):
//
//   - that the RBAC actually binds and authorizes on a live apiserver;
//   - that the NetworkPolicies are enforced by the cluster CNI (a CNI without
//     NetworkPolicy support silently ignores them);
//   - the egress_mode=loadbalancer source-IP caveat (spec §20): with MetalLB L2,
//     a per-server Service must set externalTrafficPolicy=Local or the client IP
//     is SNAT'd and the 25565 ipBlock allowlist will not match the Velocity host.
//
// A Helm-chart wrapper around these objects is intentionally NOT produced: it
// could not be validated here (no helm/kubeconform), and the typed generator
// (RenderYAML, surfaced by `felis manifests`) is the verifiable source of truth a
// chart would only re-encode.
package platform
