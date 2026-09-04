package docscontroller_test

import (
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/docscontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/docstypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderConcepts(t *testing.T) {
	t.Parallel()

	file, err := docscontroller.RenderConcepts(docstypes.Concepts{
		Tool:    "forge-factory",
		Summary: "One file says what a workspace is made of.",
		Verbs: []docstypes.Verb{
			{Name: "sync", Summary: "Materialise every generated file"},
			{Name: "bump", Args: "<dep> <version>", Summary: "Rewrite one version line"},
		},
		Concepts: []docstypes.Concept{
			{
				Name: "The factory file", Summary: "It declares members.", Detail: "One file.",
				SeeAlso: []string{"No lone clones"},
			},
			{Name: "Bare", Summary: "Nothing else."},
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "docs/concepts.md", file.Path)
	assert.Contains(t, file.Content, docscontroller.Header)
	assert.Contains(t, file.Content, "| `sync` | Materialise every generated file |")
	assert.Contains(t, file.Content, "| `bump <dep> <version>` |")
	assert.Contains(t, file.Content, "### The factory file")
	assert.Contains(t, file.Content, "See also: No lone clones")
	assert.Contains(t, file.Content, "### Bare")
}

func TestRenderConceptsNeedsAToolName(t *testing.T) {
	t.Parallel()

	_, err := docscontroller.RenderConcepts(docstypes.Concepts{})
	require.Error(t, err)
}

func TestRenderConceptsWithNoVerbsOrConcepts(t *testing.T) {
	t.Parallel()

	file, err := docscontroller.RenderConcepts(docstypes.Concepts{Tool: "x", Summary: "y"})
	require.NoError(t, err)
	assert.NotContains(t, file.Content, "## Verbs")
	assert.NotContains(t, file.Content, "## Concepts")
}

func TestRenderDecisions(t *testing.T) {
	t.Parallel()

	file, err := docscontroller.RenderDecisions(docstypes.Decisions{
		Decisions: []docstypes.Decision{{
			Title:    "Generated files are never committed",
			Date:     "2026-08-19",
			Decision: "Everything written is gitignored.",
			Because:  "A version committed twice costs a pull request per repo.",
		}},
	})
	require.NoError(t, err)

	assert.Equal(t, "docs/decisions.md", file.Path)
	assert.Contains(t, file.Content, "## Generated files are never committed")
	assert.Contains(t, file.Content, "**Decided 2026-08-19.**")
}

func TestRenderDecisionsNeedsATitle(t *testing.T) {
	t.Parallel()

	_, err := docscontroller.RenderDecisions(docstypes.Decisions{
		Decisions: []docstypes.Decision{{Date: "2026-08-19"}},
	})
	require.Error(t, err)
}

func engine() docstypes.Engine {
	e := docstypes.Engine{
		Name:        "factory-lang-go",
		Kind:        "mcp-server",
		Version:     "0.1.0",
		Description: "Render Go manifests.",
	}
	e.Layout.Tools = []docstypes.Tool{
		{Name: "render", Description: "Render every manifest.", Input: "RenderInput", Output: "RenderOutput"},
	}

	return e
}

func TestRenderUsage(t *testing.T) {
	t.Parallel()

	file, err := docscontroller.RenderUsage("cmd/factory-lang-go", engine())
	require.NoError(t, err)

	assert.Equal(t, "cmd/factory-lang-go/docs/usage.md", file.Path)
	assert.Contains(t, file.Content, docscontroller.Header)
	assert.Contains(t, file.Content, "cmd/factory-lang-go@v0.1.0")
	assert.Contains(t, file.Content, "| `render` | Render every manifest. | `RenderInput` | `RenderOutput` |")
	assert.Contains(t, file.Content, "go run ./cmd/factory-lang-go --mcp")
}

func TestRenderUsageNeedsAnEngineName(t *testing.T) {
	t.Parallel()

	_, err := docscontroller.RenderUsage("cmd/x", docstypes.Engine{})
	require.Error(t, err)
}
