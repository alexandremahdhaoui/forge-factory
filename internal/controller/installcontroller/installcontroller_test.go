package installcontroller_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/controller/installcontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/runtimetypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type entry struct {
	name    string
	body    string
	mode    int64
	dir     bool
	link    string
	badType bool
}

// buildTar writes a tar (optionally gzipped) holding the entries and answers
// the file path the built archive sits at.
func buildTar(t *testing.T, gz bool, entries []entry) string {
	t.Helper()

	var tarBuf bytes.Buffer

	w := tar.NewWriter(&tarBuf)

	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}

		switch {
		case e.dir:
			hdr.Typeflag = tar.TypeDir
		case e.link != "":
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = e.link
		case e.badType:
			hdr.Typeflag = tar.TypeFifo
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.body))
		}

		require.NoError(t, w.WriteHeader(hdr))

		if hdr.Typeflag == tar.TypeReg {
			_, err := w.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}

	require.NoError(t, w.Close())

	data := tarBuf.Bytes()

	if gz {
		var gzBuf bytes.Buffer

		gzw := gzip.NewWriter(&gzBuf)
		_, err := gzw.Write(data)
		require.NoError(t, err)
		require.NoError(t, gzw.Close())

		data = gzBuf.Bytes()
	}

	p := filepath.Join(t.TempDir(), "archive")
	require.NoError(t, os.WriteFile(p, data, 0o600))

	return p
}

func buildZip(t *testing.T, entries []entry) string {
	t.Helper()

	var buf bytes.Buffer

	w := zip.NewWriter(&buf)

	for _, e := range entries {
		name := e.name
		if e.dir {
			name += "/"
		}

		f, err := w.Create(name)
		require.NoError(t, err)

		if !e.dir {
			_, err = f.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}

	require.NoError(t, w.Close())

	p := filepath.Join(t.TempDir(), "archive.zip")
	require.NoError(t, os.WriteFile(p, buf.Bytes(), 0o600))

	return p
}

func TestATarGzLaysOutWithStrip(t *testing.T) {
	t.Parallel()

	archive := buildTar(t, true, []entry{
		{name: "toy-1.0/bin", dir: true},
		{name: "toy-1.0/bin/toy", body: "#!/bin/sh\n", mode: 0o755},
		{name: "toy-1.0/README", body: "hi"},
	})

	prefix := t.TempDir()

	installed, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{URL: "https://x/toy.tar.gz", Unpack: "tar-gz", Strip: 1},
		Path:     archive,
	}}, prefix)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"bin", "README"}, installed)

	info, err := os.Stat(filepath.Join(prefix, "bin", "toy"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0o100, "the executable bit survives")
}

// Rust's component tarballs are the reason picks exist: rustc/, cargo/ and
// rust-std-*/ each merge INTO the one prefix.
func TestPicksMergeSubtreesIntoOnePrefix(t *testing.T) {
	t.Parallel()

	archive := buildTar(t, true, []entry{
		{name: "rustc/bin/rustc", body: "r", mode: 0o755},
		{name: "cargo/bin/cargo", body: "c", mode: 0o755},
		{name: "install.sh", body: "ignored"},
	})

	prefix := t.TempDir()

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{
			URL: "https://x/rust.tar.gz", Unpack: "tar-gz",
			Picks: []runtimetypes.Pick{{From: "rustc"}, {From: "cargo"}},
		},
		Path: archive,
	}}, prefix)
	require.NoError(t, err)

	for _, p := range []string{"bin/rustc", "bin/cargo"} {
		_, err := os.Stat(filepath.Join(prefix, p))
		require.NoError(t, err, p)
	}

	_, err = os.Stat(filepath.Join(prefix, "install.sh"))
	assert.True(t, os.IsNotExist(err), "what no pick names never lands")
}

