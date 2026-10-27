package synccontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	"github.com/alexandremahdhaoui/forge-factory/internal/util/gomod"
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

	ErrHandEdited    = errors.New("a rendered file differs from the last render")
	ErrNotRendered   = errors.New("forge-factory renders no such file")
	ErrNothingToTake = errors.New("the file is not on disk")
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
	Merged     bool     `json:"merged,omitempty"`
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
	Take(root, file string) (TakeReport, error)
}

type TakeReport struct {
	Path   string `json:"path"`
	Backup string `json:"backup"`
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

	var missing unregistered

	if err := c.resolveToolchain(ctx, f, root, &report, &missing); err != nil {
		return Report{}, err
	}

	rendered, err := c.renderMembers(ctx, f, root, only, &report, true, ignores, &missing)
	if err != nil {
		return Report{}, err
	}

	lockErr := c.lock(ctx, rendered.settling, &report)

	settled, err := c.recordOnDisk(root, rendered.readBack(), rendered.declared)
	if err != nil {
		return Report{}, err
	}

	maps.Copy(rendered.digests, settled)

	if err := c.recordGenerated(root, rendered.recorded(), rendered.digests, &report); err != nil {
		return Report{}, err
	}

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

	if lockErr != nil {
		return Report{}, lockErr
	}

	if err := missing.err(); err != nil {
		return Report{}, err
	}

	return report, nil
}

type unregistered []error

func (u *unregistered) absorb(err error) error {
	if !errors.Is(err, resolvecontroller.ErrUnregistered) {
		return err
	}

	*u = append(*u, err)

	return nil
}

func (u unregistered) err() error {
	if len(u) == 0 {
		return nil
	}

	return fmt.Errorf("resolving every register entry the factory names:\n%w", errors.Join(u...))
}

func (c *Controller) resolveToolchain(ctx context.Context, f config.Factory, root string, report *Report, missing *unregistered) error {
	if f.Toolchain == nil {
		return nil
	}

	for _, name := range sortedKeys(f.Toolchain.Runtimes) {
		r := f.Toolchain.Runtimes[name]

		version, err := c.trackedVersion(ctx, f, root, r.Version, r.Track, report)
		if err != nil {
			if err := missing.absorb(fmt.Errorf("resolving runtime %s: %w", name, err)); err != nil {
				return err
			}

			continue
		}

		report.Runtimes = append(report.Runtimes, runtimecontroller.Pin{
			Name: name, Version: version, Params: r.Params, Targets: r.Targets,
		})
	}

	for _, b := range f.Toolchain.Binaries {
		version, err := c.trackedVersion(ctx, f, root, b.Version, b.Track, report)
		if err != nil {
			if err := missing.absorb(fmt.Errorf("resolving toolchain binary %s: %w", b.Name, err)); err != nil {
				return err
			}

			continue
		}

		report.Toolchain = append(report.Toolchain, toolingcontroller.Binary{
			Name: b.Name, Module: b.Module, Version: version,
		})
	}

	img := f.Toolchain.Image
	if img == nil {
		return nil
	}

	version, err := c.trackedVersion(ctx, f, root, img.Version, img.Track, report)
	if err != nil {
		return missing.absorb(fmt.Errorf("resolving the toolchain image: %w", err))
	}

	report.Image = img.Ref + ":" + version

	changed, err := c.writeIfChanged(
		filepath.Join(root, filepath.FromSlash(ToolchainImagePath)),
		[]byte(report.Image+"\n"))
	if err != nil {
		return fmt.Errorf("persisting the toolchain image: %w", err)
	}

	if changed {
		report.Written = append(report.Written, ToolchainImagePath)
	}

	return nil
}

func (c *Controller) trackedVersion(ctx context.Context, f config.Factory, root, version, track string, report *Report) (string, error) {
	if track == "" {
		return version, nil
	}

	resolved, notes, err := c.resolver.ResolveTool(ctx, f, root, track)
	report.Notes = append(report.Notes, notes...)

	return resolved, err
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

	var missing unregistered

	rendered, err := c.renderMembers(ctx, f, root, only, &report, false, nil, &missing)
	if err != nil {
		return Report{}, err
	}

	if err := missing.err(); err != nil {
		return Report{}, err
	}

	if err := c.guardRendered(root, rendered); err != nil {
		return Report{}, err
	}

	lockErr := c.lock(ctx, rendered.commands, &report)

	entries, err := c.recordOnDisk(root, rendered.generated, rendered.declared)
	if err != nil {
		return Report{}, err
	}

	if err := c.recordGenerated(root, rendered.recorded(), entries, &report); err != nil {
		return Report{}, err
	}

	if lockErr != nil {
		return Report{}, lockErr
	}

	if err := c.recordLockfiles(root, rendered.lockfiles, &report); err != nil {
		return Report{}, err
	}

	return report, nil
}

