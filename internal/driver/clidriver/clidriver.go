package clidriver

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/distadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/clonecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/resolvecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/revisioncontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/runcontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/speccontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/statuscontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/synccontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/toolingcontroller"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

const DefaultPath = "forge-factory.yaml"

var (
	ErrUsage = errors.New(
		"usage: forge-factory <clone|sync|add|bump|checkout|status|validate|run|bootstrap> [args] " +
			"[--config path] [--root dir] [--offline] [--register-head]")
	ErrDrift = errors.New("the workspace disagrees with the factory")

	ErrUnsettled = errors.New("the files were written and the workspace does not build")
)

type Driver struct {
	offline     bool
	prune       bool
	only        string
	toolingFrom string
	out         io.Writer
	fs          fsadapter.FS
	clone       clonecontroller.Cloner
	sync        synccontroller.Syncer
	revise      revisioncontroller.Reviser
	state       statuscontroller.Stater
	run         runcontroller.Runner
	tooling     toolingcontroller.Applier
	exit        func(int)
}

func New(
	out io.Writer,
	fs fsadapter.FS,
	clone clonecontroller.Cloner,
	sync synccontroller.Syncer,
	revise revisioncontroller.Reviser,
	state statuscontroller.Stater,
	run runcontroller.Runner,
	tooling toolingcontroller.Applier,
	exit func(int),
) *Driver {
	return &Driver{
		out: out, fs: fs, clone: clone, sync: sync,
		revise: revise, state: state, run: run, tooling: tooling, exit: exit,
	}
}

func (d *Driver) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return ErrUsage
	}

	verb := args[0]

	// run and bootstrap need no local factory file: the target or the
	// bootstrap URL names everything.
	switch verb {
	case "run":
		return d.runRun(ctx, args[1:])
	case "bootstrap":
		return d.runBootstrap(ctx, args[1:])
	}

	f, path, root, rest, err := d.load(verb, args[1:])
	if err != nil {
		return err
	}

	switch verb {
	case "validate":
		return d.write(describe(f))
	case "clone":
		return d.runClone(ctx, f, path, root)
	case "sync":
		return d.runSync(ctx, f, path, root)
	case "status":
		return d.runStatus(ctx, f, root)
	case "add":
		return d.runAdd(ctx, path, root, rest)
	case "bump":
		return d.runBump(ctx, f, path, root, rest)
	case "checkout":
		return d.runCheckout(ctx, f, path, root, rest)
	default:
		return fmt.Errorf("unknown subcommand %q: %w", verb, ErrUsage)
	}
}

// runClone fetches what is missing and then syncs, because a member with no
// manifest is not yet something forge can build.
func (d *Driver) runClone(ctx context.Context, f config.Factory, path, root string) error {
	report, err := d.clone.Clone(ctx, f, root)
	if err != nil {
		return err
	}

	if err := d.write(renderClone(report)); err != nil {
		return err
	}

	return d.runSync(ctx, f, path, root)
}

func renderClone(report clonecontroller.Report) string {
	var b strings.Builder

	fmt.Fprintf(&b, "cloned into %s\n", report.Root)

	for _, name := range report.Cloned {
		fmt.Fprintf(&b, "  cloned %s\n", name)
	}

	if len(report.Cloned) == 0 {
		fmt.Fprintf(&b, "  every member was already there\n")
	}

	return b.String()
}

func (d *Driver) runSync(ctx context.Context, f config.Factory, path, root string) error {
	report, err := d.sync.Sync(ctx, f, root, d.only)
	if err != nil {
		return err
	}

	if err := d.write(renderSync(report)); err != nil {
		return err
	}

	if d.prune {
		d.prune = false // one pass: the re-sync must not loop

		updated, pruned, err := d.pruneDeadPins(path, report)
		if err != nil {
			return err
		}

		if pruned {
			return d.runSync(ctx, updated, path, root)
		}
	}

	// A sync that writes a version nothing can resolve leaves every member
	// unbuildable. Reporting that and exiting zero is a lie, so it is an error
	// unless the caller says it is offline on purpose.
	if len(report.Unsettled) > 0 && !d.offline {
		return fmt.Errorf("%w: %s", ErrUnsettled, report.Unsettled[0])
	}

	return d.applyTooling(root)
}