func TestAZipLaysOut(t *testing.T) {
	t.Parallel()

	archive := buildZip(t, []entry{
		{name: "go/bin", dir: true},
		{name: "go/bin/go", body: "g"},
	})

	prefix := t.TempDir()

	installed, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{URL: "https://x/go.zip", Unpack: "zip", Strip: 1},
		Path:     archive,
	}}, prefix)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"bin"}, installed)
}

// A bare file lands executable, and an at-only pick renames it: a release
// asset called pnpm-linux-x64 lands as pnpm.
func TestAFileLandsExecutableAndAnAtOnlyPickRenames(t *testing.T) {
	t.Parallel()

	src := filepath.Join(t.TempDir(), "pnpm-linux-x64")
	require.NoError(t, os.WriteFile(src, []byte("#!bin"), 0o600))

	prefix := t.TempDir()

	installed, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{
			URL: "https://x/pnpm-linux-x64", Unpack: "file",
			Picks: []runtimetypes.Pick{{At: "pnpm"}},
		},
		Path: src,
	}}, prefix)
	require.NoError(t, err)
	assert.Equal(t, []string{"pnpm"}, installed)

	info, err := os.Stat(filepath.Join(prefix, "pnpm"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0o100)
}

// Containment is the contract: an entry that would climb out refuses the
// whole install, and so does a symlink whose target escapes.
func TestAnEscapingEntryRefusesTheInstall(t *testing.T) {
	t.Parallel()

	archive := buildTar(t, false, []entry{
		{name: "../outside", body: "evil"},
	})

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{URL: "https://x/evil.tar", Unpack: "tar"},
		Path:     archive,
	}}, t.TempDir())
	require.ErrorIs(t, err, installcontroller.ErrEscape)
}

func TestAnEscapingSymlinkRefusesTheInstall(t *testing.T) {
	t.Parallel()

	archive := buildTar(t, false, []entry{
		{name: "bin/evil", link: "../../../../etc/passwd"},
	})

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{URL: "https://x/evil.tar", Unpack: "tar"},
		Path:     archive,
	}}, t.TempDir())
	require.ErrorIs(t, err, installcontroller.ErrEscape)
}

func TestAnAbsoluteSymlinkRefusesTheInstall(t *testing.T) {
	t.Parallel()

	archive := buildTar(t, false, []entry{
		{name: "bin/evil", link: "/etc/passwd"},
	})

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{URL: "https://x/evil.tar", Unpack: "tar"},
		Path:     archive,
	}}, t.TempDir())
	require.ErrorIs(t, err, installcontroller.ErrEscape)
}

// A contained relative symlink is how every runtime ships its bin dir.
func TestAContainedSymlinkLands(t *testing.T) {
	t.Parallel()

	archive := buildTar(t, false, []entry{
		{name: "bin/real", body: "r", mode: 0o755},
		{name: "bin/alias", link: "real"},
	})

	prefix := t.TempDir()

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{URL: "https://x/a.tar", Unpack: "tar"},
		Path:     archive,
	}}, prefix)
	require.NoError(t, err)

	target, err := os.Readlink(filepath.Join(prefix, "bin", "alias"))
	require.NoError(t, err)
	assert.Equal(t, "real", target)
}

func TestAnUnknownUnpackKindIsRefused(t *testing.T) {
	t.Parallel()

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{URL: "https://x/a.xz", Unpack: "tar-xz"},
		Path:     "irrelevant",
	}}, t.TempDir())
	require.ErrorContains(t, err, `unpack kind "tar-xz"`)
}

func TestAnUnexpectedEntryTypeIsRefused(t *testing.T) {
	t.Parallel()

	archive := buildTar(t, false, []entry{
		{name: "a-fifo", badType: true},
	})

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{URL: "https://x/a.tar", Unpack: "tar"},
		Path:     archive,
	}}, t.TempDir())
	require.ErrorContains(t, err, "which this engine does not lay out")
}

