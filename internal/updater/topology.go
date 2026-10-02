package updater

import "felis.lolicon.best/internal/updates"

// sourceKind is how a component's latest upstream version is discovered.
type sourceKind int

const (
	sourceNone     sourceKind = iota // pinned components are never queried
	sourceGitHub                     // GitHub Releases (Coord = "owner/repo")
	sourcePaperMC                    // PaperMC Fill v3 (Coord = project id)
	sourceTemurin                    // adoptium/temurin<feature>-binaries, feature from Current
	sourcePostgres                   // postgresql.org/versions.json, within Current's major
)

// officialRepo is Felis's own GitHub repository, which is public.
const officialRepo = "FelisMC/Felis"

// Spec is one platform component's static update policy plus how to find its latest
// upstream version. Current is deliberately NOT here — it is gathered at runtime
// (integration: an image tag, `k3s --version`, a jar manifest) and combined with the
// Spec to form an updates.Component. The topology is the pure, testable expression of
// the user's stated decisions: what Felis keeps current, and how aggressively.
type Spec struct {
	Name       string
	Policy     updates.Policy
	Manageable bool
	Source     sourceKind
	// Coord is the source-specific coordinate: "owner/repo" for GitHub, the project
	// id for PaperMC, empty for pinned components.
	Coord string
}

// Topology returns the fixed platform components Felis tracks, each with the update
// policy the user set. The two user red lines shape every entry: "不要强制自动更新"
// (nothing is force-upgraded — the strongest policy is Scheduled, gated on a
// SysAdmin window) and "能不动的就别动" (Minecraft is always pinned).
//
//   - felis-api   — the control plane Felis ships. Felis MANAGES it (image bump +
//     rollout), so Scheduled: applied only inside a SysAdmin-set window, else notify.
//   - k3s         — the single node the whole platform runs on. Upgrading it is
//     high-blast-radius, so Notify only and NOT manageable: Felis reads its latest to
//     tell the SysAdmin but never applies it; a human drives the node upgrade.
//   - cloudflared — the edge tunnel binary + service Felis manages, so Scheduled.
//   - velocity    — the proxy, but off-cluster on an admin-operated macvlan host, so
//     NOT manageable: even under a schedule it can only ever be Notify. Its releases
//     come from PaperMC (Fill v3), not GitHub.
//   - jre         — the Temurin runtime Velocity runs on. The installer pins its patch
//     build, so a newer build reaches a host through a Felis release: Notify only.
//   - postgresql  — the control-plane database, the felis-postgres Deployment running
//     the image the installer pins by digest. Notify only: a newer minor reaches a
//     host through a Felis release that moves the pin, a major one through a dump and
//     restore.
//
// Minecraft is deliberately ABSENT: every MC server is Pinned and is appended to the
// plan at runtime from the live fleet (integration), never force-tracked here.
// Keeping Minecraft out of the static topology is the code-level expression of the
// pin — there is no policy path by which Topology can propose changing it.
func Topology() []Spec {
	return []Spec{
		// Felis's own release repo, and the same slug deploy/bootstrap.sh clones from
		// (FELIS_REPO_URL). It is public, like k3s and cloudflared; the github source's
		// optional token is for a build whose coord names a private fork.
		{Name: "felis-api", Policy: updates.PolicyScheduled, Manageable: true, Source: sourceGitHub, Coord: officialRepo},
		{Name: "k3s", Policy: updates.PolicyNotify, Manageable: false, Source: sourceGitHub, Coord: "k3s-io/k3s"},
		{Name: "cloudflared", Policy: updates.PolicyScheduled, Manageable: true, Source: sourceGitHub, Coord: "cloudflare/cloudflared"},
		{Name: "velocity", Policy: updates.PolicyNotify, Manageable: false, Source: sourcePaperMC, Coord: "velocity"},
		{Name: "jre", Policy: updates.PolicyNotify, Manageable: false, Source: sourceTemurin},
		{Name: "postgresql", Policy: updates.PolicyNotify, Manageable: false, Source: sourcePostgres},
	}
}
