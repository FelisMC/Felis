# Deploy: one-line bootstrap installer + demo bring-up (ledger backfill)

- **Type:** feature + fix — retroactive ledger entry
- **Date:** 2026-06-27 – 2026-07-03
- **Area:** `deploy/` (bootstrap.sh, Dockerfiles, demo-up.sh), image build context
- **Commits:**
  - `58fa4b0` feat(deploy): one-line bootstrap installer + distroless felis image (auto-detects apt/dnf, installs Docker/k3s/PostgreSQL, opens pg_hba to the pod CIDR, runs migrations, applies the control-plane bundle, leaves Web disabled pending `felis setup`)
  - `94a3b7b` fix(deploy): harden bootstrap for RHEL-family Linux
  - `deaa2f8` feat(deploy): zypper support (openSUSE/SLES)
  - `318a724` feat(deploy): pacman support (Arch)
  - `e5f1682` refactor(deploy)!: TUI (breaking walkthrough restructure)
  - `28c3eee` refactor(deploy): improved TUI walkthrough
  - `c14ed17` fix(docker): keep embedded `panel/` and `deploy/` in the image build context
  - `d9e866f` fix(deploy): make the lobby image actually build (re-include `plugins/paper`, build on `gradle:8.14-jdk21`)
  - `b84debf` feat(deploy): one-shot `demo-up.sh` — bootstrap → build/import limbo+lobby images → wire `[velocity]` image refs → `felis setup`, ending in the interactive Owner TUI
- **Tasks:** #26 (Phase A bootstrap verified end-to-end on the Demo VM)

## What it did

Made a bare Linux box a running Felis with one command. `bootstrap.sh` auto-detects the
host package manager across the four major families (apt/dnf/zypper/pacman), installs
whatever is missing (Docker, k3s, PostgreSQL, cloudflared), builds+imports the distroless
felis image, opens `pg_hba` to the pod CIDR, runs migrations, and applies the rendered
control-plane bundle. `demo-up.sh` wraps that plus the login-limbo/lobby image build and
`felis setup` into a single command, stopping only at the Owner-creation TUI it cannot
automate.

## Why

The spec calls for a self-hostable single-node deployment a SysAdmin can stand up without
a Kubernetes background. The package-manager fan-out and the demo wrapper are what make
"one line" true across real distros rather than only on the author's box.

> **Backfill note.** Reconstructed 2026-07-07 from the commit history to close the
> change-ledger's detail-doc axis. `deploy/` is shell + Dockerfiles (not Go-oracle
> verifiable); `d9e866f` records a real build+boot check (limbo `/healthz` 200, lobby
> reaches "Done"). Not independently re-verified for this doc; current tree green at
> `9911b8c`.
