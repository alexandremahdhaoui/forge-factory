package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

var (
	aliasPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	uriPattern   = regexp.MustCompile(`^(forge|alias)://.+`)
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
	Toolchain    *Toolchain                           `json:"toolchain,omitempty"`
}

type Toolchain struct {
	Binaries []ToolchainBinary      `json:"binaries,omitempty"`
	Runtimes map[string]RuntimeSpec `json:"runtimes,omitempty"`
	Satisfy  map[string]string      `json:"satisfy,omitempty"`
	Image    *ToolchainImage        `json:"image,omitempty"`
}

type ToolchainImage struct {
	Ref     string `json:"ref"`
	Track   string `json:"track,omitempty"`
	Version string `json:"version,omitempty"`
}

type RuntimeSpec struct {
	Track   string            `json:"track,omitempty"`
	Version string            `json:"version,omitempty"`
	Params  map[string]string `json:"params,omitempty"`
	Targets []string          `json:"targets,omitempty"`
}

type ToolchainBinary struct {
	Name        string            `json:"name"`
	Module      string            `json:"module"`
	Track       string            `json:"track,omitempty"`
	Version     string            `json:"version,omitempty"`
	Acknowledge []Acknowledgement `json:"acknowledge,omitempty"`
}

type Register struct {
	URL      string `json:"url"`
	Path     string `json:"path,omitempty"`
	Revision string `json:"revision,omitempty"`
}

