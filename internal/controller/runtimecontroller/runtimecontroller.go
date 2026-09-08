package runtimecontroller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/engineadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/lockadapter"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

const (
	DefaultFetchEngine   = "forge://github.com/alexandremahdhaoui/forge-factory/cmd/factory-fetch"
	DefaultInstallEngine = "forge://github.com/alexandremahdhaoui/forge-factory/cmd/factory-install"

	ToolDescribe = "describe"
	ToolFetch    = "fetch"
	ToolInstall  = "install"

	EnvPath = ".forge/env"
)

type Pin struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	Params  map[string]string `json:"params,omitempty"`
	Targets []string          `json:"targets,omitempty"`
}

type Report struct {
	Installed []string
	Reused    []string
	Rebuilt   []string
	Satisfied []string
	Exposed   []string
}

type Provisioner interface {
	Provision(ctx context.Context, f config.Factory, root, storeDir string, pins []Pin) (Report, error)
}

type Controller struct {
	caller engineadapter.Caller
	fs     fsadapter.FS
	lock   lockadapter.Locker

	lookPath func(string) (string, error)
	platform string
}

var _ Provisioner = (*Controller)(nil)

func New(caller engineadapter.Caller, fs fsadapter.FS, lock lockadapter.Locker) *Controller {
	return &Controller{caller: caller, fs: fs, lock: lock, lookPath: exec.LookPath}
}

type describeInput struct {
	Runtime string            `json:"runtime"`
	Version string            `json:"version"`
	OS      string            `json:"os"`
	Arch    string            `json:"arch"`
	Params  map[string]string `json:"params"`
	Targets []string          `json:"targets,omitempty"`
	Spec    map[string]any    `json:"spec"`
}

type pickWire struct {
	From string `json:"from,omitempty"`
	At   string `json:"at,omitempty"`
}

type artifactWire struct {
	URL    string     `json:"url"`
	SHA256 string     `json:"sha256"`
	Unpack string     `json:"unpack"`
	Strip  int        `json:"strip,omitempty"`
	Picks  []pickWire `json:"picks,omitempty"`
}

type prerequisiteWire struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
	Verify string `json:"verify,omitempty"`
}

type descriptionWire struct {
	Runtime       string             `json:"runtime"`
	Version       string             `json:"version"`
	Artifacts     []artifactWire     `json:"artifacts"`
	Bins          []string           `json:"bins,omitempty"`
	Env           map[string]string  `json:"env,omitempty"`
	Prerequisites []prerequisiteWire `json:"prerequisites,omitempty"`
	Provides      []string           `json:"provides,omitempty"`
}

type fetchInput struct {
	Artifact artifactWire   `json:"artifact"`
	Dest     string         `json:"dest"`
	Spec     map[string]any `json:"spec"`
}

type fetchOutput struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type fetchedArchiveWire struct {
	Artifact artifactWire `json:"artifact"`
	Path     string       `json:"path"`
}

type installInput struct {
	Archives []fetchedArchiveWire `json:"archives"`
	Prefix   string               `json:"prefix"`
	Spec     map[string]any       `json:"spec"`
}

type installOutput struct {
	Installed []string `json:"installed"`
}

type described struct {
	pin    Pin
	desc   descriptionWire
	prefix string
}

func (c *Controller) Provision(
	ctx context.Context, f config.Factory, root, storeDir string, pins []Pin,
) (Report, error) {
	report := Report{Installed: []string{}, Reused: []string{}, Satisfied: []string{}, Exposed: []string{}}

	if len(pins) == 0 {
		return report, nil
	}

	store, err := resolveStoreDir(storeDir)
	if err != nil {
		return Report{}, err
	}

	absStore, err := filepath.Abs(store)
	if err != nil {
		return Report{}, fmt.Errorf("resolving the store: %w", err)
	}

	described, err := c.describeAll(ctx, f, absStore, pins)
	if err != nil {
		return Report{}, err
	}

	if err := c.checkPrerequisites(f, described, &report); err != nil {
		return Report{}, err
	}

	for _, d := range described {
		if err := c.materialize(ctx, f, absStore, d, &report); err != nil {
			return Report{}, err
		}
	}

	if err := c.expose(root, described, &report); err != nil {
		return Report{}, err
	}

	return report, nil
}

