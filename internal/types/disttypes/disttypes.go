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
	// The index carries no module, version or kind. All three were declared
	// here with no producer on the other side: the pipeline tags each member
	// on its own line and a binary's name says nothing about which member
	// built it, so the only version available at index time was the
	// release's own, stamped on every tool alike. A field that can only be
	// filled in wrongly is better absent.
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
}