// applyTooling consumes a distribution into the store and links the
// workspace to it, when a source is in reach: the --tooling-from flag, else
// the FORGE_DIST_MIRROR environment - the airgap door, where the mirrored
// release assets ARE the bundle.
func (d *Driver) applyTooling(root string) error {
	base := d.toolingFrom
	if base == "" {
		base = os.Getenv("FORGE_DIST_MIRROR")
	}

	if base == "" || d.tooling == nil {
		return nil
	}

	report, err := d.tooling.Apply(toolingcontroller.Request{
		Root:       root,
		Source:     distadapter.New(base),
		SourceName: base,
	})
	if err != nil {
		return fmt.Errorf("consuming the distribution from %s: %w", base, err)
	}

	return d.write(renderTooling(report))
}

func renderTooling(report toolingcontroller.Report) string {
	var b strings.Builder

	fmt.Fprintf(&b, "tooling %s (%s)\n", report.Revision, report.Platform)

	for _, name := range report.Installed {
		fmt.Fprintf(&b, "  installed %s\n", name)
	}

	for _, name := range report.Reused {
		fmt.Fprintf(&b, "  reused %s\n", name)
	}

	if report.BinDir != "" {
		fmt.Fprintf(&b, "  linked %s\n", report.BinDir)
	}

	return b.String()
}

// pruneDeadPins deletes the soft pins the resolver named dead, so the factory
// file ends the run already clean. It acts only on what the notes said.
func (d *Driver) pruneDeadPins(
	path string,
	report synccontroller.Report,
) (config.Factory, bool, error) {
	var dead []string

	for _, note := range report.Notes {
		if dep, ok := resolvecontroller.DeadPin(note); ok {
			dead = append(dead, dep)
		}
	}

	if len(dead) == 0 {
		return config.Factory{}, false, nil
	}

	raw, err := d.fs.ReadFile(path)
	if err != nil {
		return config.Factory{}, false, err
	}

	next, edits, err := speccontroller.PrunePins(raw, dead)
	if err != nil {
		return config.Factory{}, false, err
	}

	if err := d.fs.WriteFile(path, next); err != nil {
		return config.Factory{}, false, err
	}

	for _, e := range edits {
		err := d.write(fmt.Sprintf("%s:%d pruned a dead pin\n  was %s\n  now %s\n",
			path, e.Line, strings.TrimSpace(e.Was), strings.TrimSpace(e.Now)))
		if err != nil {
			return config.Factory{}, false, err
		}
	}

	updated, err := config.Parse(next)
	if err != nil {
		return config.Factory{}, false, err
	}

	return updated, true, nil
}

// runRun materialises the context a runnable needs and delegates execution
// to forge, propagating the program's exit code verbatim.
func (d *Driver) runRun(ctx context.Context, args []string) error {
	req := runcontroller.Request{}

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(d.out)
	factory := fs.String("factory", "", "resolve through this factory url[@rev], overriding everything")
	force := fs.Bool("force", false, "refresh the run cache")
	quiet := fs.Bool("quiet", false, "silence the progress lines")
	cache := fs.String("cache", "", "cache directory, defaults to the user cache")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}

	rest := fs.Args()

	for i, a := range rest {
		if a == "--" {
			req.Args = rest[i+1:]
			rest = rest[:i]

			break
		}
	}

	switch len(rest) {
	case 1:
		req.Target = rest[0]
	case 2:
		req.Target, req.Name = rest[0], rest[1]
	default:
		return fmt.Errorf("run takes a target and an optional runnable name: %w", ErrUsage)
	}

	req.Factory = *factory
	req.Force = *force
	req.Quiet = *quiet
	req.CacheDir = *cache

	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	req.WorkDir = wd

	code, err := d.run.Run(ctx, req)
	if err != nil {
		return err
	}

	if code != 0 {
		d.exit(code)
	}

	return nil
}