const LockManifestPath = ".forge/dependency-locks.json"

const ToolchainImagePath = ".forge/toolchain-image"

const EnginesPath = ".forge/engines.yaml"

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
	Version int            `json:"version"`
	Files   []RenderedFile `json:"files"`
}

type RenderedFile struct {
	Path     string            `json:"path"`
	SHA256   string            `json:"sha256,omitempty"`
	Versions map[string]string `json:"versions,omitempty"`
}

func (f *RenderedFile) UnmarshalJSON(raw []byte) error {
	var path string
	if err := json.Unmarshal(raw, &path); err == nil {
		f.Path = path

		return nil
	}

	type plain RenderedFile

	var entry plain
	if err := json.Unmarshal(raw, &entry); err != nil {
		return fmt.Errorf("reading a rendered file entry: %w", err)
	}

	*f = RenderedFile(entry)

	return nil
}

type Record map[string]RenderedFile

func Digest(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func ReadRecord(fs fsadapter.FS, root string) (Record, error) {
	target := filepath.Join(root, filepath.FromSlash(GeneratedManifestPath))

	exists, err := fs.Exists(target)
	if err != nil {
		return nil, fmt.Errorf("reading the render record at %s: %w", target, err)
	}

	if !exists {
		return Record{}, nil
	}

	raw, err := fs.ReadFile(target)
	if err != nil {
		return nil, fmt.Errorf("reading the render record at %s: %w", target, err)
	}

	var manifest generatedManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("reading the render record at %s: %w", target, err)
	}

	out := make(Record, len(manifest.Files))
	for _, entry := range manifest.Files {
		out[entry.Path] = entry
	}

	return out, nil
}

func WriteRendered(fs fsadapter.FS, root, path string, data []byte, record Record) (string, error) {
	if err := writeGuarded(fs, root, path, data, record, nil); err != nil {
		return "", err
	}

	return Digest(data), nil
}

func writeGuarded(fs fsadapter.FS, root, path string, data []byte, record Record, declared map[string]map[string]string) error {
	if err := guard(fs, root, path, record, declared); err != nil {
		return err
	}

	if err := fs.WriteFile(path, data); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}

func guard(fs fsadapter.FS, root, path string, record Record, declared map[string]map[string]string) error {
	recorded := record[relative(root, path)]

	if modules, merged := declared[path]; merged {
		return guardDeclaredVersions(fs, root, path, recorded.Versions, modules)
	}

	return guardWholeFile(fs, root, path, recorded.SHA256)
}

func guardWholeFile(fs fsadapter.FS, root, path, recorded string) error {
	if recorded == "" {
		return nil
	}

	existing, found, err := beforeRendering(fs, path)
	if err != nil || !found {
		return err
	}

	if Digest(existing) == recorded {
		return nil
	}

	return handEditedError(root, path)
}

func guardDeclaredVersions(fs fsadapter.FS, root, path string, recorded, declared map[string]string) error {
	if len(recorded) == 0 {
		return nil
	}

	existing, found, err := beforeRendering(fs, path)
	if err != nil || !found {
		return err
	}

	onDisk, err := gomod.Requires(string(existing))
	if err != nil {
		return fmt.Errorf("reading the declared versions of %s before rendering over it: %w", path, err)
	}

	var moved []string

	for _, module := range sortedKeys(recorded) {
		if movedByHand(module, onDisk, recorded, declared) {
			moved = append(moved, fmt.Sprintf("  %s: the record holds %s, the copy on disk requires %s",
				module, recorded[module], onDisk[module]))
		}
	}

	if len(moved) == 0 {
		return nil
	}

	return declaredVersionMovedError(root, path, moved)
}

func movedByHand(module string, onDisk, recorded, declared map[string]string) bool {
	version, required := onDisk[module]
	now, stillDeclared := declared[module]

	return stillDeclared && required && version != recorded[module] && version != now
}

