package describecontroller

import "github.com/alexandremahdhaoui/forge-factory/internal/types/runtimetypes"

// Archive describes runtimes that are plain archives or single binaries
// with no language engine of their own - a JRE, a standalone package
// manager, a generator jar. One generic provider, registered under one
// alias per runtime, so adding such a runtime is a table row rather than an
// engine.
type Archive struct{}

// archiveEntry is one (runtime, version) the provider knows: its artifacts
// per platform plus what it exposes. The "any" platform key serves an
// architecture-independent artifact, like a jar.
type archiveEntry struct {
	artifacts map[string]runtimetypes.Artifact
	bins      []string
	env       map[string]string
	provides  []string
	prereqs   []runtimetypes.Prerequisite
}

var archiveTable = map[string]map[string]archiveEntry{
	"jre": {
		"21.0.5+11": {
			artifacts: map[string]runtimetypes.Artifact{
				"linux/amd64": {
					URL:    "https://github.com/adoptium/temurin21-binaries/releases/download/jdk-21.0.5%2B11/OpenJDK21U-jre_x64_linux_hotspot_21.0.5_11.tar.gz",
					SHA256: "553dda64b3b1c3c16f8afe402377ffebe64fb4a1721a46ed426a91fd18185e62",
					Unpack: "tar-gz", Strip: 1,
				},
				"linux/arm64": {
					URL:    "https://github.com/adoptium/temurin21-binaries/releases/download/jdk-21.0.5%2B11/OpenJDK21U-jre_aarch64_linux_hotspot_21.0.5_11.tar.gz",
					SHA256: "e4d02c33aeaf8e1148c1c505e129a709c5bc1889e855d4fb4f001b1780db42b4",
					Unpack: "tar-gz", Strip: 1,
				},
			},
			bins:     []string{"bin/java"},
			env:      map[string]string{"JAVA_HOME": "{prefix}"},
			provides: []string{"jre"},
		},
	},
	"pnpm": {
		"10.33.0": {
			artifacts: map[string]runtimetypes.Artifact{
				"linux/amd64": {
					URL:    "https://github.com/pnpm/pnpm/releases/download/v10.33.0/pnpm-linux-x64",
					SHA256: "8d4e8f7d778e8ac482022e2577011706a872542f6f6f233e795a4d9f978ea8b5",
					Unpack: "file",
					// A file artifact with an at-only pick is renamed: the
					// release asset is pnpm-linux-x64, the binary is pnpm.
					Picks: []runtimetypes.Pick{{At: "pnpm"}},
				},
				"linux/arm64": {
					URL:    "https://github.com/pnpm/pnpm/releases/download/v10.33.0/pnpm-linux-arm64",
					SHA256: "06755ad2817548b84317d857d5c8003dc6e9e28416a3ea7467256c49ab400d48",
					Unpack: "file",
					Picks:  []runtimetypes.Pick{{At: "pnpm"}},
				},
			},
			bins: []string{"pnpm"},
		},
	},
	"uv": {
		"0.8.17": {
			artifacts: map[string]runtimetypes.Artifact{
				"linux/amd64": {
					URL:    "https://github.com/astral-sh/uv/releases/download/0.8.17/uv-x86_64-unknown-linux-gnu.tar.gz",
					SHA256: "920cbcaad514cc185634f6f0dcd71df5e8f4ee4456d440a22e0f8c0f142a8203",
					Unpack: "tar-gz", Strip: 1,
				},
				"linux/arm64": {
					URL:    "https://github.com/astral-sh/uv/releases/download/0.8.17/uv-aarch64-unknown-linux-gnu.tar.gz",
					SHA256: "9a20d65b110770bbaa2ee89ed76eb963d8c6a480b9ebef584ea9df2ae85b4f0f",
					Unpack: "tar-gz", Strip: 1,
				},
			},
			bins: []string{"uv", "uvx"},
		},
	},
	"bun": {
		"1.3.14": {
			artifacts: map[string]runtimetypes.Artifact{
				"linux/amd64": {
					URL:    "https://github.com/oven-sh/bun/releases/download/bun-v1.3.14/bun-linux-x64.zip",
					SHA256: "951ee2aee855f08595aeec6225226a298d3fea83a3dcd6465c09cbccdf7e848f",
					Unpack: "zip", Strip: 1,
				},
				"linux/arm64": {
					URL:    "https://github.com/oven-sh/bun/releases/download/bun-v1.3.14/bun-linux-aarch64.zip",
					SHA256: "a27ffb63a8310375836e0d6f668ae17fa8d8d18b88c37c821c65331973a19a3b",
					Unpack: "zip", Strip: 1,
				},
			},
			bins: []string{"bun"},
		},
	},
	"openapi-generator": {
		"7.19.0": {
			artifacts: map[string]runtimetypes.Artifact{
				"any": {
					URL:    "https://repo1.maven.org/maven2/org/openapitools/openapi-generator-cli/7.19.0/openapi-generator-cli-7.19.0.jar",
					SHA256: "3d8140c691410e0004b1bb9b1e431c1293734830f30d6d5922f8e5dbf2e42e19",
					Unpack: "file",
				},
			},
			env: map[string]string{
				"OPENAPI_GENERATOR_JAR": "{prefix}/openapi-generator-cli-7.19.0.jar",
			},
			prereqs: []runtimetypes.Prerequisite{{
				Name: "jre", Reason: "a jar runs on a JVM", Verify: "java",
			}},
		},
	},
}

// Describe answers the table's entry for one build. A file artifact with no
// picks lands under the prefix named after the URL's base name, which is
// what the env template above points at.
func (Archive) Describe(in runtimetypes.Input) (runtimetypes.Description, error) {
	versions, ok := archiveTable[in.Runtime]
	if !ok {
		return runtimetypes.Description{}, refuse(ErrVersion, in.Runtime, in.Version, in.OS, in.Arch)
	}

	entry, ok := versions[in.Version]
	if !ok {
		return runtimetypes.Description{}, refuse(ErrVersion, in.Runtime, in.Version, in.OS, in.Arch)
	}

	artifact, ok := entry.artifacts[platformKey(in.OS, in.Arch)]
	if !ok {
		artifact, ok = entry.artifacts["any"]
	}

	if !ok {
		return runtimetypes.Description{}, refuse(ErrPlatform, in.Runtime, in.Version, in.OS, in.Arch)
	}

	return runtimetypes.Description{
		Runtime:       in.Runtime,
		Version:       in.Version,
		Artifacts:     []runtimetypes.Artifact{artifact},
		Bins:          entry.bins,
		Env:           entry.env,
		Provides:      entry.provides,
		Prerequisites: entry.prereqs,
	}, nil
}
