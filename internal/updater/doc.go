// Package updater is the integration/caller side of the component self-update
// subsystem — it gives the pure decision core in internal/updates its first real
// caller. internal/updates declares the seams (ReleaseSource / Notifier / Applier)
// and orchestrates them (updates.Run) but performs no I/O; this package supplies the
// concrete wiring: the platform topology (which components Felis tracks and under
// what policy), the upstream release sources, and the Runner that gathers current
// versions, runs the plan against a maintenance window, and renders the "版本号状态"
// report.
//
// Verification boundary — stated honestly so a green test suite is not mistaken for
// "the updater works against real infra":
//
//   - BUILT + UNIT-VERIFIED (Go tests, WSL oracle): the topology, BOTH release
//     sources (PaperMC Fill v3 for Velocity; GitHub Releases for felis-api, k3s and
//     cloudflared), the routing source, and the Runner's report-only composition. Each
//     source's parser is a contract test whose fixture is captured from — and whose
//     live call was exercised out-of-band against — the real endpoint: PaperMC on
//     2026-07-04 (3.4.0 is newest stable, 3.5.0-SNAPSHOT filtered), GitHub on
//     2026-07-05 (cloudflared 2026.6.1; k3s v1.36.2+k3s1, its "+k3s1"/"v" tolerated and
//     its "-rcN"/prerelease builds rejected). These prove the parse/plan/compose LOGIC
//     and that the core is wired to a caller for every tracked component.
//
//   - CAVEATS on what the tests do NOT prove: they run against httptest, not the live
//     hosts, so future upstream shape drift is not caught. On User-Agent the two APIs
//     differ and the code reflects it: GitHub ENFORCES a UA (a bare request is 403'd,
//     verified 2026-07-05) so Felis's UA is load-bearing there; PaperMC does NOT
//     enforce (a bare request got HTTP 200 on 2026-07-04) so its UA is only etiquette.
//     Also: felis-api's topology coord is now the real repository slug, not a placeholder,
//     so that component resolves at runtime like the others — but only with a credential.
//     The repo is private, so an unauthenticated poll gets GitHub's 404-for-hidden-repo and
//     degrades to "latest unknown"; FELIS_GITHUB_TOKEN is what lights it up. k3s and
//     cloudflared are public and need none.
//
//   - ALSO BUILT + UNIT-VERIFIED: the VersionGatherer's extraction core and dispatch.
//     Three pure extractors turn raw system text into a Version — a `--version` banner
//     (k3s, cloudflared), a container image tag (felis-api), a proxy jar filename
//     (velocity) — and sysGatherer routes each component to the right one over an
//     injected seam, all exercised with a fake runner (gatherer_test.go). The
//     load-bearing case is proven: k3s's registry tag "v1.36.2-k3s1" is repaired to the
//     "+k3s1" build form the binary reports (a Docker tag cannot hold '+'), so an image
//     read and a CLI read agree instead of the image masquerading as a prerelease. The
//     CLI seam (execRunner) is wired for real; only its exec I/O is un-verified here.
//
//   - REMAINING INTEGRATION (genuinely I/O-bound — needs a node/cluster/mailbox): the
//     two current-version PRODUCING seams the gatherer still lacks — the k8s read of the
//     control-plane Deployment's image (felis-api) and the off-cluster Velocity jar
//     inspection, both left nil so those components surface a gather error rather than a
//     wrong version — plus the concrete Notifier (SMTP + in-game) and Applier
//     (control-plane image bump, cloudflared swap), the `felis update` CLI + CronJob
//     entry point, and the runtime append of the live Pinned Minecraft fleet.
package updater
