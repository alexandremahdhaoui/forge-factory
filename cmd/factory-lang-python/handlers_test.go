package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThePythonEngineLocksEachMemberWithUvLockAndInstallsNothing(t *testing.T) {
	t.Parallel()

	out, err := NewHandlers().Render(t.Context(), RenderInput{
		Root: "/w",
		Repos: []Repo{
			{Name: "alpha", Path: "/w/alpha", Languages: []string{"python"}, Identity: map[string]string{"package": "alpha"}},
			{Name: "beta", Path: "/w/beta", Languages: []string{"python"}, Identity: map[string]string{"package": "beta"}},
		},
	})
	require.NoError(t, err)

	require.Len(t, out.DependencyLock, 2)

	for i, dir := range []string{"/w/alpha", "/w/beta"} {
		assert.Equal(t, dir, out.DependencyLock[i].Dir)
		assert.Equal(t, "uv", out.DependencyLock[i].Command)
		assert.Equal(t, []string{"lock"}, out.DependencyLock[i].Args)
	}
}
