// Package runcontroller materialises the dependency context a runnable needs
// and hands execution to forge. It decides WHAT context a run resolves in -
// the enclosing workspace, the runnable's own factory, or an explicit
// override - and never learns HOW a target executes: the boundary is one
// attached exec of `forge run` inside the materialised repo.
package runcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/gitadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/lockadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/synccontroller"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

var (
	// ErrNoTarget means nothing in reach matches what the caller named.
	ErrNoTarget = errors.New("no runnable matches")
	// ErrNoFactory means a runnable declares no factory and no override was
	// given. A runnable without a factory fails forge.yaml validation, so
	// meeting this means the repo's forge.yaml predates the contract.
	ErrNoFactory = errors.New("no factory to resolve from")
	// ErrNotAMember means the factory's repos list does not carry the repo.
	// The factory is the trust boundary in every mode.
	ErrNotAMember = errors.New("not a member of the factory")
	// ErrUnpublished means the register's internal track has no entry for
	// the repo, so a no-version run has no version to pick.
	ErrUnpublished = errors.New("not published in the register's internal track")
	// ErrMissingInput means the runnable's generated inputs name something
	// the environment does not provide.
	ErrMissingInput = errors.New("missing runnable input")
)

// Runner materialises and delegates one run, and stands workspaces up. It is
// what a driver accepts, declared in the package that implements it.
type Runner interface {
	Run(ctx context.Context, req Request) (int, error)
	Bootstrap(ctx context.Context, req BootstrapRequest) (config.Factory, string, error)
}

// Request is one run, already split by the driver: the target, the program's
// own args, and the flags.
type Request struct {
	Target   string
	Name     string
	Args     []string
	Factory  string
	Force    bool
	Quiet    bool
	WorkDir  string
	CacheDir string
}

type Controller struct {
	fs       fsadapter.FS
	git      gitadapter.Git
	runner   execadapter.Runner
	sync     synccontroller.Syncer
	lock     lockadapter.Locker
	progress io.Writer
}

var _ Runner = (*Controller)(nil)

func New(
	fs fsadapter.FS,
	git gitadapter.Git,
	runner execadapter.Runner,
	sync synccontroller.Syncer,
	lock lockadapter.Locker,
	progress io.Writer,
) *Controller {
	return &Controller{fs: fs, git: git, runner: runner, sync: sync, lock: lock, progress: progress}
}

// runnableFile is the slice of a repo's forge.yaml this controller reads.
// forge owns the file; this parse is lenient on purpose and takes only what
// context determination needs.
type runnableFile struct {
	Name string `json:"name"`
	Run  []struct {
		Name            string `json:"name"`
		Src             string `json:"src"`
		Factory         string `json:"factory"`
		FactoryRevision string `json:"factoryRevision"`
	} `json:"run"`
	Build []struct {
		Name string `json:"name"`
		Src  string `json:"src"`
		Dest string `json:"dest"`
	} `json:"build"`
}

type runnable struct {
	Name            string
	Src             string
	Factory         string
	FactoryRevision string
}

func (c *Controller) say(quiet bool, format string, args ...interface{}) {
	if quiet {
		return
	}

	_, _ = fmt.Fprintf(c.progress, "forge-factory run: "+format+"\n", args...)
}

// warn is for self-heal actions the user should know about. It ignores
// quiet: a heal changed cache state, and silence there breeds mistrust.
func (c *Controller) warn(format string, args ...interface{}) {
	_, _ = fmt.Fprintf(c.progress, "WARN: "+format+"\n", args...)
}

// worktreeAdd registers a worktree, healing the one wound git names but
// never fixes on its own: a registration whose directory was removed by
// hand. The registration points at nothing, so prune-and-retry is safe.
func (c *Controller) worktreeAdd(ctx context.Context, clone, sha, dest string) error {
	// Worktree registrations live inside the clone: serialize against any
	// other process touching the same clone.
	release, lerr := c.lock.Lock(clone)
	if lerr != nil {
		return lerr
	}
	defer release()

	err := c.git.WorktreeAdd(ctx, clone, sha, dest)
	if err == nil || !strings.Contains(err.Error(), "already registered worktree") {
		return err
	}

	if perr := c.git.WorktreePrune(ctx, clone); perr != nil {
		return err
	}

	c.warn("healed the run cache: pruned a dangling worktree registration in %s", clone)

	return c.git.WorktreeAdd(ctx, clone, sha, dest)
}

// materialisedMarker is the proof a run context was built to the end. A
// root without it is a checkout that died halfway - resuming it fails
// later and deeper, so it is discarded and rebuilt instead.
const materialisedMarker = ".forge-materialised"

func (c *Controller) Run(ctx context.Context, req Request) (int, error) {
	if req.CacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return 0, fmt.Errorf("finding the cache directory: %w", err)
		}

		req.CacheDir = filepath.Join(base, "forge-factory")
	}

	if isModulePath(req.Target) {
		return c.runRemote(ctx, req)
	}

	return c.runLocal(ctx, req)
}

