package updater

import "felis.lolicon.best/internal/updates"

type sourceKind int

const (
	sourceNone sourceKind = iota
	sourceGitHub
	sourcePaperMC
	sourceTemurin
	sourcePostgres
)

const officialRepo = "FelisMC/Felis"

type Spec struct {
	Name       string
	Policy     updates.Policy
	Manageable bool
	Source     sourceKind
	Coord      string
}

// Topology excludes Minecraft server versions, which remain individually pinned and cannot be proposed for automatic change.
func Topology() []Spec {
	return []Spec{
		{Name: "felis-api", Policy: updates.PolicyScheduled, Manageable: true, Source: sourceGitHub, Coord: officialRepo},
		{Name: "k3s", Policy: updates.PolicyNotify, Manageable: false, Source: sourceGitHub, Coord: "k3s-io/k3s"},
		{Name: "cloudflared", Policy: updates.PolicyScheduled, Manageable: true, Source: sourceGitHub, Coord: "cloudflare/cloudflared"},
		{Name: "velocity", Policy: updates.PolicyNotify, Manageable: false, Source: sourcePaperMC, Coord: "velocity"},
		{Name: "jre", Policy: updates.PolicyNotify, Manageable: false, Source: sourceTemurin},
		{Name: "postgresql", Policy: updates.PolicyNotify, Manageable: false, Source: sourcePostgres},
	}
}
