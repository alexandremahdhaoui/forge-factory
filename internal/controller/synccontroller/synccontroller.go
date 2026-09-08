package synccontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/engineadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/repoadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/resolvecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/runtimecontroller"
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

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return "..." + s[len(s)-400:]
	}

	return s
}

type Report struct {
	Root      string                     `json:"root"`
	Written   []string                   `json:"written"`
	Ignored   []string                   `json:"ignored"`
	Locked    []string                   `json:"locked"`
	Unlocked  []string                   `json:"unlocked"`
	Notes     []string                   `json:"notes,omitempty"`
	Toolchain []toolingcontroller.Binary `json:"toolchain,omitempty"`
	Runtimes  []runtimecontroller.Pin    `json:"runtimes,omitempty"`
	Image     string                     `json:"image,omitempty"`
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
	Files          []fileWire    `json:"files"`
	DependencyLock []commandWire `json:"dependencyLock,omitempty"`
	Lockfiles      []string      `json:"lockfiles,omitempty"`
}

type languageOutput struct {
	Language string `json:"language"`
}

type Syncer interface {
	Sync(ctx context.Context, f config.Factory, root, only string) (Report, error)
	Lock(ctx context.Context, f config.Factory, root, only string) (Report, error)
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

func (c *Controller) Sync(ctx context.Context, f config.Factory, root, only string) (Report, error) {
	f = restrictTo(f, only)

	report := Report{
		Root:     root,
		Written:  []string{},
		Ignored:  []string{},
		Locked:   []string{},
		Unlocked: []string{},
	}

	ignores := map[string][]string{}

	if err := c.ensureEnvrcs(f, root, &report); err != nil {
		return Report{}, err
	}

	for _, r := range f.Repos {
		ignores[r.Name] = append(ignores[r.Name], ".envrc")
	}

	if f.Toolchain != nil {
		for _, name := range sortedKeys(f.Toolchain.Runtimes) {
			r := f.Toolchain.Runtimes[name]
			version := r.Version

			if r.Track != "" {
				resolved, runtimeNotes, err := c.resolver.ResolveTool(ctx, f, root, r.Track)
				report.Notes = append(report.Notes, runtimeNotes...)

				if err != nil {
					return Report{}, fmt.Errorf("resolving runtime %s: %w", name, err)
				}

				version = resolved
			}

			report.Runtimes = append(report.Runtimes, runtimecontroller.Pin{
				Name: name, Version: version, Params: r.Params, Targets: r.Targets,
			})
		}

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

		if img := f.Toolchain.Image; img != nil {
			version := img.Version

			if img.Track != "" {
				resolved, imgNotes, err := c.resolver.ResolveTool(ctx, f, root, img.Track)
				report.Notes = append(report.Notes, imgNotes...)

				if err != nil {
					return Report{}, fmt.Errorf("resolving the toolchain image: %w", err)
				}

				version = resolved
			}

			report.Image = img.Ref + ":" + version

			changed, err := c.writeIfChanged(
				filepath.Join(root, filepath.FromSlash(ToolchainImagePath)),
				[]byte(report.Image+"\n"))
			if err != nil {
				return Report{}, fmt.Errorf("persisting the toolchain image: %w", err)
			}

			if changed {
				report.Written = append(report.Written, ToolchainImagePath)
			}
		}
	}

	_, lockfiles, generated, err := c.renderMembers(ctx, f, root, only, &report, true, ignores)
	if err != nil {
		return Report{}, err
	}

	if err := c.recordGenerated(root, append(append([]string{}, generated...), lockfiles...), &report); err != nil {
		return Report{}, err
	}

	// The factory's engines, by alias, for every member to consult: the
	// outer ring of forge's engine registry. A member's own forge.yaml entry
	// wins over one here; a name in neither falls through to forge's own
	// module. Written only when the bytes moved, like everything else here.
	if len(f.Engines) > 0 {
		changed, err := c.writeIfChanged(
			filepath.Join(root, filepath.FromSlash(EnginesPath)),
			RenderEngines(f.Engines))
		if err != nil {
			return Report{}, fmt.Errorf("persisting the engine registry: %w", err)
		}

		if changed {
			report.Written = append(report.Written, EnginesPath)
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

	return report, nil
}

func (c *Controller) Lock(ctx context.Context, f config.Factory, root, only string) (Report, error) {
	f = restrictTo(f, only)

	report := Report{
		Root:     root,
		Written:  []string{},
		Ignored:  []string{},
		Locked:   []string{},
		Unlocked: []string{},
	}

	commands, lockfiles, generated, err := c.renderMembers(ctx, f, root, only, &report, false, nil)
	if err != nil {
		return Report{}, err
	}

	if err := c.recordGenerated(root, append(append([]string{}, generated...), lockfiles...), &report); err != nil {
		return Report{}, err
	}

	if err := c.lock(ctx, commands, &report); err != nil {
		return Report{}, err
	}

	if err := c.recordLockfiles(root, lockfiles, &report); err != nil {
		return Report{}, err
	}

	return report, nil
}

const LockManifestPath = ".forge/dependency-locks.json"

const ToolchainImagePath = ".forge/toolchain-image"

// EnginesPath is where sync writes the factory's engines by alias, the file
// forge reads as the outer ring of its engine registry.
const EnginesPath = ".forge/engines.yaml"

// RenderEngines is the registry document: one alias to engine pair per
// factory engine, in the factory's order, and nothing a member did not
// declare in its factory.
func RenderEngines(engines []config.Engine) []byte {
	var b bytes.Buffer

	b.WriteString("# Generated by forge-factory sync. DO NOT EDIT.\n")
	b.WriteString("# The factory's engines by alias: the outer ring of every member's engine\n")
	b.WriteString("# registry. A member's own forge.yaml entry wins over one here.\n")
	b.WriteString("engines:\n")

	for _, e := range engines {
		fmt.Fprintf(&b, "  - alias: %s\n    engine: %s\n", e.Alias, e.Engine)
	}

	return b.Bytes()
}

const GeneratedManifestPath = ".forge/factory-generated.json"

func (c *Controller) writeIfChanged(target string, data []byte) (bool, error) {
	if existing, err := c.fs.ReadFile(target); err == nil && bytes.Equal(existing, data) {
		return false, nil
	}

	if err := c.fs.MkdirAll(filepath.Dir(target)); err != nil {
		return false, err
	}

	if err := c.fs.WriteFile(target, data); err != nil {
		return false, err
	}

	return true, nil
}

type lockManifest struct {
	Version int          `json:"version"`
	Locks   []lockedFile `json:"locks"`
}

type generatedManifest struct {
	Version int      `json:"version"`
	Files   []string `json:"files"`
}

func (c *Controller) recordGenerated(root string, paths []string, report *Report) error {
	seen := map[string]bool{}
	manifest := generatedManifest{Version: 1, Files: []string{}}

	for _, path := range paths {
		rel := relative(root, path)
		if rel == "" || seen[rel] {
			continue
		}

		seen[rel] = true
		manifest.Files = append(manifest.Files, rel)
	}

	sort.Strings(manifest.Files)

	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("recording generated files: %w", err)
	}

	target := filepath.Join(root, filepath.FromSlash(GeneratedManifestPath))

	changed, err := c.writeIfChanged(target, append(raw, '\n'))
	if err != nil {
		return fmt.Errorf("recording generated files: %w", err)
	}

	if changed {
		report.Written = append(report.Written, target)
	}

	return nil
}

type lockedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func (c *Controller) recordLockfiles(root string, lockfiles []string, report *Report) error {
	manifest := lockManifest{Version: 1, Locks: []lockedFile{}}

	for _, path := range lockfiles {
		exists, err := c.fs.Exists(path)
		if err != nil {
			return fmt.Errorf("recording lockfile %s: %w", path, err)
		}

		if !exists {
			report.Notes = append(report.Notes,
				fmt.Sprintf("no lockfile at %s; it is not recorded", relative(root, path)))

			continue
		}

		raw, err := c.fs.ReadFile(path)
		if err != nil {
			return fmt.Errorf("recording lockfile %s: %w", path, err)
		}

		sum := sha256.Sum256(raw)
		manifest.Locks = append(manifest.Locks, lockedFile{
			Path:   relative(root, path),
			SHA256: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(manifest.Locks, func(i, j int) bool {
		return manifest.Locks[i].Path < manifest.Locks[j].Path
	})

	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("recording lockfiles: %w", err)
	}

	target := filepath.Join(root, filepath.FromSlash(LockManifestPath))
	if err := c.fs.WriteFile(target, append(raw, '\n')); err != nil {
		return fmt.Errorf("recording lockfiles: %w", err)
	}

	report.Written = append(report.Written, target)

	return nil
}

func relative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}

	return filepath.ToSlash(rel)
}

func (c *Controller) renderMembers(
	ctx context.Context,
	f config.Factory,
	root, only string,
	report *Report,
	write bool,
	ignores map[string][]string,
) ([]commandWire, []string, []string, error) {
	resolved, err := c.resolve(f, root)
	if err != nil {
		return nil, nil, nil, err
	}

	var (
		lockCommands []commandWire
		lockfiles    []string
		generated    []string
	)

	for _, language := range f.Languages() {
		uri, ok := f.EngineFor(language)
		if !ok {
			return nil, nil, nil, fmt.Errorf("no engine is registered for %q", language)
		}

		if err := c.checkLanguage(ctx, uri, language, resolved); err != nil {
			return nil, nil, nil, err
		}

		deps, depNotes, err := c.resolver.Resolve(ctx, f, root, language, f.DependenciesFor(language))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolving %s dependencies: %w", language, err)
		}

		dev, devNotes, err := c.resolver.Resolve(ctx, f, root, language, f.DevFor(language))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolving %s devDependencies: %w", language, err)
		}

		members, err := c.resolver.ResolveMembers(ctx, f, root, language)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolving %s member modules: %w", language, err)
		}

		required := c.internalRequires(resolved, language)

		for name, version := range members {
			if _, declared := deps[name]; declared {
				continue
			}

			if !required[name] {
				continue
			}

			deps[name] = version
			report.Notes = append(report.Notes, fmt.Sprintf(
				"%s:%s pinned to %s by the register's internal track", language, name, version))
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
			return nil, nil, nil, fmt.Errorf("rendering %s: %w", language, err)
		}

		for _, file := range out.Files {
			if only != "" && !underMemberOrRoot(root, only, file.Path) {
				continue
			}

			generated = append(generated, file.Path)
		}

		if write {
			for _, file := range out.Files {
				if only != "" && !underMemberOrRoot(root, only, file.Path) {
					continue
				}

				if err := c.fs.WriteFile(file.Path, []byte(file.Content)); err != nil {
					return nil, nil, nil, fmt.Errorf("writing %s: %w", file.Path, err)
				}

				report.Written = append(report.Written, file.Path)

				if file.Gitignore != "" {
					ignores[file.Gitignore] = append(
						ignores[file.Gitignore],
						append([]string{filepath.Base(file.Path)}, file.AlsoIgnore...)...)
				}
			}
		}

		for _, cmd := range out.DependencyLock {
			if only != "" && cmd.Dir != filepath.Join(root, only) && cmd.Dir != root {
				continue
			}

			lockCommands = append(lockCommands, cmd)
		}

		for _, lf := range out.Lockfiles {
			if only != "" && !underMemberOrRoot(root, only, lf) {
				continue
			}

			lockfiles = append(lockfiles, lf)
		}
	}

	return lockCommands, lockfiles, generated, nil
}

