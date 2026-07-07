# System servers: login-limbo + lobby (always-on gate) (ledger backfill)

- **Type:** feature — retroactive ledger entry
- **Date:** 2026-07-02
- **Area:** `internal/config`, `internal/naming`, `internal/api` (CRD readiness), `internal/operator`, `internal/platform`, `cmd/felis`, `plugins/limbo`, `deploy/limbo` + `deploy/lobby`
- **Commits:**
  - `9bed51b` feat(config): `[velocity] login_image/lobby_image` — setup provisions the always-on system services only when set (empty = fail-loud skip; no official LOOHP/Limbo image exists)
  - `9ef817f` feat(naming): reserved system-server names + service-token identifiers (single source of truth for the internal-API credential Secret)
  - `159107b` feat(api): HTTP readiness knob on `MinecraftServer` + user-server fallback defaults to the login gate
  - `dc23cb5` feat(operator): system-server pod HTTP readiness probe + login-only `FELIS_SERVICE_TOKEN` env (keyed off the reserved name so it can never leak into a user pod; sourced via `secretKeyRef`, never inlined)
  - `3fdb3d0` feat(platform): internal-API base-URL helper + single-sourced token Secret
  - `f554d52` feat(cli): provision the reaper-exempt login/lobby servers + replicate the service-token Secret into the minecraft namespace
  - `241fe21` feat(limbo): felis-limbo in-game login flow (join → blacklist check → mint bind code → open book to `console.<root_domain>` → poll link-status → BungeeCord transfer to lobby; fail-closed)
  - `c7315e4` feat(deploy): login-limbo + lobby images with game-port pinning (server-port pinned to GamePort 25565 on every start)
- **Tasks:** #53–#68 (system-server plumbing L1–L4, limbo plugin, operator env injection)

## What it did

Stood up the always-on authentication gate: reserved, reaper-exempt login/lobby
`MinecraftServer`s provisioned by setup, an HTTP readiness path for the RCON-less LOOHP/Limbo
loader (which reports "started" only after the first tick), and the felis-limbo plugin that
runs the whole onboarding *inside* Limbo before transferring an admitted player to the
lobby. A fresh connection always lands on the login gate, never a user backend, so
authentication is always in front.

## Why

The spec requires that a player authenticate before reaching any real server. That needs a
purpose-built always-on front server (Limbo) that speaks to the internal API — hence the
login-only service-token injection (keyed to the reserved name so it can never reach a user
pod) and the HTTP readiness knob for a loader that has no RCON.

> **Backfill note.** Reconstructed 2026-07-07 from the commit history. The Go layer
> (config/naming/readiness/operator env/platform) was unit-tested at each commit; the
> felis-limbo plugin is Java verified against a real Limbo jar via podman (#65), and the
> images carry a real build+boot check (#63, limbo `/healthz` 200 on 25565). Not
> independently re-verified for this doc; current tree green at `9911b8c`.
