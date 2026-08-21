package config_test

import (
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
    engine: go://example.com/lang-go
  - alias: typescript
    engine: alias://lang-ts
state:
  engine: go://example.com/ci-state-git
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
    engine: go://a
  - alias: go
    engine: go://b
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
		"engine must start with go:// or alias://",
		"duplicate engine alias",
		`repos declare the language "Rust" and no engine has that alias`,
		`dependencies declare the language "python" and no engine has that alias`,
		"state: engine must start with go:// or alias://",
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
    engine: go://example.com/lang-go
`))
	require.NoError(t, err, "a spec repo is a member and nothing is generated into it")
	assert.Empty(t, f.Languages())
}

func TestModulesResolveLocallyOrRemotely(t *testing.T) {
	t.Parallel()

	f, err := config.Parse([]byte(`version: "1"
name: golden
repos:
  - name: a
    url: u
    languages: [go]
engines:
  - alias: go
    engine: go://x
modules:
  github.com/alexandremahdhaoui/golden-spec:
    path: ./golden-spec
    version: v0.2.0
    specs: [api/golden.v1.yaml]
`))
	require.NoError(t, err)
	assert.Equal(t, []string{"github.com/alexandremahdhaoui/golden-spec"}, f.ModulePaths())
	assert.Equal(t, "./golden-spec", f.Modules["github.com/alexandremahdhaoui/golden-spec"].Path)
}

func TestAModuleNeedsSomewhereToResolveFrom(t *testing.T) {
	t.Parallel()

	err := config.Factory{
		Name:    "x",
		Repos:   []config.Repo{{Name: "a", URL: "u"}},
		Engines: []config.Engine{{Alias: "go", Engine: "go://x"}},
		Modules: map[string]config.Module{"example.com/m": {}},
	}.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs a path, a version, or both")
}
