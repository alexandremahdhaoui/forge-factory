package config

import (
	"encoding/json"
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
	Version      string                               `json:"version"`
	Name         string                               `json:"name"`
	Repos        []Repo                               `json:"repos"`
	Register     *Register                            `json:"register,omitempty"`
	Dependencies map[string]map[string]DependencySpec `json:"dependencies,omitempty"`
	Dev          map[string]map[string]DependencySpec `json:"devDependencies,omitempty"`
	Engines      []Engine                             `json:"engines"`
	State        *State                               `json:"state,omitempty"`
}

// Register names the catalog this workspace resolves versions from. The local
// checkout wins, like any member; the URL is the remote fallback, cloned at
// the pinned revision when the checkout is absent.
type Register struct {
	URL      string `json:"url"`
	Path     string `json:"path,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// DependencySpec is one dependency entry. A bare string is a version written
// verbatim, exactly as before the register existed. An object resolves from
// the register: the track's current version, floored by a soft pin or frozen
// by a hard one. Every pin carries a reason - a pin without one is a config
// error, not a warning.
type DependencySpec struct {
	// Version is the legacy form: written verbatim, register not consulted.
	Version string `json:"-"`

	Track   string `json:"track,omitempty"`
	Pin     string `json:"pin,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Expires string `json:"expires,omitempty"`
	// Wraps renders the resolved version into a larger value, %s replaced -
	// a rust inline table carrying features, a python specifier prefix.
	Wraps string `json:"wraps,omitempty"`
}

// FromRegister reports whether this entry resolves from the register.
func (d DependencySpec) FromRegister() bool {
	return d.Version == ""
}

// UnmarshalJSON keeps the legacy form parsing: a bare string is a version.
func (d *DependencySpec) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &d.Version)
	}

	type alias DependencySpec

	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}

	*d = DependencySpec(a)

	return nil
}

// MarshalJSON round-trips the legacy form.
func (d DependencySpec) MarshalJSON() ([]byte, error) {
	if d.Version != "" {
		return json.Marshal(d.Version)
	}

	type alias DependencySpec

	return json.Marshal(alias(d))
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

	for _, l := range sortedKeys(f.Dev) {
		if !aliases[l] {
			add("devDependencies declare the language %q and no engine has that alias", l)
		}
	}

	if f.State != nil && !uriPattern.MatchString(f.State.Engine) {
		add("state: engine must start with go:// or alias://")
	}

	if f.Register != nil && strings.TrimSpace(f.Register.URL) == "" && strings.TrimSpace(f.Register.Path) == "" {
		add("register: needs a url, a path, or both")
	}

	validateDeps := func(section string, deps map[string]map[string]DependencySpec) {
		for _, language := range sortedKeys(deps) {
			for _, name := range sortedKeys(deps[language]) {
				d := deps[language][name]
				where := fmt.Sprintf("%s.%s.%s", section, language, name)

				if d.FromRegister() && f.Register == nil {
					add("%s: resolves from the register and no register: block is declared", where)
				}

				if d.Pin != "" && strings.TrimSpace(d.Reason) == "" {
					add("%s: a pin without a reason is a config error, not a warning", where)
				}

				if d.Pin != "" && d.Mode != "soft" && d.Mode != "hard" {
					add("%s: a pin needs mode: soft or mode: hard", where)
				}

				if d.Pin == "" && d.Mode != "" {
					add("%s: mode without a pin means nothing", where)
				}

				if d.Wraps != "" && !strings.Contains(d.Wraps, "%s") {
					add("%s: wraps must contain %%s for the resolved version", where)
				}
			}
		}
	}

	validateDeps("dependencies", f.Dependencies)
	validateDeps("devDependencies", f.Dev)

	if len(errs) == 0 {
		return nil
	}

	return fmt.Errorf("invalid factory:\n  %s", strings.Join(errs, "\n  "))
}

// DependenciesFor returns the declared entries for one language, never nil.
func (f Factory) DependenciesFor(language string) map[string]DependencySpec {
	if deps, ok := f.Dependencies[language]; ok {
		return deps
	}

	return map[string]DependencySpec{}
}

// DevFor returns what only the tests and the tooling need for one language,
// never nil.
func (f Factory) DevFor(language string) map[string]DependencySpec {
	if deps, ok := f.Dev[language]; ok {
		return deps
	}

	return map[string]DependencySpec{}
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
