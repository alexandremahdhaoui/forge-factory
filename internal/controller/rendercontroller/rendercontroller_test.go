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

func find(t *testing.T, out rendertypes.Output, suffix string) rendertypes.File {
	t.Helper()

	for _, f := range out.Files {
		if strings.HasSuffix(f.Path, suffix) {
			return f
		}
	}

	t.Fatalf("no file ending in %q among %d files", suffix, len(out.Files))

	return rendertypes.File{}
}

func TestGoRender(t *testing.T) {
	t.Parallel()

	in := input()
	in.Dependencies = map[string]string{
		"sigs.k8s.io/yaml":            "v1.6.0",
		"github.com/stretchr/testify": "v1.12.0",
	}

	out, err := rendercontroller.Go{}.Render(in)
	require.NoError(t, err)
	require.Len(t, out.Files, 2)

	mod := find(t, out, "go.mod")
	assert.Equal(t, "/w/alpha-go/go.mod", mod.Path)
	assert.Equal(t, "alpha-go", mod.Gitignore)
	assert.Equal(t, []string{"go.sum"}, mod.AlsoIgnore, "a derived file is never committed either")
	assert.True(t, strings.HasPrefix(mod.Content, "//"),
		"a # line in go.mod is an unknown directive and every go command refuses the file")
	assert.Contains(t, mod.Content, "module example.com/alpha")
	assert.Contains(t, mod.Content, "go 1.26")
	assert.Contains(t, mod.Content, "\tgithub.com/stretchr/testify v1.12.0\n\tsigs.k8s.io/yaml v1.6.0\n")

	work := find(t, out, "go.work")
	assert.Equal(t, "/w/go.work", work.Path)
	assert.Empty(t, work.Gitignore, "the root is not a git repo, so nothing ignores it")
	assert.Contains(t, work.Content, "./alpha-go")
}

func TestACommittedManifestIsWrittenAndNeverIgnored(t *testing.T) {
	t.Parallel()

	in := input()
	in.Repos[0].Identity["manifest"] = "committed"

	out, err := rendercontroller.Go{}.Render(in)
	require.NoError(t, err)

	mod := find(t, out, "go.mod")
	assert.Contains(t, mod.Content, "module example.com/alpha",
		"sync still writes the file; committing it is what makes bare go run work")
	assert.Empty(t, mod.Gitignore)
	assert.Empty(t, mod.AlsoIgnore)
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

	out, err := rendercontroller.Go{}.Render(in)
	require.NoError(t, err)

	assert.Contains(t, find(t, out, "go.work").Content, "go 1.26.5")
}

func TestGoWorkIgnoresAnUnreadableVersion(t *testing.T) {
	t.Parallel()

	in := rendertypes.Input{Root: "/w", Repos: []rendertypes.Repo{
		{
			Name: "a", Path: "/w/a", Languages: []string{"go"},
			Identity: map[string]string{"module": "example.com/a", "goVersion": "tip"},
		},
	}}

	out, err := rendercontroller.Go{}.Render(in)
	require.NoError(t, err)

	assert.Contains(t, find(t, out, "go.work").Content, "go 1.25")
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
		out, err := r.Render(in)
		require.NoError(t, err, r.Language())
		assert.Empty(t, out.Files, r.Language())
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

	out, err := rendercontroller.Rust{}.Render(in)
	require.NoError(t, err)
	require.Len(t, out.Files, 1, "cargo centralises versions, so nothing is written inside a repo")

	assert.Equal(t, "/w/Cargo.toml", out.Files[0].Path)
	assert.Contains(t, out.Files[0].Content, `    "beta-rust",`)
	assert.Contains(t, out.Files[0].Content, `    "beta-rust/inner",`)
	assert.Contains(t, out.Files[0].Content, "[workspace.dependencies]\nserde = \"1\"\nthiserror = \"2\"\n")
}

func TestPythonRendersAWholeProject(t *testing.T) {
	t.Parallel()

	in := input()
	in.Dependencies = map[string]string{"httpx": ">=0.28"}
	in.Dev = map[string]string{"pytest": ">=8", "ruff": ">=0.6"}

	out, err := rendercontroller.Python{}.Render(in)
	require.NoError(t, err)
	require.Len(t, out.Files, 1)

	assert.Equal(t, "/w/gamma-py/pyproject.toml", out.Files[0].Path)
	assert.Equal(t, "gamma-py", out.Files[0].Gitignore)
	assert.Contains(t, out.Files[0].Content, `name = "gamma-py"`)
	assert.Contains(t, out.Files[0].Content, `requires-python = ">=3.12"`)
	assert.Contains(t, out.Files[0].Content, `    "pytest>=8",`)
	assert.Contains(t, out.Files[0].Content, `packages = ["src/gamma"]`)
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

	out, err := rendercontroller.Python{}.Render(in)
	require.NoError(t, err)
	assert.Contains(t, out.Files[0].Content, `version = "2.3.4"`)
	assert.Contains(t, out.Files[0].Content, `requires-python = ">=3.13"`)
}

