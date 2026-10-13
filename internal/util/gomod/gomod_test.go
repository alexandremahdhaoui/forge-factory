package gomod_test

import (
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/util/gomod"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const member = `module example.com/member

go 1.25

require example.com/single v1.0.0 // indirect

require (
	// a block comment
	example.com/declared v1.1.0
	example.com/undeclared v2.0.0
	example.com/indirect v0.3.0 // indirect
)

replace example.com/declared => ../declared

exclude example.com/declared v1.0.9

exclude (
	example.com/indirect v0.3.1
)
`

func TestEditMovesOnlyTheRequiresTheMapNames(t *testing.T) {
	t.Parallel()

	got, err := gomod.Edit(member, "example.com/member", "1.25", map[string]string{
		"example.com/single":   "v1.0.5",
		"example.com/declared": "v1.2.0",
		"example.com/indirect": "v0.4.0",
		"example.com/absent":   "v9.0.0",
	})
	require.NoError(t, err)

	assert.Equal(t, `module example.com/member

go 1.25

require example.com/single v1.0.5 // indirect

require (
	// a block comment
	example.com/declared v1.2.0
	example.com/undeclared v2.0.0
	example.com/indirect v0.4.0 // indirect
)

replace example.com/declared => ../declared

exclude example.com/declared v1.0.9

exclude (
	example.com/indirect v0.3.1
)
`, got)
}

func TestRequiresNamesEverySingleAndBlockRequire(t *testing.T) {
	t.Parallel()

	got, err := gomod.Requires(member)
	require.NoError(t, err)

	assert.Equal(t, map[string]bool{
		"example.com/single":     true,
		"example.com/declared":   true,
		"example.com/undeclared": true,
		"example.com/indirect":   true,
	}, got)
}

const spaced = "module example.com/member\n\ngo 1.25\n\n" +
	"require(\n\texample.com/a v1.0.0\n)\n\n" +
	"require  (\n\texample.com/b v1.0.0\n)\n\n" +
	"require ( // grouped\n\texample.com/c v1.0.0 // indirect\n)\n\n" +
	"require\texample.com/d v1.0.0\n\n" +
	"require \"example.com/e\" v1.0.0\n"

func TestEditSetsARequireWrittenWithAnySpacingOrATrailingComment(t *testing.T) {
	t.Parallel()

	got, err := gomod.Edit(spaced, "example.com/member", "1.25", map[string]string{
		"example.com/a": "v1.1.0",
		"example.com/b": "v1.1.0",
		"example.com/c": "v1.1.0",
		"example.com/d": "v1.1.0",
		"example.com/e": "v1.1.0",
	})
	require.NoError(t, err)

	assert.Equal(t, "module example.com/member\n\ngo 1.25\n\n"+
		"require(\n\texample.com/a v1.1.0\n)\n\n"+
		"require  (\n\texample.com/b v1.1.0\n)\n\n"+
		"require ( // grouped\n\texample.com/c v1.1.0 // indirect\n)\n\n"+
		"require\texample.com/d v1.1.0\n\n"+
		"require \"example.com/e\" v1.1.0\n", got)
}

func TestRequiresReadsARequireWrittenWithAnySpacingOrATrailingComment(t *testing.T) {
	t.Parallel()

	got, err := gomod.Requires(spaced)
	require.NoError(t, err)

	assert.Equal(t, map[string]bool{
		"example.com/a": true,
		"example.com/b": true,
		"example.com/c": true,
		"example.com/d": true,
		"example.com/e": true,
	}, got)
}

var unreadable = map[string]struct {
	requires string
	refusal  string
}{
	"a require with no version": {
		"require example.com/a\n", "line 5 cannot be read: require example.com/a",
	},
	"a require with two versions": {
		"require example.com/a v1.0.0 v1.1.0\n", "line 5 cannot be read: require example.com/a v1.0.0 v1.1.0",
	},
	"a block entry with no version": {
		"require (\n\texample.com/a\n)\n", "line 6 cannot be read: example.com/a",
	},
	"a block opened with an entry inline": {
		"require ( example.com/a v1.0.0\n)\n", "line 5 cannot be read: require ( example.com/a v1.0.0",
	},
	"a block opened and closed inline": {
		"require ()\n", "line 5 cannot be read: require ()",
	},
}

func TestEditRefusesARequireItCannotReadAndNamesTheLine(t *testing.T) {
	t.Parallel()

	for name, tc := range unreadable {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := gomod.Edit("module example.com/m\n\ngo 1.25\n\n"+tc.requires, "example.com/m", "1.25", nil)
			require.ErrorIs(t, err, gomod.ErrRefused)
			assert.ErrorContains(t, err, tc.refusal)
		})
	}
}

func TestRequiresRefusesARequireItCannotReadAndNamesTheLine(t *testing.T) {
	t.Parallel()

	for name, tc := range unreadable {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := gomod.Requires("module example.com/m\n\ngo 1.25\n\n" + tc.requires)
			require.ErrorIs(t, err, gomod.ErrRefused)
			assert.ErrorContains(t, err, tc.refusal)
		})
	}
}

func TestEditRefusesAFactoredModuleBlockAndNamesTheLine(t *testing.T) {
	t.Parallel()

	for _, opener := range []string{"module (", "module(", "module ( // grouped"} {
		t.Run(opener, func(t *testing.T) {
			t.Parallel()

			_, err := gomod.Edit("// the module\n"+opener+"\n\texample.com/old\n)\n\ngo 1.25\n", "example.com/new", "1.25", nil)
			require.ErrorIs(t, err, gomod.ErrRefused)
			assert.ErrorContains(t, err, "line 2 opens a factored module block: "+opener)
		})
	}
}

func TestEditRefusesAGoModWithNoModuleOrNoGoLine(t *testing.T) {
	t.Parallel()

	_, err := gomod.Edit("go 1.25\n", "example.com/m", "1.26", nil)
	require.ErrorIs(t, err, gomod.ErrRefused)
	assert.ErrorContains(t, err, "it has no module line")

	_, err = gomod.Edit("module example.com/m\n", "example.com/m", "1.26", nil)
	require.ErrorIs(t, err, gomod.ErrRefused)
	assert.ErrorContains(t, err, "it has no go line")
}

func TestEveryReaderRefusesAnEscapedQuotedStringAndNamesTheLine(t *testing.T) {
	t.Parallel()

	content := "module example.com/m\n\ngo 1.25\n\nrequire \"example.com/\\x61\" v1.0.0\n"
	refusal := "line 5 carries an escaped quoted string: require \"example.com/\\x61\" v1.0.0"

	_, err := gomod.Edit(content, "example.com/m", "1.26", map[string]string{"example.com/a": "v1.1.0"})
	require.ErrorIs(t, err, gomod.ErrRefused)
	assert.ErrorContains(t, err, refusal)

	_, err = gomod.Requires(content)
	require.ErrorIs(t, err, gomod.ErrRefused)
	assert.ErrorContains(t, err, refusal)

	_, err = gomod.GoVersion(content)
	require.ErrorIs(t, err, gomod.ErrRefused)
	assert.ErrorContains(t, err, refusal)
}

func TestEditRefusesALineWhoseTokenItCannotFindAndNamesTheLine(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		content string
		refusal string
	}{
		"an anchor missing from the line": {
			"module example.com/m\n\ngo (\n\t1.24\n)\n",
			`line 4 cannot be edited: 1.24: "go" is not on the line`,
		},
		"an old token missing after the anchor": {
			"module example.com/m\n\ngo (\n\tgogo\n)\n",
			`line 4 cannot be edited: gogo: "gogo" does not follow "go" on the line`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := gomod.Edit(tc.content, "example.com/m", "1.26", nil)
			require.ErrorIs(t, err, gomod.ErrRefused)
			assert.ErrorContains(t, err, tc.refusal)
		})
	}
}

func TestEditKeepsTheGoLineWhenNoGoVersionIsGiven(t *testing.T) {
	t.Parallel()

	got, err := gomod.Edit("module example.com/old\n\ngo 1.24.3 // pinned\n", "example.com/new", "", nil)
	require.NoError(t, err)

	assert.Equal(t, "module example.com/new\n\ngo 1.24.3 // pinned\n", got)
}

func TestGoVersionReadsTheGoLine(t *testing.T) {
	t.Parallel()

	got, err := gomod.GoVersion("// header\r\nmodule example.com/m\r\n\r\ngo 1.27rc1 // pinned\r\n")
	require.NoError(t, err)

	assert.Equal(t, "1.27rc1", got)
}

func TestGoVersionRefusesAGoModWithNoGoLineOrAnUnreadableLine(t *testing.T) {
	t.Parallel()

	_, err := gomod.GoVersion("module example.com/m\n")
	require.ErrorIs(t, err, gomod.ErrRefused)
	assert.ErrorContains(t, err, "it has no go line")

	_, err = gomod.GoVersion("module example.com/m\n\ngo\n")
	require.ErrorIs(t, err, gomod.ErrRefused)
	assert.ErrorContains(t, err, "line 3 cannot be read: go")
}

func TestEditKeepsTheTrailingCommentOfTheModuleAndGoLines(t *testing.T) {
	t.Parallel()

	got, err := gomod.Edit("module example.com/old // Deprecated: use other\n\ngo 1.24 // pinned\n", "example.com/new", "1.26", nil)
	require.NoError(t, err)

	assert.Equal(t, "module example.com/new // Deprecated: use other\n\ngo 1.26 // pinned\n", got)
}

func TestEditKeepsTheCarriageReturnOfEveryLineItRewrites(t *testing.T) {
	t.Parallel()

	got, err := gomod.Edit("module example.com/old\r\n\r\ngo 1.24\r\n\r\nrequire example.com/a v1.0.0\r\n",
		"example.com/new", "1.26", map[string]string{"example.com/a": "v1.1.0"})
	require.NoError(t, err)

	assert.Equal(t, "module example.com/new\r\n\r\ngo 1.26\r\n\r\nrequire example.com/a v1.1.0\r\n", got)
}