func underMemberOrRoot(root, only, path string) bool {
	if strings.HasPrefix(path, filepath.Join(root, only)+string(filepath.Separator)) {
		return true
	}

	return filepath.Dir(path) == filepath.Clean(root)
}

func (c *Controller) internalRequires(repos []repoWire, language string) map[string]bool {
	out := map[string]bool{}

	if language != "go" {
		return out
	}

	for _, r := range repos {
		if !slices.Contains(r.Languages, language) {
			continue
		}

		raw, err := c.fs.ReadFile(filepath.Join(r.Path, "go.mod"))
		if err != nil {
			continue
		}

		for module := range requiredModules(string(raw)) {
			out[module] = true
		}
	}

	return out
}

func requiredModules(gomod string) map[string]bool {
	out := map[string]bool{}
	inRequire := false

	for _, line := range strings.Split(gomod, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}

		if inRequire {
			if line == ")" {
				inRequire = false

				continue
			}

			addRequired(out, line)

			continue
		}

		if line == "require (" {
			inRequire = true

			continue
		}

		if rest, ok := strings.CutPrefix(line, "require "); ok {
			addRequired(out, rest)
		}
	}

	return out
}

func addRequired(out map[string]bool, line string) {
	module, _, _ := strings.Cut(strings.TrimSpace(line), " ")
	if strings.Contains(module, "/") {
		out[module] = true
	}
}

