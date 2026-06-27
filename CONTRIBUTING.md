# Felis Contributor Guide

This document is the practical entry point for contributors. It focuses on how to
run, test, and reason about the project while keeping changes small and aligned
with the current codebase.

## Project Shape

Felis is a Kubernetes-native Minecraft server control plane. The repository has
four main areas:

- `cmd/felis/`: the single Go CLI binary. It dispatches subcommands such as
  `api`, `operator`, `migrate`, `reaper`, `restore`, `manifests`, and
  `breakGlass`.
- `internal/`: backend packages for API handlers, store migrations, Kubernetes
  rendering, operator reconciliation, build, backup, restore, and related domain
  logic.
- `panel/`: the React/Vite web control panel.
- `plugins/`: Minecraft-side plugins and mods for Velocity, Paper, Fabric,
  Forge, and NeoForge.

The codebase is intentionally split by responsibility. Prefer changing the
smallest owning module instead of adding broad abstractions or rebuilding nearby
code.

## Local Development

You can do most day-to-day development on macOS or Linux without a full cluster.
The full product needs Postgres and Kubernetes, but unit tests and frontend work
run locally.

Recommended local tools:

- Go matching `go.mod`
- Node.js and npm for `panel/`
- Optional: JDK/Gradle for plugin work
- Optional integration environment: a clean Linux VM or server with Docker,
  k3s, and Postgres

Check tool versions:

```bash
go version
node --version
npm --version
java -version
```

## Backend Commands

Run these from the repository root:

```bash
cd /path/to/Felis
```

Run all Go tests:

```bash
go test ./...
```

Run a focused package:

```bash
go test ./internal/api
go test ./cmd/felis
```

Build the CLI:

```bash
go build -o /tmp/felis-dev ./cmd/felis
/tmp/felis-dev help
```

Render Kubernetes manifests without contacting a cluster:

```bash
/tmp/felis-dev manifests \
  --felis-image registry.felis.svc:5000/felis:dev \
  --velocity-cidr 10.0.0.5/32
```

`felis api`, `felis operator`, `felis migrate up`, and `felis reaper` are real
runtime commands. They need external services such as Postgres and/or a
Kubernetes config, so they are not the first choice for quick local iteration.

## Frontend Commands

Run these from the frontend workspace:

```bash
cd /path/to/Felis/panel
```

Install dependencies:

```bash
npm ci
```

Run against a real backend at `http://localhost:8080`:

```bash
npm run dev
```

Run with the local mock API:

```bash
npm run dev:mock
```

The mock dev server prints its accounts, link code, and reset command when it
starts. Use it for frontend work when you do not have the Go API and cluster
running.

Common frontend checks:

```bash
npm run typecheck
npm test
npm run build
```

## Mock API

The frontend mock API lives under `panel/dev/` and is loaded only by
`npm run dev:mock`. It must not leak into production code or business
components.

Run mock commands from the frontend workspace:

```bash
cd /path/to/Felis/panel
npm run dev:mock
```

Current mock accounts:

| Username | Password | Scenario |
| --- | --- | --- |
| `owner` | `devpassword` | admin, linked |
| `user` | `devpassword` | normal user, not linked |
| `linked` | `devpassword` | normal user, linked |
| `setup` | `devpassword` | admin, first-login password change |

Mock Minecraft link code:

```text
LINK1234
```

Reset mock state:

```bash
curl -X POST http://127.0.0.1:5173/api/v1/__mock/reset
```

Mock rules:

- Keep mock-only logic in `panel/dev/`.
- Do not import mock code from `panel/src/`.
- Keep response shapes aligned with `panel/src/lib/types.ts` and the Go API
  handlers.
- Prefer realistic error codes over happy-path-only mocks.
- Do not present mock data as live production data.

## Full Integration Environment

Run integration/deploy commands from the repository root on the Linux host:

```bash
cd /path/to/Felis
```

