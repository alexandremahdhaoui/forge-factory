package toolingcontroller

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveStoreDirPrecedence(t *testing.T) {
	home := t.TempDir()

	t.Run("an explicit override wins", func(t *testing.T) {
		t.Setenv("FORGE_STORE_DIR", filepath.Join(home, "from-env"))

		got, err := resolveStoreDir(filepath.Join(home, "explicit"))
		require.NoError(t, err)
		require.Equal(t, filepath.Join(home, "explicit"), got)
	})

	t.Run("then the environment", func(t *testing.T) {
		t.Setenv("FORGE_STORE_DIR", filepath.Join(home, "from-env"))

		got, err := resolveStoreDir("")
		require.NoError(t, err)
		require.Equal(t, filepath.Join(home, "from-env"), got)
	})

	t.Run("then the user cache", func(t *testing.T) {
		t.Setenv("FORGE_STORE_DIR", "")
		t.Setenv("HOME", home)
		t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))

		got, err := resolveStoreDir("")
		require.NoError(t, err)
		require.Equal(t, filepath.Join(home, "cache", "forge", "store"), got)
	})

	// The store holds verified binaries keyed by digest. With nowhere to put
	// them the answer is a failure that names the problem, never a relative
	// path in whatever directory the process happened to start in.
	t.Run("and nowhere is an error", func(t *testing.T) {
		t.Setenv("FORGE_STORE_DIR", "")
		t.Setenv("HOME", "")
		t.Setenv("XDG_CACHE_HOME", "")

		_, err := resolveStoreDir("")
		require.ErrorContains(t, err, "resolving the store dir")
	})
}
