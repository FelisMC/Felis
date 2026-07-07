# felis-api security + robustness hardening (audit sweep) (ledger backfill)

- **Type:** fix — retroactive ledger entry
- **Date:** 2026-06-30 – 2026-07-01
- **Area:** `internal/api` (login, request-id, listeners, SSE relays, quota/claim, MyServers), `internal/operator`
- **Commits:**
  - `7a51c1d` fix(api): bound concurrent login bcrypt to shed CPU-pin floods (429 `auth_busy` before the compare; a cap, not a per-account lockout) — *audit #2*
  - `164ac44` fix(api): validate inbound `X-Request-Id` before echo + audit persist (≤64 bytes, log-safe charset) — *audit-integrity*
  - `c6c0772` fix(api): read/idle timeouts on all three listeners via a `newAPIServer` factory (closes Slowloris via `ReadHeaderTimeout`; `WriteTimeout` left unset so SSE isn't severed) — *audit #3*
  - `3c1d647` fix(api): per-principal SSE stream cap (429 `too_many_streams`) — *audit #1, blast-radius bound*
  - `d6e3189` fix(api): per-write deadline on SSE relay to sever a stalled reader (the real leak close behind the cap) — *audit #1*
  - `8f41a00` fix(api): clear the SSE write deadline on return so it can't leak onto a reused keep-alive connection — *audit #1*
  - `6368ab1` fix(api): `COALESCE` the MyServers `owned` flag so an ownerless row doesn't 500 the listing
  - `2a4a81b` fix(api): don't burn the wake cooldown when refused at capacity
  - `9873904` fix(operator): populate `Status.Players` from an RCON `list` probe (so the panel doesn't report 0/0)
- **Tasks:** #33 (wake cooldown), #34 (Status.Players), #41–#46 (audit #1–#4)

## What it did

A hardening sweep across the API's abuse and robustness surface: bound the two unbounded
CPU/goroutine amplifiers (concurrent bcrypt, per-principal SSE streams), close the SSE
relay's real stalled-reader leak with a per-write deadline (and clear it so it can't leak
onto a pooled connection), validate the caller-supplied request id before it reaches the
audit trail, set listener timeouts to close Slowloris, and fix two functional bugs — the
ownerless-row 500 and the wake cooldown burned on a capacity refusal.

## Why

Each is a specific, demonstrated failure mode: a login flood pins every core in bcrypt; a
stalled SSE reader leaks a relay goroutine + its upstream kube-apiserver follow *for the
life of the process*; an unvalidated `X-Request-Id` is a CR/LF log-forgery vector. The
`WriteTimeout`-left-unset detail is load-bearing — a blanket write timeout would sever the
healthy long-lived console/build-log streams the platform depends on.

> **Backfill note.** Reconstructed 2026-07-07 from the commit history. Each fix shipped a
> targeted test at its commit — notably `d6e3189`/`8f41a00` use a deadline-aware
> `ResponseWriter` that fails closed if the guard is removed. The quota-claim TOCTOU
> (audit #4) is a documented KNOWN-LIMITATION (`2c56d17`), closeable only against a real
> Postgres. Not independently re-verified for this doc; current tree green at `9911b8c`.