// isModulePath reports whether a target names a repo rather than something
// in the current one: a URL-ish module path whose first segment is a host
// with a dot in it, or an absolute path to a repo.
func isModulePath(target string) bool {
	if strings.HasPrefix(target, "./") || strings.HasPrefix(target, "../") {
		return false
	}

	if filepath.IsAbs(target) {
		return true
	}

	first, _, found := strings.Cut(target, "/")

	return found && strings.Contains(first, ".")
}

// --- local -----------------------------------------------------------------

func (c *Controller) runLocal(ctx context.Context, req Request) (int, error) {
	repoRoot, err := c.walkUpFor(req.WorkDir, "forge.yaml")
	if err != nil {
		return 0, fmt.Errorf("finding the repo: %w", err)
	}

	target, err := c.resolveRunnable(repoRoot, req.Target)
	if err != nil {
		return 0, err
	}

	if req.Factory != "" {
		c.say(req.Quiet, "rule 1: --factory %s overrides everything", req.Factory)

		url, rev := splitRev(req.Factory)

		return c.materialiseAndExec(ctx, req, repoRoot, target, url, rev, "")
	}

	wsRoot, member := c.enclosingWorkspaceClaims(repoRoot)
	if member {
		c.say(req.Quiet, "rule 2: the enclosing workspace %s claims this repo", wsRoot)

		if code, err := c.syncWorkspace(ctx, wsRoot, ""); err != nil {
			return code, err
		}

		if err := c.checkInputs(repoRoot, target); err != nil {
			return 0, err
		}

		return c.exec(ctx, repoRoot, target.Name, req.Args)
	}

	if target.Factory == "" {
		return 0, fmt.Errorf("%w: runnable %s declares none and no --factory was given", ErrNoFactory, target.Name)
	}

	c.say(req.Quiet, "rule 3: resolving through the runnable's factory %s", target.Factory)

	return c.materialiseAndExec(ctx, req, repoRoot, target, target.Factory, target.FactoryRevision, "")
}

// enclosingWorkspaceClaims walks up for a forge-factory.yaml whose repos list
// carries this repo by directory name. Only a claiming workspace wins:
// a checkout that merely sits inside someone else's workspace resolves
// through its own factory instead.
func (c *Controller) enclosingWorkspaceClaims(repoRoot string) (string, bool) {
	wsRoot, err := c.walkUpFor(filepath.Dir(repoRoot), "forge-factory.yaml")
	if err != nil {
		return "", false
	}

	raw, err := c.fs.ReadFile(filepath.Join(wsRoot, "forge-factory.yaml"))
	if err != nil {
		return "", false
	}

	f, err := config.Parse(raw)
	if err != nil {
		return "", false
	}

	name := filepath.Base(repoRoot)
	for _, r := range f.Repos {
		if r.Name == name {
			return wsRoot, true
		}
	}

	return "", false
}

func (c *Controller) syncWorkspace(ctx context.Context, wsRoot, only string) (int, error) {
	raw, err := c.fs.ReadFile(filepath.Join(wsRoot, "forge-factory.yaml"))
	if err != nil {
		return 0, err
	}

	f, err := config.Parse(raw)
	if err != nil {
		return 0, fmt.Errorf("reading the workspace factory: %w", err)
	}

	if _, err := c.sync.Sync(ctx, f, wsRoot, only); err != nil {
		return 0, fmt.Errorf("syncing %s: %w", wsRoot, err)
	}

	return 0, nil
}

// --- remote ----------------------------------------------------------------

