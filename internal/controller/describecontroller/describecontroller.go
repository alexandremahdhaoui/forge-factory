package describecontroller

import (
	"errors"
	"fmt"

	"github.com/alexandremahdhaoui/forge-factory/internal/types/runtimetypes"
)

var (
	ErrVersion  = errors.New("no artifact table for this version")
	ErrPlatform = errors.New("no artifact for this platform")
	ErrTarget   = errors.New("no standard library artifact for this target")
)

func refuse(err error, runtime, version, os, arch string) error {
	return fmt.Errorf(
		"describing %s %s for %s/%s: %w; add the version's published sha256 to the engine's table",
		runtime, version, os, arch, err)
}

func refuseTarget(runtime, version, target string) error {
	return fmt.Errorf(
		"describing %s %s for the target %s: %w; add the target's published sha256 to the engine's table",
		runtime, version, target, ErrTarget)
}

func platformKey(os, arch string) string { return os + "/" + arch }

type Go struct{}

func (Go) Language() string { return "go" }

var goArtifacts = map[string]map[string]runtimetypes.Artifact{
	"1.26.5": {
		"linux/amd64": {
			URL:    "https://proxy.golang.org/golang.org/toolchain/@v/v0.0.1-go1.26.5.linux-amd64.zip",
			SHA256: "fa1f92e41b70c6649bd78b1ab98b940f19adf3e71aeb0bcb5a177bcc25699df5",
			Unpack: "zip", Strip: 2,
		},
		"linux/arm64": {
			URL:    "https://proxy.golang.org/golang.org/toolchain/@v/v0.0.1-go1.26.5.linux-arm64.zip",
			SHA256: "3cfc9c4e6df7487d753692a704ac81e25fc07b0be4cd02d59c2f9884c1d114cf",
			Unpack: "zip", Strip: 2,
		},
	},
}

func (Go) Describe(in runtimetypes.Input) (runtimetypes.Description, error) {
	artifact, err := lookup(goArtifacts, in)
	if err != nil {
		return runtimetypes.Description{}, err
	}

	return runtimetypes.Description{
		Runtime:   in.Runtime,
		Version:   in.Version,
		Artifacts: []runtimetypes.Artifact{artifact},
		Bins:      []string{"bin/go", "bin/gofmt"},
		Prerequisites: []runtimetypes.Prerequisite{{
			Name: "c-compiler", Reason: "cgo (go test -race)", Verify: "cc",
		}},
	}, nil
}

type Rust struct{}

func (Rust) Language() string { return "rust" }

type rustBuild struct {
	url, sha, triple string
}

var rustArtifacts = map[string]map[string]rustBuild{
	"1.97.0": {
		"linux/amd64": {
			url:    "https://static.rust-lang.org/dist/2026-07-09/rust-1.97.0-x86_64-unknown-linux-gnu.tar.gz",
			sha:    "eb89b20287153391c49ebcdb7fd91b683a12438d129bfb92eadcc495545af3a7",
			triple: "x86_64-unknown-linux-gnu",
		},
		"linux/arm64": {
			url:    "https://static.rust-lang.org/dist/2026-07-09/rust-1.97.0-aarch64-unknown-linux-gnu.tar.gz",
			sha:    "70fb01b4894d56fad34c260ce63aee647589be57a26ab33730110d8228cbcf02",
			triple: "aarch64-unknown-linux-gnu",
		},
	},
}

var rustStdArtifacts = map[string]map[string]rustBuild{
	"1.97.0": {
		"x86_64-unknown-linux-gnu": {
			url:    "https://static.rust-lang.org/dist/2026-07-09/rust-std-1.97.0-x86_64-unknown-linux-gnu.tar.gz",
			sha:    "26abd06b9c4221811af1baec86dfb6de9535862fc853b85388dcc314c96cea6d",
			triple: "x86_64-unknown-linux-gnu",
		},
		"aarch64-unknown-linux-gnu": {
			url:    "https://static.rust-lang.org/dist/2026-07-09/rust-std-1.97.0-aarch64-unknown-linux-gnu.tar.gz",
			sha:    "ae6c76b70be4768ecf2f320f1e6fa28525730b7fd7d6cfc822a3a23e0c07fcc2",
			triple: "aarch64-unknown-linux-gnu",
		},
		"x86_64-pc-windows-gnu": {
			url:    "https://static.rust-lang.org/dist/2026-07-09/rust-std-1.97.0-x86_64-pc-windows-gnu.tar.gz",
			sha:    "7ef19232b379616509007c7a8db45c4ee7031c22d9614fa2b7f18b2a30ff85fa",
			triple: "x86_64-pc-windows-gnu",
		},
	},
}

