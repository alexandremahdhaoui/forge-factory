package rendercontroller_test

import (
	"strings"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/rendercontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/rendertypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func input() rendertypes.Input {
	return rendertypes.Input{
		Root: "/w",
		Repos: []rendertypes.Repo{
			{
				Name:      "alpha-go",
				Path:      "/w/alpha-go",
				Languages: []string{"go"},
				Identity:  map[string]string{"module": "example.com/alpha", "goVersion": "1.26"},
			},
			{
				Name:      "beta-rust",
				Path:      "/w/beta-rust",
				Languages: []string{"rust"},
				Identity:  map[string]string{"cargoMembers": "beta-rust/inner"},
			},
			{
				Name:      "gamma-py",
				Path:      "/w/gamma-py",
				Languages: []string{"python"},
				Identity:  map[string]string{"package": "gamma"},
			},
			{
				Name:      "delta-ts",
				Path:      "/w/delta-ts",
				Languages: []string{"typescript"},
				Identity:  map[string]string{"bin": "dist/cmd/delta.js"},
			},
		},
	}
}

func find(t *testing.T, files []rendertypes.File, suffix string) rendertypes.File {
	t.Helper()

	for _, f := range files {
		if strings.HasSuffix(f.Path, suffix) {
			return f
		}
	}

	t.Fatalf("no file ending in %q among %d files", suffix, len(files))

	return rendertypes.File{}
}

func TestGoRender(t *testing.T) {
	t.Parallel()

	in := input()
	in.Dependencies = map[string]string{
		"sigs.k8s.io/yaml":            "v1.6.0",
		"github.com/stretchr/testify": "v1.12.0",
	}

	files, err := rendercontroller.Go{}.Render(in)
	require.NoError(t, err)
	require.Len(t, files, 2)

	mod := find(t, files, "go.mod")
	assert.Equal(t, "/w/alpha-go/go.mod", mod.Path)
	assert.Equal(t, "alpha-go", mod.Gitignore)
	assert.Contains(t, mod.Content, "module example.com/alpha")
	assert.Contains(t, mod.Content, "go 1.26")
	assert.Contains(t, mod.Content, "\tgithub.com/stretchr/testify v1.12.0\n\tsigs.k8s.io/yaml v1.6.0\n")

	work := find(t, files, "go.work")
	assert.Equal(t, "/w/go.work", work.Path)
	assert.Empty(t, work.Gitignore, "the root is not a git repo, so nothing ignores it")
	assert.Contains(t, work.Content, "./alpha-go")
}

func TestGoWorkTakesTheNewestMemberVersion(t *testing.T) {
	t.Parallel()

	in := rendertypes.Input{Root: "/w", Repos: []rendertypes.Repo{
		{
			Name: "a", Path: "/w/a", Languages: []string{"go"},
			Identity: map[string]string{"module": "example.com/a", "goVersion": "1.24"},
		},
		{
			Name: "b", Path: "/w/b", Languages: []string{"go"},
			Identity: map[string]string{"module": "example.com/b", "goVersion": "1.26.5"},
		},
	}}

	files, err := rendercontroller.Go{}.Render(in)
	require.NoError(t, err)

	assert.Contains(t, find(t, files, "go.work").Content, "go 1.26.5")
}

func TestGoWorkIgnoresAnUnreadableVersion(t *testing.T) {
	t.Parallel()

	in := rendertypes.Input{Root: "/w", Repos: []rendertypes.Repo{
		{
			Name: "a", Path: "/w/a", Languages: []string{"go"},
			Identity: map[string]string{"module": "example.com/a", "goVersion": "tip"},
		},
	}}

	files, err := rendercontroller.Go{}.Render(in)
	require.NoError(t, err)

	assert.Contains(t, find(t, files, "go.work").Content, "go 1.25")
}

func TestGoRefusesARepoWithNoModulePath(t *testing.T) {
	t.Parallel()

	in := rendertypes.Input{Root: "/w", Repos: []rendertypes.Repo{
		{Name: "a", Path: "/w/a", Languages: []string{"go"}},
	}}

	_, err := rendercontroller.Go{}.Render(in)
	require.ErrorIs(t, err, rendercontroller.ErrIdentity)
	assert.Contains(t, err.Error(), "factory.module")
}

func TestARendererWritesNothingWhenNobodySpeaksItsLanguage(t *testing.T) {
	t.Parallel()

	in := rendertypes.Input{Root: "/w", Repos: []rendertypes.Repo{
		{Name: "a", Path: "/w/a", Languages: []string{"cobol"}},
	}}

	for _, r := range []rendercontroller.Renderer{
		rendercontroller.Go{},
		rendercontroller.Rust{},
		rendercontroller.Python{},
		rendercontroller.TypeScript{},
	} {
		files, err := r.Render(in)
		require.NoError(t, err, r.Language())
		assert.Empty(t, files, r.Language())
	}
}

func TestEachRendererNamesItsOwnLanguage(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "go", rendercontroller.Go{}.Language())
	assert.Equal(t, "rust", rendercontroller.Rust{}.Language())
	assert.Equal(t, "python", rendercontroller.Python{}.Language())
	assert.Equal(t, "typescript", rendercontroller.TypeScript{}.Language())
}