func (c *Controller) runRemote(ctx context.Context, req Request) (int, error) {
	module, sub, rev := splitTarget(req.Target)

	// The tuple-keyed early entry: a request that pins everything itself -
	// a full sha on the target and a full sha on --factory - names its run
	// context with no resolution, so a warm one enters at the inputs step
	// with no clone, fetch or register lookup. Anything that floats (a
	// branch, a tag, the internal track, the factory's head) keeps
	// resolving, because floating is the point of those forms.
	if key := warmTupleKey(req); key != "" && !req.Force {
		if code, err, hit := c.enterWarmTuple(ctx, req, key); hit {
			return code, err
		}
	}

	c.say(req.Quiet, "clone: %s", module)

	repoClone, err := c.cloneOrFetch(ctx, req.CacheDir, moduleToURL(module))
	if err != nil {
		return 0, err
	}

	headSha, err := c.git.ResolveRev(ctx, repoClone, "origin/HEAD")
	if err != nil {
		return 0, fmt.Errorf("resolving the default branch of %s: %w", module, err)
	}

	if sub == "" {
		sub = req.Name
	}

	c.say(req.Quiet, "resolve-target: forge.yaml at %s", shortSha(headSha))

	target, err := c.runnableAtRev(ctx, repoClone, headSha, sub)
	if err != nil {
		return 0, err
	}

	factoryURL, factoryRev := target.Factory, target.FactoryRevision
	if req.Factory != "" {
		factoryURL, factoryRev = splitRev(req.Factory)
		c.say(req.Quiet, "resolve-factory: --factory %s overrides the runnable's", factoryURL)
	} else if factoryURL == "" {
		return 0, fmt.Errorf("%w: runnable %s declares none and no --factory was given", ErrNoFactory, target.Name)
	} else {
		c.say(req.Quiet, "resolve-factory: %s", factoryURL)
	}

	repoSha := ""

	if rev != "" {
		repoSha, err = c.git.ResolveRev(ctx, repoClone, rev)
		if err != nil {
			return 0, fmt.Errorf("resolving %s in %s: %w", rev, module, err)
		}

		c.say(req.Quiet, "UNPROVEN: %s runs at %s and the factory floats to its head; --factory url@rev pins it", module, rev)
	}

	return c.materialiseRemote(ctx, req, module, repoClone, repoSha, target, factoryURL, factoryRev)
}

func (c *Controller) materialiseRemote(
	ctx context.Context,
	req Request,
	module, repoClone, repoSha string,
	target runnable,
	factoryURL, factoryRev string,
) (int, error) {
	factory, factorySha, err := c.factoryAt(ctx, req.CacheDir, factoryURL, factoryRev)
	if err != nil {
		return 0, err
	}

	member, ok := memberFor(factory.Factory, module)
	if !ok {
		return 0, fmt.Errorf("%s is %w at %s - the factory's repos list is the trust boundary", module, ErrNotAMember, factoryURL)
	}

	registerURL := ""
	if factory.Register != nil {
		registerURL = factory.Register.URL
	}

	if registerURL == "" {
		return 0, fmt.Errorf("the factory at %s names no register, so nothing resolves a version", factoryURL)
	}

	registerClone, err := c.cloneOrFetch(ctx, req.CacheDir, registerURL)
	if err != nil {
		return 0, err
	}

	registerRev := "origin/HEAD"
	if factory.Register.Revision != "" {
		registerRev = factory.Register.Revision
	}

	registerSha, err := c.git.ResolveRev(ctx, registerClone, registerRev)
	if err != nil {
		return 0, fmt.Errorf("resolving the register at %s: %w", registerRev, err)
	}

	pinnedVersion := ""

	if repoSha == "" {
		c.say(req.Quiet, "resolve-version: the internal track of %s at %s", module, shortSha(registerSha))

		version, provenance, err := c.internalCurrent(ctx, registerClone, registerSha, module)
		if err != nil {
			return 0, err
		}

		c.say(req.Quiet, "pin: %s %s proven by revision %s", module, version, provenance)

		pinnedVersion = version

		pinned := c.provenancePins(ctx, req, factory.Factory, provenance)

		if sha, ok := pinned[member.Name]; ok {
			repoSha = sha
		} else if sha, err := c.git.ResolveRev(ctx, repoClone, version); err == nil {
			repoSha = sha
		} else {
			return 0, fmt.Errorf("pinning %s: the revision names no sha and the tag %s is not in the clone", module, version)
		}

		// The register is deliberately NOT pinned by provenance: provenance
		// answers what a repo's build inputs were when proven, and members
		// above are pinned for exactly that. The register is the catalog of
		// what exists - and the pipeline records repo heads before its own
		// publish stage writes into the register, so a provenance-pinned
		// register predates its own run's admissions by construction. Pin
		// the answers, never the phonebook: the catalog reads at the
		// resolved head (or the factory's explicit register.revision).
	}

	root := filepath.Join(req.CacheDir, "run",
		sanitize(module)+"@"+shortSha(repoSha)+"+"+sanitize(factoryURL)+"@"+shortSha(factorySha))
	repoDir := filepath.Join(root, member.Name)

	// The whole examine-and-build of the run root sits under its lock, so
	// a second process materialising the same tuple waits and then finds
	// the finished root instead of half of one.
	if code, err := func() (int, error) {
		release, err := c.lock.Lock(root)
		if err != nil {
			return 0, err
		}
		defer release()

		warm, _ := c.fs.IsDir(repoDir)

		if marked, _ := c.fs.Exists(filepath.Join(root, materialisedMarker)); warm && !marked {
			// The root exists but its materialisation never finished: a partial
			// checkout half-trusted here fails later and deeper (a missing
			// .envrc, a manifest with no workspace root), with no trace back.
			c.warn("healed the run cache: %s was an incomplete checkout - discarded and rebuilt", root)

			if err := os.RemoveAll(root); err != nil {
				return 0, fmt.Errorf("discarding the incomplete checkout %s: %w (try: forge-factory cache clean)", root, err)
			}

			warm = false
		}

		if warm && !req.Force {
			c.say(req.Quiet, "cache: warm at %s", root)

			return 0, nil
		}

		if err := c.checkoutContext(ctx, req, root, factory, member, repoClone, repoSha, registerClone, registerSha, registerURL); err != nil {
			return 0, err
		}

		if code, err := c.syncWorkspace(ctx, root, member.Name); err != nil {
			return code, staleTupleHint(err, module, pinnedVersion)
		}

		if err := c.fs.WriteFile(filepath.Join(root, materialisedMarker), []byte("")); err != nil {
			return 0, fmt.Errorf("marking %s materialised: %w", root, err)
		}

		return 0, nil
	}(); err != nil {
		return code, err
	}

	if err := c.checkInputs(repoDir, target); err != nil {
		return 0, staleTupleHint(err, module, pinnedVersion)
	}

	if key := warmTupleKey(req); key != "" {
		c.writeWarmTuple(req.CacheDir, key, repoDir, target)
	}

	c.say(req.Quiet, "exec: forge run %s in %s", target.Name, repoDir)

	code, err := c.exec(ctx, repoDir, target.Name, req.Args)
	if (err != nil || code != 0) && pinnedVersion != "" {
		// The run executed the tuple the register last proved, which may
		// be older than the repo's head: a failure here is often the
		// register's staleness, not the operator's mistake. One line
		// names the check and the cure.
		c.say(req.Quiet,
			"note: this ran %s at %s, the tuple the register last proved; `forge-register status` marks stale internal tracks, and a green workspace pipeline republishes them",
			module, pinnedVersion)
	}

	return code, err
}

