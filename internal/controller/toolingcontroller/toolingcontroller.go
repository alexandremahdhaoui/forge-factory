// Package toolingcontroller consumes a distribution index into the store
// and exposes the pinned view to a workspace. The store is content
// addressed and immutable: a blob is written once behind its sha256 and
// never touched again, so revisions coexist and nothing ever conflicts
// with what a user installed themselves.
//
// Layout, under the store root (FORGE_STORE_DIR or ~/.cache/forge/store):
//
//	blobs/sha256/<hex>              the binaries, chmod 555
//	rev/<revision>/<os>-<arch>/bin/<name>   symlinks into blobs
//	rev/<revision>/index.json       the index that produced this view
//	tmp/                            staging, renamed into blobs atomically
//
// The workspace's <root>/.forge/bin links into the revision view, and the
// .envrc line sync writes puts it on PATH - which is how "the pinned store
// outranks a stale ~/go/bin" is delivered without touching either.
package toolingcontroller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/disttypes"
)

// Fetcher answers a distribution file's bytes by name. distadapter
// implements it over a directory or an http(s) base.
type Fetcher interface {
	Fetch(name string) ([]byte, error)
}

// Request is one apply: consume the source's index into the store and link
// the workspace at root to it.
type Request struct {
	// Root is the workspace root; empty skips the workspace linking and
	// only populates the store.
	Root string
	// StoreDir overrides the store root; empty resolves FORGE_STORE_DIR
	// then ~/.cache/forge/store.
	StoreDir string
	// Source fetches the index and the binaries.
	Source Fetcher
	// SourceName is the human-readable origin recorded in the report.
	SourceName string
	// Platform overrides "GOOS/GOARCH", for tests.
	Platform string
}

// Report says what one apply did.
type Report struct {
	Revision  string
	Platform  string
	Installed []string
	Reused    []string
	BinDir    string
}

// Applier consumes a distribution into the store, declared in the package
// that implements it.
type Applier interface {
	Apply(req Request) (Report, error)
}

type Controller struct {
	fs fsadapter.FS
}

var _ Applier = (*Controller)(nil)

func New(fs fsadapter.FS) *Controller {
	return &Controller{fs: fs}
}

// Apply consumes the source's index: verify every blob into the store,
// materialise the revision view, and link the workspace's .forge/bin to
// it. Every byte is digest-checked before it lands; a mismatch fails the
// whole apply loud.
func (c *Controller) Apply(req Request) (Report, error) {
	index, err := c.readIndex(req.Source)
	if err != nil {
		return Report{}, err
	}

	platform := req.Platform
	if platform == "" {
		platform = runtime.GOOS + "/" + runtime.GOARCH
	}

	req.Platform = platform

	store, err := resolveStoreDir(req.StoreDir)
	if err != nil {
		return Report{}, err
	}

	report := Report{
		Revision:  index.Revision,
		Platform:  platform,
		Installed: []string{},
		Reused:    []string{},
	}

	viewBin := filepath.Join(store, "rev", index.Revision, platformDir(platform), "bin")

	for _, tool := range index.Tools {
		entry, ok := tool.Platforms[platform]
		if !ok {
			return Report{}, fmt.Errorf(
				"tool %s carries no binary for %s: the distribution is incomplete for this machine", tool.Name, platform)
		}

		hexDigest, err := parseDigest(tool.Name, entry.Digest)
		if err != nil {
			return Report{}, err
		}

		blob := filepath.Join(store, "blobs", "sha256", hexDigest)

		if ok, _ := c.fs.Exists(blob); ok {
			report.Reused = append(report.Reused, tool.Name)
		} else {
			if err := c.installBlob(store, req.Source, blob, tool.Name, entry, hexDigest); err != nil {
				return Report{}, err
			}

			report.Installed = append(report.Installed, tool.Name)
		}

		// The view links relatively so the store can move as a unit.
		relBlob := filepath.Join("..", "..", "..", "..", "blobs", "sha256", hexDigest)
		if err := c.fs.Symlink(relBlob, filepath.Join(viewBin, tool.Name)); err != nil {
			return Report{}, fmt.Errorf("materialising the %s view: %w", index.Revision, err)
		}
	}

	raw, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return Report{}, fmt.Errorf("re-encoding the index: %w", err)
	}

	indexPath := filepath.Join(store, "rev", index.Revision, "index.json")
	if err := c.fs.WriteFile(indexPath, append(raw, '\n')); err != nil {
		return Report{}, err
	}

	if req.Root != "" {
		binDir, err := c.linkWorkspace(req, index, viewBin)
		if err != nil {
			return Report{}, err
		}

		report.BinDir = binDir
	}

	return report, nil
}

