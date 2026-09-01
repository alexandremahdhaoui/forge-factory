// Package describecontroller answers runtime descriptions for the language
// engines: where a runtime's bytes live, what they hash to, how they lay
// out, what they expose and what they assume. Everything here is data - a
// describer touches neither network nor disk.
//
// Versions are embedded per describer, keyed (version, os, arch), because a
// description must be deterministic and offline: the engine either knows a
// version's artifacts - url and sha256 taken from the distribution's own
// published checksums - or refuses naming the fix. A version bump is a code
// change here, reviewed like any other pin.
package describecontroller

import (
	"errors"
	"fmt"

	"github.com/alexandremahdhaoui/forge-factory/internal/types/runtimetypes"
)

var (
	// ErrVersion means the describer does not know this version's artifacts.
	ErrVersion = errors.New("no artifact table for this version")
	// ErrPlatform means the version is known and this os/arch is not.
	ErrPlatform = errors.New("no artifact for this platform")
)

// refuse builds the loud refusal every describer shares: it names the
// runtime, the version, the platform and where the table lives, so the
// reader knows the fix is a reviewed pin rather than a retry.
func refuse(err error, runtime, version, os, arch string) error {
	return fmt.Errorf(
		"describing %s %s for %s/%s: %w; add the version's published sha256 to the engine's table",
		runtime, version, os, arch, err)
}

// platformKey is how the tables index one build.
func platformKey(os, arch string) string { return os + "/" + arch }

// Go describes the Go toolchain. The artifacts are Go's own module-proxy
// toolchain distribution - the channel `go` itself downloads through - whose
// zips nest as golang.org/toolchain@<v>/..., hence strip 2.
type Go struct{}

// Language answers the runtime name this describer serves.
func (Go) Language() string { return "go" }

// goArtifacts is (version -> platform -> artifact).
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

// Describe answers Go's description for one build.
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
			// -race needs cgo and cgo needs a C toolchain. The language
			// fact lives here, with the language.
			Name: "c-compiler", Reason: "cgo (go test -race)", Verify: "cc",
		}},
	}, nil
}

// Rust describes the Rust toolchain. The combined dist tarball holds
// components - rustc/, cargo/, rust-std-<triple>/, rustfmt-preview/,
// clippy-preview/ - that its install script merges into one prefix; the
// picks express that merge as data. rustfmt and clippy ride along because
// cargo resolves `cargo fmt` and `cargo clippy` as cargo-fmt and
// cargo-clippy on PATH, and a toolchain that cannot format or lint fails
// the stages that call them. rustup is only a downloader of these same
// tarballs, so nothing here needs it.
type Rust struct{}

// Language answers the runtime name this describer serves.
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

// Describe answers Rust's description for one build.
func (Rust) Describe(in runtimetypes.Input) (runtimetypes.Description, error) {
	byPlatform, ok := rustArtifacts[in.Version]
	if !ok {
		return runtimetypes.Description{}, refuse(ErrVersion, in.Runtime, in.Version, in.OS, in.Arch)
	}

	build, ok := byPlatform[platformKey(in.OS, in.Arch)]
	if !ok {
		return runtimetypes.Description{}, refuse(ErrPlatform, in.Runtime, in.Version, in.OS, in.Arch)
	}

	return runtimetypes.Description{
		Runtime: in.Runtime,
		Version: in.Version,
		Artifacts: []runtimetypes.Artifact{{
			URL: build.url, SHA256: build.sha, Unpack: "tar-gz", Strip: 1,
			Picks: []runtimetypes.Pick{
				{From: "rustc"},
				{From: "cargo"},
				{From: "rust-std-" + build.triple},
				{From: "rustfmt-preview"},
				{From: "clippy-preview"},
			},
		}},
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

// Python describes CPython from python-build-standalone - the relocatable
// builds uv itself installs - so the tree runs from wherever it is laid.
type Python struct{}

// Language answers the runtime name this describer serves.
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

// Describe answers Python's description for one build.
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

// TypeScript describes the Node runtime, which is what the typescript
// members exec. The package manager is its own runtime entry - pnpm ships a
// standalone binary - so different managers are more entries, not special
// cases.
type TypeScript struct{}

// Language answers the runtime name this describer serves.
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

// Describe answers the Node description for one build. Only node itself is
// exposed: npm's bin is a relative symlink into lib/ that breaks when
// linked from elsewhere, and this fleet's manager is pnpm, declared as its
// own runtime.
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

// lookup resolves the common (version -> platform -> artifact) table shape.
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