// staleTupleHint wraps a failure inside a register-resolved run context
// with the one likely cause the raw error never names: the proven tuple
// is older than the repo's head. Runs pinned by the caller pass through
// untouched.
func staleTupleHint(err error, module, pinnedVersion string) error {
	if err == nil || pinnedVersion == "" {
		return err
	}

	return fmt.Errorf(
		"%w (this is the tuple the register last proved: %s at %s; if the repo moved since, `forge-register status` marks the stale track and a green workspace pipeline republishes it)",
		err, module, pinnedVersion)
}

// warmTuple is the marker a fully pinned run leaves behind: enough to
// re-enter at the inputs step with no resolution.
type warmTuple struct {
	RepoDir string `json:"repoDir"`
	Target  struct {
		Name            string `json:"name"`
		Src             string `json:"src"`
		Factory         string `json:"factory"`
		FactoryRevision string `json:"factoryRevision"`
	} `json:"target"`
}

// warmTupleKey names the request's tuple, or answers "" when anything
// floats. Only a full 40-hex sha pins immutably - a branch or tag can
// move under the cache and the internal track advances by design.
func warmTupleKey(req Request) string {
	module, sub, rev := splitTarget(req.Target)
	if !isFullSha(rev) {
		return ""
	}

	factoryURL, factoryRev := splitRev(req.Factory)
	if factoryURL == "" || !isFullSha(factoryRev) {
		return ""
	}

	name := sub
	if name == "" {
		name = req.Name
	}

	return sanitize(module) + "@" + shortSha(rev) +
		"+" + sanitize(name) +
		"+" + sanitize(factoryURL) + "@" + shortSha(factoryRev)
}

func isFullSha(s string) bool {
	if len(s) != 40 {
		return false
	}

	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}

	return true
}

func warmTuplePath(cacheDir, key string) string {
	return filepath.Join(cacheDir, "run", ".ready", key+".json")
}

// enterWarmTuple re-enters a marked run context at the inputs step. A
// missing or unreadable marker, or a context that is gone, is a miss and
// the run resolves normally; the marker is convenience, never authority.
func (c *Controller) enterWarmTuple(ctx context.Context, req Request, key string) (int, error, bool) {
	raw, err := c.fs.ReadFile(warmTuplePath(req.CacheDir, key))
	if err != nil {
		return 0, nil, false
	}

	var mark warmTuple
	if err := json.Unmarshal(raw, &mark); err != nil {
		return 0, nil, false
	}

	if ok, _ := c.fs.IsDir(mark.RepoDir); !ok {
		return 0, nil, false
	}

	target := runnable(mark.Target)

	if err := c.checkInputs(mark.RepoDir, target); err != nil {
		return 0, err, true
	}

	c.say(req.Quiet, "cache: warm tuple, entering at inputs")
	c.say(req.Quiet, "exec: forge run %s in %s", target.Name, mark.RepoDir)

	code, err := c.exec(ctx, mark.RepoDir, target.Name, req.Args)

	return code, err, true
}

