package build

import (
	"strconv"
	"strings"
)

// Build Jobs have no internet egress; mirror every required tool into the platform registry.
type Tool struct {
	Name string
	// Source is the upstream reference; executor images are pinned by index digest, the DBs follow their tag.
	Source string
	Mirror string // repository:tag inside the platform registry
}

var Tools = []Tool{
	{Name: "kaniko", Source: "gcr.io/kaniko-project/executor:v1.24.0@sha256:4e7a52dd1f14872430652bb3b027405b8dfd17c4538751c620ac005741ef9698", Mirror: "mirror/kaniko-executor:v1.24.0"},
	{Name: "trivy", Source: "ghcr.io/aquasecurity/trivy:0.74.0@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969", Mirror: "mirror/trivy:0.74.0"},
	{Name: "trivy-db", Source: "mirror.gcr.io/aquasec/trivy-db:2", Mirror: "mirror/trivy-db:2"},
	{Name: "trivy-java-db", Source: "mirror.gcr.io/aquasec/trivy-java-db:1", Mirror: "mirror/trivy-java-db:1"},
}

func tool(name string) Tool {
	for _, t := range Tools {
		if t.Name == name {
			return t
		}
	}
	panic("build: unknown tool " + name)
}

// toolRef is where a build reads tool name: its registry mirror, or the upstream source when registry is empty.
func toolRef(registry, name string) string {
	t := tool(name)
	if registry == "" {
		return t.Source
	}
	return strings.TrimSuffix(registry, "/") + "/" + t.Mirror
}

// ToolRefs returns the kaniko and trivy images and the two Trivy DB repositories a build reads, after defaults; the registry pruner keeps them.
func (c Config) ToolRefs() []string {
	c = c.withDefaults()
	return []string{c.KanikoImage, c.TrivyImage, c.TrivyDBRepository, c.TrivyJavaDBRepository}
}

// Mirror digest-pinned Java runtimes alongside tools; protect mirror repositories from pruning.
var RuntimeImages = []Tool{
	{Name: "runtime-java8", Source: "docker.io/library/eclipse-temurin:8-jre@sha256:5c9a04052cf8002b19d79c52986e93ec5f5222ecb1cdf87e309d1f96ef712afc", Mirror: "mirror/eclipse-temurin:8-jre"},
	{Name: "runtime-java17", Source: "docker.io/library/eclipse-temurin:17-jre@sha256:a9b54b51aaa867507be76277a98c6b11b99515cac6330891b534d070f607214c", Mirror: "mirror/eclipse-temurin:17-jre"},
	{Name: "runtime-java21", Source: "docker.io/library/eclipse-temurin:21-jre@sha256:ccbad9e7cb84c27092b12c40a4143e5733e050db9c3784d84f632dcf423fb2ef", Mirror: "mirror/eclipse-temurin:21-jre"},
	{Name: "runtime-java25", Source: "docker.io/library/eclipse-temurin:25-jre@sha256:fd4b1c02c64eef378212e23656fc0b0caa41a357876fa0b60e51ceda94a3db64", Mirror: "mirror/eclipse-temurin:25-jre"},
}

// RuntimeImageRef is the platform registry reference of the runtime image for a Java major version, or "" if none.
func RuntimeImageRef(registry string, java int) string {
	name := "runtime-java" + strconv.Itoa(java)
	for _, t := range RuntimeImages {
		if t.Name == name {
			return strings.TrimSuffix(registry, "/") + "/" + t.Mirror
		}
	}
	return ""
}
