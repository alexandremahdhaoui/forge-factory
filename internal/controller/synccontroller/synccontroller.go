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
	Root    string   `json:"root"`
	Written []string `json:"written"`
	Ignored []string `json:"ignored"`
	// Locked is each dependency-lock command that ran clean; Unlocked is each
	// optional one that failed, with why. The vocabulary is deliberate: what
	// these commands resolve is a language's dependency closure into its
	// lockfile, and "tidy" is one language's word for it.
	Locked   []string `json:"locked"`
	Unlocked []string `json:"unlocked"`
	Notes    []string `json:"notes,omitempty"`
	// Toolchain is every declared binary resolved to its pinned version;
	// the driver provisions them into the store.
	Toolchain []toolingcontroller.Binary `json:"toolchain,omitempty"`
	// Runtimes is every declared runtime resolved to its pinned version;
	// the driver provisions them before the binaries, because building a
	// binary needs the runtime that compiles it.
	Runtimes []runtimecontroller.Pin `json:"runtimes,omitempty"`
	// Image is the resolved toolchain container reference, tag included,
	// when the factory declares one. Sync persists it into
	// ToolchainImagePath for the CI layer to read.
	Image string `json:"image,omitempty"`
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

// Syncer is what a driver accepts. It is declared here, in the package that
// implements it, as golden-go does.
type Syncer interface {
	// Sync writes every member's manifest and stops: resolving the
	// dependency closure is Lock's job, because cloning is not building.
	// Only restricts writes to one member - the ephemeral run context uses
	// it, where the other members are not on disk at all. Empty means
	// everything.
	Sync(ctx context.Context, f config.Factory, root, only string) (Report, error)
	// Lock resolves every member's dependency closure by running the
	// commands its language engine declares - the manifests sync wrote name
	// only the direct requires, and these put the indirect ones and the
	// integrity sums back. The build phase calls it; a clone never does.
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

// Sync asks every language engine what to write, writes it, and keeps each
// repo's gitignore in step. A version is written in the factory and nowhere
// else, so everything written here is ignored by git.
//
// Sync never runs the engines' dependency-lock commands: cloning is not
// building, and a workspace stood up offline must come up clean. Lock is
// the verb that resolves the closure, and the build phase owns calling it.
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

	// forge sources a .envrc in every repo it builds, so the file must
	// exist; its content is the machine's own (gitignored). Creating it here
	// retires the touch-loop every fresh workspace used to need. One managed
	// block is kept present in every file - the workspace tooling PATH entry,
	// which puts the pinned store's view ahead of whatever a user installed -
	// and the rest of the file is never touched.
	//
	// The ignore travels with it. This writes a file into somebody's checkout
	// that holds their machine's own environment, and the .gitignore lines
	// keeping it out of git were hand-written per repo - so a member added
	// without one got an untracked .envrc that a careless `git add -A`
	// commits, secrets and all.
	if err := c.ensureEnvrcs(f, root, &report); err != nil {
		return Report{}, err
	}

	for _, r := range f.Repos {
		ignores[r.Name] = append(ignores[r.Name], ".envrc")
	}

	// Toolchain binaries resolve here - a literal pin as written, a track
	// through the register like any dependency - and the driver provisions
	// what this reports into the store.
	if f.Toolchain != nil {
		// Runtimes resolve the same way, and first: a binary's `go install`
		// needs the go the runtimes provision.
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
				Name: name, Version: version, Params: r.Params,
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

		// The image resolves like a binary and lands in a generated file,
		// so a pipeline reads the pin instead of someone typing it. The
		// write is change-aware: an unchanged pin reports nothing.
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

	// What this factory generates is recorded beside the other root
	// contracts: every rendered file plus every declared lockfile,
	// root-relative. forge-ci reads it to keep factory churn out of the
	// revision's dirty measurement - the register moves these files by
	// design, and only a language engine knows their names.
	if err := c.recordGenerated(root, append(append([]string{}, generated...), lockfiles...), &report); err != nil {
		return Report{}, err
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

// Lock resolves every member's dependency closure by running the commands
// its language engine declares. The engines are rendered again to learn the
// commands and nothing is written: the manifests on disk are whatever the
// last sync produced, and a lock over drifted manifests would only prove
// the wrong thing louder.
//
// An optional command that fails lands in Unlocked and the caller decides -
// the driver refuses unless it was told the machine is offline on purpose.
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

	// What resolved is recorded: each lockfile the engines declared, hashed,
	// into one manifest the build phase can hand to whoever keys state by
	// revision. The names mean nothing here - only a language engine knows
	// what a lockfile is, and this side only proves which bytes were locked.
	if err := c.recordLockfiles(root, lockfiles, &report); err != nil {
		return Report{}, err
	}

	return report, nil
}

// LockManifest is the recorded outcome of a lock: each resolved lockfile,
// root-relative, with the hash of the exact bytes. The path is a contract
// with forge-ci, which reads it to store one dependency-lock record per file
// beside the revision.
const LockManifestPath = ".forge/dependency-locks.json"

// ToolchainImagePath is where sync persists the resolved toolchain
// container reference, root-relative. The CI layer reads the pin from here,
// which is what keeps it out of hand-written pipeline files.
const ToolchainImagePath = ".forge/toolchain-image"

// GeneratedManifestPath is where sync records every file this factory
// generates - each rendered manifest and each declared lockfile,
// root-relative. The path is a contract with forge-ci, which excludes
// exactly these files from a repo's dirty measurement: the register moves
// them by design, so their churn is the system working, not drift.
const GeneratedManifestPath = ".forge/factory-generated.json"

// writeIfChanged writes target only when the bytes differ, and answers
// whether it did - a sync that converges nothing must report nothing.
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

// recordGenerated writes the factory-generated file list, root-relative,
// sorted and deduplicated, only when the bytes changed.
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

		// An optional command that could not run leaves no lockfile, and an
		// absent file is reported rather than recorded: a manifest entry
		// nothing hashed would be a claim about bytes nobody saw.
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

// relative answers path relative to root, falling back to the path itself
// when it does not resolve - a report line beats an error for a display name.
func relative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}

	return filepath.ToSlash(rel)
}