// writeWarmTuple records a successful materialisation. Failing to write
// it costs the next run a resolution, nothing more, so it stays quiet.
func (c *Controller) writeWarmTuple(cacheDir, key, repoDir string, target runnable) {
	var mark warmTuple
	mark.RepoDir = repoDir
	mark.Target = struct {
		Name            string `json:"name"`
		Src             string `json:"src"`
		Factory         string `json:"factory"`
		FactoryRevision string `json:"factoryRevision"`
	}(target)

	raw, err := json.Marshal(mark)
	if err != nil {
		return
	}

	path := warmTuplePath(cacheDir, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return
	}

	_ = c.fs.WriteFile(path, raw)
}

func (c *Controller) checkoutContext(
	ctx context.Context,
	req Request,
	root string,
	factory materialFactory,
	member config.Repo,
	repoClone, repoSha, registerClone, registerSha, registerURL string,
) error {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return fmt.Errorf("creating the run context: %w", err)
	}

	if err := c.fs.WriteFile(filepath.Join(root, "forge-factory.yaml"), factory.raw); err != nil {
		return fmt.Errorf("placing the factory file: %w", err)
	}

	c.say(req.Quiet, "checkout: %s at %s", member.Name, shortSha(repoSha))

	repoDir := filepath.Join(root, member.Name)
	if ok, _ := c.fs.IsDir(repoDir); !ok {
		if err := c.worktreeAdd(ctx, repoClone, repoSha, repoDir); err != nil {
			return err
		}
	}

	registerName := path.Base(strings.TrimSuffix(registerURL, ".git"))

	registerDir := filepath.Join(root, registerName)
	if ok, _ := c.fs.IsDir(registerDir); !ok {
		if err := c.worktreeAdd(ctx, registerClone, registerSha, registerDir); err != nil {
			return err
		}
	}

	// forge sources the repo's envFile and the file is gitignored, so a
	// fresh worktree lacks it.
	envrc := filepath.Join(repoDir, ".envrc")
	if ok, _ := c.fs.Exists(envrc); !ok {
		if err := c.fs.WriteFile(envrc, []byte("")); err != nil {
			return fmt.Errorf("creating %s: %w", envrc, err)
		}
	}

	return nil
}

// materialiseAndExec is the local rule 3 and rule 1 path: the checkout in
// place is the code, and only the dependency context is ephemeral.
func (c *Controller) materialiseAndExec(
	ctx context.Context,
	req Request,
	repoRoot string,
	target runnable,
	factoryURL, factoryRev, _ string,
) (int, error) {
	factory, factorySha, err := c.factoryAt(ctx, req.CacheDir, factoryURL, factoryRev)
	if err != nil {
		return 0, err
	}

	name := filepath.Base(repoRoot)

	member, ok := memberByName(factory.Factory, name)
	if !ok {
		return 0, fmt.Errorf("%s is %w at %s - the factory's repos list is the trust boundary", name, ErrNotAMember, factoryURL)
	}

	registerURL := ""
	if factory.Register != nil {
		registerURL = factory.Register.URL
	}

	root := filepath.Join(req.CacheDir, "run",
		"local-"+sanitize(repoRoot)+"+"+sanitize(factoryURL)+"@"+shortSha(factorySha))

	// The local run context is shared state too: hold its lock while
	// building it, so a concurrent run of the same checkout queues.
	release, err := c.lock.Lock(root)
	if err != nil {
		return 0, err
	}
	defer release()

	if err := os.MkdirAll(root, 0o750); err != nil {
		return 0, fmt.Errorf("creating the run context: %w", err)
	}

	if err := c.fs.WriteFile(filepath.Join(root, "forge-factory.yaml"), factory.raw); err != nil {
		return 0, fmt.Errorf("placing the factory file: %w", err)
	}

	link := filepath.Join(root, member.Name)
	if ok, _ := c.fs.Exists(link); !ok {
		if err := os.Symlink(repoRoot, link); err != nil {
			return 0, fmt.Errorf("linking the checkout into the context: %w", err)
		}
	}

	if registerURL != "" {
		registerClone, err := c.cloneOrFetch(ctx, req.CacheDir, registerURL)
		if err != nil {
			return 0, err
		}

		registerRev := "origin/HEAD"
		if factory.Register.Revision != "" {
			registerRev = factory.Register.Revision
		}

		registerSha, err := c.git.ResolveRev(ctx, registerClone, registerRev)
		if err != nil {
			return 0, fmt.Errorf("resolving the register at %s: %w", registerRev, err)
		}

		registerName := path.Base(strings.TrimSuffix(registerURL, ".git"))

		registerDir := filepath.Join(root, registerName)
		if ok, _ := c.fs.IsDir(registerDir); !ok {
			if err := c.worktreeAdd(ctx, registerClone, registerSha, registerDir); err != nil {
				return 0, err
			}
		}
	}

	if code, err := c.syncWorkspace(ctx, root, member.Name); err != nil {
		return code, err
	}

	if err := c.checkInputs(repoRoot, target); err != nil {
		return 0, err
	}

	return c.exec(ctx, repoRoot, target.Name, req.Args)
}

