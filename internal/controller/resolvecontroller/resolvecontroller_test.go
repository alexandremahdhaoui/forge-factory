package resolvecontroller_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/gitadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/resolvecontroller"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

var now = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

func newController() *resolvecontroller.Controller {
	return resolvecontroller.New(fsadapter.New(), gitadapter.New(execadapter.New()),
		func() time.Time { return now })
}

// register materialises a register checkout with the given track files.
func register(t *testing.T, tracks map[string]string) (config.Factory, string) {
	t.Helper()

	root := t.TempDir()
	dir := filepath.Join(root, "golden-register")

	for key, content := range tracks {
		path := filepath.Join(dir, "index", filepath.FromSlash(key)+".json")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}

	require.NoError(t, os.MkdirAll(dir, 0o750))

	f := config.Factory{
		Register: &config.Register{URL: "git@github.com:example/golden-register.git"},
	}

	return f, root
}

func track(current string) string {
	raw, _ := json.Marshal(map[string]any{
		"package": "p", "ecosystem": "go", "prefix": "1",
		"current": current, "updatedAt": now,
	})

	return string(raw)
}

func TestALegacyVersionPassesThroughVerbatim(t *testing.T) {
	got, notes, err := newController().Resolve(context.Background(), config.Factory{}, t.TempDir(), "rust",
		map[string]config.DependencySpec{
			"chrono": {Version: `{ version = "0.4", features = ["serde"] }`},
		})
	require.NoError(t, err)
	require.Empty(t, notes)
	require.Equal(t, `{ version = "0.4", features = ["serde"] }`, got["chrono"])
}

func TestATrackResolvesToItsCurrent(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})

	got, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
	require.NoError(t, err)
	require.Empty(t, notes)
	require.Equal(t, "v1.6.0", got["example.com/pkg"])
}

func TestTheDefaultTrackIsTheHighest(t *testing.T) {
	f, root := register(t, map[string]string{
		"go/example.com/pkg/1": track("v1.6.0"),
		"go/example.com/pkg/2": track("v2.3.0"),
	})

	got, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {}})
	require.NoError(t, err)
	require.Equal(t, "v2.3.0", got["example.com/pkg"])
}

func TestWrapsRendersTheResolvedVersion(t *testing.T) {
	f, root := register(t, map[string]string{"python/fastapi/0": track("0.141.1")})

	got, _, err := newController().Resolve(context.Background(), f, root, "python",
		map[string]config.DependencySpec{"fastapi": {Track: "0", Wraps: ">=%s"}})
	require.NoError(t, err)
	require.Equal(t, ">=0.141.1", got["fastapi"])
}

func TestASoftPinFloorsAndExplains(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})

	// Ahead of the track: the pin wins and asks for an upgrade.
	got, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1", Pin: "v1.7.0", Mode: "soft", Reason: "needs the new parser",
		}})
	require.NoError(t, err)
	require.Equal(t, "v1.7.0", got["example.com/pkg"])
	require.Len(t, notes, 1)
	require.Contains(t, notes[0], "ahead of track")

	// Behind the track: the register wins and the pin is dead.
	got, notes, err = newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1", Pin: "v1.5.0", Mode: "soft", Reason: "old workaround",
		}})
	require.NoError(t, err)
	require.Equal(t, "v1.6.0", got["example.com/pkg"])
	require.Contains(t, notes[0], "remove this pin")
}

func TestAHardPinFreezesLoudly(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})

	got, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1", Pin: "v1.4.0", Mode: "hard", Reason: "v1.5 broke the parser",
		}})
	require.NoError(t, err)
	require.Equal(t, "v1.4.0", got["example.com/pkg"])
	require.Contains(t, notes[0], "frozen, visibly")
}

func TestAnAdvisoryPiercesEveryPin(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"package": "p", "ecosystem": "go", "prefix": "1",
		"current": "v1.6.0", "updatedAt": now,
		"advisory": map[string]any{
			"vulnIds": []string{"CVE-2026-1"}, "severity": "critical", "since": now,
		},
	})

	f, root := register(t, map[string]string{"go/example.com/pkg/1": string(raw)})

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1", Pin: "v1.4.0", Mode: "hard", Reason: "frozen anyway",
		}})
	require.ErrorIs(t, err, resolvecontroller.ErrAdvisory)
	require.ErrorContains(t, err, "CVE-2026-1")
	require.ErrorContains(t, err, "a pin cannot silence this")
}