func (c *Controller) declaredVersionsMoved(root, path string, data []byte, declared map[string]map[string]string) ([]string, error) {
	modules, merged := declared[path]
	if !merged {
		return nil, nil
	}

	existing, found, err := beforeRendering(c.fs, path)
	if err != nil || !found {
		return nil, err
	}

	onDisk, err := gomod.Requires(string(existing))
	if err != nil {
		return nil, fmt.Errorf("reading the declared versions of %s before rendering over it: %w", path, err)
	}

	written, err := gomod.Requires(string(data))
	if err != nil {
		return nil, fmt.Errorf("reading the declared versions the render writes into %s: %w", path, err)
	}

	var notes []string

	for _, module := range sortedKeys(modules) {
		before, wasRequired := onDisk[module]
		after, isRequired := written[module]

		if wasRequired && isRequired && before != after {
			notes = append(notes, fmt.Sprintf("%s: the sync moved %s from %s on disk to %s, the version forge-factory declares",
				relative(root, path), module, before, after))
		}
	}

	return notes, nil
}

func beforeRendering(fs fsadapter.FS, path string) ([]byte, bool, error) {
	exists, err := fs.Exists(path)
	if err != nil {
		return nil, false, fmt.Errorf("inspecting %s before rendering over it: %w", path, err)
	}

	if !exists {
		return nil, false, nil
	}

	existing, err := fs.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("reading %s before rendering over it: %w", path, err)
	}

	return existing, true, nil
}

func declaredVersionMovedError(root, path string, moved []string) error {
	rel := relative(root, path)

	return fmt.Errorf(
		"%w: %s\n"+
			"forge-factory sets the version of every module it declares in this file, and a declared version on disk moved by hand.\n"+
			"%s\n"+
			"Writing the render would lose that edit, so nothing was written.\n"+
			"Take keeps the file on disk as %s.bak, and the next sync sets every declared version again.\n"+
			"To keep the edit, declare that version in forge-factory.yaml before you take the render.\n"+
			"Run: forge-factory take %s",
		ErrHandEdited, rel, strings.Join(moved, "\n"), rel, rel)
}

func handEditedError(root, path string) error {
	rel := relative(root, path)

	return fmt.Errorf(
		"%w: %s\n"+
			"forge-factory renders this file, and the copy on disk is not the one it last wrote.\n"+
			"Somebody edited it by hand. Writing the render would lose that edit, so nothing was written.\n"+
			"Two ways forward.\n"+
			"  Keep the edit. Move it into the factory repo, under workspace/, then sync again.\n"+
			"  Take the render, and keep the file on disk as %s.bak. Run: forge-factory take %s",
		ErrHandEdited, rel, rel, rel)
}

func WriteRecord(fs fsadapter.FS, root string, paths []string, entries Record) (string, bool, error) {
	previous, err := ReadRecord(fs, root)
	if err != nil {
		return "", false, err
	}

	seen := map[string]bool{}
	manifest := generatedManifest{Version: 1, Files: []RenderedFile{}}

	for _, path := range paths {
		rel := relative(root, path)
		if rel == "" || seen[rel] {
			continue
		}

		seen[rel] = true

		entry, known := entries[rel]
		if !known {
			entry = previous[rel]
		}

		entry.Path = rel
		manifest.Files = append(manifest.Files, entry)
	}

	sort.Slice(manifest.Files, func(i, j int) bool {
		return manifest.Files[i].Path < manifest.Files[j].Path
	})

	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", false, fmt.Errorf("recording the render of %s: %w", root, err)
	}

	target := filepath.Join(root, filepath.FromSlash(GeneratedManifestPath))
	data := append(raw, '\n')

	if existing, err := fs.ReadFile(target); err == nil && bytes.Equal(existing, data) {
		return target, false, nil
	}

	if err := fs.WriteFile(target, data); err != nil {
		return "", false, fmt.Errorf("recording the render of %s: %w", root, err)
	}

	return target, true, nil
}

func (c *Controller) guardRendered(root string, rendered rendering) error {
	record, err := ReadRecord(c.fs, root)
	if err != nil {
		return err
	}

	for _, path := range rendered.generated {
		if err := guard(c.fs, root, path, record, rendered.declared); err != nil {
			return err
		}
	}

	return nil
}

func (c *Controller) recordOnDisk(root string, paths []string, declared map[string]map[string]string) (Record, error) {
	out := Record{}

	for _, path := range paths {
		exists, err := c.fs.Exists(path)
		if err != nil {
			return nil, fmt.Errorf("inspecting %s as the lock left it: %w", path, err)
		}

		if !exists {
			continue
		}

		raw, err := c.fs.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s as the lock left it: %w", path, err)
		}

		entry, err := entryOf(path, raw, declared)
		if err != nil {
			return nil, err
		}

		out[relative(root, path)] = entry
	}

	return out, nil
}