// An exact pick match lands the file at the pick's target - the file form
// of the subtree rule.
func TestAnExactPickMatchRenames(t *testing.T) {
	t.Parallel()

	archive := buildTar(t, false, []entry{
		{name: "tool-v1", body: "t", mode: 0o755},
	})

	prefix := t.TempDir()

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{
			URL: "https://x/a.tar", Unpack: "tar",
			Picks: []runtimetypes.Pick{{From: "tool-v1", At: "bin/tool"}},
		},
		Path: archive,
	}}, prefix)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(prefix, "bin", "tool"))
	require.NoError(t, err)
}

func TestAMissingArchiveFileIsAnError(t *testing.T) {
	t.Parallel()

	for _, unpack := range []string{"file", "zip", "tar"} {
		_, err := installcontroller.New().Install([]installcontroller.Fetched{{
			Artifact: runtimetypes.Artifact{URL: "https://x/a", Unpack: unpack},
			Path:     filepath.Join(t.TempDir(), "not-there"),
		}}, t.TempDir())
		require.Error(t, err, unpack)
	}
}

func TestACorruptZipIsRefusedByName(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "broken.zip")
	require.NoError(t, os.WriteFile(p, []byte("not a zip"), 0o600))

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{URL: "https://x/a.zip", Unpack: "zip"},
		Path:     p,
	}}, t.TempDir())
	require.ErrorContains(t, err, "opening the zip")
}

func TestACorruptGzipStreamIsRefusedByName(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "broken.tar.gz")
	require.NoError(t, os.WriteFile(p, []byte("not gzip"), 0o600))

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{URL: "https://x/a.tar.gz", Unpack: "tar-gz"},
		Path:     p,
	}}, t.TempDir())
	require.ErrorContains(t, err, "opening the gzip stream")
}

func TestAnEscapingZipEntryRefusesTheInstall(t *testing.T) {
	t.Parallel()

	archive := buildZip(t, []entry{{name: "../outside", body: "evil"}})

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{URL: "https://x/evil.zip", Unpack: "zip"},
		Path:     archive,
	}}, t.TempDir())
	require.ErrorIs(t, err, installcontroller.ErrEscape)
}

// A file artifact whose pick names a path outside the prefix refuses.
func TestAnEscapingFilePickRefuses(t *testing.T) {
	t.Parallel()

	src := filepath.Join(t.TempDir(), "tool")
	require.NoError(t, os.WriteFile(src, []byte("t"), 0o600))

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{
			URL: "https://x/tool", Unpack: "file",
			Picks: []runtimetypes.Pick{{At: "../outside"}},
		},
		Path: src,
	}}, t.TempDir())
	require.ErrorIs(t, err, installcontroller.ErrEscape)
}

// Entries stripped into nothing simply do not land - the root directory
// entry of every tarball.
func TestStripSwallowsTheRootEntry(t *testing.T) {
	t.Parallel()

	archive := buildTar(t, false, []entry{
		{name: "./", dir: true},
		{name: "toy-1.0", dir: true},
		{name: "toy-1.0/f", body: "x"},
	})

	prefix := t.TempDir()

	installed, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{URL: "https://x/a.tar", Unpack: "tar", Strip: 1},
		Path:     archive,
	}}, prefix)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"f"}, installed)
}

// An exact pick with an empty At lays the subtree at the prefix root.
func TestAnExactPickWithNoAtLandsAtTheRoot(t *testing.T) {
	t.Parallel()

	archive := buildTar(t, false, []entry{
		{name: "sub", dir: true},
		{name: "sub/f", body: "x"},
	})

	prefix := t.TempDir()

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{
			URL: "https://x/a.tar", Unpack: "tar",
			Picks: []runtimetypes.Pick{{From: "sub"}},
		},
		Path: archive,
	}}, prefix)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(prefix, "f"))
	require.NoError(t, err)
}