func TestTypeScriptRendersNoScripts(t *testing.T) {
	t.Parallel()

	in := input()
	in.Dependencies = map[string]string{"neverthrow": "^8", "vitest": "^3"}

	out, err := rendercontroller.TypeScript{}.Render(in)
	require.NoError(t, err)
	require.Len(t, out.Files, 2)

	pkg := find(t, out, "package.json")
	assert.Equal(t, "delta-ts", pkg.Gitignore)
	assert.NotContains(t, pkg.Content, `"scripts"`, "scripts duplicate forge stages")
	assert.Contains(t, pkg.Content, `"bin": { "delta-ts": "dist/cmd/delta.js" }`)
	assert.Contains(t, pkg.Content, "\"neverthrow\": \"^8\",\n    \"vitest\": \"^3\"\n")

	ws := find(t, out, "pnpm-workspace.yaml")
	assert.Contains(t, ws.Content, `  - "delta-ts"`)
}

func TestTypeScriptOmitsBinWhenNoRepoDeclaresOne(t *testing.T) {
	t.Parallel()

	in := rendertypes.Input{Root: "/w", Repos: []rendertypes.Repo{
		{Name: "a", Path: "/w/a", Languages: []string{"typescript"}},
	}}

	out, err := rendercontroller.TypeScript{}.Render(in)
	require.NoError(t, err)
	assert.NotContains(t, find(t, out, "package.json").Content, `"bin"`)
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
		out, err := r.Render(in)
		require.NoError(t, err)

		for _, f := range out.Files {
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

	out, err := rendercontroller.Go{}.Render(in)
	require.NoError(t, err)
	assert.Contains(t, find(t, out, "go.work").Content, "go 1.25.1")
}

func TestGoAsksForATidyPerMember(t *testing.T) {
	t.Parallel()

	out, err := rendercontroller.Go{}.Render(input())
	require.NoError(t, err)
	require.Len(t, out.Settle, 2)

	assert.Equal(t, "/w/alpha-go", out.Settle[0].Dir)
	assert.Equal(t, "go", out.Settle[0].Command)
	assert.Equal(t, []string{"mod", "tidy"}, out.Settle[0].Args)
	assert.Equal(t, map[string]string{"GOWORK": "off"}, out.Settle[0].Env,
		"in workspace mode a tidy writes no per module sums")
	assert.True(t, out.Settle[0].Optional, "a sync must still work offline")

	assert.Equal(t, "/w", out.Settle[1].Dir)
	assert.Equal(t, []string{"work", "sync"}, out.Settle[1].Args)
	assert.Equal(t, map[string]string{"GOWORK": "/w/go.work"}, out.Settle[1].Env,
		"GOWORK=off in the caller makes go work sync deny the file this sync wrote")
}

func TestNoOtherLanguageNeedsSettling(t *testing.T) {
	t.Parallel()

	for _, r := range []rendercontroller.Renderer{
		rendercontroller.Rust{},
		rendercontroller.Python{},
		rendercontroller.TypeScript{},
	} {
		out, err := r.Render(input())
		require.NoError(t, err)
		assert.Empty(t, out.Settle, r.Language())
	}
}

func ownManifest() rendertypes.Input {
	in := input()

	for i := range in.Repos {
		in.Repos[i].Identity["manifest"] = rendercontroller.OwnManifest
	}

	return in
}

func TestARepoThatKeepsItsOwnManifestStillGetsMembership(t *testing.T) {
	t.Parallel()

	in := ownManifest()

	out, err := rendercontroller.Go{}.Render(in)
	require.NoError(t, err)
	require.Len(t, out.Files, 1)
	assert.Equal(t, "/w/go.work", out.Files[0].Path)
	require.Len(t, out.Settle, 1)
	assert.Equal(t, []string{"work", "sync"}, out.Settle[0].Args,
		"nothing to tidy, and the workspace still needs syncing")

	out, err = rendercontroller.TypeScript{}.Render(in)
	require.NoError(t, err)
	require.Len(t, out.Files, 1)
	assert.Equal(t, "/w/pnpm-workspace.yaml", out.Files[0].Path)
	assert.Contains(t, out.Files[0].Content, "delta-ts")

	out, err = rendercontroller.Python{}.Render(in)
	require.NoError(t, err)
	assert.Empty(t, out.Files, "python has no membership file, so nothing is written at all")
}

func TestARepoThatKeepsItsOwnManifestNeedsNoIdentity(t *testing.T) {
	t.Parallel()

	in := rendertypes.Input{Root: "/w", Repos: []rendertypes.Repo{
		{
			Name: "a", Path: "/w/a", Languages: []string{"go", "python"},
			Identity: map[string]string{"manifest": rendercontroller.OwnManifest},
		},
	}}

	_, err := rendercontroller.Go{}.Render(in)
	require.NoError(t, err, "the module path is only needed to write a go.mod")

	_, err = rendercontroller.Python{}.Render(in)
	require.NoError(t, err)
}

func TestRustTakesAVersionWithFeaturesVerbatim(t *testing.T) {
	t.Parallel()

	in := input()
	in.Dependencies = map[string]string{
		"serde":  `{ version = "1", features = ["derive"] }`,
		"anyhow": "1",
		"empty":  "",
	}

	out, err := rendercontroller.Rust{}.Render(in)
	require.NoError(t, err)

	got := out.Files[0].Content
	assert.Contains(t, got, `serde = { version = "1", features = ["derive"] }`)
	assert.Contains(t, got, `anyhow = "1"`)
	assert.Contains(t, got, `empty = ""`)
}

func TestTypeScriptCollectsWhatEveryMemberNeedsBuilt(t *testing.T) {
	t.Parallel()

	in := rendertypes.Input{Root: "/w", Repos: []rendertypes.Repo{
		{
			Name: "a", Path: "/w/a", Languages: []string{"typescript"},
			Identity: map[string]string{"allowBuilds": "esbuild, sharp"},
		},
		{
			Name: "b", Path: "/w/b", Languages: []string{"typescript"},
			Identity: map[string]string{"allowBuilds": "esbuild"},
		},
	}}

	out, err := rendercontroller.TypeScript{}.Render(in)
	require.NoError(t, err)

	ws := find(t, out, "pnpm-workspace.yaml").Content
	assert.Contains(t, ws, "allowBuilds:\n  esbuild: true\n  sharp: true\n",
		"pnpm blocks a build script by default and the list is per workspace")
}

func TestNoAllowBuildsSectionWhenNobodyNeedsOne(t *testing.T) {
	t.Parallel()

	out, err := rendercontroller.TypeScript{}.Render(input())
	require.NoError(t, err)
	assert.NotContains(t, find(t, out, "pnpm-workspace.yaml").Content, "allowBuilds")
}

func TestPythonCarriesWhatOnlyTheRepoKnows(t *testing.T) {
	t.Parallel()

	in := rendertypes.Input{Root: "/w", Repos: []rendertypes.Repo{
		{Name: "a", Path: "/w/a", Languages: []string{"python"}, Identity: map[string]string{
			"package":     "a_pkg",
			"description": "a golden repo",
			"entrypoint":  "a_pkg.cmd.main:main",
			"toolConfig":  "[tool.ruff]\nline-length = 100\n",
		}},
	}}

	out, err := rendercontroller.Python{}.Render(in)
	require.NoError(t, err)

	got := out.Files[0].Content
	assert.Contains(t, got, `description = "a golden repo"`)
	assert.Contains(t, got, "[project.scripts]\na = \"a_pkg.cmd.main:main\"")
	assert.Contains(t, got, "[tool.ruff]\nline-length = 100",
		"pytest and ruff settings are the repo's business and no factory declares them")
	assert.Equal(t, []string{"uv.lock"}, out.Files[0].AlsoIgnore)
}

func TestPythonOmitsWhatNobodyDeclared(t *testing.T) {
	t.Parallel()

	in := rendertypes.Input{Root: "/w", Repos: []rendertypes.Repo{
		{
			Name: "a", Path: "/w/a", Languages: []string{"python"},
			Identity: map[string]string{"package": "a_pkg"},
		},
	}}

	out, err := rendercontroller.Python{}.Render(in)
	require.NoError(t, err)

	got := out.Files[0].Content
	assert.NotContains(t, got, "description =")
	assert.NotContains(t, got, "[project.scripts]")
	assert.NotContains(t, got, "[dependency-groups]")
	assert.NotContains(t, got, "[tool.ruff]")
	assert.Contains(t, got, "[tool.hatch.build.targets.wheel]", "the build backend is always written")
}

func TestTypeScriptSeparatesWhatOnlyTheToolingNeeds(t *testing.T) {
	t.Parallel()

	in := input()
	in.Dependencies = map[string]string{"fastify": "^5"}
	in.Dev = map[string]string{"vitest": "^3", "typescript": "^5.6"}
	in.Repos[3].Identity["description"] = "a golden repo"

	out, err := rendercontroller.TypeScript{}.Render(in)
	require.NoError(t, err)

	pkg := find(t, out, "package.json").Content
	assert.Contains(t, pkg, `"description": "a golden repo"`)
	assert.Contains(t, pkg, "\"dependencies\": {\n    \"fastify\": \"^5\"\n  },")
	assert.Contains(t, pkg, "\"devDependencies\": {\n    \"typescript\": \"^5.6\",\n    \"vitest\": \"^3\"\n  }\n}")
	assert.NotContains(t, pkg, `"scripts"`, "forge stages already run what a script would")
}

func TestTypeScriptWithNoDevDependencies(t *testing.T) {
	t.Parallel()

	in := input()
	in.Dependencies = map[string]string{"fastify": "^5"}

	out, err := rendercontroller.TypeScript{}.Render(in)
	require.NoError(t, err)

	pkg := find(t, out, "package.json").Content
	assert.NotContains(t, pkg, "devDependencies")
	assert.Contains(t, pkg, "\"fastify\": \"^5\"\n  }\n}")
}