func entryOf(path string, raw []byte, declared map[string]map[string]string) (RenderedFile, error) {
	modules, merged := declared[path]
	if !merged {
		return RenderedFile{SHA256: Digest(raw)}, nil
	}

	requires, err := gomod.Requires(string(raw))
	if err != nil {
		return RenderedFile{}, fmt.Errorf("reading the declared versions of %s as the lock left it: %w", path, err)
	}

	versions := map[string]string{}

	for module := range modules {
		if version, required := requires[module]; required {
			versions[module] = version
		}
	}

	return RenderedFile{Versions: versions}, nil
}

func (c *Controller) recordGenerated(root string, paths []string, entries Record, report *Report) error {
	target, changed, err := WriteRecord(c.fs, root, paths, entries)
	if err != nil {
		return err
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

func (c *Controller) Take(root, file string) (TakeReport, error) {
	path := file
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, filepath.FromSlash(file))
	}

	rel := relative(root, path)

	record, err := ReadRecord(c.fs, root)
	if err != nil {
		return TakeReport{}, err
	}

	if _, known := record[rel]; !known {
		return TakeReport{}, fmt.Errorf(
			"%w: %s\n"+
				"take gives a rendered file back to forge-factory, and %s is not one it rendered.\n"+
				"Every file it rendered is listed in %s",
			ErrNotRendered, rel, rel, GeneratedManifestPath)
	}

	exists, err := c.fs.Exists(path)
	if err != nil {
		return TakeReport{}, fmt.Errorf("inspecting %s before taking it: %w", rel, err)
	}

	if !exists {
		return TakeReport{}, fmt.Errorf(
			"%w: %s\n"+
				"take backs a file up before the render replaces it, and there is nothing there to back up.\n"+
				"Run: forge-factory sync",
			ErrNothingToTake, rel)
	}

	existing, err := c.fs.ReadFile(path)
	if err != nil {
		return TakeReport{}, fmt.Errorf("reading %s before taking it: %w", rel, err)
	}

	backup, err := c.freeBackup(path)
	if err != nil {
		return TakeReport{}, err
	}

	if err := c.fs.WriteFile(backup, existing); err != nil {
		return TakeReport{}, fmt.Errorf("backing %s up to %s: %w", rel, relative(root, backup), err)
	}

	delete(record, rel)

	paths := make([]string, 0, len(record))
	for name := range record {
		paths = append(paths, name)
	}

	if _, _, err := WriteRecord(c.fs, root, paths, record); err != nil {
		return TakeReport{}, err
	}

	return TakeReport{Path: rel, Backup: relative(root, backup)}, nil
}

