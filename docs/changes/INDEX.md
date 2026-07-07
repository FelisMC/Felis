# Felis change ledger

The index of every functional change to Felis — what it did and which commit records
it. This is the durable, in-repo map that `git log` alone doesn't give: it links
substantial changes to their detail docs and flags work that is built and verified but
not yet committed.

## Convention

- **Every functional change** (a feature addition, a behaviour change, a bug fix) gets:
  1. a dated detail doc in this directory — `docs/changes/YYYY-MM-DD-<slug>.md`, covering
     _what it did, why, the files touched, and the verification evidence_; and
  2. a row in the ledger below, carrying its **commit record** (the short SHA).
- A change that is **built and verified but not yet committed** (e.g. while PGP signing
  is locked) sits in **Pending** with `commit: pending`, and moves into the ledger with
  its real SHA once committed.
- Pure-cosmetic or non-functional commits (docs, style) still appear in the ledger table
  for completeness, but do not require a dedicated detail doc.
- The ledger table is generated losslessly from git history and can be regenerated:
  ```
  git log --reverse --pretty=format:'| %h | %ad | %s |' --date=short
  ```

## Pending (built + verified, not yet committed)

| Change | Detail doc | Status |
|---|---|---|
| _None._ | | |

## Detail docs

Depth docs for substantial changes, keyed to the commit(s) they cover. The committed
ledger table below stays a lossless mirror of `git log` (so it can be regenerated); this
section is where a row's detail doc, when it has one, is found. Most rows — panel/UI,
docs, chore, style — have no detail doc by convention and are recorded by their table row
alone. Entries marked *(backfill)* were reconstructed retroactively on 2026-07-07 from git
history to close the ledger's detail-doc axis for the pre-convention functional commits;
each carries a backfill note stating it was not independently re-verified. Frontend/`panel`
commits are the collaborator's UI work and are not given detail docs here.

