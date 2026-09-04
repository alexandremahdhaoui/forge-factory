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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/lockadapter"
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

// Applier provisions tooling into the store, declared in the package that
// implements it: distributions by digest, toolchain binaries by pinned
// build.
type Applier interface {
	Apply(req Request) (Report, error)
	ProvisionBinaries(ctx context.Context, root, storeDir string, binaries []Binary) (BinaryReport, error)
}

type Controller struct {
	fs     fsadapter.FS
	runner execadapter.Runner
	// lock serializes builds of one (module, version, platform) across
	// processes. The store is user-global; two workspaces syncing at once,
	// or two CI jobs on one restored cache, is the ordinary case.
	lock lockadapter.Locker
}

var _ Applier = (*Controller)(nil)

func New(fs fsadapter.FS, runner execadapter.Runner, lock lockadapter.Locker) *Controller {
	return &Controller{fs: fs, runner: runner, lock: lock}
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

// linkWorkspace links every distributed tool into the workspace's
// .forge/bin - a real directory, so provisioned toolchain binaries can sit
// beside the distribution - and records what it pinned, so status and a
// later doctor can say which revision this workspace runs.
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
	if err := c.fs.MkdirAll(binDir); err != nil {
		return "", fmt.Errorf("linking the workspace tooling: %w", err)
	}

	for _, tool := range index.Tools {
		if err := c.fs.Symlink(filepath.Join(absView, tool.Name), filepath.Join(binDir, tool.Name)); err != nil {
			return "", fmt.Errorf("linking the workspace tooling: %w", err)
		}
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

// Binary is one toolchain binary to provision, already resolved to a
// version: the config's literal pin, or the register track's current.
type Binary struct {
	Name    string
	Module  string
	Version string
}

// BinaryReport says what provisioning did.
type BinaryReport struct {
	Installed []string
	Reused    []string
}

// ProvisionBinaries builds each binary at its pinned version into the
// store and links it into the workspace's .forge/bin. The build is
// `go install module@version` into a staging GOBIN; the result is hashed
// into the content-addressed blobs like a distributed binary, and a
// (module, version) already built is reused without touching the network.
func (c *Controller) ProvisionBinaries(
	ctx context.Context, root, storeDir string, binaries []Binary,
) (BinaryReport, error) {
	report := BinaryReport{Installed: []string{}, Reused: []string{}}

	if len(binaries) == 0 {
		return report, nil
	}

	store, err := resolveStoreDir(storeDir)
	if err != nil {
		return BinaryReport{}, err
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return BinaryReport{}, fmt.Errorf("resolving the workspace root: %w", err)
	}

	binDir := filepath.Join(absRoot, ".forge", "bin")
	if err := c.fs.MkdirAll(binDir); err != nil {
		return BinaryReport{}, fmt.Errorf("provisioning the toolchain: %w", err)
	}

	for _, binary := range binaries {
		built, reused, err := c.buildBinary(ctx, store, binary)
		if err != nil {
			return BinaryReport{}, err
		}

		if err := c.fs.Symlink(built, filepath.Join(binDir, binary.Name)); err != nil {
			return BinaryReport{}, fmt.Errorf("linking %s: %w", binary.Name, err)
		}

		if reused {
			report.Reused = append(report.Reused, binary.Name)
		} else {
			report.Installed = append(report.Installed, binary.Name)
		}
	}

	return report, nil
}

// buildBinary answers the absolute store path of one (module, version)
// build, building it when the store does not carry it yet.
func (c *Controller) buildBinary(
	ctx context.Context, store string, binary Binary,
) (string, bool, error) {
	if binary.Version == "" || binary.Version == "latest" {
		return "", false, fmt.Errorf(
			"toolchain binary %s: nothing pins a version; latest is never a fallback", binary.Name)
	}

	// The path carries the platform. A store restored from a cache, or one
	// shared between a container job and a host job, is read on more than
	// one architecture, and a (module, version) built for the other one
	// answered by name alone.
	entry := filepath.Join(store, "tools",
		sanitize(binary.Module)+"@"+sanitize(binary.Version), runtime.GOOS+"-"+runtime.GOARCH)

	absEntry, err := filepath.Abs(entry)
	if err != nil {
		return "", false, fmt.Errorf("resolving the store: %w", err)
	}

	absTool := filepath.Join(absEntry, binary.Name)

	if ok, _ := c.fs.Exists(absTool); ok {
		return absTool, true, nil
	}

	// The store is user-global and two workspaces may sync at once, so the
	// entry is built under a lock and re-checked once it is held: the loser
	// of the race finds the winner's build and reuses it, instead of both
	// building and the last symlink silently winning.
	if err := c.fs.MkdirAll(filepath.Dir(absEntry)); err != nil {
		return "", false, fmt.Errorf("preparing the store for %s: %w", binary.Name, err)
	}

	release, err := c.lock.Lock(absEntry)
	if err != nil {
		return "", false, fmt.Errorf("locking the store for %s: %w", binary.Name, err)
	}

	defer release()

	if ok, _ := c.fs.Exists(absTool); ok {
		return absTool, true, nil
	}

	// Keyed by module, version AND process, so the staging dir of one build
	// is never another's.
	staging := filepath.Join(store, "tmp", fmt.Sprintf("gobin-%s@%s-%d",
		sanitize(binary.Module), sanitize(binary.Version), os.Getpid()))

	absStaging, err := filepath.Abs(staging)
	if err != nil {
		return "", false, fmt.Errorf("resolving the staging dir: %w", err)
	}

	// The staging GOBIN starts empty so whatever go install leaves there is
	// the build - its own naming rules (a /vN module installs under the
	// unversioned name) never have to be guessed.
	if err := c.fs.Remove(absStaging); err != nil {
		return "", false, err
	}

	if err := c.fs.MkdirAll(absStaging); err != nil {
		return "", false, err
	}

	env := map[string]string{"GOBIN": absStaging, "GOWORK": "off", "GOFLAGS": ""}

	res, err := c.runner.RunEnv(ctx, "", env, "go", "install", binary.Module+"@"+binary.Version)
	if err != nil {
		return "", false, fmt.Errorf("building %s: %w", binary.Name, err)
	}

	if res.ExitCode != 0 {
		return "", false, fmt.Errorf("building %s: go install %s@%s exited %d: %s",
			binary.Name, binary.Module, binary.Version, res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	built, err := c.fs.List(absStaging)
	if err != nil {
		return "", false, fmt.Errorf("reading what go install built for %s: %w", binary.Name, err)
	}

	if len(built) != 1 {
		return "", false, fmt.Errorf(
			"building %s: go install left %d files in the staging GOBIN, want exactly the binary", binary.Name, len(built))
	}

	data, err := c.fs.ReadFile(filepath.Join(absStaging, built[0]))
	if err != nil {
		return "", false, fmt.Errorf("reading what go install built for %s: %w", binary.Name, err)
	}

	// The name is unique per process, so nothing else will ever reuse it.
	// Leaving it would grow the store by one directory per build.
	defer func() { _ = c.fs.Remove(absStaging) }()

	// The blob is content-addressed like a distributed binary; the tools
	// path is the (module, version) name resolution finds it under.
	sum := sha256.Sum256(data)
	blob := filepath.Join(store, "blobs", "sha256", hex.EncodeToString(sum[:]))

	if ok, _ := c.fs.Exists(blob); !ok {
		stagedBlob := filepath.Join(store, "tmp",
			fmt.Sprintf("%s-%d", hex.EncodeToString(sum[:]), os.Getpid()))
		if err := c.fs.WriteExecutable(stagedBlob, data); err != nil {
			return "", false, err
		}

		if err := c.fs.Rename(stagedBlob, blob); err != nil {
			return "", false, err
		}
	}

	absBlob, err := filepath.Abs(blob)
	if err != nil {
		return "", false, fmt.Errorf("resolving the store: %w", err)
	}

	if err := c.fs.Symlink(absBlob, absTool); err != nil {
		return "", false, err
	}

	return absTool, false, nil
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', ':', '@', '~':
			return '-'
		default:
			return r
		}
	}, s)
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