func TestRustRendersOnlyMembership(t *testing.T) {
	t.Parallel()

	in := input()
	in.Dependencies = map[string]string{"serde": `"1"`, "thiserror": `"2"`}

	files, err := rendercontroller.Rust{}.Render(in)
	require.NoError(t, err)
	require.Len(t, files, 1, "cargo centralises versions, so nothing is written inside a repo")

	assert.Equal(t, "/w/Cargo.toml", files[0].Path)
	assert.Contains(t, files[0].Content, `    "beta-rust",`)
	assert.Contains(t, files[0].Content, `    "beta-rust/inner",`)
	assert.Contains(t, files[0].Content, "[workspace.dependencies]\nserde = \"1\"\nthiserror = \"2\"\n")
}

func TestPythonRendersAWholeProject(t *testing.T) {
	t.Parallel()

	in := input()
	in.Dependencies = map[string]string{"pytest": ">=8"}

	files, err := rendercontroller.Python{}.Render(in)
	require.NoError(t, err)
	require.Len(t, files, 1)

	assert.Equal(t, "/w/gamma-py/pyproject.toml", files[0].Path)
	assert.Equal(t, "gamma-py", files[0].Gitignore)
	assert.Contains(t, files[0].Content, `name = "gamma-py"`)
	assert.Contains(t, files[0].Content, `requires-python = ">=3.12"`)
	assert.Contains(t, files[0].Content, `    "pytest>=8",`)
	assert.Contains(t, files[0].Content, `packages = ["src/gamma"]`)
}

func TestPythonRefusesARepoWithNoPackage(t *testing.T) {
	t.Parallel()

	in := rendertypes.Input{Root: "/w", Repos: []rendertypes.Repo{
		{Name: "a", Path: "/w/a", Languages: []string{"python"}},
	}}

	_, err := rendercontroller.Python{}.Render(in)
	require.ErrorIs(t, err, rendercontroller.ErrIdentity)
}

func TestPythonTakesTheVersionAndPythonAMemberDeclares(t *testing.T) {
	t.Parallel()

	in := rendertypes.Input{Root: "/w", Repos: []rendertypes.Repo{
		{Name: "a", Path: "/w/a", Languages: []string{"python"}, Identity: map[string]string{
			"package": "a", "version": "2.3.4", "requiresPython": ">=3.13",
		}},
	}}

	files, err := rendercontroller.Python{}.Render(in)
	require.NoError(t, err)
	assert.Contains(t, files[0].Content, `version = "2.3.4"`)
	assert.Contains(t, files[0].Content, `requires-python = ">=3.13"`)
}

func TestTypeScriptRendersNoScripts(t *testing.T) {
	t.Parallel()

	in := input()
	in.Dependencies = map[string]string{"neverthrow": "^8", "vitest": "^3"}

	files, err := rendercontroller.TypeScript{}.Render(in)
	require.NoError(t, err)
	require.Len(t, files, 2)

	pkg := find(t, files, "package.json")
	assert.Equal(t, "delta-ts", pkg.Gitignore)
	assert.NotContains(t, pkg.Content, `"scripts"`, "scripts duplicate forge stages")
	assert.Contains(t, pkg.Content, `"bin": { "delta-ts": "dist/cmd/delta.js" }`)
	assert.Contains(t, pkg.Content, "\"neverthrow\": \"^8\",\n    \"vitest\": \"^3\"\n")

	ws := find(t, files, "pnpm-workspace.yaml")
	assert.Contains(t, ws.Content, `  - "delta-ts"`)
}

func TestTypeScriptOmitsBinWhenNoRepoDeclaresOne(t *testing.T) {
	t.Parallel()

	in := rendertypes.Input{Root: "/w", Repos: []rendertypes.Repo{
		{Name: "a", Path: "/w/a", Languages: []string{"typescript"}},
	}}

	files, err := rendercontroller.TypeScript{}.Render(in)
	require.NoError(t, err)
	assert.NotContains(t, find(t, files, "package.json").Content, `"bin"`)
}

func TestEveryGeneratedFileSaysItIsGenerated(t *testing.T) {
	t.Parallel()

	in := input()

	for _, r := range []rendercontroller.Renderer{
		rendercontroller.Go{},
		rendercontroller.Rust{},
		rendercontroller.Python{},
		rendercontroller.TypeScript{},
	} {
		files, err := r.Render(in)
		require.NoError(t, err)

		for _, f := range files {
			assert.Contains(t, strings.ToLower(f.Content), "do not edit", f.Path)
		}
	}
}

func TestSpeaks(t *testing.T) {
	t.Parallel()

	repo := rendertypes.Repo{Languages: []string{"go", "typescript"}}

	assert.True(t, repo.Speaks("typescript"))
	assert.False(t, repo.Speaks("rust"))
}

func TestGoWorkPrefersALongerVersionOverItsOwnPrefix(t *testing.T) {
	t.Parallel()

	in := rendertypes.Input{Root: "/w", Repos: []rendertypes.Repo{
		{
			Name: "a", Path: "/w/a", Languages: []string{"go"},
			Identity: map[string]string{"module": "example.com/a", "goVersion": "1.25.1"},
		},
	}}

	files, err := rendercontroller.Go{}.Render(in)
	require.NoError(t, err)
	assert.Contains(t, find(t, files, "go.work").Content, "go 1.25.1")
}
