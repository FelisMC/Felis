# Break-glass "halt a running server" op (#31 B4)

- **Type:** feature (addition)
- **Date:** 2026-07-05
- **Area:** `cmd/felis` — break-glass recovery console (Go, oracle-verifiable)
- **Commit:** `c2ee21a` — feat(breakglass): add halt-a-server op to the recovery console (§B4)
- **Task:** #31 Phase B4 (felis TUI break-glass ops)

## What it does

Adds a **"Halt a running server"** operation to the root-gated break-glass console.
The operator picks a server from the live fleet and the console flips that
`MinecraftServer` CRD's `spec.desiredState` to `Stopped`, letting the operator
reconcile it into a graceful shutdown. It is the emergency "stop this now" lever for
when the panel is unreachable but the box still has `root` + a kubeconfig.

## Why

The break-glass console already provisions the Owner and adds Operators, but there
was no local, panel-independent way to **stop** a misbehaving server (runaway,
compromised, resource-pinning). Halting is a reversible state nudge — the safest
possible break-glass power — so it belongs in the same root-gated recovery surface.

## Design decisions

- **CRD write, not pod kill.** The console flips `spec.desiredState=Stopped` with a
  **spec-only merge patch** (`client.MergeFrom`), never a full-object `Update`. The
  operator writes `status` on the same object continuously; a merge patch of
  `spec.desiredState` touches a disjoint field and cannot race/clobber the operator's
  status writes. A halt is therefore exactly the CRD write the operator already knows
  how to honour.
- **Authority = root + kubeconfig.** The accountable actor is the OS user who
  escalated to root (`osUser`), recorded for attribution — not proof. The root gate
  plus kubeconfig possession *is* the authority, so (unlike the owner/operator paths)
  no credential-minting auth sub-flow is needed for a reversible state change.
- **System servers allowed but named.** Halting the `login`/`lobby` system servers
  takes the shared front door down (login has no fallback). Break-glass is deliberately
  full power, so the console **warns** rather than forbids: a `⚠ system` tag in the
  picker and an explicit `WARNING` line in the post-exit summary.
- **Audit is best-effort.** `performHalt` mirrors `performBreakGlass`: the halt
  succeeds even if the audit sink is down (break-glass must work with logging broken);
  any audit error rides back in the outcome and is surfaced as a summary `WARNING`.
- **Already-stopped is a no-op** reported distinctly ("was already stopped" vs "is now
  stopping"), so the console never claims a stop it didn't perform.
- **Namespace from config.** The target namespace is `cfg.K8s.Namespace`, threaded
  through the console constructors — never hardcoded.

## Files

| File | Change |
|---|---|
| `cmd/felis/halt.go` | **new** — pure core (no bubbletea): `listServersForHalt`, `haltServer` (merge patch), `isSystemServer`, `performHalt`, `auditHalt` |
| `cmd/felis/halt_test.go` | **new** — table tests against a controller-runtime **fake client** (applies patches for real): running→stopped persists, already-stopped no-op, missing→error, system flag, list projection + desired-state fallback, audit success, audit-failure-still-halts |
| `cmd/felis/tui_halt.go` | **new** — bubbletea/huh shell mirroring `ownerModel` (load → pick → work → done), empty-fleet guard, `⚠ system` picker labels, outcome card |
| `cmd/felis/tui_menu.go` | `bgHaltServer` enum + "Halt a running server" menu option |
| `cmd/felis/tui_root.go` | `namespace` field; `bgHaltServer` dispatch to `newHaltModel`; `haltResultMsg` terminal handling |
| `cmd/felis/breakglass.go` | halt fields on `breakGlassResult`; `namespace` threaded through `runBreakGlassTUI`/`runSetupTUI`/`runConsoleTUI`; post-exit halt summary (stopping / already-stopped, system + audit warnings, restart hint) |
| `cmd/felis/setup.go` | pass `cfg.K8s.Namespace` into `runSetupTUI` |
| `cmd/felis/tui_root_test.go` | pass `"minecraft"` namespace into `newRootModel` test call |

## Verification

WSL oracle (go1.26.4, FedoraLinux-44), authoritative for Go:

```
go build ./...            → BUILD_OK
go vet ./cmd/felis/...    → VET_OK
go test ./...             → all 20 packages ok, ALL_GREEN
```

The core (`halt.go`) is fully unit-tested against a real `fake.Client`, which applies
the merge patch, so the test asserts the **persisted** `spec.desiredState`, not merely
that `Patch` was called. `tui_halt.go` is thin bubbletea glue (untested by house
convention, mirrors the existing `tui_owner.go`).

## Self-review outcome

- **ponytail (over-engineering):** lean — no one-impl interface, every field consumed,
  audit seam justified. Nothing cut.
- **correctness:** caught and fixed a misleading restart hint — the summary originally
  pointed at `felis apply`, but that command is **create-only** (errors "already
  exists" on an existing server); corrected to "restart from the panel, or set
  `spec.desiredState` back to Running."
