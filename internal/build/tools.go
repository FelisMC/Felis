package build

import "strings"

// Tool is an image or OCI artifact a build Job runs or reads: where it comes
// from upstream, and the repository:tag the platform registry keeps its copy
// under. A build pulls only the copy. The build namespace has no internet egress,
// so Trivy cannot reach the upstream DBs, and the node pulls the executor images
// from the in-cluster registry like any other image, which also survives an image
// GC. `felis mirror-build-tools` (deploy/bootstrap.sh runs it at install and from
// felis-build-tools.timer twice a day) copies each Source to its Mirror.
type Tool struct {
	Name string
	// Source is the upstream reference. The executor images are pinned by the
	// digest of their multi-platform index, so a moved or re-pushed upstream tag
	// cannot change what a build runs; the DBs follow their tag, since a fresh
	// vulnerability DB is the point of refreshing them.
	Source string
	// Mirror is repository:tag inside the platform registry.
	Mirror string
}

// Tools lists every tool a build needs. Kaniko is archived upstream (June 2025)
// and v1.24.0 is its final release; docs/troubleshooting.md §8e covers moving to a
// maintained fork.
var Tools = []Tool{
	{Name: "kaniko", Source: "gcr.io/kaniko-project/executor:v1.24.0@sha256:4e7a52dd1f14872430652bb3b027405b8dfd17c4538751c620ac005741ef9698", Mirror: "mirror/kaniko-executor:v1.24.0"},
	{Name: "trivy", Source: "ghcr.io/aquasecurity/trivy:0.74.0@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969", Mirror: "mirror/trivy:0.74.0"},
	{Name: "trivy-db", Source: "mirror.gcr.io/aquasec/trivy-db:2", Mirror: "mirror/trivy-db:2"},
	{Name: "trivy-java-db", Source: "mirror.gcr.io/aquasec/trivy-java-db:1", Mirror: "mirror/trivy-java-db:1"},
}

// tool returns the named entry of Tools.
func tool(name string) Tool {
	for _, t := range Tools {
		if t.Name == name {
			return t
		}
	}
	panic("build: unknown tool " + name)
}

// toolRef is where a build reads tool name: its copy in registry, or the upstream
// source when there is no platform registry (tests, a bare Config).
func toolRef(registry, name string) string {
	t := tool(name)
	if registry == "" {
		return t.Source
	}
	return strings.TrimSuffix(registry, "/") + "/" + t.Mirror
}

// ToolRefs returns the four references a build of this Config reads, after
// defaults: the kaniko and trivy images and the two Trivy DB repositories. The
// registry pruner keeps each of them.
func (c Config) ToolRefs() []string {
	c = c.withDefaults()
	return []string{c.KanikoImage, c.TrivyImage, c.TrivyDBRepository, c.TrivyJavaDBRepository}
}