func (c *Controller) describeAll(
	ctx context.Context, f config.Factory, store string, pins []Pin,
) ([]described, error) {
	sorted := append([]Pin{}, pins...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	platform := c.platform
	if platform == "" {
		platform = runtime.GOOS + "/" + runtime.GOARCH
	}

	osName, arch, _ := strings.Cut(platform, "/")

	out := make([]described, 0, len(sorted))

	for _, pin := range sorted {
		if strings.TrimSpace(pin.Version) == "" {
			return nil, fmt.Errorf("runtime %s: nothing pins a version; latest is never a fallback", pin.Name)
		}

		uri, ok := f.EngineFor(pin.Name)
		if !ok {
			return nil, fmt.Errorf("runtime %s: no engine is declared under that alias", pin.Name)
		}

		params := pin.Params
		if params == nil {
			params = map[string]string{}
		}

		var desc descriptionWire

		in := describeInput{
			Runtime: pin.Name, Version: pin.Version,
			OS: osName, Arch: arch,
			Params: params, Targets: pin.Targets, Spec: orEmpty(f.SpecFor(pin.Name)),
		}

		if err := c.caller.Call(ctx, uri, ToolDescribe, in, &desc); err != nil {
			return nil, fmt.Errorf("describing runtime %s@%s: %w", pin.Name, pin.Version, err)
		}

		if len(desc.Artifacts) == 0 {
			return nil, fmt.Errorf(
				"describing runtime %s@%s: the provider answered no artifacts", pin.Name, pin.Version)
		}

		out = append(out, described{
			pin:    pin,
			desc:   desc,
			prefix: filepath.Join(store, "runtimes", storeEntry(pin)),
		})
	}

	return out, nil
}

func storeEntry(pin Pin) string {
	entry := sanitize(pin.Name) + "@" + sanitize(pin.Version)

	targets := append([]string{}, pin.Targets...)
	sort.Strings(targets)

	for _, target := range targets {
		entry += "+" + sanitize(target)
	}

	return entry
}

func (c *Controller) checkPrerequisites(f config.Factory, ds []described, report *Report) error {
	provides := map[string]string{}

	for _, d := range ds {
		for _, capability := range d.desc.Provides {
			provides[capability] = d.pin.Name
		}
	}

	satisfy := map[string]string{}
	if f.Toolchain != nil {
		satisfy = f.Toolchain.Satisfy
	}

	for _, d := range ds {
		for _, p := range d.desc.Prerequisites {
			how, err := c.satisfyOne(d.pin.Name, p, provides, satisfy)
			if err != nil {
				return err
			}

			report.Satisfied = append(report.Satisfied,
				fmt.Sprintf("%s needs %s (%s): %s", d.pin.Name, p.Name, p.Reason, how))
		}
	}

	return nil
}

func (c *Controller) satisfyOne(
	runtimeName string, p prerequisiteWire, provides, satisfy map[string]string,
) (string, error) {
	if override, ok := satisfy[p.Name]; ok {
		if override == "host" {
			return c.verifyHost(runtimeName, p, "satisfy: names the host")
		}

		if provider, ok := provides[p.Name]; ok && provider == override {
			return "provided by the declared runtime " + override + " (satisfy:)", nil
		}

		return "", fmt.Errorf(
			"runtime %s needs %s (%s): satisfy: names %q, which is not a declared runtime providing it",
			runtimeName, p.Name, p.Reason, override)
	}

	if provider, ok := provides[p.Name]; ok {
		return "provided by the declared runtime " + provider, nil
	}

	if p.Verify != "" {
		return c.verifyHost(runtimeName, p, "found on the host")
	}

	return "", fmt.Errorf(
		"runtime %s needs %s (%s), nothing provides it, and it cannot be verified on the host: "+
			"declare a runtime whose provides lists %q, or run on a base that carries it",
		runtimeName, p.Name, p.Reason, p.Name)
}

func (c *Controller) verifyHost(runtimeName string, p prerequisiteWire, how string) (string, error) {
	if p.Verify == "" {
		return "", fmt.Errorf(
			"runtime %s needs %s (%s): only a providing runtime can satisfy it and none is declared",
			runtimeName, p.Name, p.Reason)
	}

	path, err := c.lookPath(p.Verify)
	if err != nil {
		return "", fmt.Errorf(
			"runtime %s needs %s (%s) and %q is not on this machine's PATH: "+
				"declare a runtime whose provides lists %q, set toolchain.satisfy accordingly, "+
				"or run on a base that carries it",
			runtimeName, p.Name, p.Reason, p.Verify, p.Name)
	}

	return fmt.Sprintf("%s (%s at %s)", how, p.Verify, path), nil
}

func (c *Controller) materialize(
	ctx context.Context, f config.Factory, store string, d described, report *Report,
) error {
	name := storeEntry(d.pin)

	if ok, _ := c.fs.Exists(d.prefix); ok && c.entryCarriesTheBins(d) {
		report.Reused = append(report.Reused, name)

		return nil
	}

	release, err := c.lock.Lock(d.prefix)
	if err != nil {
		return fmt.Errorf("locking the store for %s: %w", name, err)
	}

	defer release()

	rebuilt := false

	if ok, _ := c.fs.Exists(d.prefix); ok {
		if c.entryCarriesTheBins(d) {
			report.Reused = append(report.Reused, name)

			return nil
		}

		if err := c.fs.Remove(d.prefix); err != nil {
			return fmt.Errorf("removing the stale store entry %s: %w", name, err)
		}

		rebuilt = true
	}

	fetchURI, fetchSpec := c.engineOr(f, "fetch", DefaultFetchEngine)
	installURI, installSpec := c.engineOr(f, "install", DefaultInstallEngine)

	staging := filepath.Join(store, "tmp", fmt.Sprintf("runtime-%s-%d", name, os.Getpid()))

	if err := c.fs.Remove(staging); err != nil {
		return err
	}

	defer func() { _ = c.fs.Remove(staging) }()

	archives := make([]fetchedArchiveWire, 0, len(d.desc.Artifacts))

	for i, artifact := range d.desc.Artifacts {
		dest := filepath.Join(staging, "fetch", fmt.Sprintf("%d-%s", i, safeBase(artifact.URL)))

		var fetched fetchOutput

		in := fetchInput{Artifact: artifact, Dest: dest, Spec: fetchSpec}
		if err := c.caller.Call(ctx, fetchURI, ToolFetch, in, &fetched); err != nil {
			return fmt.Errorf("fetching %s for %s: %w", artifact.URL, name, err)
		}

		archives = append(archives, fetchedArchiveWire{Artifact: artifact, Path: fetched.Path})
	}

	stagedPrefix := filepath.Join(staging, "prefix")
	if err := c.fs.MkdirAll(stagedPrefix); err != nil {
		return fmt.Errorf("staging %s: %w", name, err)
	}

	var installed installOutput

	in := installInput{Archives: archives, Prefix: stagedPrefix, Spec: installSpec}
	if err := c.caller.Call(ctx, installURI, ToolInstall, in, &installed); err != nil {
		return fmt.Errorf("installing %s: %w", name, err)
	}

	if err := c.fs.Rename(stagedPrefix, d.prefix); err != nil {
		return fmt.Errorf("landing %s: %w", name, err)
	}

	if rebuilt {
		report.Rebuilt = append(report.Rebuilt, name)
	} else {
		report.Installed = append(report.Installed, name)
	}

	return nil
}

func (c *Controller) entryCarriesTheBins(d described) bool {
	for _, bin := range d.desc.Bins {
		target := filepath.Join(d.prefix, filepath.FromSlash(bin))
		if ok, _ := c.fs.Exists(target); !ok {
			return false
		}
	}

	return true
}

func (c *Controller) engineOr(f config.Factory, alias, fallback string) (string, map[string]any) {
	if uri, ok := f.EngineFor(alias); ok {
		return uri, orEmpty(f.SpecFor(alias))
	}

	return fallback, map[string]any{}
}

func (c *Controller) expose(root string, ds []described, report *Report) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolving the workspace root: %w", err)
	}

	binDir := filepath.Join(absRoot, ".forge", "bin")
	if err := c.fs.MkdirAll(binDir); err != nil {
		return fmt.Errorf("exposing the runtimes: %w", err)
	}

	env := map[string]string{}

	for _, d := range ds {
		for _, bin := range d.desc.Bins {
			target := filepath.Join(d.prefix, filepath.FromSlash(bin))
			link := filepath.Join(binDir, filepath.Base(filepath.FromSlash(bin)))

			if err := c.fs.Symlink(target, link); err != nil {
				return fmt.Errorf("exposing %s: %w", d.pin.Name, err)
			}

			report.Exposed = append(report.Exposed, filepath.Base(filepath.FromSlash(bin)))
		}

		for _, key := range sortedKeys(d.desc.Env) {
			env[key] = strings.ReplaceAll(d.desc.Env[key], "{prefix}", d.prefix)
		}
	}

	var b strings.Builder

	b.WriteString("# Generated by forge-factory. DO NOT EDIT.\n")
	b.WriteString("# The runtimes' environment; the managed .envrc block sources it.\n")

	for _, key := range sortedKeys(env) {
		fmt.Fprintf(&b, "export %s=%q\n", key, env[key])
	}

	if err := c.fs.WriteFile(filepath.Join(absRoot, filepath.FromSlash(EnvPath)), []byte(b.String())); err != nil {
		return fmt.Errorf("writing the runtime environment: %w", err)
	}

	for _, key := range sortedKeys(env) {
		if err := os.Setenv(key, env[key]); err != nil {
			return fmt.Errorf("applying the runtime environment: %w", err)
		}
	}

	if current := os.Getenv("PATH"); !strings.HasPrefix(current, binDir+string(os.PathListSeparator)) {
		if err := os.Setenv("PATH", binDir+string(os.PathListSeparator)+current); err != nil {
			return fmt.Errorf("applying the runtime environment: %w", err)
		}
	}

	return nil
}

func safeBase(url string) string {
	base := url
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}

	base = sanitize(base)
	if base == "" || base == "." || base == ".." {
		return "artifact"
	}

	return base
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', ':', '@', '~', '\\':
			return '-'
		default:
			return r
		}
	}, s)
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}

	return m
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

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