func TestADeprecatedTrackWarns(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"package": "p", "ecosystem": "go", "prefix": "1",
		"current": "v1.6.0", "updatedAt": now,
		"deprecated": map[string]any{"reason": "stale", "since": now},
	})

	f, root := register(t, map[string]string{"go/example.com/pkg/1": string(raw)})

	_, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
	require.NoError(t, err)
	require.Contains(t, notes[0], "deprecated (stale)")
}

func TestAMissingPackageFailsLoudAndFilesARequest(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/absent": {}})
	require.ErrorIs(t, err, resolvecontroller.ErrUnregistered)
	require.ErrorContains(t, err, "sync again")

	filed, err2 := filepath.Glob(filepath.Join(root, "golden-register", "request", "go", "example.com", "absent", "*.json"))
	require.NoError(t, err2)
	require.Len(t, filed, 1, "strict resolution files the request it failed on")

	raw, _ := os.ReadFile(filed[0])
	require.Contains(t, string(raw), `"type":"admission"`)
}

func TestAMissingRegisterCheckoutSaysHowToGetIt(t *testing.T) {
	f := config.Factory{Register: &config.Register{URL: "git@github.com:example/golden-register.git"}}

	_, _, err := newController().Resolve(context.Background(), f, t.TempDir(), "go",
		map[string]config.DependencySpec{"example.com/pkg": {}})
	require.ErrorIs(t, err, resolvecontroller.ErrNoRegister)
	require.ErrorContains(t, err, "forge-factory clone")
}

func TestARegisterEntryWithoutARegisterBlockIsCaughtAtParse(t *testing.T) {
	_, err := config.Parse([]byte(`
name: x
repos:
  - name: r
    url: git@example.com:r.git
    languages: [go]
engines:
  - alias: go
    engine: forge://example.com/lang-go
dependencies:
  go:
    example.com/pkg: {}
`))
	require.ErrorContains(t, err, "no register: block")
}

func TestAPinWithoutAReasonIsAConfigError(t *testing.T) {
	_, err := config.Parse([]byte(`
name: x
repos:
  - name: r
    url: git@example.com:r.git
    languages: [go]
register:
  url: git@example.com:reg.git
engines:
  - alias: go
    engine: forge://example.com/lang-go
dependencies:
  go:
    example.com/pkg: { pin: v1.2.3, mode: soft }
`))
	require.ErrorContains(t, err, "a pin without a reason is a config error")
}

func TestACorruptTrackFileIsAnError(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": "{not json"})

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
	require.ErrorContains(t, err, "decoding track")
}

func TestPinOrderingHandlesPrefixesAndPrereleases(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})

	// A pre-release pin sorts below its release: the register wins.
	_, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1", Pin: "v1.6.0-rc.1", Mode: "soft", Reason: "tried the rc",
		}})
	require.NoError(t, err)
	require.Contains(t, notes[0], "remove this pin")

	// More parts beat fewer at the same numbers.
	_, notes, err = newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1", Pin: "v1.6.0.1", Mode: "soft", Reason: "hotfix line",
		}})
	require.NoError(t, err)
	require.Contains(t, notes[0], "ahead of track")
}

func TestDependencySpecRoundTripsBothForms(t *testing.T) {
	legacy := config.DependencySpec{Version: "v1.2.3"}
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.Equal(t, `"v1.2.3"`, string(raw))

	entry := config.DependencySpec{Track: "1", Pin: "v1.4.0", Mode: "hard", Reason: "r"}
	raw, err = json.Marshal(entry)
	require.NoError(t, err)

	var back config.DependencySpec
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, entry, back)
}

func TestDevEntriesResolveTheSameWay(t *testing.T) {
	f, root := register(t, map[string]string{"python/pytest/9": track("9.1.1")})
	f.Dev = map[string]map[string]config.DependencySpec{
		"python": {"pytest": {Wraps: ">=%s"}},
	}

	got, _, err := newController().Resolve(context.Background(), f, root, "python", f.DevFor("python"))
	require.NoError(t, err)
	require.Equal(t, ">=9.1.1", got["pytest"])
}

func TestAnExplicitTrackTheRegisterLacksFilesARequest(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {Track: "2", Reason: "needs v2"}})
	require.ErrorIs(t, err, resolvecontroller.ErrUnregistered)

	filed, _ := filepath.Glob(filepath.Join(root, "golden-register", "request", "go", "example.com", "pkg", "*.json"))
	require.Len(t, filed, 1)

	raw, _ := os.ReadFile(filed[0])
	require.Contains(t, string(raw), `"track":"2"`)
	require.Contains(t, string(raw), "needs v2")
}

