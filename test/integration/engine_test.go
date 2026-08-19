//go:build integration

package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/engineadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var binDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "forge-factory-bin")
	if err != nil {
		panic(err)
	}

	binDir = dir

	cmd := exec.Command("go", "build", "-o", dir, "./cmd/...")
	cmd.Dir = repoRoot()
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		panic(err)
	}

	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		panic(err)
	}

	code := m.Run()

	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	return filepath.Dir(filepath.Dir(wd))
}

func caller() *engineadapter.MCPCaller {
	return engineadapter.NewMCPCaller("", "test", os.Stderr)
}

type repo struct {
	Name      string            `json:"name"`
	Path      string            `json:"path"`
	Languages []string          `json:"languages"`
	Identity  map[string]string `json:"identity,omitempty"`
}

type renderInput struct {
	Root         string            `json:"root"`
	Repos        []repo            `json:"repos"`
	Dependencies map[string]string `json:"dependencies"`
}

type file struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Gitignore string `json:"gitignore,omitempty"`
}

type renderOutput struct {
	Files []file `json:"files"`
}

type languageOutput struct {
	Language string `json:"language"`
}

// empty is the smallest input an engine accepts. A nil slice or map travels as
// null and every engine's generated schema refuses it.
func empty() renderInput {
	return renderInput{Repos: []repo{}, Dependencies: map[string]string{}}
}

func uri(name string) string {
	return "go://" + name
}

func TestEveryEngineReportsItsOwnLanguage(t *testing.T) {
	for engine, language := range map[string]string{
		"factory-lang-go":         "go",
		"factory-lang-rust":       "rust",
		"factory-lang-python":     "python",
		"factory-lang-typescript": "typescript",
	} {
		t.Run(engine, func(t *testing.T) {
			var out languageOutput

			require.NoError(t, caller().Call(t.Context(), uri(engine), "language", empty(), &out))
			assert.Equal(t, language, out.Language)
		})
	}
}

func TestTheGoEngineRendersOverMCP(t *testing.T) {
	in := renderInput{
		Root: "/w",
		Repos: []repo{{
			Name:      "golden-go",
			Path:      "/w/golden-go",
			Languages: []string{"go"},
			Identity:  map[string]string{"module": "example.com/golden", "goVersion": "1.26"},
		}},
		Dependencies: map[string]string{"sigs.k8s.io/yaml": "v1.6.0"},
	}

	var out renderOutput

	require.NoError(t, caller().Call(t.Context(), uri("factory-lang-go"), "render", in, &out))
	require.Len(t, out.Files, 2)

	byPath := map[string]file{}
	for _, f := range out.Files {
		byPath[f.Path] = f
	}

	mod, ok := byPath["/w/golden-go/go.mod"]
	require.True(t, ok)
	assert.Equal(t, "golden-go", mod.Gitignore)
	assert.Contains(t, mod.Content, "module example.com/golden")
	assert.Contains(t, mod.Content, "sigs.k8s.io/yaml v1.6.0")

	work, ok := byPath["/w/go.work"]
	require.True(t, ok)
	assert.Contains(t, work.Content, "./golden-go")
}

func TestAnEngineReportsAMemberMissingItsIdentity(t *testing.T) {
	in := empty()
	in.Root = "/w"
	in.Repos = []repo{{Name: "golden-go", Path: "/w/golden-go", Languages: []string{"go"}}}

	var out renderOutput

	err := caller().Call(t.Context(), uri("factory-lang-go"), "render", in, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "factory.module")
}

func TestARenderThatConcernsNoMemberAnswersNoFiles(t *testing.T) {
	in := empty()
	in.Root = "/w"
	in.Repos = []repo{{Name: "golden-go", Path: "/w/golden-go", Languages: []string{"go"}}}

	var out renderOutput

	require.NoError(t, caller().Call(t.Context(), uri("factory-lang-rust"), "render", in, &out))
	assert.Empty(t, out.Files)
}

func TestEveryEngineAnswersItsOwnDocsValidate(t *testing.T) {
	for _, engine := range []string{
		"factory-lang-go",
		"factory-lang-rust",
		"factory-lang-python",
		"factory-lang-typescript",
	} {
		t.Run(engine, func(t *testing.T) {
			cmd := exec.Command(filepath.Join(binDir, engine), "docs", "list")
			cmd.Dir = repoRoot()

			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))
			assert.Contains(t, string(out), "usage")
			assert.Contains(t, string(out), "schema")
			assert.GreaterOrEqual(t, strings.Count(string(out), "\n"), 2)
		})
	}
}
