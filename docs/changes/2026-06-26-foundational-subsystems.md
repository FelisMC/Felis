# Foundational subsystems: the initial Felis import (ledger backfill)

- **Type:** feature (initial import) — retroactive ledger entry
- **Date:** 2026-06-26
- **Area:** `apis/`, `internal/` (naming, rcon, store, config, build, backup, operator,
  submit, api, platform), `cmd/felis`, `plugins/`
- **Commits:**
  - `7fbebfe` feat(apis): MinecraftServer CRD types (v1alpha1) — the lifecycle source of truth (§1)
  - `708cdfc` feat(core): naming, RCON, store (Postgres + embedded migrations), config, image-build libraries
  - `43ab921` feat(backup): archive-based world backup/restore + the retention/idle reaper
  - `78b8cf6` feat(operator): MinecraftServer controller and reconcilers
  - `d39605e` feat(submit): user modpack build + admin-approval pipeline (see [modpack-submission-lane](2026-06-26-modpack-submission-lane.md))
  - `b508fcc` feat(api): dual-faced felis-api — permissions/LuckPerms, modpack lane, admin fleet read
  - `47fcd90` feat(platform): node orchestration + the `cmd/felis` single-binary entrypoint
  - `93f143f` feat(plugins): Velocity proxy + Fabric/Forge/NeoForge/Paper integration mods
- **Tasks:** #23 (permissions), #24 (modpack lane), #25 (fleet read)

## What it did

Stood up the whole backend spine in one build-order sweep: the Kubernetes CRD that is
the lifecycle source of truth, the core libraries (deterministic resource naming, the
RCON client, the Postgres store with embedded SQL migrations, config loading, container
image-build helpers), the backup/restore/reaper subsystems, the operator controller
that drives `MinecraftServer` resources, the user-modpack submit+approval pipeline, the
dual-faced (internal/external) felis-api behind a Zero-Trust guard, the platform
orchestrator that wires it all together under `cmd/felis`, and the server-side
integration plugins.

## Why

This is the project's first functional import — the substrate every later change edits.
It predates the change-ledger convention (established `fad48ff`, 2026-07-06), so it never
got a contemporaneous detail doc; this entry backfills one.

> **Backfill note.** Reconstructed 2026-07-07 from the commit history to close the
> change-ledger's detail-doc axis (§ Convention). This entry deliberately describes only
> what these eight commits **introduced** on 2026-06-26 — the named subsystems have been
> extended and reworked many times since (auth, passkey, metrics, quotas, updates), and
> that later work lives in its own dated detail docs, not here. Not independently
> re-verified for this doc; each subsystem was verified at its original commit and the
> current tree builds green at `9911b8c` (WSL oracle, go1.26.4).
