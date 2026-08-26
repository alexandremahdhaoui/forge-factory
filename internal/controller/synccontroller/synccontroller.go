package synccontroller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/engineadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/repoadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/resolvecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/toolingcontroller"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

const (
	ToolLanguage = "language"
	ToolRender   = "render"

	gitignoreHeader = "# forge-factory materialises these. A version is written in forge-factory.yaml."
)

var (
	ErrLanguage = errors.New("an engine speaks a different language than its alias claims")
	ErrCommand  = errors.New("a command an engine asked for failed")
)

// tail keeps the end of a command's stderr, which is where the reason is.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return "..." + s[len(s)-400:]
	}

	return s
}

// Report is what a sync did, so a caller can print it and a test can assert it.
// Notes carry the resolver's diagnostics: pins standing, pins to remove,
// deprecated tracks.
type Report struct {
	Root      string   `json:"root"`
	Written   []string `json:"written"`
	Ignored   []string `json:"ignored"`
	Settled   []string `json:"settled"`
	Unsettled []string `json:"unsettled"`
	Notes     []string `json:"notes,omitempty"`
	// Toolchain is every declared binary resolved to its pinned version;
	// the driver provisions them into the store.
	Toolchain []toolingcontroller.Binary `json:"toolchain,omitempty"`
}

type repoWire struct {
	Name      string            `json:"name"`
	Path      string            `json:"path"`
	Languages []string          `json:"languages"`
	Identity  map[string]string `json:"identity,omitempty"`
}

type renderInput struct {
	Root         string            `json:"root"`
	Repos        []repoWire        `json:"repos"`
	Dependencies map[string]string `json:"dependencies"`
	Dev          map[string]string `json:"devDependencies"`
}

type fileWire struct {
	Path       string   `json:"path"`
	Content    string   `json:"content"`
	Gitignore  string   `json:"gitignore,omitempty"`
	AlsoIgnore []string `json:"alsoIgnore,omitempty"`
}

