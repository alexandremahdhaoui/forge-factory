package speccontroller_test

import (
	"strings"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/speccontroller"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const factory = `version: "1"
name: golden

# The members. One line per repo.
repos:
  - name: golden-go
    url: git@github.com:x/golden-go.git
    languages: [go]
  - name: golden-rust
    url: git@github.com:x/golden-rust.git
    languages: [rust]

dependencies:
  go:
    github.com/stretchr/testify: v1.11.1
    sigs.k8s.io/yaml: v1.6.0
  rust:
    serde: "1"

engines:
  - alias: go
    engine: go://example.com/lang-go
  - alias: rust
    engine: go://example.com/lang-rust
`

func TestBumpRewritesOneLineAndKeepsComments(t *testing.T) {
	t.Parallel()

	out, edit, err := speccontroller.Bump([]byte(factory), "github.com/stretchr/testify", "v1.12.0")
	require.NoError(t, err)

	assert.Contains(t, string(out), "# The members. One line per repo.")
	assert.Contains(t, string(out), "github.com/stretchr/testify: v1.12.0")
	assert.NotContains(t, string(out), "v1.11.1")
	assert.Contains(t, edit.Was, "v1.11.1")
	assert.Contains(t, edit.Now, "v1.12.0")
	assert.Positive(t, edit.Line)

	f, err := config.Parse(out)
	require.NoError(t, err)
	assert.Equal(t, "v1.12.0", f.DependenciesFor("go")["github.com/stretchr/testify"].Version)
}

func TestBumpQuotesAVersionYAMLWouldReadAsANumber(t *testing.T) {
	t.Parallel()

	out, _, err := speccontroller.Bump([]byte(factory), "rust:serde", "2")
	require.NoError(t, err)
	assert.Contains(t, string(out), `serde: "2"`)

	out, _, err = speccontroller.Bump([]byte(factory), "rust:serde", "1.5")
	require.NoError(t, err)
	assert.Contains(t, string(out), `serde: "1.5"`)
}

func TestBumpRefusesADependencyNobodyDeclares(t *testing.T) {
	t.Parallel()

	_, _, err := speccontroller.Bump([]byte(factory), "example.com/nope", "v1")
	require.ErrorIs(t, err, speccontroller.ErrNotFound)
}

func TestBumpRefusesABareNameTwoLanguagesShare(t *testing.T) {
	t.Parallel()

	shared := strings.Replace(factory,
		"  rust:\n    serde: \"1\"\n",
		"  rust:\n    serde: \"1\"\n    github.com/stretchr/testify: v1.0.0\n", 1)

	_, _, err := speccontroller.Bump([]byte(shared), "github.com/stretchr/testify", "v1.12.0")
	require.ErrorIs(t, err, speccontroller.ErrAmbiguous)
}

func TestBumpTakesALanguagePrefixToPickOne(t *testing.T) {
	t.Parallel()

	out, _, err := speccontroller.Bump([]byte(factory), "go:sigs.k8s.io/yaml", "v1.7.0")
	require.NoError(t, err)
	assert.Contains(t, string(out), "sigs.k8s.io/yaml: v1.7.0")
}

func TestBumpRefusesAnEditThatBreaksTheFactory(t *testing.T) {
	t.Parallel()

	// An empty version used to be tolerated; under a register it means
	// "resolve from the register", which needs a register block, so the
	// re-parse refuses the edit.
	_, _, err := speccontroller.Bump([]byte(factory), "go:sigs.k8s.io/yaml", "")
	require.Error(t, err, "an empty version now means the register, and this factory has none")

	_, _, err = speccontroller.Bump([]byte("name: x\n"), "anything", "v1")
	require.ErrorIs(t, err, speccontroller.ErrNotFound)
}

func TestAddAppendsInsideTheReposList(t *testing.T) {
	t.Parallel()

	repo := config.Repo{
		Name:      "golden-python",
		URL:       "git@github.com:x/golden-python.git",
		Languages: []string{"python"},
	}

	withEngine := factory + "  - alias: python\n    engine: go://example.com/lang-python\n"

	out, edit, err := speccontroller.Add([]byte(withEngine), repo)
	require.NoError(t, err)

	f, err := config.Parse(out)
	require.NoError(t, err)
	require.Len(t, f.Repos, 3)
	assert.Equal(t, "golden-python", f.Repos[2].Name)
	assert.Equal(t, []string{"python"}, f.Repos[2].Languages)
	assert.Contains(t, edit.Now, "golden-python")
	assert.Contains(t, string(out), "dependencies:", "the sections after repos survive")
}

func TestAddRefusesAMemberAlreadyDeclared(t *testing.T) {
	t.Parallel()

	_, _, err := speccontroller.Add([]byte(factory), config.Repo{
		Name: "golden-go", URL: "u", Languages: []string{"go"},
	})
	require.ErrorIs(t, err, speccontroller.ErrExists)
}

func TestAddRefusesAMemberWithNoEngine(t *testing.T) {
	t.Parallel()

	_, _, err := speccontroller.Add([]byte(factory), config.Repo{
		Name: "golden-python", URL: "u", Languages: []string{"python"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no engine has that alias")
}

func TestAddRefusesAFactoryWithNoReposList(t *testing.T) {
	t.Parallel()

	_, _, err := speccontroller.Add([]byte("not: yaml: at: all\n"), config.Repo{Name: "a"})
	require.Error(t, err)
}

func TestDependenciesNamesEveryDeclaredVersion(t *testing.T) {
	t.Parallel()

	f, err := config.Parse([]byte(factory))
	require.NoError(t, err)

	assert.Equal(t, []string{
		"go:github.com/stretchr/testify",
		"go:sigs.k8s.io/yaml",
		"rust:serde",
	}, speccontroller.Dependencies(f))
}

func TestBumpSkipsCommentsAndSectionsThatAreNotDependencies(t *testing.T) {
	t.Parallel()

	noisy := strings.Replace(factory, "dependencies:", "# a comment\n\ndependencies:", 1)

	out, _, err := speccontroller.Bump([]byte(noisy), "rust:serde", "2")
	require.NoError(t, err)
	assert.Contains(t, string(out), "# a comment")
	assert.Contains(t, string(out), `serde: "2"`)
}

func TestBumpKeepsAVersionThatIsAlreadyQuoted(t *testing.T) {
	t.Parallel()

	out, _, err := speccontroller.Bump([]byte(factory), "rust:serde", `"3"`)
	require.NoError(t, err)
	assert.Contains(t, string(out), `serde: "3"`)
}

func TestAddLandsAfterAReposListFollowedByNothing(t *testing.T) {
	t.Parallel()

	minimal := `version: "1"
name: golden
engines:
  - alias: go
    engine: go://x
repos:
  - name: a
    url: u
    languages: [go]
`

	out, _, err := speccontroller.Add([]byte(minimal), config.Repo{
		Name: "b", URL: "u", Languages: []string{"go"},
	})
	require.NoError(t, err)

	f, err := config.Parse(out)
	require.NoError(t, err)
	require.Len(t, f.Repos, 2)
}
