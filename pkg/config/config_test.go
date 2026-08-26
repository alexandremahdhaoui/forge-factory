package config_test

import (
	"fmt"
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

	// The register subsumed the modules map; the strict parse names the
	// dead key so every stale factory file fails at once, loudly.
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

// The toolchain section is the one governed place standalone tool versions
// live. Exactly one of track or version pins each entry.
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

// A track resolves from the register, so declaring one without a register
// block is a config error.
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
