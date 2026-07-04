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
//     v3 parser, the routing source, and the Runner's report-only composition. The
//     PaperMC fixture is captured from the live endpoint, and the live call was
//     exercised out-of-band on 2026-07-04 (curl fill.papermc.io/v3/projects/velocity):
//     the response shape matches the fixture and 3.4.0 is confirmed the newest stable
//     (3.5.0-SNAPSHOT correctly filtered). These prove the parse/plan/compose LOGIC
//     and that the core is now wired to a caller.
//
//   - NOT YET BUILT, but VERIFIABLE HERE (same technique as PaperMC — HTTP GET, JSON
//     decode, tolerant Parse, prerelease filter, all httptest-testable): the GitHub
//     Releases source. It is why 3 of the 4 components (felis-api, k3s, cloudflared)
//     currently report "latest unknown" — RoutingSource returns errGitHubNotWired for
//     them. This is the next VERIFIABLE slice, not integration remainder; until it
//     exists the verifiable release-source work is only ~half done.
//
//   - CAVEATS on what the tests do NOT prove: they run against httptest, not the live
//     host, so future upstream shape drift is not caught; and while Felis sends a
//     descriptive User-Agent (PaperMC etiquette), upstream UA enforcement was not
//     active on the project endpoint on 2026-07-04 (a bare UA got HTTP 200), so the UA
//     is defensive, not load-bearing.
//
//   - REMAINING INTEGRATION (pure I/O, no verifiable-here logic): the concrete
//     VersionGatherer (`k3s --version`, image-tag / jar inspection), the concrete
//     Notifier (SMTP + in-game) and Applier (control-plane image bump, cloudflared
//     swap), the `felis update` CLI + CronJob entry point, and the runtime append of
//     the live Pinned Minecraft fleet.
package updater