func TestAnExplicitRegisterPathWins(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "elsewhere")
	path := filepath.Join(dir, "index", "go", "example.com", "pkg", "1.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(track("v1.6.0")), 0o600))

	f := config.Factory{Register: &config.Register{Path: "elsewhere"}}

	got, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
	require.NoError(t, err)
	require.Equal(t, "v1.6.0", got["example.com/pkg"])
}

func TestEqualVersionsCompareEqualThroughAPin(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})

	_, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1", Pin: "v1.6.0", Mode: "soft", Reason: "same as the track",
		}})
	require.NoError(t, err)
	require.Contains(t, notes[0], "remove this pin",
		"a pin equal to the track is already dead")
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()

	// Signing off explicitly, so a machine's global tag.gpgsign or
	// commit.gpgsign cannot turn these plain commands into signed ones.
	full := append([]string{"-c", "tag.gpgsign=false", "-c", "commit.gpgsign=false"}, args...)

	res, err := execadapter.New().Run(context.Background(), dir, "git", full...)
	require.NoError(t, err)
	require.Zero(t, res.ExitCode, "%v: %s", args, res.Stderr)
}

func TestAPinnedRevisionIsWhatConsumersConsume(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})
	dir := filepath.Join(root, "golden-register")

	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "index")
	gitIn(t, dir, "tag", "v0.1.0")

	// The worktree moves past the tag.
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "index", "go", "example.com", "pkg", "1.json"),
		[]byte(track("v1.7.0")), 0o600))

	// Pinned: the tag's version, whatever the worktree says.
	f.Register.Revision = "v0.1.0"
	got, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {}})
	require.NoError(t, err)
	require.Equal(t, "v1.6.0", got["example.com/pkg"])

	// Unpinned: the checkout as it stands.
	f.Register.Revision = ""
	got, _, err = newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {}})
	require.NoError(t, err)
	require.Equal(t, "v1.7.0", got["example.com/pkg"])
}

func TestASoftPinAheadFilesAnUpgradeRequest(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})

	_, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1", Pin: "v1.7.0", Mode: "soft", Reason: "needs the new parser",
		}})
	require.NoError(t, err)
	require.Contains(t, notes[0], "filed")

	filed, _ := filepath.Glob(filepath.Join(root, "golden-register", "request", "go", "example.com", "pkg", "*-upgrade.json"))
	require.Len(t, filed, 1, "the pin doubles as the request")

	raw, _ := os.ReadFile(filed[0])
	require.Contains(t, string(raw), `"type":"upgrade"`)
	require.Contains(t, string(raw), `"version":"v1.7.0"`)
	require.Contains(t, string(raw), "needs the new parser")
}

func TestAnUnknownTrackOnAKnownPackageFilesOpenTrack(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1.27", Reason: "1.28 breaks the v1 config API",
		}})
	require.ErrorIs(t, err, resolvecontroller.ErrUnregistered)

	filed, _ := filepath.Glob(filepath.Join(root, "golden-register", "request", "go", "example.com", "pkg", "*-open-track.json"))
	require.Len(t, filed, 1, "an unknown track on a known package is an open-track request")

	raw, _ := os.ReadFile(filed[0])
	require.Contains(t, string(raw), `"track":"1.27"`)
}

func TestAnExpiredHardPinIsAnError(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1", Pin: "v1.4.0", Mode: "hard",
			Reason: "v1.5 broke the parser", Expires: "2026-08-01",
		}})
	require.ErrorIs(t, err, resolvecontroller.ErrExpired)
	require.ErrorContains(t, err, "re-decide it or remove it")

	// A pin that has not expired yet stands.
	_, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1", Pin: "v1.4.0", Mode: "hard",
			Reason: "v1.5 broke the parser", Expires: "2026-12-01",
		}})
	require.NoError(t, err)
	require.Contains(t, notes[0], "frozen, visibly")
}

func TestADeprecationPastGraceIsAnError(t *testing.T) {
	deprecated := func(since string) string {
		raw, _ := json.Marshal(map[string]any{
			"package": "p", "ecosystem": "go", "prefix": "1",
			"current": "v1.6.0", "updatedAt": now,
			"deprecated": map[string]any{"reason": "stale", "since": since},
		})

		return string(raw)
	}

	// Inside the grace window: a warning with the deadline.
	f, root := register(t, map[string]string{
		"go/example.com/pkg/1": deprecated("2026-08-10T00:00:00Z"),
	})

	_, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
	require.NoError(t, err)
	require.Contains(t, notes[0], "before")

	// Past it: the resolution fails.
	f, root = register(t, map[string]string{
		"go/example.com/pkg/1": deprecated("2026-06-01T00:00:00Z"),
	})

	_, _, err = newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
	require.ErrorIs(t, err, resolvecontroller.ErrDeprecated)
}