func (c *Controller) freeBackup(path string) (string, error) {
	candidate := path + ".bak"

	for n := 2; ; n++ {
		taken, err := c.fs.Exists(candidate)
		if err != nil {
			return "", fmt.Errorf("looking for a free backup name beside %s: %w", path, err)
		}

		if !taken {
			return candidate, nil
		}

		candidate = fmt.Sprintf("%s.bak.%d", path, n)
	}
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
	missing *unregistered,
) (rendering, error) {
	resolved, err := c.resolve(f, root)
	if err != nil {
		return rendering{}, err
	}

	record, err := ReadRecord(c.fs, root)
	if err != nil {
		return rendering{}, err
	}

	out := rendering{digests: Record{}, declared: map[string]map[string]string{}}

	for _, language := range f.Languages() {
		uri, ok := f.EngineFor(language)
		if !ok {
			return rendering{}, fmt.Errorf("no engine is registered for %q", language)
		}

		if err := c.checkLanguage(ctx, uri, language, resolved); err != nil {
			return rendering{}, err
		}

		known := len(*missing)

		deps, depNotes, err := c.resolver.Resolve(ctx, f, root, language, f.DependenciesFor(language))
		if err != nil {
			if err := missing.absorb(fmt.Errorf("resolving %s dependencies: %w", language, err)); err != nil {
				return rendering{}, err
			}
		}

		dev, devNotes, err := c.resolver.Resolve(ctx, f, root, language, f.DevFor(language))
		if err != nil {
			if err := missing.absorb(fmt.Errorf("resolving %s devDependencies: %w", language, err)); err != nil {
				return rendering{}, err
			}
		}

		if len(*missing) > known {
			report.Notes = append(report.Notes, depNotes...)
			report.Notes = append(report.Notes, devNotes...)
			out.carried = recordedPaths(root, record)

			continue
		}

		members, err := c.resolver.ResolveMembers(ctx, f, root, language)
		if err != nil {
			return rendering{}, fmt.Errorf("resolving %s member modules: %w", language, err)
		}

		required, err := c.internalRequires(resolved, language)
		if err != nil {
			return rendering{}, err
		}

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

		var engine renderOutput

		if err := c.caller.Call(ctx, uri, ToolRender, in, &engine); err != nil {
			return rendering{}, fmt.Errorf("rendering %s: %w", language, err)
		}

		for _, file := range engine.Files {
			if only != "" && !underMemberOrRoot(root, only, file.Path) {
				continue
			}

			out.generated = append(out.generated, file.Path)

			if file.Merged {
				out.declared[file.Path] = deps
			}
		}

		changed := map[string]bool{}

		if write {
			members := memberDirs(engine.DependencyLock, resolved)

			for _, file := range engine.Files {
				if only != "" && !underMemberOrRoot(root, only, file.Path) {
					continue
				}

				data := []byte(file.Content)

				if dir, ok := owner(members, file.Path); ok && c.differsOnDisk(file.Path, data) {
					changed[dir] = true
				}

				notes, err := c.declaredVersionsMoved(root, file.Path, data, out.declared)
				if err != nil {
					return rendering{}, err
				}

				report.Notes = append(report.Notes, notes...)

				if err := writeGuarded(c.fs, root, file.Path, data, record, out.declared); err != nil {
					return rendering{}, err
				}

				if !file.Merged {
					out.digests[relative(root, file.Path)] = RenderedFile{SHA256: Digest(data)}
				}

				report.Written = append(report.Written, file.Path)

				if file.Gitignore != "" {
					ignores[file.Gitignore] = append(
						ignores[file.Gitignore],
						append([]string{filepath.Base(file.Path)}, file.AlsoIgnore...)...)
				}
			}
		}

		for _, cmd := range engine.DependencyLock {
			if only != "" && cmd.Dir != filepath.Join(root, only) && cmd.Dir != root {
				continue
			}

			out.commands = append(out.commands, cmd)

			if changed[cmd.Dir] {
				out.settling = append(out.settling, cmd)
			}
		}

		for _, lf := range engine.Lockfiles {
			if only != "" && !underMemberOrRoot(root, only, lf) {
				continue
			}

			out.lockfiles = append(out.lockfiles, lf)
		}
	}

	return out, nil
}

type rendering struct {
	commands  []commandWire
	settling  []commandWire
	lockfiles []string
	generated []string
	carried   []string
	digests   Record
	declared  map[string]map[string]string
}

func (r rendering) recorded() []string {
	return slices.Concat(r.generated, r.lockfiles, r.carried)
}

func (r rendering) readBack() []string {
	out := []string{}

	for _, path := range r.generated {
		_, merged := r.declared[path]

		if merged || slices.ContainsFunc(r.settling, func(cmd commandWire) bool { return under(cmd.Dir, path) }) {
			out = append(out, path)
		}
	}

	return out
}

func memberDirs(commands []commandWire, repos []repoWire) []string {
	out := []string{}

	for _, cmd := range commands {
		if slices.ContainsFunc(repos, func(r repoWire) bool { return r.Path == cmd.Dir }) {
			out = append(out, cmd.Dir)
		}
	}

	return out
}

func owner(dirs []string, path string) (string, bool) {
	for _, dir := range dirs {
		if under(dir, path) {
			return dir, true
		}
	}

	return "", false
}

func under(dir, path string) bool {
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

func (c *Controller) differsOnDisk(path string, data []byte) bool {
	existing, err := c.fs.ReadFile(path)

	return err != nil || !bytes.Equal(existing, data)
}

func recordedPaths(root string, record Record) []string {
	out := make([]string, 0, len(record))
	for rel := range record {
		out = append(out, filepath.Join(root, filepath.FromSlash(rel)))
	}

	return out
}

func underMemberOrRoot(root, only, path string) bool {
	if under(filepath.Join(root, only), path) {
		return true
	}

	return filepath.Dir(path) == filepath.Clean(root)
}

func (c *Controller) internalRequires(repos []repoWire, language string) (map[string]bool, error) {
	out := map[string]bool{}

	if language != "go" {
		return out, nil
	}

	for _, r := range repos {
		if !slices.Contains(r.Languages, language) {
			continue
		}

		path := filepath.Join(r.Path, "go.mod")

		raw, err := c.fs.ReadFile(path)
		if err != nil {
			continue
		}

		requires, err := gomod.Requires(string(raw))
		if err != nil {
			return nil, fmt.Errorf("reading the requires of %s: %w", path, err)
		}

		for module := range requires {
			out[module] = true
		}
	}

	return out, nil
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