The one-line bootstrap script is intended for a clean Linux host, not a typical
macOS development machine:

```bash
sudo -E bash deploy/bootstrap.sh
```

Useful overrides:

```bash
export FELIS_REPO_URL=<your fork url>
export FELIS_REF=<your branch>
export FELIS_IMAGE=felis:dev
export FELIS_ROOT_DOMAIN=<node-ip>.nip.io
```

The bootstrap flow installs/configures system services, builds the Felis image,
imports it into k3s, runs migrations, applies CRDs/manifests, and deploys the
control plane.

After bootstrap, use the break-glass console to create or recover the Owner
account:

```bash
sudo felis breakGlass
```

Use a VM or disposable Linux server for this. Treat it as an integration and
acceptance environment, while keeping normal coding and quick tests local.

## Plugin Development

Plugin docs live in `plugins/README.md`.

The plugin modules intentionally use separate Gradle builds:

```bash
# cwd: repository root
cd /path/to/Felis

gradle -p plugins/velocity build
gradle -p plugins/paper build
bash plugins/fabric/gradlew -p plugins/fabric build
bash plugins/forge/gradlew -p plugins/forge build
bash plugins/neoforge/gradlew -p plugins/neoforge build
```

Notes:

- Fabric/Forge/NeoForge use module wrappers.
- Velocity/Paper use system Gradle.
- Most modules target Java 17.
- Paper needs a Java 21 toolchain.
- First builds may be slow because Minecraft dependencies are downloaded and
  remapped.

## Frontend Status

The panel is currently an early control-panel implementation, not a complete
product.

Reasonably usable today:

- Local-password login
- First-login password change
- User/admin route gates
- Dashboard shell
- My servers list
- Wake, stop, claim actions
- Read-only console log stream
- Account linking flow
- Admin create-server form
- Read-only image whitelist
- Mock API for local frontend work

Still shallow or missing:

- Server detail depth
- Log search/filter/download
- RCON command UX, if/when the backend path is ready
- Admin edit/delete/transfer flows
- Image whitelist mutations
- Backups and restore UI
- Fleet-wide ops data
- Stronger component and browser-level tests

When adding frontend features, keep UI state honest. If the backend endpoint does
not exist yet, use an explicit placeholder instead of fake live data.

## i18n Notes

i18n is not yet established as a full system. User-facing strings are currently
mostly inline in TSX and helper functions.

Good first targets:

- Error messages mapped from stable API error codes
- Navigation labels
- Page titles and primary actions
- Phase/status labels
- Empty/loading/error states

Recommended initial approach:

```text
panel/src/i18n/
  index.ts
  en.ts
  zh-CN.ts
```

Use a small typed dictionary first. Add a larger library such as
`i18next/react-i18next` only when the project needs runtime language switching,
pluralization rules, external translation workflows, or more complex
localization behavior.

Do not translate code comments, internal logs, or mock-only terminal messages
unless there is a clear contributor need.

## Contribution Style

Follow the existing code. Keep changes narrow and easy to review.

Guidelines:

- Prefer minimal changes over rewrites.
- Do not add a new abstraction unless it removes real duplication or names a
  strong local concept.
- Keep frontend mock code out of business components.
- Keep backend tests close to the package that owns the behavior.
- Preserve public API and persistence shapes unless the change explicitly needs a
  contract update.
- Do not commit generated build output such as `panel/dist/`, `node_modules/`,
  Gradle build directories, or local binaries.

AI-assisted work is welcome, but the contributor is responsible for the result.
Do not submit a PR that is entirely AI-generated and not personally reviewed.
Low-quality AI dumps, broad rewrites that ignore the current design, unverified
changes, or code the author cannot explain will not be accepted.

Before handing off a change, run the smallest meaningful checks:

```bash
go test ./...
cd panel && npm run typecheck && npm test && npm run build
```

If you cannot run a relevant check, say so explicitly in the handoff.