const (
	envrcBegin = "# BEGIN forge-factory"
	envrcEnd   = "# END forge-factory"
)

func (c *Controller) ensureEnvrcs(f config.Factory, root string, report *Report) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolving the workspace root: %w", err)
	}

	envFile := filepath.Join(absRoot, filepath.FromSlash(runtimecontroller.EnvPath))
	block := fmt.Sprintf("%s\nexport PATH=\"%s:$PATH\"\nif [ -f %q ]; then . %q; fi\n%s\n",
		envrcBegin, filepath.Join(absRoot, ".forge", "bin"), envFile, envFile, envrcEnd)

	for _, r := range f.Repos {
		envrc := filepath.Join(root, r.Name, ".envrc")

		var content string

		if ok, _ := c.fs.Exists(envrc); ok {
			raw, err := c.fs.ReadFile(envrc)
			if err != nil {
				return fmt.Errorf("reading %s: %w", envrc, err)
			}

			content = string(raw)
		}

		next := withEnvrcBlock(content, block)
		if next == content {
			continue
		}

		if err := c.fs.WriteFile(envrc, []byte(next)); err != nil {
			return fmt.Errorf("writing %s: %w", envrc, err)
		}

		report.Written = append(report.Written, envrc)
	}

	return nil
}

func withEnvrcBlock(content, block string) string {
	begin := strings.Index(content, envrcBegin)
	if begin >= 0 {
		if end := strings.Index(content[begin:], envrcEnd); end >= 0 {
			tail := begin + end + len(envrcEnd)
			if strings.HasPrefix(content[tail:], "\n") {
				tail++
			}

			return content[:begin] + block + content[tail:]
		}
	}

	if content == "" {
		return block
	}

	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	return block + content
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

func (c *Controller) lock(ctx context.Context, commands []commandWire, report *Report) error {
	for _, cmd := range commands {
		what := cmd.Command + " " + strings.Join(cmd.Args, " ") + " in " + cmd.Dir

		result, err := c.runner.RunEnv(ctx, cmd.Dir, cmd.Env, cmd.Command, cmd.Args...)

		if err == nil && result.ExitCode != 0 {
			err = fmt.Errorf("%w: exit %d: %s", ErrCommand, result.ExitCode, tail(result.Stderr))
		}

		if err != nil {
			if !cmd.Optional {
				return fmt.Errorf("running %s: %w", what, err)
			}

			report.Unlocked = append(report.Unlocked, what+": "+err.Error())

			continue
		}

		report.Locked = append(report.Locked, what)
	}

	return nil
}

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