// renderMembers runs every language engine's render and answers the
// dependency-lock commands it declared. With write set the files land on
// disk and their gitignore entries accumulate in ignores; without it the
// render is only the vehicle for learning the commands.
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

		// A member module another member imports - a shared spec - is
		// declared in no dependency list, so its version was left to the
		// lock that follows, which takes the highest published tag. The
		// register's internal track is what governs it.
		members, err := c.resolver.ResolveMembers(ctx, f, root, language)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolving %s member modules: %w", language, err)
		}

		required := c.internalRequires(resolved, language)

		for name, version := range members {
			// A declared dependency wins. Naming a module in the factory is
			// a deliberate act and the internal track must not override it.
			if _, declared := deps[name]; declared {
				continue
			}

			// Only a module some member already requires. Every runnable
			// member has an internal track too, carrying the pipeline's own
			// dev label: a real revision, but not a tag any proxy can
			// fetch. Writing one into a manifest makes the lock that follows
			// try to resolve it and fail, because a require has to resolve
			// before it can be pruned as unused.
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

// underMemberOrRoot keeps a write inside the one materialised member or at
// the context root itself: the workspace files reference only the member,
// because restrictTo already narrowed the factory.
func underMemberOrRoot(root, only, path string) bool {
	if strings.HasPrefix(path, filepath.Join(root, only)+string(filepath.Separator)) {
		return true
	}

	return filepath.Dir(path) == filepath.Clean(root)
}

// internalRequires answers the module paths this language's members already
// require. The generated manifest is rewritten on every sync, so the
// committed one is where a member records what it actually imports.
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

// requiredModules reads the module paths a go.mod requires.
//
// Only a require line counts, and only inside a require block or behind the
// require keyword. That sounds obvious and it is the whole fix: the scanner
// this replaces trimmed an optional "require " prefix and then took any line
// holding a slash, so every module path in a replace or exclude block read
// as a require.
//
// The blast radius is why it is parsed rather than grepped. The answer is
// merged into one per-language map that every member's manifest is rendered
// from, so one replace block in one member writes its right-hand side into
// every member, and every following tidy fails on a version no proxy can
// serve. Six manifests broke that way once.
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

		// Any other block - replace, exclude, tool - falls through. Its
		// body lines carry no require keyword, so nothing reads them, and
		// its closing paren cannot end a require block that never opened.
		if rest, ok := strings.CutPrefix(line, "require "); ok {
			addRequired(out, rest)
		}
	}

	return out
}

// addRequired records the module a require line names. The right-hand side
// is dropped, so an // indirect marker still counts: indirect is how a
// module is required, not whether.
func addRequired(out map[string]bool, line string) {
	module, _, _ := strings.Cut(strings.TrimSpace(line), " ")
	if strings.Contains(module, "/") {
		out[module] = true
	}
}

// The markers that fence off the block this tool owns. Everything between
// them is rewritten on every sync; everything outside is the machine's own
// and is never read, moved or removed.
const (
	envrcBegin = "# BEGIN forge-factory"
	envrcEnd   = "# END forge-factory"
)

// ensureEnvrcs keeps the managed block present in each member's .envrc: the
// PATH entry for the workspace's .forge/bin, where sync links the pinned
// tooling. Everything outside the markers is the machine's own and stays
// untouched.
func (c *Controller) ensureEnvrcs(f config.Factory, root string, report *Report) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolving the workspace root: %w", err)
	}

	// The env file is where runtime provisioning composes each runtime's
	// environment - JAVA_HOME and its kind. Sourced conditionally: it does
	// not exist until the first provision, and a workspace with no declared
	// runtimes never writes it. An if, not `[ -f ] && .`: that form's exit
	// status is 1 when the file is absent, and when the block is the last
	// thing in .envrc that status is the file's - forge refused to source
	// every .envrc a fresh runner had, in a factory with no runtimes
	// (forge-self run 87).
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

// withEnvrcBlock puts the managed block into content, wherever the reader
// chose to keep it, and leaves every other line alone.
//
// This used to append the line once and then skip any file mentioning
// "/.forge/bin" anywhere - so a workspace that moved kept a stale absolute
// path forever, and an unrelated mention in a comment suppressed the write
// entirely. Rewriting between markers is what makes the call site's claim
// true: one managed line is KEPT present, not written once and abandoned.
//
// An unmatched BEGIN is treated as absent and a fresh block is prepended.
// Nothing this function did not write is ever deleted.
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

	// Prepended, so the workspace tooling wins over whatever a machine puts
	// on PATH later in its own file.
	return block + content
}

// restrictTo narrows the factory to one member. The ephemeral run context
// holds only that member and the register, so rendering the others would
// read identities that are not on disk.
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

// lock runs each engine's dependency-lock commands after its files landed. A
// generated manifest names only the direct requires, so these are what put
// the indirect ones and the integrity sums back. An optional command that
// fails is reported and the sync still succeeds, because resolving a closure
// needs the network and a sync must work offline.
func (c *Controller) lock(ctx context.Context, commands []commandWire, report *Report) error {
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

			report.Unlocked = append(report.Unlocked, what+": "+err.Error())

			continue
		}

		report.Locked = append(report.Locked, what)
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
