# `/felis migrate` in-game command (§B3 inherit, Velocity side)

- **Type:** feature (addition)
- **Date:** 2026-07-05
- **Area:** `plugins/velocity` + `plugins/shared` — Velocity proxy plugin (Java, compile-verified)
- **Commit:** `c1aa38b` — feat(velocity): add /felis migrate to open an account migration (§B3 inherit)
- **Task:** completes the code-only gap named in `internal/api/handlers_account_migrate.go`

## What it does

Adds the in-game `/felis migrate` command that a player runs to **open an account
migration** — the first step of handing their owned servers to another account (spec
§B3 "inherit", scenario A). The command posts the player's Mojang-verified UUID to the
backend, which puts that account into migrate mode (`state=initiated`). The player then
finishes the migration on the web console (prove it's them, name the receiving account,
redeem a one-time code).

The Go backend (`handleMigrateStart` and the web-driven steps 2–4) already existed and
was tested; its header comment explicitly named **"the `/felis migrate` command that
calls handleMigrateStart"** as the code-only gap. This change closes that gap.

## Why

Without the in-game command, the migration flow had no entry point — the backend
handler was reachable only in theory. `/felis migrate` is the trustworthy initiator:
Velocity has already established the caller's online-mode UUID, so the sensitive proof
can be deferred to the web step-up while the in-game command just opens the migration.

## Design decisions

- **Mirrors the existing command suite verbatim.** `doMigrate` follows `doClaim`;
  `migrateError` follows `claimError`; `migrateStart` follows `claim`/`opLoginApprove`.
  No new imports, types, or idioms — every construct already appears in the same files.
- **Identity-bound + out-of-limbo, but server-independent.** Like `claim`, it requires
  a real player past the login limbo (`requirePlayer` + `ensureOutOfLimbo`). Unlike
  `claim`, it acts on the caller's *account*, not the server they stand on, so there is
  **no** `registry`/current-server check.
- **Expects HTTP 201.** `migrateStart` posts to
  `/api/v1/internal/account/migrate/start` and expects **201 Created** (`handleMigrateStart`
  returns `StatusCreated`) — not 200 like the other calls. A 201 that does not affirm
  `started:true` is treated as a contract breach, not a refusal.
- **Error mapping matches the handler's refusals:** 404 `not_linked` → "Link your
  account on the web console before migrating"; 409 `account_retired` → "This account
  can't start a migration (already migrated or retired)"; transport (0) and default →
  generic retry text.
- **Points the player to the console on success.** The command only *opens* the
  migration, so on success it prints the player web console URL
  (`https://console.<root_domain>`, derived from config — never a hardcoded domain) and
  a one-line description of the remaining steps. A proxy-side `logger.info` records the
  initiating username against the UUID (the backend audit only has the UUID).

## Files

| File | Change |
|---|---|
| `plugins/shared/.../link/FelisApiClient.java` | **+`migrateStart(UUID)`** — POST mc_uuid, expect 201, affirm `started:true` |
| `plugins/velocity/.../FelisVelocityPlugin.java` | `migrate` literal in the Brigadier tree; **`doMigrate`** handler; **`migrateError`** mapper; `/felis migrate` help line |

## Verification

Java is not oracle-verifiable via the Go suite, but it **is** compile-verifiable via
the podman gradle toolchain established in #63/#65:

```
podman run --rm -v plugins:/work -w /work/velocity \
  docker.io/library/gradle:jdk17 gradle --no-daemon compileJava
→ BUILD SUCCESSFUL in 19s   (compiled against real velocity-api:3.3.0-SNAPSHOT)
```

The change compiles clean against the real Velocity API jar (including the shared
`FelisApiClient` compiled straight into the velocity module). The backend contract it
speaks to (`handleMigrateStart`) is covered by `handlers_account_migrate_test.go` on
the Go side.

## Self-review outcome

- **ponytail (over-engineering):** lean — pure mirror of three existing, compiling
  methods; no speculative abstraction. Nothing cut.
- **correctness:** the one contract divergence (201 vs 200) was verified against the Go
  handler source before writing.