// runBootstrap places a factory's workspace files, then clones every member
// and syncs: one command from nothing to a working workspace.
func (d *Driver) runBootstrap(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	fs.SetOutput(d.out)
	quiet := fs.Bool("quiet", false, "silence the progress lines")
	cache := fs.String("cache", "", "cache directory, defaults to the user cache")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}

	rest := fs.Args()
	if len(rest) < 1 || len(rest) > 2 {
		return fmt.Errorf("bootstrap takes a factory url[@rev] and an optional directory: %w", ErrUsage)
	}

	dir := "."
	if len(rest) == 2 {
		dir = rest[1]
	}

	f, root, err := d.run.Bootstrap(ctx, runcontroller.BootstrapRequest{
		Factory: rest[0], Dir: dir, Quiet: *quiet, CacheDir: *cache,
	})
	if err != nil {
		return err
	}

	return d.runClone(ctx, f, filepath.Join(root, DefaultPath), root)
}

func (d *Driver) runStatus(ctx context.Context, f config.Factory, root string) error {
	report, err := d.state.Status(ctx, f, root, d.offline)
	if err != nil {
		return err
	}

	if err := d.write(renderStatus(report)); err != nil {
		return err
	}

	if !report.Agrees() {
		return ErrDrift
	}

	return nil
}

func (d *Driver) runAdd(ctx context.Context, path, root string, rest []string) error {
	if len(rest) < 3 {
		return fmt.Errorf("add takes a name, a url and at least one language: %w", ErrUsage)
	}

	repo := config.Repo{Name: rest[0], URL: rest[1], Languages: rest[2:]}

	raw, err := d.fs.ReadFile(path)
	if err != nil {
		return err
	}

	next, edit, err := speccontroller.Add(raw, repo)
	if err != nil {
		return err
	}

	if err := d.fs.WriteFile(path, next); err != nil {
		return err
	}

	if err := d.write(fmt.Sprintf("%s:%d added\n%s\n", path, edit.Line, edit.Now)); err != nil {
		return err
	}

	updated, err := config.Parse(next)
	if err != nil {
		return err
	}

	return d.runSync(ctx, updated, path, root)
}

func (d *Driver) runBump(
	ctx context.Context,
	f config.Factory,
	path, root string,
	rest []string,
) error {
	if len(rest) != 2 {
		return fmt.Errorf("bump takes a dependency and a version: %w", ErrUsage)
	}

	raw, err := d.fs.ReadFile(path)
	if err != nil {
		return err
	}

	next, edit, err := speccontroller.Bump(raw, rest[0], rest[1])
	if err != nil {
		if declared := speccontroller.Dependencies(f); len(declared) > 0 {
			return fmt.Errorf("%w\n  the factory declares: %s", err, strings.Join(declared, ", "))
		}

		return err
	}

	if err := d.fs.WriteFile(path, next); err != nil {
		return err
	}

	err = d.write(fmt.Sprintf("%s:%d\n  was %s\n  now %s\n",
		path, edit.Line, strings.TrimSpace(edit.Was), strings.TrimSpace(edit.Now)))
	if err != nil {
		return err
	}

	updated, err := config.Parse(next)
	if err != nil {
		return err
	}

	return d.runSync(ctx, updated, path, root)
}

func (d *Driver) runCheckout(
	ctx context.Context,
	f config.Factory,
	path, root string,
	rest []string,
) error {
	if len(rest) != 1 {
		return fmt.Errorf("checkout takes one revision id: %w", ErrUsage)
	}

	result, err := d.revise.Checkout(ctx, f, root, rest[0])
	if err != nil {
		return err
	}

	if err := d.write(renderCheckout(result)); err != nil {
		return err
	}

	return d.runSync(ctx, f, path, root)
}

