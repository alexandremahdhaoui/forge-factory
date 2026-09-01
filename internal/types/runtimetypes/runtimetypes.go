// Package runtimetypes is the internal shape of a runtime description: what
// a provider engine answers when the factory declares a runtime. It mirrors
// the wire types in api/factory.v1.yaml and is mapped at each engine's
// boundary, so the generated types never reach a controller.
package runtimetypes

// Input identifies the one runtime build being described. OS and Arch use
// Go's naming (linux, darwin; amd64, arm64), which every engine in this
// fleet already speaks.
type Input struct {
	Runtime string
	Version string
	OS      string
	Arch    string

	// Params carries extra keys from the runtime's factory entry, verbatim.
	// A generic provider parameterizes on them; a language provider may
	// ignore them.
	Params map[string]string
}

// Pick lays the contents of one archive subtree at a path under the install
// prefix. Rust's component tarballs merge rustc/, cargo/ and rust-std-*/
// into one prefix this way - what its install script does imperatively,
// expressed as data.
type Pick struct {
	// From is the subtree inside the archive, after Strip.
	From string
	// At is the destination under the prefix. Empty is the root.
	At string
}

// Artifact is one archive to fetch and lay out. The sha256 is the whole
// trust model: however the bytes arrive - upstream, a mirror, a seeded
// cache - they must hash to it.
type Artifact struct {
	URL    string
	SHA256 string
	// Unpack is one of tar, tar-gz, zip, file. file is a single executable
	// written as-is.
	Unpack string
	// Strip drops leading path components while unpacking.
	Strip int
	// Picks lays out subtrees; empty lays the whole tree at the prefix root.
	Picks []Pick
}

// Prerequisite is something the runtime assumes about the machine and
// cannot carry itself. It is verified and named, never guessed, and never
// installed into the host.
type Prerequisite struct {
	// Name is a capability, matched against other runtimes' Provides.
	Name string
	// Reason names the feature that needs it, for the human reading a
	// refusal.
	Reason string
	// Verify is an executable whose presence on PATH satisfies the check
	// when no declared runtime provides the capability. Empty means only a
	// providing runtime can.
	Verify string
}

// Description is a provider's whole answer: what to fetch, how it lays
// out, what it exposes, what it assumes. Pure data - printable, diffable,
// recordable.
type Description struct {
	Runtime   string
	Version   string
	Artifacts []Artifact
	// Bins are prefix-relative executables to expose on .forge/bin.
	Bins []string
	// Env is composed into the managed .envrc block; the literal token
	// {prefix} in a value is replaced with the store path at expose time.
	Env           map[string]string
	Prerequisites []Prerequisite
	// Provides are capability names this runtime satisfies for other
	// runtimes' prerequisites.
	Provides []string
}
