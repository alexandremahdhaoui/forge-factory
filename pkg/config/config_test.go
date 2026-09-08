package config_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const valid = `version: "1"
name: golden
repos:
  - name: golden-go
    url: git@github.com:x/golden-go.git
    languages: [go]
  - name: golden-both
    url: git@github.com:x/golden-both.git
    languages: [go, typescript]
dependencies:
  go:
    sigs.k8s.io/yaml: v1.6.0
engines:
  - alias: go
    engine: forge://example.com/lang-go
  - alias: typescript
    engine: alias://lang-ts
state:
  engine: forge://example.com/ci-state-git
  spec:
    path: ../golden-state
`

func TestParseReadsAWholeFactory(t *testing.T) {
	t.Parallel()

	f, err := config.Parse([]byte(valid))
	require.NoError(t, err)

	assert.Equal(t, "golden", f.Name)
	assert.Len(t, f.Repos, 2)
	assert.Equal(t, []string{"go", "typescript"}, f.Languages())
	assert.Equal(t, map[string]config.DependencySpec{"sigs.k8s.io/yaml": {Version: "v1.6.0"}}, f.DependenciesFor("go"))
	assert.Equal(t, map[string]config.DependencySpec{}, f.DependenciesFor("typescript"),
		"a language with no versions still gets a map an engine can range over")
	assert.Equal(t, map[string]any{"path": "../golden-state"}, f.State.Spec)
}

func TestEngineFor(t *testing.T) {
	t.Parallel()

	f, err := config.Parse([]byte(valid))
	require.NoError(t, err)

	uri, ok := f.EngineFor("typescript")
	assert.True(t, ok)
	assert.Equal(t, "alias://lang-ts", uri)

	_, ok = f.EngineFor("cobol")
	assert.False(t, ok)
}