// --- shared ----------------------------------------------------------------

func (c *Controller) exec(ctx context.Context, repoDir, name string, args []string) (int, error) {
	full := append([]string{"run", name}, "--")
	full = append(full, args...)

	bin, pre := c.forgeCommand()

	return c.runner.RunAttached(ctx, repoDir,
		map[string]string{"FORGE_RUN_MATERIALIZED": "1"}, bin, append(pre, full...)...)
}

// forgeCommand answers how to exec forge at the boundary: PATH when
// installed, else go run pinned to the forge this binary was built against -
// never latest. A workspace build records no dependency version and keeps
// the bare name, whose failure names the missing install.
func (c *Controller) forgeCommand() (string, []string) {
	if _, ok := c.runner.LookPath("forge"); ok {
		return "forge", nil
	}

	if v := forgeDepVersion(); v != "" {
		return "go", []string{"run", "github.com/alexandremahdhaoui/forge/cmd/forge@" + v}
	}

	return "forge", nil
}

// forgeDepVersion answers the forge version this binary was built against,
// from build info; "" for workspace builds, which carry "(devel)".
func forgeDepVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}

	return depVersion(info.Deps)
}

// depVersion is the reading, split from the reading of build info so it can
// be driven over the shapes that matter: a replace directive, a workspace
// build, and forge absent altogether.
func depVersion(deps []*debug.Module) string {
	for _, dep := range deps {
		if dep == nil || dep.Path != "github.com/alexandremahdhaoui/forge" {
			continue
		}

		m := dep
		if m.Replace != nil {
			m = m.Replace
		}

		if m.Version == "" || m.Version == "(devel)" {
			return ""
		}

		return m.Version
	}

	return ""
}

func (c *Controller) walkUpFor(start, name string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}

	for {
		if ok, _ := c.fs.Exists(filepath.Join(dir, name)); ok {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s found walking up from %s", name, start)
		}

		dir = parent
	}
}

func (c *Controller) resolveRunnable(repoRoot, target string) (runnable, error) {
	raw, err := c.fs.ReadFile(filepath.Join(repoRoot, "forge.yaml"))
	if err != nil {
		return runnable{}, err
	}

	return matchRunnable(raw, target)
}

func (c *Controller) runnableAtRev(ctx context.Context, clone, sha, sub string) (runnable, error) {
	raw, found, err := c.git.Show(ctx, clone, sha, "forge.yaml")
	if err != nil {
		return runnable{}, err
	}

	if !found {
		return runnable{}, fmt.Errorf("%w: the repo carries no forge.yaml", ErrNoTarget)
	}

	return matchRunnable([]byte(raw), sub)
}

func matchRunnable(raw []byte, target string) (runnable, error) {
	var file runnableFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return runnable{}, fmt.Errorf("reading forge.yaml: %w", err)
	}

	isPath := strings.Contains(target, "/") || strings.HasPrefix(target, ".")

	for _, r := range file.Run {
		if (!isPath && r.Name == target) ||
			(isPath && path.Clean(strings.TrimPrefix(r.Src, "./")) == path.Clean(strings.TrimPrefix(target, "./"))) {
			return runnable{Name: r.Name, Src: r.Src, Factory: r.Factory, FactoryRevision: r.FactoryRevision}, nil
		}
	}

	declared := []string{}
	for _, r := range file.Run {
		declared = append(declared, r.Name)
	}

	sort.Strings(declared)

	list := "none"
	if len(declared) > 0 {
		list = strings.Join(declared, ", ")
	}

	return runnable{}, fmt.Errorf("%w %q; declared runnables: %s", ErrNoTarget, target, list)
}

// materialFactory is a parsed factory plus the raw bytes it was read from,
// so the placed file is byte-identical to the one the factory repo carries.
type materialFactory struct {
	config.Factory
	raw []byte
}

// factoryAt clones the factory repo and reads workspace/forge-factory.yaml at
// the asked rev, or the default branch head.
func (c *Controller) factoryAt(ctx context.Context, cacheDir, url, rev string) (materialFactory, string, error) {
	clone, err := c.cloneOrFetch(ctx, cacheDir, url)
	if err != nil {
		return materialFactory{}, "", err
	}

	if rev == "" {
		rev = "origin/HEAD"
	}

	sha, err := c.git.ResolveRev(ctx, clone, rev)
	if err != nil {
		return materialFactory{}, "", fmt.Errorf("resolving the factory at %s: %w", rev, err)
	}

	raw, found, err := c.git.Show(ctx, clone, sha, "workspace/forge-factory.yaml")
	if err != nil {
		return materialFactory{}, "", err
	}

	if !found {
		return materialFactory{}, "", fmt.Errorf("%s carries no workspace/forge-factory.yaml at %s", url, rev)
	}

	f, err := config.Parse([]byte(raw))
	if err != nil {
		return materialFactory{}, "", fmt.Errorf("reading the factory at %s: %w", url, err)
	}

	return materialFactory{Factory: f, raw: []byte(raw)}, sha, nil
}

