package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTheRustEngineLockUpdatesOnlyTheWorkspaceOwnPackages(t *testing.T) {
	t.Parallel()

	out, err := NewHandlers().Render(t.Context(), RenderInput{
		Root:  "/w",
		Repos: []Repo{{Name: "member", Path: "/w/member", Languages: []string{"rust"}}},
	})
	require.NoError(t, err)

	require.Len(t, out.DependencyLock, 1)
	assert.Equal(t, "cargo", out.DependencyLock[0].Command)
	assert.Equal(t, []string{"update", "--workspace"}, out.DependencyLock[0].Args)
}