type DependencySpec struct {
	Version     string            `json:"-"`
	Track       string            `json:"track,omitempty"`
	Pin         string            `json:"pin,omitempty"`
	Mode        string            `json:"mode,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	Expires     string            `json:"expires,omitempty"`
	Wraps       string            `json:"wraps,omitempty"`
	Acknowledge []Acknowledgement `json:"acknowledge,omitempty"`
}

type Acknowledgement struct {
	ID      string `json:"id"`
	Reason  string `json:"reason"`
	Expires string `json:"expires,omitempty"`
}

func (d DependencySpec) FromRegister() bool {
	return d.Version == ""
}

func (d *DependencySpec) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &d.Version)
	}

	type alias DependencySpec

	var a alias

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&a); err != nil {
		return err
	}

	*d = DependencySpec(a)

	return nil
}

func (d DependencySpec) MarshalJSON() ([]byte, error) {
	if d.Version != "" {
		return json.Marshal(d.Version)
	}

	type alias DependencySpec

	return json.Marshal(alias(d))
}

type Repo struct {
	Name      string   `json:"name"`
	URL       string   `json:"url"`
	Languages []string `json:"languages,omitempty"`
}

type Engine struct {
	Alias  string         `json:"alias"`
	Engine string         `json:"engine"`
	Spec   map[string]any `json:"spec,omitempty"`
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

		if strings.HasPrefix(e.Engine, "go://") {
			add("%s: the go:// scheme is removed; use forge://", where)
		} else if !uriPattern.MatchString(e.Engine) {
			add("%s: engine must start with forge:// or alias://", where)
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

	if f.State != nil && strings.HasPrefix(f.State.Engine, "go://") {
		add("state: the go:// scheme is removed; use forge://")
	} else if f.State != nil && !uriPattern.MatchString(f.State.Engine) {
		add("state: engine must start with forge:// or alias://")
	}

	if f.Register != nil && strings.TrimSpace(f.Register.URL) == "" && strings.TrimSpace(f.Register.Path) == "" {
		add("register: needs a url, a path, or both")
	}

	validateAcks := func(where string, acks []Acknowledgement) {
		seen := map[string]bool{}

		for i, ack := range acks {
			at := fmt.Sprintf("%s.acknowledge[%d]", where, i)

			if strings.TrimSpace(ack.ID) == "" {
				add("%s: names no advisory id, so it accepts everything and nothing", at)
			}

			if strings.TrimSpace(ack.Reason) == "" {
				add("%s: accepting a risk without a reason is a config error, "+
					"not a warning", at)
			}

			if seen[ack.ID] {
				add("%s: %s is acknowledged twice", at, ack.ID)
			}

			seen[ack.ID] = true

			if ack.Expires != "" {
				if _, err := time.Parse("2006-01-02", ack.Expires); err != nil {
					add("%s: expires must be a date, YYYY-MM-DD, got %q", at, ack.Expires)
				}
			}
		}
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

				validateAcks(where, d.Acknowledge)
			}
		}
	}

	validateDeps("dependencies", f.Dependencies)
	validateDeps("devDependencies", f.Dev)

	if f.Toolchain != nil {
		names := map[string]bool{}

		for i, b := range f.Toolchain.Binaries {
			where := fmt.Sprintf("toolchain.binaries[%d]", i)

			if strings.TrimSpace(b.Name) == "" {
				add("%s: a binary needs a name", where)
			} else if names[b.Name] {
				add("%s: duplicate binary name %q", where, b.Name)
			}

			names[b.Name] = true

			validateAcks(where, b.Acknowledge)

			if strings.TrimSpace(b.Module) == "" {
				add("%s: a binary needs the module path go install builds", where)
			}

			switch {
			case b.Track == "" && b.Version == "":
				add("%s: exactly one of track or version pins a binary; neither means nothing resolves it", where)
			case b.Track != "" && b.Version != "":
				add("%s: exactly one of track or version pins a binary, not both", where)
			case b.Track != "":
				if !strings.Contains(b.Track, ":") {
					add("%s: a track is named <ecosystem>:<package>", where)
				}

				if f.Register == nil {
					add("%s: resolves from the register and no register: block is declared", where)
				}
			}
		}

		for _, name := range sortedKeys(f.Toolchain.Runtimes) {
			r := f.Toolchain.Runtimes[name]
			where := fmt.Sprintf("toolchain.runtimes.%s", name)

			if !aliasPattern.MatchString(name) {
				add("%s: a runtime name must be lowercase kebab-case", where)
			}

			if !aliases[name] {
				add("%s: no engine is declared under the alias %q, and the name is the binding", where, name)
			}

			switch {
			case r.Track == "" && r.Version == "":
				add("%s: exactly one of track or version pins a runtime; neither means nothing resolves it", where)
			case r.Track != "" && r.Version != "":
				add("%s: exactly one of track or version pins a runtime, not both", where)
			case r.Track != "":
				if !strings.Contains(r.Track, ":") {
					add("%s: a track is named <ecosystem>:<package>", where)
				}

				if f.Register == nil {
					add("%s: resolves from the register and no register: block is declared", where)
				}
			}

			validateTargets(add, where, r.Targets)
		}

		for _, capability := range sortedKeys(f.Toolchain.Satisfy) {
			target := f.Toolchain.Satisfy[capability]
			where := fmt.Sprintf("toolchain.satisfy.%s", capability)

			if target == "host" {
				continue
			}

			if _, ok := f.Toolchain.Runtimes[target]; !ok {
				add("%s: %q is neither \"host\" nor a declared runtime", where, target)
			}
		}

		if img := f.Toolchain.Image; img != nil {
			const where = "toolchain.image"

			if strings.TrimSpace(img.Ref) == "" {
				add("%s: the image needs a ref, the reference without a tag", where)
			}

			switch {
			case img.Track == "" && img.Version == "":
				add("%s: exactly one of track or version pins the image; neither means nothing resolves it", where)
			case img.Track != "" && img.Version != "":
				add("%s: exactly one of track or version pins the image, not both", where)
			case img.Track != "":
				if !strings.Contains(img.Track, ":") {
					add("%s: a track is named <ecosystem>:<package>", where)
				}

				if f.Register == nil {
					add("%s: resolves from the register and no register: block is declared", where)
				}
			}
		}
	}

	if len(errs) == 0 {
		return nil
	}

	return fmt.Errorf("invalid factory:\n  %s", strings.Join(errs, "\n  "))
}

func validateTargets(add func(string, ...any), where string, targets []string) {
	seen := map[string]bool{}

	for i, target := range targets {
		at := fmt.Sprintf("%s.targets[%d]", where, i)

		if strings.TrimSpace(target) == "" {
			add("%s: a target names a triple, it cannot be empty", at)
		}

		if seen[target] {
			add("%s: %q is declared twice", at, target)
		}

		seen[target] = true
	}
}

func (f Factory) DependenciesFor(language string) map[string]DependencySpec {
	if deps, ok := f.Dependencies[language]; ok {
		return deps
	}

	return map[string]DependencySpec{}
}

func (f Factory) DevFor(language string) map[string]DependencySpec {
	if deps, ok := f.Dev[language]; ok {
		return deps
	}

	return map[string]DependencySpec{}
}

func (f Factory) EngineFor(language string) (string, bool) {
	for _, e := range f.Engines {
		if e.Alias == language {
			return e.Engine, true
		}
	}

	return "", false
}

func (f Factory) SpecFor(alias string) map[string]any {
	for _, e := range f.Engines {
		if e.Alias == alias {
			return e.Spec
		}
	}

	return nil
}

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