func TestTheRegisterSetsItsOwnGraceWindow(t *testing.T) {
	deprecated := `{"package":"p","ecosystem":"go","prefix":"1","current":"v1.6.0",` +
		`"deprecated":{"reason":"superseded","since":"2026-06-01T00:00:00Z"}}`
	f, root := register(t, map[string]string{"go/example.com/pkg/1": deprecated})

	// Past the 30-day fallback but inside the register's own 120-day window:
	// the knob is register-level, and the consumer reads it.
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "golden-register", "forge-register.yaml"),
		[]byte("params:\n  deprecatedGraceDays: 120\n"), 0o600))

	_, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
	require.NoError(t, err)
	require.Contains(t, notes[0], "before")
}

func TestAnUnreadableExpiresDateIsAnError(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1", Pin: "v1.5.0", Mode: "hard", Reason: "frozen", Expires: "soon",
		}})
	require.ErrorContains(t, err, "not a date")
}

func TestAnRFC3339ExpiresStillInTheFutureHolds(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})

	got, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1", Pin: "v1.5.0", Mode: "hard", Reason: "frozen",
			Expires: "2027-01-01T00:00:00Z",
		}})
	require.NoError(t, err)
	require.Equal(t, "v1.5.0", got["example.com/pkg"])
	require.Contains(t, notes[0], "frozen, visibly")
}

func TestADirectoryAmongTrackFilesIsNotATrack(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "golden-register", "index", "go", "example.com", "pkg", "junk"), 0o750))

	got, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {}})
	require.NoError(t, err)
	require.Equal(t, "v1.6.0", got["example.com/pkg"])
}

func TestAMissingPackageAtAPinnedRevisionNamesTheRevision(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})
	dir := filepath.Join(root, "golden-register")

	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "index")
	gitIn(t, dir, "tag", "v0.1.0")

	f.Register.Revision = "v0.1.0"
	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/other": {}})
	require.ErrorIs(t, err, resolvecontroller.ErrUnregistered)
	require.ErrorContains(t, err, "v0.1.0")
}

func TestDeadPinReadsTheNoteBack(t *testing.T) {
	_, notes, err := func() (map[string]string, []string, error) {
		f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})

		return newController().Resolve(context.Background(), f, root, "go",
			map[string]config.DependencySpec{"example.com/pkg": {
				Track: "1", Pin: "v1.5.0", Mode: "soft", Reason: "old workaround",
			}})
	}()
	require.NoError(t, err)

	dep, ok := resolvecontroller.DeadPin(notes[0])
	require.True(t, ok)
	require.Equal(t, "go:example.com/pkg", dep)

	_, ok = resolvecontroller.DeadPin("hard pin go:x v1 (reason: r) - frozen, visibly")
	require.False(t, ok)
	_, ok = resolvecontroller.DeadPin("remove this pin")
	require.False(t, ok)
}

func TestAPEP440PrereleasePinComparesBelowItsRelease(t *testing.T) {
	f, root := register(t, map[string]string{"python/httpx/0": track("0.28.1")})

	// 1.0.dev5 carries no hyphen and is still a pre-release: it must sort
	// above the 0.x track (numerically) but below 1.0 - so a soft pin on it
	// reads as ahead of track 0, the same reading the register uses.
	_, notes, err := newController().Resolve(context.Background(), f, root, "python",
		map[string]config.DependencySpec{"httpx": {
			Track: "0", Pin: "1.0.dev5", Mode: "soft", Reason: "trying the beta",
		}})
	require.NoError(t, err)
	require.Contains(t, notes[0], "ahead of track")
}

func TestVersionTailsCombineAndOrder(t *testing.T) {
	f, root := register(t, map[string]string{"python/httpx/1": track("1.0rc1-x")})

	// A hyphen tail on top of a PEP 440 segment still parses; the pin below
	// the current is named dead, which pins the ordering both ways.
	_, notes, err := newController().Resolve(context.Background(), f, root, "python",
		map[string]config.DependencySpec{"httpx": {
			Track: "1", Pin: "1.0.dev5", Mode: "soft", Reason: "old",
		}})
	require.NoError(t, err)
	require.Contains(t, notes[0], "remove this pin")
}

