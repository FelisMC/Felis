# Separate ClusterIP Service for the felis-api internal face (8081)

- **Type:** bug fix (latent networking gap) + enabling change
- **Date:** 2026-07-07
- **Area:** `internal/platform` — Go (struct render oracle-verified; packet path **not**
  verifiable in this environment — see Verification)
- **Commit:** `2ba9948`
- **Task:** #31 Phase B4 — surfaced while wiring the break-glass backup console peer
  (phase 2b): the peer needs a routable path to the internal face, and that path was
  broken for the login pod too.

## What it does

Renders a **new ClusterIP-only Service `felis-api-internal`** (control namespace)
that fronts the felis-api pod's internal port 8081, and repoints
`InternalAPIBaseURL` (the URL baked into the login pod's `FELIS_API_BASE_URL`) at
that Service name. Adds exported `APIInternalServiceName` / `APIInternalPort` so the
on-node break-glass console can resolve the Service's ClusterIP and dial it.

## Why (the latent bug)

The login limbo pod is configured with
`FELIS_API_BASE_URL = http://felis-api.<ns>.svc.cluster.local:8081` (setup.go) and
dials the internal face with the service token to mint bind codes and poll link
status. But the only Service named `felis-api` is the **external** face: a NodePort
Service that declares **only** port 443. A Service answers only on its declared
ports, so `felis-api:8081` had no backend — **every login-pod call to the internal
API silently failed to connect.** `deploy/limbo/README.md` even documented the
"login-pod → felis-api internal-port (8081) path" as reachable; it was not.

## Design decisions

- **A separate Service, not a second port on `felis-api`.** A `Type: NodePort`
  Service allocates a node port for **every** declared port, with no per-port
  opt-out. Folding 8081 into the NodePort `felis-api` Service would therefore publish
  the internal face — which is service-token-only, explicitly **no Zero Trust** — on
  every node's external IP. That violates the two-face security posture. A distinct
  `ClusterIP` Service exposes 8081 **in-cluster only**: reachable by the login pod via
  cross-namespace DNS, and by the on-node console via the ClusterIP (kube-proxy
  programs ClusterIPs into the node's routing).
- **Repoint `InternalAPIBaseURL` to the new Service name.** The helper single-sources
  the name the login pod is told to call; pointing it at `felis-api-internal` keeps
  the login pod and the Service in agreement by construction.
- **Export the name + port for the console.** The break-glass backup peer (phase 2b)
  resolves `APIInternalServiceName`'s ClusterIP at runtime and dials
  `http://<clusterIP>:APIInternalPort` — it cannot use the cluster-DNS form because
  the host's resolver is not CoreDNS.

## Files

| File | Change |
|---|---|
| `internal/platform/workloads.go` | **+`apiInternalService`** (ClusterIP, 8081→`internal`), wired into `Workloads()`; **+exported `APIInternalServiceName`/`APIInternalPort`**; `InternalAPIBaseURL` repointed at the internal Service, comment corrected |
| `internal/platform/workloads_test.go` | **+`TestAPIInternalService_ClusterIP`** — ClusterIP (never NodePort), 8081→`internal`, no nodePort, selects the api pods, name distinct from `felis-api` |
| `deploy/limbo/README.md` | document the `felis-api-internal` Service; correct the reachability note |
| `docs/troubleshooting.md` | §6 note: internal calls reached via `felis-api-internal`; a *connect* failure (not 401) points at that Service |

## Verification

WSL oracle (go1.26.4, authoritative for Go):

```
go build ./...  &&  go vet ./...  &&  go test ./...   → ALL GREEN
```

`TestAPIInternalService_ClusterIP` freezes the Service's shape. **This is a
code-level fix only.** `go build/vet/test` verifies the Service *struct* renders
correctly; it verifies **nothing** about packets flowing — not the login pod's
in-cluster call, not the console's host→ClusterIP dial (which relies on kube-proxy's
OUTPUT-chain DNAT, present on k3s but unverified here), not that 8081 is programmed
on a live cluster. Per the project's "Java/K8s code-only" reality, the runtime path
is **pending real-cluster verification**; the manifest-level defect (a DNS name with
no backing port) is fixed and asserted.

## Self-review outcome

- **ponytail (over-engineering):** one Service + two exported identifiers, all
  load-bearing (the login pod and the console both need the routable 8081). No new
  abstraction; `apiInternalService` mirrors `apiService`/`registryService`.
- **correctness / security:** the ClusterIP-not-NodePort choice is the crux — it keeps
  the no-Zero-Trust internal face off every node's external interface, which a second
  port on the NodePort Service could not.
