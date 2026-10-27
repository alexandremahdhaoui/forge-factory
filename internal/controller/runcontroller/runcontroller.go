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
	ErrNoTarget     = errors.New("no runnable matches")
	ErrNoFactory    = errors.New("no factory to resolve from")
	ErrNotAMember   = errors.New("not a member of the factory")
	ErrUnpublished  = errors.New("not published in the register's internal track")
	ErrMissingInput = errors.New("missing runnable input")
)

type Runner interface {
	Run(ctx context.Context, req Request) (int, error)
	Bootstrap(ctx context.Context, req BootstrapRequest) (config.Factory, string, error)
}

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

func (c *Controller) warn(format string, args ...interface{}) {
	_, _ = fmt.Fprintf(c.progress, "WARN: "+format+"\n", args...)
}

func (c *Controller) worktreeAdd(ctx context.Context, clone, sha, dest string) error {
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

	report, err := c.sync.Lock(ctx, f, wsRoot, only)
	if err != nil {
		return 0, fmt.Errorf("locking %s: %w", wsRoot, err)
	}

	if len(report.Unlocked) > 0 {
		return 0, fmt.Errorf("locking %s: %s", wsRoot, report.Unlocked[0])
	}

	return 0, nil
}

func (c *Controller) runRemote(ctx context.Context, req Request) (int, error) {
	module, sub, rev := splitTarget(req.Target)

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

	}

	root := filepath.Join(req.CacheDir, "run",
		sanitize(module)+"@"+shortSha(repoSha)+"+"+sanitize(factoryURL)+"@"+shortSha(factorySha))
	repoDir := filepath.Join(root, member.Name)

	if code, err := func() (int, error) {
		release, err := c.lock.Lock(root)
		if err != nil {
			return 0, err
		}
		defer release()

		warm, _ := c.fs.IsDir(repoDir)

		if marked, _ := c.fs.Exists(filepath.Join(root, materialisedMarker)); warm && !marked {
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
		c.say(req.Quiet,
			"note: this ran %s at %s, the tuple the register last proved; `forge-register status` marks stale internal tracks, and a green workspace pipeline republishes them",
			module, pinnedVersion)
	}

	return code, err
}

func staleTupleHint(err error, module, pinnedVersion string) error {
	if err == nil || pinnedVersion == "" {
		return err
	}

	return fmt.Errorf(
		"%w (this is the tuple the register last proved: %s at %s; if the repo moved since, `forge-register status` marks the stale track and a green workspace pipeline republishes it)",
		err, module, pinnedVersion)
}

type warmTuple struct {
	RepoDir string `json:"repoDir"`
	Target  struct {
		Name            string `json:"name"`
		Src             string `json:"src"`
		Factory         string `json:"factory"`
		FactoryRevision string `json:"factoryRevision"`
	} `json:"target"`
}

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

	envrc := filepath.Join(repoDir, ".envrc")
	if ok, _ := c.fs.Exists(envrc); !ok {
		if err := c.fs.WriteFile(envrc, []byte("")); err != nil {
			return fmt.Errorf("creating %s: %w", envrc, err)
		}
	}

	return nil
}

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

func (c *Controller) exec(ctx context.Context, repoDir, name string, args []string) (int, error) {
	full := append([]string{"run", name}, "--")
	full = append(full, args...)

	bin, pre := c.forgeCommand()

	return c.runner.RunAttached(ctx, repoDir,
		map[string]string{"FORGE_RUN_MATERIALIZED": "1"}, bin, append(pre, full...)...)
}

func (c *Controller) forgeCommand() (string, []string) {
	if _, ok := c.runner.LookPath("forge"); ok {
		return "forge", nil
	}

	if v := forgeDepVersion(); v != "" {
		return "go", []string{"run", "github.com/alexandremahdhaoui/forge/cmd/forge@" + v}
	}

	return "forge", nil
}

func forgeDepVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}

	return depVersion(info.Deps)
}

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

type materialFactory struct {
	config.Factory
	raw []byte
}

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

func (c *Controller) cloneOrFetch(ctx context.Context, cacheDir, url string) (string, error) {
	dir := filepath.Join(cacheDir, "git", sanitize(url))

	release, err := c.lock.Lock(dir)
	if err != nil {
		return "", err
	}
	defer release()

	if ok, _ := c.fs.IsDir(dir); ok {
		if err := c.git.Fetch(ctx, dir); err != nil {
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
		Current    string `json:"current"`
		Provenance string `json:"provenance"`
	}

	if err := json.Unmarshal([]byte(raw), &track); err != nil {
		return "", "", fmt.Errorf("decoding the internal track of %s: %w", module, err)
	}

	if track.Provenance == "" {
		return "", "", fmt.Errorf(
			"the internal track of %s names %s with no proving revision; "+
				"re-run the pipeline that publishes it", module, track.Current)
	}

	return track.Current, track.Provenance, nil
}

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

type BootstrapRequest struct {
	Factory  string
	Dir      string
	CacheDir string
	Quiet    bool
}

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

	clone := filepath.Join(req.CacheDir, "git", sanitize(url))

	record, err := synccontroller.ReadRecord(c.fs, root)
	if err != nil {
		return config.Factory{}, "", err
	}

	placed := map[string][]byte{"forge-factory.yaml": factory.raw}

	for _, extra := range []string{"forge-ci.yaml", "CLAUDE.md", "FOLLOWUP.md"} {
		content, found, err := c.git.Show(ctx, clone, sha, "workspace/"+extra)
		if err != nil || !found {
			continue
		}

		placed[extra] = []byte(content)
	}

	digests := synccontroller.Record{}
	paths := make([]string, 0, len(placed))

	for _, name := range sortedKeys(placed) {
		digest, err := synccontroller.WriteRendered(
			c.fs, root, filepath.Join(root, name), placed[name], record)
		if err != nil {
			return config.Factory{}, "", err
		}

		digests[name] = digest
		paths = append(paths, name)
	}

	for name := range record {
		if _, replaced := digests[name]; !replaced {
			paths = append(paths, name)
		}
	}

	if _, _, err := synccontroller.WriteRecord(c.fs, root, paths, digests); err != nil {
		return config.Factory{}, "", err
	}

	return factory.Factory, root, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
