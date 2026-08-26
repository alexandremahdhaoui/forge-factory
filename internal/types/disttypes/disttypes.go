// Package disttypes is the distribution index: the manifest that maps one
// proven revision to the concrete tool binaries it shipped with. The
// pipeline writes it (as a release asset and a state record) and
// forge-factory consumes it into the store. Plain data, depends on nothing.
package disttypes

// Index is one revision's distribution: every tool, every platform, every
// digest. The digests are the trust anchor; asset names and the release
// coordinates are hints a mirror may override.
type Index struct {
	// Revision is the minted revision id this distribution was built from.
	Revision string `json:"revision"`
	// CreatedAt is when the distribution was assembled, RFC 3339.
	CreatedAt string `json:"createdAt,omitempty"`
	// Release names where the aggregated release lives.
	Release Release `json:"release,omitempty"`
	// Tools is every distributed binary.
	Tools []Tool `json:"tools"`
}

// Release names the aggregated release carrying the binaries.
type Release struct {
	Repo string `json:"repo,omitempty"`
	Tag  string `json:"tag,omitempty"`
}

// Tool is one distributed binary across its platforms.
type Tool struct {
	// Name is the binary's base name, e.g. "forge-factory".
	Name string `json:"name"`
	// Module is the full main-package module path.
	Module string `json:"module,omitempty"`
	// Version is the human-readable version label.
	Version string `json:"version,omitempty"`
	// Kind says where the tool came from: "internal" for toolchain members,
	// "go" for third-party register packages.
	Kind string `json:"kind,omitempty"`
	// Platforms maps "os/arch" to the concrete binary.
	Platforms map[string]Platform `json:"platforms"`
}

// Platform is one concrete binary: its digest, size, and the asset name it
// travels under.
type Platform struct {
	// Digest is "sha256:<hex>" of the binary. Verification is mandatory.
	Digest string `json:"digest"`
	// Size in bytes, informational.
	Size int64 `json:"size,omitempty"`
	// Asset is the file name in the release or mirror.
	Asset string `json:"asset"`
	// Runner names an interpreter for non-native tools (e.g. "java -jar");
	// empty means the asset executes directly.
	Runner string `json:"runner,omitempty"`
}