func TestACorruptTarIsRefusedByName(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "broken.tar")
	require.NoError(t, os.WriteFile(p, []byte("definitely not a tar header block"), 0o600))

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{URL: "https://x/a.tar", Unpack: "tar"},
		Path:     p,
	}}, t.TempDir())
	require.ErrorContains(t, err, "reading the tar")
}

// A file artifact with a from-bearing pick keeps the url's base name: the
// rename form is at-only, and anything else is not a rename.
func TestAFileWithAFromPickKeepsItsName(t *testing.T) {
	t.Parallel()

	src := filepath.Join(t.TempDir(), "tool-x64")
	require.NoError(t, os.WriteFile(src, []byte("t"), 0o600))

	prefix := t.TempDir()

	installed, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{
			URL: "https://x/tool-x64", Unpack: "file",
			Picks: []runtimetypes.Pick{{From: "something", At: "tool"}},
		},
		Path: src,
	}}, prefix)
	require.NoError(t, err)
	assert.Equal(t, []string{"tool-x64"}, installed)
}

// The pax global header every GNU tarball opens with carries no file and is
// skipped rather than refused.
func TestAPaxGlobalHeaderIsSkipped(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	w := tar.NewWriter(&buf)
	require.NoError(t, w.WriteHeader(&tar.Header{
		Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader,
	}))
	require.NoError(t, w.WriteHeader(&tar.Header{
		Name: "f", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1,
	}))
	_, err := w.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	p := filepath.Join(t.TempDir(), "a.tar")
	require.NoError(t, os.WriteFile(p, buf.Bytes(), 0o600))

	prefix := t.TempDir()

	installed, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{URL: "https://x/a.tar", Unpack: "tar"},
		Path:     p,
	}}, prefix)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"f"}, installed)
}

// A file entry under a path already taken by a regular file cannot create
// its parent directory and fails rather than clobbering.
func TestAFileBlockingADirectoryFailsTheLayout(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"tar", "zip"} {
		entries := []entry{
			{name: "a", body: "file"},
			{name: "a/b", body: "child"},
		}

		var archive string
		if kind == "tar" {
			archive = buildTar(t, false, entries)
		} else {
			archive = buildZip(t, entries)
		}

		_, err := installcontroller.New().Install([]installcontroller.Fetched{{
			Artifact: runtimetypes.Artifact{URL: "https://x/a." + kind, Unpack: kind},
			Path:     archive,
		}}, t.TempDir())
		require.Error(t, err, kind)
	}
}

func TestAFilePickBlockedByAFileFails(t *testing.T) {
	t.Parallel()

	src := filepath.Join(t.TempDir(), "tool")
	require.NoError(t, os.WriteFile(src, []byte("t"), 0o600))

	prefix := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(prefix, "bin"), []byte("in the way"), 0o600))

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{
			URL: "https://x/tool", Unpack: "file",
			Picks: []runtimetypes.Pick{{At: "bin/tool"}},
		},
		Path: src,
	}}, prefix)
	require.Error(t, err)
}

// A symlink may climb directories as long as it RESOLVES inside the prefix:
// the JRE ships legal/jdk.localedata/LICENSE -> ../java.base/LICENSE.
func TestAClimbingSymlinkThatStaysInsideLands(t *testing.T) {
	t.Parallel()

	archive := buildTar(t, false, []entry{
		{name: "legal/java.base/LICENSE", body: "GPL"},
		{name: "legal/jdk.localedata/LICENSE", link: "../java.base/LICENSE"},
	})

	prefix := t.TempDir()

	_, err := installcontroller.New().Install([]installcontroller.Fetched{{
		Artifact: runtimetypes.Artifact{URL: "https://x/jre.tar.gz", Unpack: "tar"},
		Path:     archive,
	}}, prefix)
	require.NoError(t, err)

	target, err := os.Readlink(filepath.Join(prefix, "legal", "jdk.localedata", "LICENSE"))
	require.NoError(t, err)
	assert.Equal(t, "../java.base/LICENSE", target)
}
