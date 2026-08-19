package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

var (
	aliasPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	uriPattern   = regexp.MustCompile(`^(go|alias)://.+`)
)

type Factory struct {
	Version      string                       `json:"version"`
	Name         string                       `json:"name"`
	Repos        []Repo                       `json:"repos"`
	Dependencies map[string]map[string]string `json:"dependencies,omitempty"`
	Engines      []Engine                     `json:"engines"`
	State        *State                       `json:"state,omitempty"`
	Modules      map[string]Module            `json:"modules,omitempty"`
}

// Module maps a module path to a sibling checkout so codegen resolves a spec
// from the workspace instead of the network. A local checkout wins. The version
// is the remote fallback when the path is absent.
type Module struct {
	Path    string   `json:"path"`
	Version string   `json:"version,omitempty"`
	Specs   []string `json:"specs,omitempty"`
}

// Repo is one member. A repo with no languages is a member that carries no
// manifest, which is what a spec repo is: it is checked out and versioned and
// nothing is generated into it.
type Repo struct {
	Name      string   `json:"name"`
	URL       string   `json:"url"`
	Languages []string `json:"languages,omitempty"`
}

type Engine struct {
	Alias  string `json:"alias"`
	Engine string `json:"engine"`
}

type State struct {
	Engine string         `json:"engine"`
	Spec   map[string]any `json:"spec,omitempty"`
}

func Parse(data []byte) (Factory, error) {
	var f Factory

	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return Factory{}, fmt.Errorf("reading the factory: %w", err)
	}

	if err := f.Validate(); err != nil {
		return Factory{}, err
	}

	return f, nil
}

func (f Factory) Validate() error {
	var errs []string

	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	if strings.TrimSpace(f.Name) == "" {
		add("name is required")
	}

	if len(f.Repos) == 0 {
		add("repos: at least one repo is required")
	}

	names := map[string]bool{}
	languages := map[string]bool{}

	for i, r := range f.Repos {
		where := fmt.Sprintf("repos[%d] (%s)", i, r.Name)

		if !aliasPattern.MatchString(r.Name) {
			add("%s: name must be lowercase kebab-case", where)
		}

		if strings.TrimSpace(r.URL) == "" {
			add("%s: url is required", where)
		}

		for _, l := range r.Languages {
			if !aliasPattern.MatchString(l) {
				add("%s: language %q must be lowercase kebab-case", where, l)
			}

			languages[l] = true
		}

		if names[r.Name] {
			add("%s: duplicate repo name", where)
		}

		names[r.Name] = true
	}

	if len(f.Engines) == 0 {
		add("engines: at least one language engine is required")
	}

	aliases := map[string]bool{}

	for i, e := range f.Engines {
		where := fmt.Sprintf("engines[%d] (%s)", i, e.Alias)

		if !aliasPattern.MatchString(e.Alias) {
			add("%s: alias must be lowercase kebab-case", where)
		}

		if !uriPattern.MatchString(e.Engine) {
			add("%s: engine must start with go:// or alias://", where)
		}

		if aliases[e.Alias] {
			add("%s: duplicate engine alias", where)
		}

		aliases[e.Alias] = true
	}

	for _, l := range sortedKeys(languages) {
		if !aliases[l] {
			add("repos declare the language %q and no engine has that alias", l)
		}
	}

	for _, l := range sortedKeys(f.Dependencies) {
		if !aliases[l] {
			add("dependencies declare the language %q and no engine has that alias", l)
		}
	}

	if f.State != nil && !uriPattern.MatchString(f.State.Engine) {
		add("state: engine must start with go:// or alias://")
	}

	for _, path := range sortedKeys(f.Modules) {
		m := f.Modules[path]

		if strings.TrimSpace(m.Path) == "" && strings.TrimSpace(m.Version) == "" {
			add("modules[%s]: needs a path, a version, or both", path)
		}
	}

	if len(errs) == 0 {
		return nil
	}

	return fmt.Errorf("invalid factory:\n  %s", strings.Join(errs, "\n  "))
}

// DependenciesFor returns the declared versions for one language, never nil so
// an engine always receives a map it can range over.
func (f Factory) DependenciesFor(language string) map[string]string {
	if deps, ok := f.Dependencies[language]; ok {
		return deps
	}

	return map[string]string{}
}

// EngineFor returns the engine URI registered for a language.
func (f Factory) EngineFor(language string) (string, bool) {
	for _, e := range f.Engines {
		if e.Alias == language {
			return e.Engine, true
		}
	}

	return "", false
}

// ModulePaths returns every declared module path, sorted.
func (f Factory) ModulePaths() []string {
	return sortedKeys(f.Modules)
}

// Languages returns every language any repo declares, sorted, so a sync is
// deterministic.
func (f Factory) Languages() []string {
	seen := map[string]bool{}

	for _, r := range f.Repos {
		for _, l := range r.Languages {
			seen[l] = true
		}
	}

	return sortedKeys(seen)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