func (c *Controller) readIndex(source Fetcher) (disttypes.Index, error) {
	if source == nil {
		return disttypes.Index{}, fmt.Errorf("no distribution source given")
	}

	raw, err := source.Fetch("index.json")
	if err != nil {
		return disttypes.Index{}, fmt.Errorf("fetching the distribution index: %w", err)
	}

	var index disttypes.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return disttypes.Index{}, fmt.Errorf("parsing the distribution index: %w", err)
	}

	if index.Revision == "" {
		return disttypes.Index{}, fmt.Errorf("the distribution index names no revision")
	}

	if strings.HasSuffix(index.Revision, "-dirty") {
		return disttypes.Index{}, fmt.Errorf(
			"the distribution index names dirty revision %s; a distribution is built from clean, minted revisions only",
			index.Revision)
	}

	if len(index.Tools) == 0 {
		return disttypes.Index{}, fmt.Errorf("the distribution index names no tools")
	}

	return index, nil
}

// installBlob fetches, verifies and lands one binary. Staging into tmp and
// renaming keeps a half-written blob invisible; the digest check happens
// before anything lands, so a tampered or corrupted asset fails the apply
// with nothing written.
func (c *Controller) installBlob(
	store string, source Fetcher, blob, name string, entry disttypes.Platform, hexDigest string,
) error {
	if entry.Asset == "" {
		return fmt.Errorf("tool %s names no asset to fetch", name)
	}

	data, err := source.Fetch(entry.Asset)
	if err != nil {
		return fmt.Errorf("fetching %s for %s: %w", entry.Asset, name, err)
	}

	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != hexDigest {
		return fmt.Errorf(
			"tool %s: asset %s digests to sha256:%s, the index pins sha256:%s; refusing a binary the pipeline did not prove",
			name, entry.Asset, got, hexDigest)
	}

	if entry.Size > 0 && entry.Size != int64(len(data)) {
		return fmt.Errorf("tool %s: asset %s is %d bytes, the index says %d", name, entry.Asset, len(data), entry.Size)
	}

	staging := filepath.Join(store, "tmp", hexDigest)
	if err := c.fs.WriteExecutable(staging, data); err != nil {
		return err
	}

	return c.fs.Rename(staging, blob)
}

// linkWorkspace points the workspace's .forge/bin at the revision view and
// records what it pinned, so status and a later doctor can say which
// revision this workspace runs.
func (c *Controller) linkWorkspace(req Request, index disttypes.Index, viewBin string) (string, error) {
	root, err := filepath.Abs(req.Root)
	if err != nil {
		return "", fmt.Errorf("resolving the workspace root: %w", err)
	}

	absView, err := filepath.Abs(viewBin)
	if err != nil {
		return "", fmt.Errorf("resolving the store view: %w", err)
	}

	binDir := filepath.Join(root, ".forge", "bin")
	if err := c.fs.Symlink(absView, binDir); err != nil {
		return "", fmt.Errorf("linking the workspace tooling: %w", err)
	}

	pin := map[string]string{
		"revision": index.Revision,
		"platform": req.Platform,
		"source":   req.SourceName,
	}

	raw, err := json.MarshalIndent(pin, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding the tooling pin: %w", err)
	}

	if err := c.fs.WriteFile(filepath.Join(root, ".forge", "tooling.json"), append(raw, '\n')); err != nil {
		return "", err
	}

	return binDir, nil
}

func resolveStoreDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}

	if env := os.Getenv("FORGE_STORE_DIR"); env != "" {
		return env, nil
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolving the store dir: %w", err)
	}

	return filepath.Join(cache, "forge", "store"), nil
}

func platformDir(platform string) string {
	return strings.ReplaceAll(platform, "/", "-")
}

func parseDigest(name, digest string) (string, error) {
	hexDigest, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hexDigest) != sha256.Size*2 {
		return "", fmt.Errorf("tool %s carries digest %q; a distribution digest is sha256:<64 hex>", name, digest)
	}

	if _, err := hex.DecodeString(hexDigest); err != nil {
		return "", fmt.Errorf("tool %s carries digest %q; a distribution digest is sha256:<64 hex>", name, digest)
	}

	return hexDigest, nil
}
