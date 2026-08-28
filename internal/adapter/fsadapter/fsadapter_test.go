package fsadapter_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteCreatesTheParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "nested", "f.txt")

	require.NoError(t, fsadapter.New().WriteFile(path, []byte("hi")))

	body, err := fsadapter.New().ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "hi", string(body))
}

func TestReadingAMissingFileNamesIt(t *testing.T) {
	_, err := fsadapter.New().ReadFile("/nope/f.txt")
	require.Error(t, err)
	require.Contains(t, err.Error(), "reading /nope/f.txt")
}

func TestExistsIsTrueFalseAndErrorInTheRightCases(t *testing.T) {
	fs := fsadapter.New()
	dir := t.TempDir()

	ok, err := fs.Exists(dir)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = fs.Exists(filepath.Join(dir, "missing"))
	require.NoError(t, err)
	require.False(t, ok)

	blocker := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	_, err = fs.Exists(filepath.Join(blocker, "child", "grandchild"))
	require.Error(t, err)
}

func TestListIsSortedAndEmptyForAMissingDirectory(t *testing.T) {
	fs := fsadapter.New()
	dir := t.TempDir()

	for _, name := range []string{"c", "a", "b"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}

	names, err := fs.List(dir)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c"}, names)

	names, err = fs.List(filepath.Join(dir, "missing"))
	require.NoError(t, err)
	require.Empty(t, names)
}

func TestListReportsARealFailure(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	_, err := fsadapter.New().List(file)
	require.Error(t, err)
	require.Contains(t, err.Error(), "listing "+file)
}

func TestMkdirAllAndRemove(t *testing.T) {
	fs := fsadapter.New()
	dir := filepath.Join(t.TempDir(), "a", "b")

	require.NoError(t, fs.MkdirAll(dir))
	require.DirExists(t, dir)

	require.NoError(t, fs.Remove(dir))
	require.NoDirExists(t, dir)
}

func TestMkdirAllNamesWhatItCouldNotCreate(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))

	err := fsadapter.New().MkdirAll(filepath.Join(blocker, "child"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "creating ")
}

func TestWriteNamesWhatItCouldNotWrite(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))

	err := fsadapter.New().WriteFile(filepath.Join(blocker, "child", "f.txt"), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "creating directory for ")
}

func TestIsDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	fs := fsadapter.New()

	isDir, err := fs.IsDir(dir)
	require.NoError(t, err)
	assert.True(t, isDir)

	isDir, err = fs.IsDir(file)
	require.NoError(t, err)
	assert.False(t, isDir)

	isDir, err = fs.IsDir(filepath.Join(dir, "nope"))
	require.NoError(t, err)
	assert.False(t, isDir)

	_, err = fs.IsDir(filepath.Join(file, "under-a-file"))
	require.Error(t, err)
}

func TestWriteFileReportsADirectoryItCannotCreate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	err := fsadapter.New().WriteFile(filepath.Join(file, "under", "b.txt"), []byte("y"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating directory")
}

func TestRemoveReportsAFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	require.Error(t, fsadapter.New().Remove(filepath.Join(file, "under")))
}

// blocked returns a path whose parent directory is a regular file, so every
// MkdirAll under it fails. Each of these writers creates the directory before
// writing, and every one of those branches was unreached.
func blocked(t *testing.T) string {
	t.Helper()

	file := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	return filepath.Join(file, "sub", "f")
}

func TestWritersNameTheDirectoryTheyCouldNotCreate(t *testing.T) {
	fs := fsadapter.New()

	for name, write := range map[string]func(string) error{
		"WriteFile":       func(p string) error { return fs.WriteFile(p, nil) },
		"WriteExecutable": func(p string) error { return fs.WriteExecutable(p, nil) },
		"Rename":          func(p string) error { return fs.Rename(p, p) },
		"Symlink":         func(p string) error { return fs.Symlink("t", p) },
	} {
		t.Run(name, func(t *testing.T) {
			path := blocked(t)
			err := write(path)
			require.ErrorContains(t, err, "creating directory for ")
			// The path, not just the syscall's word. A bare "not a
			// directory" leaves the reader hunting for which one.
			require.ErrorContains(t, err, path)
		})
	}
}

func TestRenameNamesBothEnds(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "absent")
	to := filepath.Join(dir, "to")

	err := fsadapter.New().Rename(from, to)
	require.ErrorContains(t, err, "moving "+from+" to "+to)
}

func TestSymlinkReplacesWhatIsThere(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")

	fs := fsadapter.New()
	require.NoError(t, fs.Symlink("first", link))
	// Replacing rather than failing is the point: a re-sync writes the same
	// links again and must not need the tree cleaned first.
	require.NoError(t, fs.Symlink("second", link))

	got, err := os.Readlink(link)
	require.NoError(t, err)
	require.Equal(t, "second", got)
}

func TestSymlinkRefusesToRemoveADirectoryInItsPlace(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	require.NoError(t, os.MkdirAll(filepath.Join(link, "child"), 0o750))

	// A non-empty directory where a link belongs is somebody's data. Deleting
	// it to make room would be a silent loss.
	err := fsadapter.New().Symlink("t", link)
	require.ErrorContains(t, err, "replacing "+link)
}

func TestWriteFileRefusesADirectoryInItsPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.MkdirAll(path, 0o750))

	fs := fsadapter.New()
	require.ErrorContains(t, fs.WriteFile(path, nil), "writing "+path)
	require.ErrorContains(t, fs.WriteExecutable(path, nil), "writing "+path)
}
