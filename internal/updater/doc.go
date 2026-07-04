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
//     Also: felis-api's topology coord "felis/felis" is a PLACEHOLDER slug — the GitHub
//     routing/parse logic is verified, but that one component stays dark at runtime (its
//     Latest errors, degrading to "latest unknown") until a real repository is configured.
//
//   - NOT YET BUILT, but VERIFIABLE HERE (the next slice): the VersionGatherer's
//     extraction core — command output (`k3s --version`), image tag
//     (`rancher/k3s:v1.36.2-k3s1`) or jar filename → Version. That is logic over an
//     exec/read seam, testable with a fake runner à la internal/reaper's ExecRunner,
//     and load-bearing: a mis-read current version makes every plan wrong (spurious
//     applies or missed upgrades). Only the seam's actual I/O is un-verifiable here.
//
//   - REMAINING INTEGRATION (genuinely I/O-bound — needs a cluster/mailbox to exercise):
//     the concrete Notifier (SMTP + in-game) and Applier (control-plane image bump,
//     cloudflared swap), the `felis update` CLI + CronJob entry point, and the runtime
//     append of the live Pinned Minecraft fleet.
package updater
