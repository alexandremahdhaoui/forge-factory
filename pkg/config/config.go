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

// Toolchain declares the standalone binaries a workspace provisions into
// the store and exposes on .forge/bin: third-party generators and linters,
// or a user's own engines - the one governed place their versions live,
// instead of an env var here and a code default there.
type Toolchain struct {
	Binaries []ToolchainBinary `json:"binaries,omitempty"`

	// Image is the container the workspace's pipelines run their jobs in.
	// Declared once here, resolved by sync into a generated file, and read
	// from there by the CI layer - so the pin is never hand-typed in a
	// pipeline file.
	Image *ToolchainImage `json:"image,omitempty"`
}

// ToolchainImage is the toolchain container. Exactly one of track or
// version pins it, exactly as a binary is pinned: a track resolves from the
// register's index, a literal version serves a workspace without a register.
type ToolchainImage struct {
	// Ref is the image reference without a tag, e.g. a registry host and
	// repository path.
	Ref string `json:"ref"`
	// Track names a register track as "<ecosystem>:<package>".
	Track string `json:"track,omitempty"`
	// Version is the literal pin.
	Version string `json:"version,omitempty"`
}

// ToolchainBinary is one provisioned tool. Exactly one of track or version
// pins it: a track resolves from the register's index (its current
// version), a literal version serves a workspace without a register.
type ToolchainBinary struct {
	// Name is the binary's base name as it appears on PATH.
	Name string `json:"name"`
	// Module is the full main-package module path `go install` builds.
	Module string `json:"module"`
	// Track names a register track as "<ecosystem>:<package>".
	Track string `json:"track,omitempty"`
	// Version is the literal pin.
	Version string `json:"version,omitempty"`

	// Acknowledge accepts named findings on this binary, exactly as a
	// dependency does. Without it a finding on a toolchain binary was
	// unrecoverable: the gate refused, and the error told the operator to
	// write yaml the schema then rejected.
	Acknowledge []Acknowledgement `json:"acknowledge,omitempty"`
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

	// Acknowledge lists the findings this workspace has decided to live
	// with. A finding blocks until it is named here, so accepting a risk is
	// an edit somebody makes on purpose and a reviewer can see.
	Acknowledge []Acknowledgement `json:"acknowledge,omitempty"`
}

// Acknowledgement accepts one named finding on one dependency.
//
// It is keyed on the advisory id, so accepting the risk you looked at does
// not accept the next one: a finding nobody has named blocks, however many
// others are acknowledged beside it.
type Acknowledgement struct {
	// ID is the advisory this accepts, exactly as the feed names it.
	ID string `json:"id"`

	// Reason is why. Mandatory, for the same purpose as a pin's: the person
	// who reads this in six months is usually not the one who wrote it.
	Reason string `json:"reason"`

	// Expires re-opens the decision on a date, for a risk accepted only
	// until something else lands. Optional: most acknowledgements are
	// instead named dead the moment the register carries a version that
	// fixes them, which needs no date.
	Expires string `json:"expires,omitempty"`
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

	// Strict, like the document around it. Parse uses UnmarshalStrict, but a
	// custom UnmarshalJSON gets raw bytes and a plain Unmarshal here dropped
	// that strictness for the entry and everything nested in it: `trak:` and
	// `expres:` were accepted and silently ignored. A typo'd expires meant a
	// permanent, never-expiring acknowledgement with no diagnostic, so the
	// feature simply did not exist for anyone who mistyped it.
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

	// One rule for accepting a risk, wherever it is accepted. A toolchain
	// binary carries findings exactly as a dependency does.
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
