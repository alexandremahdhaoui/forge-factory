package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTheTypeScriptEngineLockWritesTheLockfileAndInstallsNothing(t *testing.T) {
	t.Parallel()

	out, err := NewHandlers().Render(t.Context(), RenderInput{
		Root:  "/w",
		Repos: []Repo{{Name: "member", Path: "/w/member", Languages: []string{"typescript"}}},
	})
	require.NoError(t, err)

	require.Len(t, out.DependencyLock, 1)
	assert.Equal(t, "/w", out.DependencyLock[0].Dir)
	assert.Equal(t, "pnpm", out.DependencyLock[0].Command)
	assert.Equal(t, []string{"install", "--lockfile-only"}, out.DependencyLock[0].Args)
}