func (Rust) Describe(in runtimetypes.Input) (runtimetypes.Description, error) {
	byPlatform, ok := rustArtifacts[in.Version]
	if !ok {
		return runtimetypes.Description{}, refuse(ErrVersion, in.Runtime, in.Version, in.OS, in.Arch)
	}

	build, ok := byPlatform[platformKey(in.OS, in.Arch)]
	if !ok {
		return runtimetypes.Description{}, refuse(ErrPlatform, in.Runtime, in.Version, in.OS, in.Arch)
	}

	artifacts := []runtimetypes.Artifact{{
		URL: build.url, SHA256: build.sha, Unpack: "tar-gz", Strip: 1,
		Picks: []runtimetypes.Pick{
			{From: "rustc"},
			{From: "cargo"},
			{From: "rust-std-" + build.triple},
			{From: "rustfmt-preview"},
			{From: "clippy-preview"},
		},
	}}

	for _, target := range in.Targets {
		if target == build.triple {
			continue
		}

		std, ok := rustStdArtifacts[in.Version][target]
		if !ok {
			return runtimetypes.Description{}, refuseTarget(in.Runtime, in.Version, target)
		}

		artifacts = append(artifacts, runtimetypes.Artifact{
			URL: std.url, SHA256: std.sha, Unpack: "tar-gz", Strip: 1,
			Picks: []runtimetypes.Pick{{From: "rust-std-" + std.triple}},
		})
	}

	return runtimetypes.Description{
		Runtime:   in.Runtime,
		Version:   in.Version,
		Artifacts: artifacts,
		Bins: []string{
			"bin/rustc", "bin/cargo", "bin/rustdoc",
			"bin/cargo-fmt", "bin/rustfmt",
			"bin/cargo-clippy", "bin/clippy-driver",
		},
		Prerequisites: []runtimetypes.Prerequisite{{
			Name: "c-compiler", Reason: "cc is the default linker driver", Verify: "cc",
		}},
	}, nil
}

type Python struct{}

func (Python) Language() string { return "python" }

var pythonArtifacts = map[string]map[string]runtimetypes.Artifact{
	"3.13.7": {
		"linux/amd64": {
			URL:    "https://github.com/astral-sh/python-build-standalone/releases/download/20250902/cpython-3.13.7%2B20250902-x86_64-unknown-linux-gnu-install_only_stripped.tar.gz",
			SHA256: "6a8280f4b08d75428eea83955678c51da00c585bb411562cd53f510680becf00",
			Unpack: "tar-gz", Strip: 1,
		},
		"linux/arm64": {
			URL:    "https://github.com/astral-sh/python-build-standalone/releases/download/20250902/cpython-3.13.7%2B20250902-aarch64-unknown-linux-gnu-install_only_stripped.tar.gz",
			SHA256: "e9dbfda219752abb36f9023d11fc7a4d66dda70a52962817f72db06b32b6bcbd",
			Unpack: "tar-gz", Strip: 1,
		},
	},
}

func (Python) Describe(in runtimetypes.Input) (runtimetypes.Description, error) {
	artifact, err := lookup(pythonArtifacts, in)
	if err != nil {
		return runtimetypes.Description{}, err
	}

	return runtimetypes.Description{
		Runtime:   in.Runtime,
		Version:   in.Version,
		Artifacts: []runtimetypes.Artifact{artifact},
		Bins:      []string{"bin/python3", "bin/python3.13", "bin/pip3"},
	}, nil
}

type TypeScript struct{}

func (TypeScript) Language() string { return "typescript" }

var nodeArtifacts = map[string]map[string]runtimetypes.Artifact{
	"22.22.2": {
		"linux/amd64": {
			URL:    "https://nodejs.org/dist/v22.22.2/node-v22.22.2-linux-x64.tar.gz",
			SHA256: "978978a635eef872fa68beae09f0aad0bbbae6757e444da80b570964a97e62a3",
			Unpack: "tar-gz", Strip: 1,
		},
		"linux/arm64": {
			URL:    "https://nodejs.org/dist/v22.22.2/node-v22.22.2-linux-arm64.tar.gz",
			SHA256: "b2f3a96f31486bfc365192ad65ced14833ad2a3c2e1bcefec4846902f264fa28",
			Unpack: "tar-gz", Strip: 1,
		},
	},
}

func (TypeScript) Describe(in runtimetypes.Input) (runtimetypes.Description, error) {
	artifact, err := lookup(nodeArtifacts, in)
	if err != nil {
		return runtimetypes.Description{}, err
	}

	return runtimetypes.Description{
		Runtime:   in.Runtime,
		Version:   in.Version,
		Artifacts: []runtimetypes.Artifact{artifact},
		Bins:      []string{"bin/node"},
	}, nil
}

func lookup(table map[string]map[string]runtimetypes.Artifact, in runtimetypes.Input) (runtimetypes.Artifact, error) {
	byPlatform, ok := table[in.Version]
	if !ok {
		return runtimetypes.Artifact{}, refuse(ErrVersion, in.Runtime, in.Version, in.OS, in.Arch)
	}

	artifact, ok := byPlatform[platformKey(in.OS, in.Arch)]
	if !ok {
		return runtimetypes.Artifact{}, refuse(ErrPlatform, in.Runtime, in.Version, in.OS, in.Arch)
	}

	return artifact, nil
}