// cloneOrFetch keeps one full clone per repo URL, forever. Every rev
// materialises from it as a worktree; nothing is ever shallow.
func (c *Controller) cloneOrFetch(ctx context.Context, cacheDir, url string) (string, error) {
	dir := filepath.Join(cacheDir, "git", sanitize(url))

	// The mirror is shared machine state: another process - this
	// workspace's, a second workspace's, an orphaned child's - may be
	// fetching or reading it right now. The lock turns the race into a
	// queue.
	release, err := c.lock.Lock(dir)
	if err != nil {
		return "", err
	}
	defer release()

	if ok, _ := c.fs.IsDir(dir); ok {
		if err := c.git.Fetch(ctx, dir); err != nil {
			// A broken mirror is cache state, not the user's work: name
			// the directory and the way out instead of the raw git error.
			return "", fmt.Errorf("refreshing the cached mirror %s: %w (try: forge-factory cache clean)", dir, err)
		}

		return dir, nil
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return "", fmt.Errorf("creating the clone cache: %w", err)
	}

	if err := c.git.Clone(ctx, url, dir); err != nil {
		return "", err
	}

	return dir, nil
}

// internalCurrent reads the register's internal track for a module at one
// rev: the highest track's current version and the revision that proved it.
func (c *Controller) internalCurrent(ctx context.Context, registerClone, sha, module string) (string, string, error) {
	rel := path.Join("index", "internal", module)

	entries, err := c.git.LsTree(ctx, registerClone, sha, rel)
	if err != nil {
		return "", "", err
	}

	best := ""

	for _, e := range entries {
		prefix := strings.TrimSuffix(e, ".json")
		if prefix == e {
			continue
		}

		if best == "" || prefix > best {
			best = prefix
		}
	}

	if best == "" {
		return "", "", fmt.Errorf(
			"%s is %w - run the pipeline that publishes it, or name a rev for an unproven dev run",
			module, ErrUnpublished)
	}

	raw, found, err := c.git.Show(ctx, registerClone, sha, path.Join(rel, best+".json"))
	if err != nil || !found {
		return "", "", fmt.Errorf("reading the internal track of %s: %w", module, err)
	}

	var track struct {
		Current string `json:"current"`
		History []struct {
			Version    string `json:"version"`
			Provenance string `json:"provenance"`
		} `json:"history"`
	}

	if err := json.Unmarshal([]byte(raw), &track); err != nil {
		return "", "", fmt.Errorf("decoding the internal track of %s: %w", module, err)
	}

	provenance := ""

	for _, h := range track.History {
		if h.Version == track.Current {
			provenance = h.Provenance
		}
	}

	return track.Current, provenance, nil
}

// provenancePins reads the proving revision's record from the factory's
// state repo and answers the member shas it pinned. Members the record does
// not name fall back elsewhere; the caller says what floated.
func (c *Controller) provenancePins(ctx context.Context, req Request, factory config.Factory, provenance string) map[string]string {
	if provenance == "" || factory.State == nil {
		return nil
	}

	statePath, _ := factory.State.Spec["path"].(string)
	if statePath == "" {
		return nil
	}

	member, ok := memberByName(factory, filepath.Base(statePath))
	if !ok {
		return nil
	}

	clone, err := c.cloneOrFetch(ctx, req.CacheDir, member.URL)
	if err != nil {
		return nil
	}

	sha, err := c.git.ResolveRev(ctx, clone, "origin/HEAD")
	if err != nil {
		return nil
	}

	raw, found, err := c.git.Show(ctx, clone, sha, path.Join("revisions", provenance+".json"))
	if err != nil || !found {
		c.say(req.Quiet, "pin: revision %s is not in %s; the member shas float to the published tags", provenance, member.Name)

		return nil
	}

	var record struct {
		Repos map[string]string `json:"repos"`
	}

	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return nil
	}

	return record.Repos
}

// checkInputs enforces the runnable's generated contract before any build:
// the env vars set and the files present, or one line naming what is not.
func (c *Controller) checkInputs(repoDir string, target runnable) error {
	manifest := filepath.Join(repoDir, filepath.FromSlash(strings.TrimPrefix(target.Src, "./")), "zz_generated.runnable.yaml")

	raw, err := c.fs.ReadFile(manifest)
	if err != nil {
		return nil
	}

	var contract struct {
		Inputs struct {
			Env   []string `json:"env"`
			Files []string `json:"files"`
		} `json:"inputs"`
	}

	if err := yaml.Unmarshal(raw, &contract); err != nil {
		return fmt.Errorf("reading %s: %w", manifest, err)
	}

	for _, name := range contract.Inputs.Env {
		if _, ok := os.LookupEnv(name); !ok {
			return fmt.Errorf("%w: environment variable %s (named by %s)", ErrMissingInput, name, manifest)
		}
	}

	for _, file := range contract.Inputs.Files {
		if ok, _ := c.fs.Exists(filepath.Join(repoDir, file)); !ok {
			return fmt.Errorf("%w: file %s (named by %s)", ErrMissingInput, file, manifest)
		}
	}

	return nil
}