type commandWire struct {
	Dir      string            `json:"dir"`
	Command  string            `json:"command"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Optional bool              `json:"optional,omitempty"`
}

type renderOutput struct {
	Files  []fileWire    `json:"files"`
	Settle []commandWire `json:"settle,omitempty"`
}

type languageOutput struct {
	Language string `json:"language"`
}

// Syncer is what a driver accepts. It is declared here, in the package that
// implements it, as golden-go does.
type Syncer interface {
	// Sync writes every member's manifest. Only restricts writes and settle
	// commands to one member - the ephemeral run context uses it, where the
	// other members are not on disk at all. Empty means everything.
	Sync(ctx context.Context, f config.Factory, root, only string) (Report, error)
}

type Controller struct {
	caller   engineadapter.Caller
	fs       fsadapter.FS
	repos    repoadapter.Reader
	runner   execadapter.Runner
	resolver resolvecontroller.Resolver
}

var _ Syncer = (*Controller)(nil)

func New(
	caller engineadapter.Caller,
	fs fsadapter.FS,
	repos repoadapter.Reader,
	runner execadapter.Runner,
	resolver resolvecontroller.Resolver,
) *Controller {
	return &Controller{caller: caller, fs: fs, repos: repos, runner: runner, resolver: resolver}
}

// Sync asks every language engine what to write, writes it, and keeps each
// repo's gitignore in step. A version is written in the factory and nowhere
// else, so everything written here is ignored by git.
func (c *Controller) Sync(ctx context.Context, f config.Factory, root, only string) (Report, error) {
	f = restrictTo(f, only)

	resolved, err := c.resolve(f, root)
	if err != nil {
		return Report{}, err
	}

	report := Report{
		Root:      root,
		Written:   []string{},
		Ignored:   []string{},
		Settled:   []string{},
		Unsettled: []string{},
	}

	ignores := map[string][]string{}

	// forge sources a .envrc in every repo it builds, so the file must
	// exist; its content is the machine's own (gitignored). Creating it here
	// retires the touch-loop every fresh workspace used to need. One managed
	// line is kept present in every file - the workspace tooling PATH entry,
	// which puts the pinned store's view ahead of whatever a user installed -
	// and the rest of the file is never touched.
	if err := c.ensureEnvrcs(f, root, &report); err != nil {
		return Report{}, err
	}

	// Toolchain binaries resolve here - a literal pin as written, a track
	// through the register like any dependency - and the driver provisions
	// what this reports into the store.
	if f.Toolchain != nil {
		for _, b := range f.Toolchain.Binaries {
			version := b.Version

			if b.Track != "" {
				resolved, toolNotes, err := c.resolver.ResolveTool(ctx, f, root, b.Track)
				report.Notes = append(report.Notes, toolNotes...)

				if err != nil {
					return Report{}, fmt.Errorf("resolving toolchain binary %s: %w", b.Name, err)
				}

				version = resolved
			}

			report.Toolchain = append(report.Toolchain, toolingcontroller.Binary{
				Name: b.Name, Module: b.Module, Version: version,
			})
		}
	}

	var settle []commandWire

	for _, language := range f.Languages() {
		uri, ok := f.EngineFor(language)
		if !ok {
			return Report{}, fmt.Errorf("no engine is registered for %q", language)
		}

		if err := c.checkLanguage(ctx, uri, language, resolved); err != nil {
			return Report{}, err
		}

		deps, depNotes, err := c.resolver.Resolve(ctx, f, root, language, f.DependenciesFor(language))
		if err != nil {
			return Report{}, fmt.Errorf("resolving %s dependencies: %w", language, err)
		}

		dev, devNotes, err := c.resolver.Resolve(ctx, f, root, language, f.DevFor(language))
		if err != nil {
			return Report{}, fmt.Errorf("resolving %s devDependencies: %w", language, err)
		}

		report.Notes = append(report.Notes, depNotes...)
		report.Notes = append(report.Notes, devNotes...)

		in := renderInput{
			Root:         root,
			Repos:        resolved,
			Dependencies: deps,
			Dev:          dev,
		}

		var out renderOutput

		if err := c.caller.Call(ctx, uri, ToolRender, in, &out); err != nil {
			return Report{}, fmt.Errorf("rendering %s: %w", language, err)
		}

		for _, file := range out.Files {
			if only != "" && !underMemberOrRoot(root, only, file.Path) {
				continue
			}

			if err := c.fs.WriteFile(file.Path, []byte(file.Content)); err != nil {
				return Report{}, fmt.Errorf("writing %s: %w", file.Path, err)
			}

			report.Written = append(report.Written, file.Path)

			if file.Gitignore != "" {
				ignores[file.Gitignore] = append(
					ignores[file.Gitignore],
					append([]string{filepath.Base(file.Path)}, file.AlsoIgnore...)...)
			}
		}

		for _, cmd := range out.Settle {
			if only != "" && cmd.Dir != filepath.Join(root, only) && cmd.Dir != root {
				continue
			}

			settle = append(settle, cmd)
		}
	}

	sort.Strings(report.Written)

	for _, repo := range sortedKeys(ignores) {
		path := filepath.Join(root, repo, ".gitignore")

		changed, err := c.ensureIgnored(path, ignores[repo])
		if err != nil {
			return Report{}, err
		}

		if changed {
			report.Ignored = append(report.Ignored, path)
		}
	}

	if err := c.settle(ctx, settle, &report); err != nil {
		return Report{}, err
	}

	return report, nil
}

// underMemberOrRoot keeps a write inside the one materialised member or at
// the context root itself: the workspace files reference only the member,
// because restrictTo already narrowed the factory.
func underMemberOrRoot(root, only, path string) bool {
	if strings.HasPrefix(path, filepath.Join(root, only)+string(filepath.Separator)) {
		return true
	}

	return filepath.Dir(path) == filepath.Clean(root)
}

// restrictTo narrows the factory to one member. The ephemeral run context
// holds only that member and the register, so rendering the others would
// read identities that are not on disk.
// ensureEnvrcs creates each member's .envrc when missing and keeps exactly
// one managed line present: the PATH entry for the workspace's .forge/bin,
// where sync links the pinned tooling. Everything else in the file is the
// machine's own and stays untouched.
func (c *Controller) ensureEnvrcs(f config.Factory, root string, report *Report) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolving the workspace root: %w", err)
	}

	line := fmt.Sprintf("export PATH=\"%s:$PATH\" # forge-factory: workspace tooling",
		filepath.Join(absRoot, ".forge", "bin"))

	for _, r := range f.Repos {
		envrc := filepath.Join(root, r.Name, ".envrc")

		ok, _ := c.fs.Exists(envrc)
		if !ok {
			if err := c.fs.WriteFile(envrc, []byte(line+"\n")); err != nil {
				return fmt.Errorf("creating %s: %w", envrc, err)
			}

			report.Written = append(report.Written, envrc)

			continue
		}

		raw, err := c.fs.ReadFile(envrc)
		if err != nil {
			return fmt.Errorf("reading %s: %w", envrc, err)
		}

		if strings.Contains(string(raw), "/.forge/bin") {
			continue
		}

		content := string(raw)
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}

		if err := c.fs.WriteFile(envrc, []byte(content+line+"\n")); err != nil {
			return fmt.Errorf("extending %s: %w", envrc, err)
		}

		report.Written = append(report.Written, envrc)
	}

	return nil
}

func restrictTo(f config.Factory, only string) config.Factory {
	if only == "" {
		return f
	}

	repos := []config.Repo{}

	for _, r := range f.Repos {
		if r.Name == only {
			repos = append(repos, r)
		}
	}

	f.Repos = repos

	return f
}

// settle runs what each engine asked for after its files landed. A generated
// go.mod names only the direct requires, so the tidy is what puts the indirect
// ones and the sums back. An optional command that fails is reported and the
// sync still succeeds, because a tidy needs the network and a sync must work
// offline.
func (c *Controller) settle(ctx context.Context, commands []commandWire, report *Report) error {
	for _, cmd := range commands {
		what := cmd.Command + " " + strings.Join(cmd.Args, " ") + " in " + cmd.Dir

		result, err := c.runner.RunEnv(ctx, cmd.Dir, cmd.Env, cmd.Command, cmd.Args...)

		// A command that exits non zero comes back with no error and an exit
		// code. Reading only the error passes every failure.
		if err == nil && result.ExitCode != 0 {
			err = fmt.Errorf("%w: exit %d: %s", ErrCommand, result.ExitCode, tail(result.Stderr))
		}

		if err != nil {
			if !cmd.Optional {
				return fmt.Errorf("running %s: %w", what, err)
			}

			report.Unsettled = append(report.Unsettled, what+": "+err.Error())

			continue
		}

		report.Settled = append(report.Settled, what)
	}

	return nil
}

// checkLanguage refuses an engine registered under the wrong alias, because a
// rust engine behind the go alias would silently render the wrong files. It
// sends a whole input rather than an empty one: a nil slice or map travels as
// null and the engine's own schema refuses it.
func (c *Controller) checkLanguage(
	ctx context.Context,
	uri, alias string,
	repos []repoWire,
) error {
	var out languageOutput

	in := renderInput{
		Repos:        repos,
		Dependencies: map[string]string{},
		Dev:          map[string]string{},
	}

	if err := c.caller.Call(ctx, uri, ToolLanguage, in, &out); err != nil {
		return fmt.Errorf("asking %q what language it speaks: %w", alias, err)
	}

	if out.Language != alias {
		return fmt.Errorf("%w: %q is registered as %q but speaks %q",
			ErrLanguage, uri, alias, out.Language)
	}

	return nil
}

func (c *Controller) resolve(f config.Factory, root string) ([]repoWire, error) {
	out := make([]repoWire, 0, len(f.Repos))

	for _, r := range f.Repos {
		path := filepath.Join(root, r.Name)

		identity, err := c.repos.Identity(path)
		if err != nil {
			return nil, err
		}

		out = append(out, repoWire{
			Name:      r.Name,
			Path:      path,
			Languages: r.Languages,
			Identity:  identity,
		})
	}

	return out, nil
}

// ensureIgnored adds each name to a gitignore without disturbing what is
// already there, and reports whether it changed anything.
func (c *Controller) ensureIgnored(path string, names []string) (bool, error) {
	existing := ""

	exists, err := c.fs.Exists(path)
	if err != nil {
		return false, err
	}

	if exists {
		raw, err := c.fs.ReadFile(path)
		if err != nil {
			return false, err
		}

		existing = string(raw)
	}

	present := map[string]bool{}
	for _, line := range strings.Split(existing, "\n") {
		present[strings.TrimSpace(line)] = true
	}

	var missing []string

	for _, name := range names {
		entry := "/" + name

		if !present[entry] {
			missing = append(missing, entry)
			present[entry] = true
		}
	}

	if len(missing) == 0 {
		return false, nil
	}

	sort.Strings(missing)

	var b strings.Builder

	b.WriteString(existing)

	if existing != "" && !strings.HasSuffix(existing, "\n") {
		b.WriteString("\n")
	}

	if !strings.Contains(existing, gitignoreHeader) {
		b.WriteString("\n" + gitignoreHeader + "\n")
	}

	for _, entry := range missing {
		b.WriteString(entry + "\n")
	}

	if err := c.fs.WriteFile(path, []byte(b.String())); err != nil {
		return false, err
	}

	return true, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