| Detail doc | Commit(s) | Scope |
|---|---|---|
| [foundational-subsystems](2026-06-26-foundational-subsystems.md) *(backfill)* | `7fbebfe` `708cdfc` `43ab921` `78b8cf6` `d39605e` `b508fcc` `47fcd90` `93f143f` | initial import: CRD, core libs, backup, operator, submit, api, platform, plugins |
| [modpack-submission-lane](2026-06-26-modpack-submission-lane.md) *(backfill)* | `d39605e` `598f3d3` | §8 modpack build/approval pipeline + local/S3 backends |
| [deploy-bootstrap-installer](2026-06-27-deploy-bootstrap-installer.md) *(backfill)* | `58fa4b0` `94a3b7b` `deaa2f8` `318a724` `e5f1682` `28c3eee` `c14ed17` `d9e866f` `b84debf` | one-line bootstrap installer + demo bring-up |
| [console-auth-passwordless](2026-06-27-console-auth-passwordless.md) *(backfill)* | `af14f02` `0c1cc59` `3b43f05` `c20b12c` | local-password login → passwordless migration + residue sweep |
| [felis-cli-break-glass-setup](2026-06-27-felis-cli-break-glass-setup.md) *(backfill)* | `e108a37` `2d0bbb0` `a94b001` `eb5875a` `f5d00f3` `9c46632` `7d91373` | break-glass recovery console + first-run setup + apply/migrate |
| [cloudflare-tunnel-access-edge](2026-06-27-cloudflare-tunnel-access-edge.md) *(backfill)* | `53a7664` `ba13839` `a531f5e` `2810fe8` `7d3be64` `346ec68` `e058a64` | §14 Tunnel + fail-closed Access edge + NodePort fence |
| [player-onboarding-b2](2026-06-27-player-onboarding-b2.md) *(backfill)* | `dbe34a1` `1f8b9bb` `116595f` `fe2ece0` `55592ed` `6c3999a` `879b177` | §B2 email-OTP, account-link, QR, Bind-Code + OTP throttle |
| [username-reclaim-b3](2026-06-27-username-reclaim-b3.md) *(backfill)* | `a29571d` `fdb6efb` | §B3 Mojang-priority reclaim + account migration |
| [felis-metrics](2026-06-30-felis-metrics.md) *(backfill)* | `75642d9` `2a93a9e` `79eae7f` `8ac5e64` | §23 felis_* Prometheus collectors |
| [felis-api-hardening](2026-06-30-felis-api-hardening.md) *(backfill)* | `7a51c1d` `164ac44` `c6c0772` `3c1d647` `d6e3189` `8f41a00` `6368ab1` `2a4a81b` `9873904` | audit #1–#3 + robustness fixes |
| [passkey-enrollment](2026-07-01-passkey-enrollment.md) *(backfill)* | `f2c916d` `742f15f` `0261204` `fce0fce` `7278cd7` `cdbb5ab` `9953275` `20e31fb` `54bc6ef` | WebAuthn enrollment + hardening a–e |
| [passkey-login](2026-07-01-passkey-login.md) *(backfill)* | `e035142` `ec468ba` `0dbd557` `9e1df12` `4f59d51` `a63f49d` | WebAuthn assertion/discoverable login + UA-guard |
| [auto-update-subsystem](2026-07-01-auto-update-subsystem.md) *(backfill)* | `c01f133` `3673af6` `7464fa7` `96b3cc9` `7d27640` `7db57b9` | update decision core + sources + gatherer + window API (report-only) |
| [system-servers-login-limbo-lobby](2026-07-02-system-servers-login-limbo-lobby.md) *(backfill)* | `9bed51b` `9ef817f` `159107b` `dc23cb5` `3fdb3d0` `f554d52` `241fe21` `c7315e4` | always-on login-limbo + lobby auth gate |
| [operator-idle-quota-readiness](2026-07-05-operator-idle-quota-readiness.md) *(backfill)* | `91bfa27` `e574749` `7f7e459` `7becb38` | idle auto-stop, quotas, timeouts, /readyz |
| [break-glass-halt](2026-07-05-break-glass-halt.md) | `c2ee21a` | §B4 break-glass halt-a-server op |
| [felis-migrate-command](2026-07-05-felis-migrate-command.md) | `c1aa38b` | §B3 `/felis migrate` account migration |
| [on-demand-world-backup](2026-07-07-on-demand-world-backup.md) | `7a7c0d5` | §B4 Sync phase 1 — external backup endpoint + Job |
| [internal-backup-endpoint](2026-07-07-internal-backup-endpoint.md) | `f2fc57c` | §B4 Sync phase 2a — internal-face backup endpoint |
| [internal-api-clusterip-service](2026-07-07-internal-api-clusterip-service.md) | `2ba9948` | felis-api internal-face ClusterIP Service |
| [break-glass-backup-peer](2026-07-07-break-glass-backup-peer.md) | `fc748d3` | §B4 Sync phase 2b — console backup peer |

## Committed change ledger

Oldest first (project build order). Commit = short SHA on `main`. Frontend/`panel`
commits are the collaborator's UI work; backend (Go/Java/K8s) is tracked here as the
primary record.