func TestParseRefusesAnUnknownKey(t *testing.T) {
	t.Parallel()

	_, err := config.Parse([]byte(valid + "surprise: true\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading the factory")
}

func TestParseRefusesSomethingThatIsNotYAML(t *testing.T) {
	t.Parallel()

	_, err := config.Parse([]byte("name: [x: y\n"))
	require.Error(t, err)
}

func TestValidateNamesEveryProblemAtOnce(t *testing.T) {
	t.Parallel()

	_, err := config.Parse([]byte(`version: "1"
repos:
  - name: Golden_Go
    languages: []
  - name: golden-go
    url: u
    languages: [go, Rust]
  - name: golden-go
    url: u
    languages: [go]
engines:
  - alias: Go
    engine: https://example.com/lang-go
  - alias: go
    engine: forge://a
  - alias: go
    engine: forge://b
dependencies:
  python:
    pytest: ">=8"
state:
  engine: https://example.com/state
`))
	require.Error(t, err)

	for _, want := range []string{
		"name is required",
		"name must be lowercase kebab-case",
		"url is required",
		`language "Rust" must be lowercase kebab-case`,
		"duplicate repo name",
		"alias must be lowercase kebab-case",
		"engine must start with forge:// or alias://",
		"duplicate engine alias",
		`repos declare the language "Rust" and no engine has that alias`,
		`dependencies declare the language "python" and no engine has that alias`,
		"state: engine must start with forge:// or alias://",
	} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestValidateRefusesAFactoryWithNothingInIt(t *testing.T) {
	t.Parallel()

	err := config.Factory{Name: "x"}.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one repo is required")
	assert.Contains(t, err.Error(), "at least one language engine is required")
}

func TestLanguagesAreSortedSoASyncIsDeterministic(t *testing.T) {
	t.Parallel()

	f := config.Factory{Repos: []config.Repo{
		{Languages: []string{"typescript", "go"}},
		{Languages: []string{"rust", "go"}},
	}}

	assert.Equal(t, []string{"go", "rust", "typescript"}, f.Languages())
}

func TestAMemberMayCarryNoManifest(t *testing.T) {
	t.Parallel()

	f, err := config.Parse([]byte(`version: "1"
name: golden
repos:
  - name: golden-spec
    url: git@github.com:x/golden-spec.git
engines:
  - alias: go
    engine: forge://example.com/lang-go
`))
	require.NoError(t, err, "a spec repo is a member and nothing is generated into it")
	assert.Empty(t, f.Languages())
}

func TestAModulesMapIsRejected(t *testing.T) {
	t.Parallel()

	_, err := config.Parse([]byte(`version: "1"
name: golden
repos:
  - name: a
    url: u
    languages: [go]
engines:
  - alias: go
    engine: forge://x
modules:
  github.com/alexandremahdhaoui/golden-spec:
    path: ./golden-spec
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "modules")
}

func TestToolchainValidation(t *testing.T) {
	t.Parallel()

	base := `version: "1"
name: w
repos:
  - name: r
    url: u
engines:
  - alias: go
    engine: forge://example.com/lang-go
register:
  url: git@example.com:owner/register.git
toolchain:
  binaries:
%s`

	cases := map[string]struct {
		binaries string
		want     string
	}{
		"a literal pin parses": {
			binaries: `    - name: mockery
      module: github.com/vektra/mockery/v3
      version: v3.5.5`,
		},
		"a track parses": {
			binaries: `    - name: mockery
      module: github.com/vektra/mockery/v3
      track: go:github.com/vektra/mockery/v3`,
		},
		"no pin at all": {
			binaries: `    - name: mockery
      module: github.com/vektra/mockery/v3`,
			want: "exactly one of track or version",
		},
		"both pins": {
			binaries: `    - name: mockery
      module: github.com/vektra/mockery/v3
      track: go:x
      version: v1`,
			want: "not both",
		},
		"no module": {
			binaries: `    - name: mockery
      version: v1`,
			want: "module path",
		},
		"no name": {
			binaries: `    - module: m/x
      version: v1`,
			want: "needs a name",
		},
		"malformed track": {
			binaries: `    - name: x
      module: m/x
      track: not-a-track`,
			want: "<ecosystem>:<package>",
		},
		"duplicate names": {
			binaries: `    - name: x
      module: m/x
      version: v1
    - name: x
      module: m/y
      version: v1`,
			want: "duplicate binary name",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Parse([]byte(fmt.Sprintf(base, tc.binaries)))

			if tc.want == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestToolchainImageValidation(t *testing.T) {
	t.Parallel()

	base := `version: "1"
name: w
repos:
  - name: r
    url: u
engines:
  - alias: go
    engine: forge://example.com/lang-go
register:
  url: git@example.com:owner/register.git
toolchain:
  image:
%s`

	cases := map[string]struct {
		image string
		want  string
	}{
		"a tracked image parses": {
			image: `    ref: ghcr.io/owner/tools
    track: internal:ghcr.io/owner/tools`,
		},
		"a literal pin parses": {
			image: `    ref: ghcr.io/owner/tools
    version: v1.0.0`,
		},
		"no ref": {
			image: `    track: internal:ghcr.io/owner/tools`,
			want:  "needs a ref",
		},
		"no pin at all": {
			image: `    ref: ghcr.io/owner/tools`,
			want:  "exactly one of track or version",
		},
		"both pins": {
			image: `    ref: ghcr.io/owner/tools
    track: internal:x
    version: v1`,
			want: "not both",
		},
		"malformed track": {
			image: `    ref: ghcr.io/owner/tools
    track: not-a-track`,
			want: "<ecosystem>:<package>",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Parse([]byte(fmt.Sprintf(base, tc.image)))

			if tc.want == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestAToolchainImageTrackNeedsARegister(t *testing.T) {
	t.Parallel()

	_, err := config.Parse([]byte(`version: "1"
name: w
repos:
  - name: r
    url: u
engines:
  - alias: go
    engine: forge://example.com/lang-go
toolchain:
  image:
    ref: ghcr.io/owner/tools
    track: internal:ghcr.io/owner/tools
`))
	require.ErrorContains(t, err, "no register: block")
}

func TestAToolchainTrackNeedsARegister(t *testing.T) {
	t.Parallel()

	_, err := config.Parse([]byte(`version: "1"
name: w
repos:
  - name: r
    url: u
engines:
  - alias: go
    engine: forge://example.com/lang-go
toolchain:
  binaries:
    - name: x
      module: m/x
      track: go:m/x
`))
	require.ErrorContains(t, err, "no register: block")
}

func TestAnAcknowledgementMustBeDeliberate(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "no id accepts everything and nothing",
			yaml: "  - reason: \"we looked\"\n",
			want: "names no advisory id",
		},
		{
			name: "no reason is a config error, like a pin without one",
			yaml: "  - id: GO-2026-5932\n",
			want: "accepting a risk without a reason",
		},
		{
			name: "the same id twice",
			yaml: "  - id: GO-1\n    reason: \"a\"\n  - id: GO-1\n    reason: \"b\"\n",
			want: "acknowledged twice",
		},
		{
			name: "an expiry that is not a date",
			yaml: "  - id: GO-1\n    reason: \"a\"\n    expires: \"soon\"\n",
			want: "expires must be a date",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := `name: w
register:
  url: git@example.com:r.git
repos:
  - name: m
    url: git@example.com:m.git
    languages: [go]
engines:
  - alias: go
    engine: forge://example.com/lang-go
dependencies:
  go:
    golang.org/x/crypto:
      track: "0"
      acknowledge:
` + indentBlock(tc.yaml)

			_, err := config.Parse([]byte(doc))
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestADeliberateAcknowledgementParses(t *testing.T) {
	doc := `name: w
register:
  url: git@example.com:r.git
repos:
  - name: m
    url: git@example.com:m.git
    languages: [go]
engines:
  - alias: go
    engine: forge://example.com/lang-go
dependencies:
  go:
    golang.org/x/crypto:
      track: "0"
      acknowledge:
        - id: GO-2026-5932
          reason: "we use nacl/box only; nothing under openpgp/ is imported"
        - id: GO-2026-9999
          reason: "accepted until the rewrite lands"
          expires: "2027-01-01"
`

	f, err := config.Parse([]byte(doc))
	require.NoError(t, err)

	acks := f.Dependencies["go"]["golang.org/x/crypto"].Acknowledge
	require.Len(t, acks, 2)
	require.Equal(t, "GO-2026-5932", acks[0].ID)
	require.Contains(t, acks[0].Reason, "nacl/box")
	require.Equal(t, "2027-01-01", acks[1].Expires)
}

func indentBlock(s string) string {
	out := ""

	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		out += "      " + line + "\n"
	}

	return out
}

func TestADependencyEntryRefusesAKeyItDoesNotKnow(t *testing.T) {
	for name, entry := range map[string]string{
		"a misspelled track":   `{ trak: "1" }`,
		"a misspelled expiry":  `{ acknowledge: [{ id: X-1, reason: r, expres: "2027-01-01" }] }`,
		"an invented key":      `{ track: "1", bogus: true }`,
		"a misspelled version": `{ verison: "v1.0.0" }`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.Parse([]byte(`version: "1"
name: w
repos:
  - name: m
    url: u
    languages: [go]
register:
  url: git@example.com:o/r.git
dependencies:
  go:
    example.com/pkg: ` + entry + `
`))
			require.ErrorContains(t, err, "unknown field")
		})
	}
}

func TestRuntimeValidation(t *testing.T) {
	t.Parallel()

	base := `version: "1"
name: w
repos:
  - name: r
    url: u
engines:
  - alias: go
    engine: forge://example.com/lang-go
  - alias: jre
    engine: forge://example.com/runtime-archive
  - alias: rust
    engine: forge://example.com/lang-rust
register:
  url: git@example.com:owner/register.git
toolchain:
%s`

	cases := map[string]struct {
		toolchain string
		want      string
	}{
		"a literal pin bound to its engine parses": {
			toolchain: `  runtimes:
    go: { version: "1.26.5" }
    jre: { version: "21.0.5+11", params: { source: temurin } }`,
		},
		"a track parses": {
			toolchain: `  runtimes:
    go: { track: "runtime:go" }`,
		},
		"no pin at all": {
			toolchain: `  runtimes:
    go: {}`,
			want: "exactly one of track or version pins a runtime",
		},
		"both pins": {
			toolchain: `  runtimes:
    go: { track: "runtime:go", version: "1.26.5" }`,
			want: "not both",
		},
		"a malformed track": {
			toolchain: `  runtimes:
    go: { track: "no-colon" }`,
			want: "a track is named <ecosystem>:<package>",
		},
		"a runtime with no engine under its alias": {
			toolchain: `  runtimes:
    zig: { version: "0.15.1" }`,
			want: `no engine is declared under the alias "zig"`,
		},
		"a name that is not kebab-case": {
			toolchain: `  runtimes:
    Go: { version: "1.26.5" }`,
			want: "lowercase kebab-case",
		},
		"satisfy naming the host parses": {
			toolchain: `  runtimes:
    go: { version: "1.26.5" }
  satisfy:
    c-compiler: host`,
		},
		"satisfy naming a declared runtime parses": {
			toolchain: `  runtimes:
    go: { version: "1.26.5" }
    jre: { version: "21.0.5+11" }
  satisfy:
    jvm: jre`,
		},
		"satisfy naming a stranger is refused": {
			toolchain: `  runtimes:
    go: { version: "1.26.5" }
  satisfy:
    c-compiler: zig`,
			want: `neither "host" nor a declared runtime`,
		},
		"the windows and aarch64 targets parse": {
			toolchain: `  runtimes:
    rust: { version: "1.97.0", targets: [x86_64-pc-windows-gnu, aarch64-unknown-linux-gnu] }`,
		},
		"a target nobody knows is refused and the known ones are named": {
			toolchain: `  runtimes:
    rust: { version: "1.97.0", targets: [wasm32-unknown-unknown] }`,
			want: `toolchain.runtimes.rust.targets[0]: "wasm32-unknown-unknown" is not a known target triple. known targets are aarch64-unknown-linux-gnu, x86_64-pc-windows-gnu, x86_64-unknown-linux-gnu`,
		},
		"a target declared twice is refused": {
			toolchain: `  runtimes:
    rust: { version: "1.97.0", targets: [x86_64-pc-windows-gnu, x86_64-pc-windows-gnu] }`,
			want: `toolchain.runtimes.rust.targets[1]: "x86_64-pc-windows-gnu" is declared twice`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Parse([]byte(fmt.Sprintf(base, tc.toolchain)))

			if tc.want == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestARuntimeKeepsItsTargetsInDeclaredOrder(t *testing.T) {
	t.Parallel()

	f, err := config.Parse([]byte(`version: "1"
name: w
repos:
  - name: r
    url: u
engines:
  - alias: rust
    engine: forge://example.com/lang-rust
toolchain:
  runtimes:
    rust: { version: "1.97.0", targets: [x86_64-pc-windows-gnu, aarch64-unknown-linux-gnu] }`))
	require.NoError(t, err)

	assert.Equal(t, []string{"x86_64-pc-windows-gnu", "aarch64-unknown-linux-gnu"},
		f.Toolchain.Runtimes["rust"].Targets)
}

func TestARuntimeTrackNeedsARegister(t *testing.T) {
	t.Parallel()

	raw := `version: "1"
name: w
repos:
  - name: r
    url: u
engines:
  - alias: go
    engine: forge://example.com/lang-go
toolchain:
  runtimes:
    go: { track: "runtime:go" }`

	_, err := config.Parse([]byte(raw))
	require.ErrorContains(t, err, "no register: block is declared")
}

func TestSpecForAnswersTheDeclaredSpec(t *testing.T) {
	t.Parallel()

	f := config.Factory{Engines: []config.Engine{
		{Alias: "fetch", Engine: "forge://x", Spec: map[string]any{"rewrite": []any{}}},
		{Alias: "bare", Engine: "forge://y"},
	}}

	assert.NotNil(t, f.SpecFor("fetch"))
	assert.Nil(t, f.SpecFor("bare"))
	assert.Nil(t, f.SpecFor("absent"))
}