// A lone checkout resolves through a cache materialisation of the register: a
// git worktree, whose .git is a link file. A request filed there is answered
// by nothing, so a missing package fails honestly instead - naming the real
// register and the commands that admit the package - and files nothing.
// Live case: a fresh bootstrap hard-blocked on the toolchain binaries with a
// message promising a self-heal that could never happen.
func TestACacheRegisterFilesNothingAndPointsAtTheRealOne(t *testing.T) {
	f, root := register(t, nil)
	dir := filepath.Join(root, "golden-register")
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git"),
		[]byte("gitdir: /elsewhere/.git/worktrees/x\n"), 0o600))

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"github.com/vektra/mockery/v3": {}})
	require.Error(t, err)
	require.ErrorIs(t, err, resolvecontroller.ErrUnregistered)
	require.Contains(t, err.Error(), "cache materialisation")
	require.Contains(t, err.Error(), "forge-register add go:github.com/vektra/mockery/v3")
	require.Contains(t, err.Error(), "git@github.com:example/golden-register.git")
	require.NotContains(t, err.Error(), "then sync again")

	_, statErr := os.Stat(filepath.Join(dir, "request"))
	require.True(t, os.IsNotExist(statErr), "no request may be filed into the cache")
}

// The toolchain path goes through the same door.
func TestACacheRegisterFailsAToolchainTrackHonestly(t *testing.T) {
	f, root := register(t, nil)
	dir := filepath.Join(root, "golden-register")
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git"),
		[]byte("gitdir: /elsewhere/.git/worktrees/x\n"), 0o600))

	_, _, err := newController().ResolveTool(context.Background(), f, root,
		"go:github.com/vektra/mockery/v3")
	require.Error(t, err)
	require.Contains(t, err.Error(), "cache materialisation")

	_, statErr := os.Stat(filepath.Join(dir, "request"))
	require.True(t, os.IsNotExist(statErr))
}

// A real checkout still files the request visibly, exactly as before.
func TestARealCheckoutStillFilesTheRequest(t *testing.T) {
	f, root := register(t, nil)
	dir := filepath.Join(root, "golden-register")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o750))

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "the register pipeline answers it, then sync again")

	entries, readErr := os.ReadDir(filepath.Join(dir, "request", "go", "example.com", "pkg"))
	require.NoError(t, readErr)
	require.Len(t, entries, 1)
}

// An admission committed locally but never pushed reads as "not in the
// register" on every other machine. The sync that resolves against such a
// checkout says so once. Live case: sixty local-only commits carried the
// toolchain admissions and a colleague's fresh clone blocked on them.
func TestAnUnpushedRegisterIsNamedInTheNotes(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})
	dir := filepath.Join(root, "golden-register")

	origin := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, root, "init", "-q", "--bare", origin)

	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "seed")
	gitIn(t, dir, "remote", "add", "origin", origin)
	gitIn(t, dir, "push", "-q", "origin", "main")
	gitIn(t, dir, "fetch", "-q", "origin")

	// One admission lands locally and never leaves the machine.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "local-only.json"), []byte("{}"), 0o600))
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "local admission")

	_, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
	require.NoError(t, err)
	require.Len(t, notes, 1)
	require.Contains(t, notes[0], "ahead of origin/main")
	require.Contains(t, notes[0], "a fresh clone will not see them")
}

// A checkout behind its origin reads a missing admission as "not in the
// register" when it may already sit upstream. The failure names that
// second cause and the pull that resolves it. Live case: sixty unpushed
// commits made three tools read as unadmitted on every other machine.
func TestABehindCheckoutNamesThePullInTheFailure(t *testing.T) {
	f, root := register(t, nil)
	dir := filepath.Join(root, "golden-register")

	origin := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, root, "init", "-q", "--bare", origin)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("seed\n"), 0o600))
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "seed")
	gitIn(t, dir, "remote", "add", "origin", origin)
	gitIn(t, dir, "push", "-q", "origin", "main")

	// The admission lands upstream through another clone; this checkout
	// never pulls it.
	other := filepath.Join(t.TempDir(), "other")
	gitIn(t, root, "clone", "-q", "-b", "main", origin, other)
	require.NoError(t, os.MkdirAll(filepath.Join(other, "index"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(other, "index", "admitted.json"), []byte("{}"), 0o600))
	gitIn(t, other, "add", ".")
	gitIn(t, other, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "admit upstream")
	gitIn(t, other, "push", "-q", "origin", "main")

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "behind origin/main")
	require.Contains(t, err.Error(), "git pull in")
}