| Commit | Date | Change |
|---|---|---|
| 5b7b38d | 2026-06-26 | chore: add Go module manifest and ignore rules |
| 5a30aa5 | 2026-06-26 | docs: add OpenAPI 3.1 served-route contract |
| 7fbebfe | 2026-06-26 | feat(apis): add MinecraftServer CRD types (v1alpha1) |
| 708cdfc | 2026-06-26 | feat(core): add naming, RCON, store, config, and image-build libraries |
| 43ab921 | 2026-06-26 | feat(backup): add backup, restore, and reaper subsystems |
| 78b8cf6 | 2026-06-26 | feat(operator): add MinecraftServer controller and reconcilers |
| d39605e | 2026-06-26 | feat(submit): add user modpack build and approval pipeline |
| b508fcc | 2026-06-26 | feat(api): add felis-api service with permissions, modpack lane, and fleet read |
| 47fcd90 | 2026-06-26 | feat(platform): add node orchestration and the felis entrypoint |
| eee00c2 | 2026-06-26 | feat(panel): add three-sided web console (User, Admin, SysAdmin) |
| 93f143f | 2026-06-26 | feat(plugins): add Velocity proxy and Fabric/Forge/NeoForge/Paper integration mods |
| ce0ba76 | 2026-06-27 | chore(api): add kubebuilder object-generation markers to v1alpha1 |
| 7d91373 | 2026-06-27 | fix(migrate): honor -config flag placed after the up verb |
| 99de43f | 2026-06-27 | chore: ignore plugin build artifacts and editor config |
| 58fa4b0 | 2026-06-27 | feat(deploy): add one-line bootstrap installer and container image |
| af14f02 | 2026-06-27 | feat(api): local-password authentication backend |
| e108a37 | 2026-06-27 | feat(cli): break-glass emergency console TUI |
| 885c4a9 | 2026-06-27 | feat(panel): local-password login and forced password change |
| 2d0bbb0 | 2026-06-27 | feat(cli): attribute break-glass recovery to the SysAdmin who runs it |
| dbe34a1 | 2026-06-27 | feat(api): add player email OTP verification (spec §B2 onboarding) |
| 1f8b9bb | 2026-06-27 | feat(api): record account-link auth source (mojang/thirdparty) |
| a29571d | 2026-06-27 | feat(api): reclaim squatted usernames for Mojang-priority players (spec §B3) |
| 53a7664 | 2026-06-27 | feat(cfsetup): recommended Cloudflare Tunnel + Access edge setup |
| ba13839 | 2026-06-27 | feat(breakglass): optional Cloudflare Tunnel + Access setup in the TUI |
| a5a6482 | 2026-06-27 | feat: dev mock |
| dd2fc6f | 2026-06-27 | feat(panel): i18n |
| 7b458da | 2026-06-27 | feat(panel): light/dark theme |
| 51c9eab | 2026-06-27 | docs: CONTRIBUTOR.md |
| 74e7e6e | 2026-06-27 | docs: CONTRIBUTING.md |
| b81b334 | 2026-06-27 | Merge branch 'main' of https://github.com/MliroLirrorsIngenuity/Felis |
| 9d13787 | 2026-06-27 | refactor(panel): dashboard |
| f3521e3 | 2026-06-27 | refactor(panel): uniform margins |
| 18b4be0 | 2026-06-27 | refactor(panel): uniform title icon styles |
| 665841b | 2026-06-27 | fix(panel): remove internal spec references from user-facing text |
| cf88bcc | 2026-06-27 | fix(panel): extract hardcoded security note into i18n keys |
| 061482d | 2026-06-27 | fix(panel): extract hardcoded Chinese text to i18n keys |
| cfe126f | 2026-06-27 | feat(panel): add RCON command input to server console |
| ae91133 | 2026-06-27 | style(panel): refine button styles with shadow, active scale, toned-down colors |
| e189265 | 2026-06-27 | refactor(panel): compact server card layout, denser grid |
| f4df3e2 | 2026-06-27 | feat(panel): pagination for server lists |
| b512a18 | 2026-06-27 | refactor(panel): adjust margins |
| a49443c | 2026-06-27 | refactor(panel): simplify sidebar |
| 86f2ae4 | 2026-06-28 | feat(panel): sidebar foot shows current account + sign-out; reorder Account cards |
| f5d00f3 | 2026-06-28 | feat(cli): implement felis apply command for direct CRD creation |
| 832b200 | 2026-06-28 | fix(panel): reactive system theme detection |
| c9cd9dc | 2026-06-28 | style(panel): unify dialog animation to fade and scale from center |
| 94a3b7b | 2026-06-28 | fix(deploy): harden bootstrap for RHEL-family Linux |
| 9c46632 | 2026-06-28 | feat(cli): add felis setup first-run console with reclaim protection and cfsetup idempotency |
| 5450c26 | 2026-06-28 | chore: normalize line endings and apply formatting |
| deaa2f8 | 2026-06-28 | feat(deploy): add zypper support for openSUSE/SLES |
| 318a724 | 2026-06-28 | feat(deploy): add pacman support for Arch Linux |
| e5f1682 | 2026-06-29 | refactor(deploy)!: TUI |
| 28c3eee | 2026-06-30 | refactor(deploy): improved TUI walkthrough |
| 116595f | 2026-06-30 | feat(api): add QR scan-login completion poll on the internal face |
| a94b001 | 2026-06-30 | feat(deploy): add break-glass Operator account provisioning |
| 346ec68 | 2026-06-30 | refactor(deploy): improved cloudflare walkthrough |
| 563041a | 2026-06-30 | feat(panel): add fail-closed role-switcher view-mode logic |
| 50b8487 | 2026-06-30 | feat(panel): wire role-switcher into the app shell |
| 75642d9 | 2026-06-30 | feat(metrics): add named felis_* Prometheus collectors |
| 2a93a9e | 2026-06-30 | feat(metrics): record felis_image_build_failures_total on failed builds |
| 79eae7f | 2026-06-30 | feat(metrics): publish felis_servers_total from a fleet snapshot |
| 8ac5e64 | 2026-06-30 | feat(metrics): observe felis_start_duration_seconds across the start lifecycle |
| 676407d | 2026-06-30 | docs(diagrams): align §28 sequence diagrams with implemented routes |
| ac02c69 | 2026-06-30 | docs(troubleshooting): add operator failure-mode checklist |
| eb5875a | 2026-06-30 | feat(felis): add Operator break-glass op behind an operation menu |
| c14ed17 | 2026-06-30 | fix(docker): keep embedded panel/ and deploy/ in the image build context |
| 2a4a81b | 2026-06-30 | fix(api): don't burn wake cooldown when refused at capacity |
| 6c3999a | 2026-06-30 | fix(api): rate-limit email-OTP sends to close the email-bomb vector |
| 9873904 | 2026-06-30 | fix(operator): populate Status.Players from an RCON list probe |
| 879b177 | 2026-06-30 | fix(api): make OTP-start throttle atomic to close concurrent-burst bypass |
| 29f5341 | 2026-07-01 | docs(api): correct cooldownLimiter doc for its OTP reuse |
| 7507cfa | 2026-07-01 | Revert "feat(panel): wire role-switcher into the app shell" |
| f2c916d | 2026-07-01 | feat(api): add passkey enrollment persistence layer |
| 742f15f | 2026-07-01 | feat(api): add passkey enrollment endpoints |
| d2de11a | 2026-07-01 | feat(panel): fleet |
| 0261204 | 2026-07-01 | feat(passkey): add go-webauthn enrollment verifier adapter |
| fce0fce | 2026-07-01 | feat(passkey): wire enrollment verifier into felis-api |
| 2810fe8 | 2026-07-01 | fix(cfsetup): repoint stale DNS record when routing a tunnel hostname |
| 7d3be64 | 2026-07-01 | feat(cfsetup): start the tunnel connector as a setup step |
| a531f5e | 2026-07-01 | fix(cfsetup): keep connector install in the host apply layer only |
| e058a64 | 2026-07-01 | feat(edge): close the panel NodePort to the public after the tunnel is up |
| c01f133 | 2026-07-01 | feat(updates): add pure decision core for component self-update |
| fe2ece0 | 2026-07-01 | feat(api): add public Bind-Code onboarding for the player console |
| 3673af6 | 2026-07-01 | feat(api): add admin API for the SysAdmin-set auto-update maintenance window |
| 7464fa7 | 2026-07-01 | fix(updates): tag Window JSON so the persisted maintenance window round-trips |
| e035142 | 2026-07-01 | feat(passkey): add WebAuthn login/assertion crypto adapter |
| f34711c | 2026-07-01 | docs(api): record passkey login-handler deferral rationale |
| 7a51c1d | 2026-07-01 | fix(api): bound concurrent login bcrypt to shed CPU-pin floods |
| 164ac44 | 2026-07-01 | fix(api): validate inbound X-Request-Id before echo and audit persist |
| c6c0772 | 2026-07-01 | fix(api): set read/idle timeouts on the felis-api listeners |
| 3c1d647 | 2026-07-01 | fix(api): cap concurrent SSE streams per principal |
| d6e3189 | 2026-07-01 | fix(api): bound SSE relay writes with a deadline to sever stalled readers |
| 2c56d17 | 2026-07-01 | docs(api): record the quota-claim TOCTOU as a KNOWN-LIMITATION (audit #4) |
| 8f41a00 | 2026-07-01 | fix(api): clear the SSE write deadline on return so it can't leak to a reused connection |
| 15c58d9 | 2026-07-01 | feat(panel): player management |
| 8ae65ae | 2026-07-02 | feat(panel): backup management |
| a15ff55 | 2026-07-02 | refactor(panel): optimize player list layout and horizontal operations |
| 149f01a | 2026-07-02 | fix(panel): change console button to outline variant on my servers page |
| 4ecaf3c | 2026-07-02 | refactor(panel): set defaultOpen parameter of whitelist card to false |
| a9dbc8b | 2026-07-02 | feat(panel): add search and status filtering to my servers page |
| fa7bab5 | 2026-07-02 | style(panel): refine search and filter layout to align with header |
| 70d17a0 | 2026-07-02 | feat(panel): align my servers page search layout with fleet table |
| 5a8eff1 | 2026-07-02 | feat(panel): remove developer comment footer cards from my servers and server admin pages |
| 92770ea | 2026-07-02 | style(panel): adjust pagination padding to pt-3 for balanced spacing |
| 6e43a46 | 2026-07-02 | fix(panel): pin sidebar navigation and enable independent content scroll |
| 3b4298d | 2026-07-02 | refactor(panel): unify servers cockpit layout, resolve duplicate pages and adjust spacing |
| c0d333b | 2026-07-02 | feat(panel): support full server config edit dialog with status prefilling |
| 6368ab1 | 2026-07-02 | fix(api): coalesce MyServers owned flag so ownerless rows do not 500 |
| cdbb5ab | 2026-07-02 | fix(api): record credential id in passkey-register audit event |
| 9953275 | 2026-07-02 | fix(api): bound webauthn_challenges growth by superseding all prior rows |
| 20e31fb | 2026-07-02 | fix(store): cascade-delete passkeys and challenges on user removal |
| 7278cd7 | 2026-07-02 | feat(passkey): require and record user verification at enrollment |
| 54bc6ef | 2026-07-02 | fix(api): clear bound passkeys on password change to close a takeover foothold |
| 19f500b | 2026-07-02 | style(panel): update destructive red color and rename wake to start |
| e0bc288 | 2026-07-02 | feat(panel): implement image build pipeline and admin whitelist with mock dev api |
| 8594622 | 2026-07-02 | feat(panel): implement email OTP verification and passkey registration management |
| 9bed51b | 2026-07-02 | feat(config): add [velocity] login_image/lobby_image for system servers |
| 9ef817f | 2026-07-02 | feat(naming): system-server names, validation, and service-token identifiers |
| 159107b | 2026-07-02 | feat(api): HTTP readiness knob on MinecraftServer and login-gate fallback default |
| dc23cb5 | 2026-07-02 | feat(operator): system-server pod readiness probe and login service-token env |
| 3fdb3d0 | 2026-07-02 | feat(platform): internal API base-URL helper and single-sourced token secret |
| f554d52 | 2026-07-02 | feat(cli): provision login/lobby system servers with login env and token replica |
| a63f49d | 2026-07-02 | feat(panel): steer WeChat/QQ in-app browsers to the system browser for passkey |
| 241fe21 | 2026-07-02 | feat(limbo): felis-limbo in-game login flow over the shared account-link client |
| c7315e4 | 2026-07-02 | feat(deploy): login-limbo and lobby images with game-port pinning |
| 191640c | 2026-07-02 | feat(panel): implement admin submission approval and reject queue |
| 598f3d3 | 2026-07-02 | feat(submit): local + S3 backends for modpack upload contexts, installer-selectable |
| adf0d99 | 2026-07-02 | feat(panel): implement user-side modpack submissions with drag & drop context upload |
| d9e866f | 2026-07-03 | fix(deploy): make the lobby image actually build |
| b84debf | 2026-07-03 | feat(deploy): one-shot demo bring-up wrapper |
| 73d6ec1 | 2026-07-03 | feat(mock): add mock submissions for owner account |
| 5427bc7 | 2026-07-03 | feat(servers): support claiming servers directly from ServersPage list |
| 55592ed | 2026-07-03 | feat(auth): support public auth bind endpoint |
| 804459c | 2026-07-03 | feat(panel): implement admin maintenance window settings page |
| dd7dff6 | 2026-07-03 | feat(panel): support importing parameters from submission with owner-restricted unapproved entries |
| a8701c1 | 2026-07-03 | fix(panel): prevent automatic wake during server claim in mock api |
| f749c2c | 2026-07-03 | style(panel): resolve double borders and uneven padding in server console |
| 5f402b1 | 2026-07-03 | fix(panel): force dark mode and pure black bg on server console card |
| b7d8000 | 2026-07-03 | feat(panel): implement dedicated LuckPerms permissions and groups management sub-page |
| e60784b | 2026-07-04 | fix(panel): eliminate page collapse and scroll shifts during LuckPerms query reload |
| 439f19e | 2026-07-04 | fix(panel): prevent page collapse and scroll shifts in players and bans management sections during reload |
| 83e57b4 | 2026-07-04 | fix(panel): implement two-step confirmation for claiming a server to prevent accidental operations |
| 3347cc0 | 2026-07-04 | feat(panel): implement user management administration panel with sessions and minecraft link support |
| 67b4e19 | 2026-07-04 | fix(panel): override generic already_exists error message during user creation and profile editing |
| 2c95da8 | 2026-07-04 | fix(panel/i18n): add missing users_col_user key to translation files |
| 627883e | 2026-07-04 | fix(panel): refine reset password messages and fix empty email placeholder in mock api response |
| 0c1cc59 | 2026-07-04 | feat(auth): migrate console login to passwordless |
| 3b43f05 | 2026-07-04 | refactor(api): drop dead login concurrency limiter and reconcile passwordless comments |
| 4f59d51 | 2026-07-04 | feat(auth): add owner-tier passkey-unbind remediation endpoint |
| c20b12c | 2026-07-04 | refactor(api): drop dead password-era ResetMailer, reconcile passkey-unbind docs |
| 0a2accd | 2026-07-04 | chore: stop tracking Autohand-generated AGENTS.md |
| 96b3cc9 | 2026-07-04 | feat(updater): wire updates.Run to a caller with PaperMC v3 release discovery |
| 9896fe1 | 2026-07-05 | docs(updater): correct PaperMC UA/fixture overclaims, re-tier the boundary |
| 7d27640 | 2026-07-05 | feat(updater): add GitHub Releases source and route felis-api/k3s/cloudflared |
| bd49313 | 2026-07-05 | refactor(panel): 抽取 10 个公共组件，消除 ~150 处重复代码 |
| 91bfa27 | 2026-07-05 | feat(operator): implement idle auto-stop (spec §8) |
| e574749 | 2026-07-05 | feat(api): enforce CPU/memory/storage quotas (spec §9.3, §22) |
| 7db57b9 | 2026-07-05 | feat(updater): add VersionGatherer extraction core and CLI gather seam |
| ec468ba | 2026-07-05 | feat(auth): add discoverable (usernameless) passkey login |
| 154002e | 2026-07-05 | docs(auth): cite MultiLogin reference for UUID-keyed reclaim split |
| 0dbd557 | 2026-07-05 | fix(store): renumber discoverable-login migration 0013 -> 0014 |
| 9e1df12 | 2026-07-05 | feat(passkey): advance sign_count, reject clone-warned assertions |
| 7f7e459 | 2026-07-05 | fix(operator): enforce startup and readiness timeouts (§5, §8) |
| 7becb38 | 2026-07-05 | fix(api): implement /readyz with real DB + K8s API + CRD checks (§7) |
| 9079a2c | 2026-07-05 | feat(panel): implement email otp and passkey login interface |
| cfe68ae | 2026-07-05 | fix(panel): align status distribution order to put Stopped at the end |
| bbcfaeb | 2026-07-05 | refactor(panel): remove redundant voxel network topology description subtitle |
| fdb6efb | 2026-07-05 | feat(account): migrate a live account's owned servers to a new account (§B3 inherit) |
| abad137 | 2026-07-06 | style(panel): unify vertical spacing below PageHeader across pages |
| c2ee21a | 2026-07-06 | feat(breakglass): add halt-a-server op to the recovery console (§B4) |
| c1aa38b | 2026-07-06 | feat(velocity): add /felis migrate to open an account migration (§B3 inherit) |
| 7a7c0d5 | 2026-07-07 | feat(api): add on-demand world backup endpoint and Job executor (§B4 Sync) |
| f2fc57c | 2026-07-07 | feat(api): add internal-face break-glass world backup endpoint (§B4 Sync) |
| 2ba9948 | 2026-07-07 | fix(platform): front the felis-api internal face on its own ClusterIP Service |
| fc748d3 | 2026-07-07 | feat(breakglass): add "back up a world now" console peer (§B4 Sync) |
| 9911b8c | 2026-07-07 | docs(changes): record the break-glass backup console peer (§B4 Sync phase 2b) |
| 096d597 | 2026-07-07 | docs(changes): backfill detail docs for pre-ledger functional commits |
| 5a7cd5a | 2026-07-07 | docs(changes): fold 346ec68 cloudflare-edge walkthrough into its detail doc |
| 4626ab5 | 2026-07-07 | docs(changes): mutation-audit the ledger's "unit-tested" safety claims |
