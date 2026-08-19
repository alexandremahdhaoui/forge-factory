package specadapter_test

import (
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/specadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/fsadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/docstypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const doc = `openapi: 3.0.3
components:
  schemas:
    RenderInput:
      type: object
      description: What an engine needs.
      required: [root]
      properties:
        root:
          type: string
          description: The workspace root.
        repos:
          type: array
          items:
            $ref: '#/components/schemas/Repo'
        dependencies:
          type: object
          additionalProperties:
            type: string
        spec:
          type: object
        anything: {}
        nested:
          type: array
    Repo:
      type: object
`

func TestReadTurnsASpecIntoDocumentableSchemas(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().ReadFile("api/x.yaml").Return([]byte(doc), nil).Once()

	got, err := specadapter.Read(fs, "api/x.yaml")
	require.NoError(t, err)
	require.Contains(t, got, "RenderInput")

	in := got["RenderInput"]
	assert.Equal(t, "What an engine needs.", in.Description)

	types := map[string]string{}
	required := map[string]bool{}

	for _, p := range in.Properties {
		types[p.Name] = p.Type
		required[p.Name] = p.Required
	}

	assert.Equal(t, "string", types["root"])
	assert.Equal(t, "array of Repo", types["repos"])
	assert.Equal(t, "map of string", types["dependencies"])
	assert.Equal(t, "object", types["spec"])
	assert.Equal(t, "any", types["anything"])
	assert.Equal(t, "array", types["nested"])
	assert.True(t, required["root"])
	assert.False(t, required["repos"])

	assert.Equal(t, []string{"anything", "dependencies", "nested", "repos", "root", "spec"},
		names(in.Properties), "fields are sorted so the doc does not churn")
}

func names(props []docstypes.Property) []string {
	out := make([]string, 0, len(props))
	for _, p := range props {
		out = append(out, p.Name)
	}

	return out
}

func TestReadReportsASpecWithNoSchemas(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().ReadFile("api/x.yaml").Return([]byte("openapi: 3.0.3\n"), nil).Once()

	_, err := specadapter.Read(fs, "api/x.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declares no schemas")
}

func TestReadReportsASpecThatDoesNotParse(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().ReadFile("api/x.yaml").Return([]byte("components: [not a map]\n"), nil).Once()

	_, err := specadapter.Read(fs, "api/x.yaml")
	require.Error(t, err)
}

func TestReadReportsADiskFailure(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().ReadFile("api/x.yaml").Return(nil, assert.AnError).Once()

	_, err := specadapter.Read(fs, "api/x.yaml")
	require.ErrorIs(t, err, assert.AnError)
}

func TestAReferenceWithNoSlashIsTakenWhole(t *testing.T) {
	t.Parallel()

	fs := fsadaptermock.NewMockFS(t)
	fs.EXPECT().ReadFile("api/x.yaml").Return([]byte(`components:
  schemas:
    A:
      type: object
      properties:
        b:
          $ref: 'Bare'
`), nil).Once()

	got, err := specadapter.Read(fs, "api/x.yaml")
	require.NoError(t, err)
	assert.Equal(t, "Bare", got["A"].Properties[0].Type)
}