func (d *Driver) load(
	verb string,
	args []string,
) (config.Factory, string, string, []string, error) {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(d.out)

	path := fs.String("config", DefaultPath, "path to the factory file")
	root := fs.String("root", "", "directory holding the repos, defaults to the factory file's parent")
	offline := fs.Bool("offline", false,
		"do not fail when a command that needs the network could not run")
	registerHead := fs.Bool("register-head", false,
		"resolve from the register checkout as it stands, ignoring the pinned revision")
	prune := fs.Bool("prune-pins", false,
		"delete the soft pins the resolver names dead, then sync again")
	only := fs.String("only", "",
		"restrict sync writes and settle commands to one member")
	toolingFrom := fs.String("tooling-from", "",
		"consume a distribution (a directory or an http(s) base URL) into the store and link .forge/bin; FORGE_DIST_MIRROR is the environment form")

	if err := fs.Parse(args); err != nil {
		return config.Factory{}, "", "", nil, fmt.Errorf("parsing flags: %w", err)
	}

	d.offline = *offline
	d.prune = *prune
	d.only = *only
	d.toolingFrom = *toolingFrom

	raw, err := d.fs.ReadFile(*path)
	if err != nil {
		return config.Factory{}, "", "", nil, err
	}

	f, err := config.Parse(raw)
	if err != nil {
		return config.Factory{}, "", "", nil, fmt.Errorf("reading %s: %w", *path, err)
	}

	// The canary is the one caller that must see the candidate index, not the
	// published tag - it exists to test what the pin would hide.
	if *registerHead && f.Register != nil {
		f.Register.Revision = ""
	}

	if *root == "" {
		abs, err := filepath.Abs(*path)
		if err != nil {
			return config.Factory{}, "", "", nil, fmt.Errorf("resolving %s: %w", *path, err)
		}

		*root = filepath.Dir(abs)
	}

	return f, *path, *root, fs.Args(), nil
}

func (d *Driver) write(s string) error {
	if _, err := io.WriteString(d.out, s); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}

	return nil
}

func describe(f config.Factory) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s: %d repos, %d engines, %d languages\n",
		f.Name, len(f.Repos), len(f.Engines), len(f.Languages()))

	for _, l := range f.Languages() {
		uri, _ := f.EngineFor(l)
		fmt.Fprintf(&b, "  %s %s (%d dependencies)\n", l, uri, len(f.DependenciesFor(l)))
	}

	return b.String()
}

func renderSync(report synccontroller.Report) string {
	var b strings.Builder

	fmt.Fprintf(&b, "synced %s\n", report.Root)

	for _, note := range report.Notes {
		fmt.Fprintf(&b, "  note: %s\n", note)
	}

	for _, path := range report.Written {
		fmt.Fprintf(&b, "  wrote %s\n", relative(report.Root, path))
	}

	for _, path := range report.Ignored {
		fmt.Fprintf(&b, "  ignored in %s\n", relative(report.Root, path))
	}

	for _, what := range report.Settled {
		fmt.Fprintf(&b, "  ran %s\n", what)
	}

	for _, what := range report.Unsettled {
		fmt.Fprintf(&b, "  could not run %s, which a build will need\n", what)
	}

	return b.String()
}

func renderStatus(report statuscontroller.Report) string {
	var b strings.Builder

	fmt.Fprintf(&b, "factory %s\n", report.Root)

	if report.Offline {
		fmt.Fprintf(&b, "  freshness skipped (--offline): measuring it needs a fetch\n")
	}

	for _, repo := range report.Repos {
		fmt.Fprintf(&b, "  %s %s\n", repo.Name, describeRepo(repo))
	}

	for _, name := range report.Unknown {
		fmt.Fprintf(&b, "  %s is a repo the factory does not declare\n", name)
	}

	return b.String()
}

func describeRepo(repo statuscontroller.RepoStatus) string {
	switch {
	case !repo.Present:
		return "is missing"
	case !repo.Cloned:
		return "is a directory and not a git repo"
	}

	out := short(repo.Head)
	if repo.Dirty {
		out += " dirty"
	}

	switch repo.Freshness {
	case statuscontroller.Ahead:
		out += fmt.Sprintf(" (%d ahead of origin/main)", repo.Ahead)
	case statuscontroller.Behind:
		out += fmt.Sprintf(" (%d behind origin/main - pull it)", repo.Behind)
	case statuscontroller.Diverged:
		out += fmt.Sprintf(" (diverged: %d ahead, %d behind origin/main - rebase or push before building on it)",
			repo.Ahead, repo.Behind)
	}

	return out
}

func renderCheckout(result revisioncontroller.Result) string {
	var b strings.Builder

	fmt.Fprintf(&b, "revision %s\n", result.Revision)

	names := make([]string, 0, len(result.Repos))
	for name := range result.Repos {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		fmt.Fprintf(&b, "  %s %s\n", name, short(result.Repos[name]))
	}

	return b.String()
}

func relative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}

	return rel
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}

	return sha
}

// Usage is what main prints when it is called with nothing.
func Usage() string {
	return ErrUsage.Error() + "\n"
}