func memberFor(f config.Factory, module string) (config.Repo, bool) {
	name := path.Base(module)

	return memberByName(f, name)
}

func memberByName(f config.Factory, name string) (config.Repo, bool) {
	for _, r := range f.Repos {
		if r.Name == name {
			return r, true
		}
	}

	return config.Repo{}, false
}

// splitTarget cuts a remote target into module, optional sub path or name,
// and optional rev: github.com/x/repo/cmd/tool@v1 or github.com/x/repo. An
// absolute path finds its repo boundary by walking up to a .git directory.
func splitTarget(target string) (module, sub, rev string) {
	bare := target
	if i := strings.LastIndex(bare, "@"); i > 0 && !strings.ContainsAny(bare[i+1:], "/:") {
		rev = bare[i+1:]
		bare = bare[:i]
	}

	if filepath.IsAbs(bare) {
		dir := bare
		for dir != string(filepath.Separator) {
			if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
				sub = strings.TrimPrefix(strings.TrimPrefix(bare, dir), "/")

				return dir, sub, rev
			}

			dir = filepath.Dir(dir)
		}

		return bare, "", rev
	}

	parts := strings.Split(bare, "/")
	if len(parts) <= 3 {
		return bare, "", rev
	}

	return strings.Join(parts[:3], "/"), strings.Join(parts[3:], "/"), rev
}

// splitRev cuts a trailing @rev off a URL. The rev part carries no slash and
// no colon, which is what separates it from the @ in an ssh URL.
func splitRev(s string) (string, string) {
	i := strings.LastIndex(s, "@")
	if i < 0 {
		return s, ""
	}

	tail := s[i+1:]
	if tail == "" || strings.ContainsAny(tail, "/:") {
		return s, ""
	}

	return s[:i], tail
}

func moduleToURL(module string) string {
	if filepath.IsAbs(module) {
		return module
	}

	parts := strings.SplitN(module, "/", 2)
	if len(parts) != 2 {
		return module
	}

	return "git@" + parts[0] + ":" + parts[1] + ".git"
}

func sanitize(s string) string {
	replacer := strings.NewReplacer("/", "-", ":", "-", "@", "-", ".", "-", "~", "-")

	return replacer.Replace(s)
}

func shortSha(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}

	return sha
}

// BootstrapRequest names a factory to stand a workspace up from.
type BootstrapRequest struct {
	Factory  string
	Dir      string
	CacheDir string
	Quiet    bool
}

// Bootstrap places a factory's workspace files into a directory, so the
// driver's clone verb can fetch every member and sync. One command from
// nothing to a working workspace.
func (c *Controller) Bootstrap(ctx context.Context, req BootstrapRequest) (config.Factory, string, error) {
	if req.CacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return config.Factory{}, "", fmt.Errorf("finding the cache directory: %w", err)
		}

		req.CacheDir = filepath.Join(base, "forge-factory")
	}

	url, rev := splitRev(req.Factory)

	factory, sha, err := c.factoryAt(ctx, req.CacheDir, url, rev)
	if err != nil {
		return config.Factory{}, "", err
	}

	root, err := filepath.Abs(req.Dir)
	if err != nil {
		return config.Factory{}, "", err
	}

	if err := os.MkdirAll(root, 0o750); err != nil {
		return config.Factory{}, "", fmt.Errorf("creating %s: %w", root, err)
	}

	c.say(req.Quiet, "bootstrap: %s at %s into %s", url, shortSha(sha), root)

	if err := c.fs.WriteFile(filepath.Join(root, "forge-factory.yaml"), factory.raw); err != nil {
		return config.Factory{}, "", fmt.Errorf("placing forge-factory.yaml: %w", err)
	}

	clone := filepath.Join(req.CacheDir, "git", sanitize(url))

	for _, extra := range []string{"forge-ci.yaml", "CLAUDE.md", "FOLLOWUP.md"} {
		content, found, err := c.git.Show(ctx, clone, sha, "workspace/"+extra)
		if err != nil || !found {
			continue
		}

		if err := c.fs.WriteFile(filepath.Join(root, extra), []byte(content)); err != nil {
			return config.Factory{}, "", fmt.Errorf("placing %s: %w", extra, err)
		}
	}

	return factory.Factory, root, nil
}
