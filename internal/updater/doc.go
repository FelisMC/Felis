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
//   - BUILT + UNIT-VERIFIED (Go tests, WSL oracle): the topology, the PaperMC Fill
//     v3 parser (its fixture is captured from the REAL live response shape on
//     2026-07-04 — a grounded contract test, not a self-referential one), the
//     routing source, and the Runner's report-only composition. These prove the
//     parse/plan/compose LOGIC and that the core is now wired to a caller.
//
//   - WRITTEN, NOT LIVE-VERIFIED: the tests assert the required non-generic
//     User-Agent is transmitted, but real fill.papermc.io network/TLS/UA-enforcement
//     is not exercised here; the fixture proves today's shape, not its future
//     stability.
//
//   - REMAINING INTEGRATION (not built here): the GitHub Releases source (felis-api,
//     k3s, cloudflared — RoutingSource returns errGitHubNotWired for them today), the
//     concrete VersionGatherer (`k3s --version`, image-tag / jar inspection), the
//     concrete Notifier (SMTP + in-game) and Applier (control-plane image bump,
//     cloudflared swap), the `felis update` CLI + CronJob entry point, and the
//     runtime append of the live Pinned Minecraft fleet.
package updater
