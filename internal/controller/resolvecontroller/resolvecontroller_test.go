package resolvecontroller_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func newController(opts ...resolvecontroller.Option) *resolvecontroller.Controller {
	// The driver wires the engine; a unit test does not shell out, and only
	// the tests that name a runner get one.
	return resolvecontroller.New(fsadapter.New(), gitadapter.New(execadapter.New()),
		func() time.Time { return now }, opts...)
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

	// A member that speaks go, because the reachability question is asked of
	// the repos that receive the dependency rather than of the register.
	f := config.Factory{
		Register: &config.Register{URL: "git@github.com:example/golden-register.git"},
		Repos: []config.Repo{
			{Name: "golden-go", URL: "u", Languages: []string{"go"}},
			{Name: "golden-rust", URL: "u", Languages: []string{"rust"}},
		},
	}

	return f, root
}

// track is a measured-clean record: the feed answered and no published
// range covers this version. The outcome is not decoration - it is what
// separates that from a package nobody ever asked about, and leaving it
// out of every fixture is why nothing noticed the consumer accepted an
// absent one.
func track(current string) string {
	raw, _ := json.Marshal(map[string]any{
		"package": "p", "ecosystem": "go", "prefix": "1",
		"current": current, "updatedAt": now,
		"vulns":   map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0},
		"outcome": "clean",
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
		"vulns":   map[string]int{"critical": 1, "high": 0, "medium": 0, "low": 0},
		"outcome": "findings",
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
	require.ErrorContains(t, err, "A pin cannot silence this")
}

func TestADeprecatedTrackWarns(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"package": "p", "ecosystem": "go", "prefix": "1",
		"current": "v1.6.0", "updatedAt": now,
		"vulns":      map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0},
		"outcome":    "clean",
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
			"vulns":      map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0},
			"outcome":    "clean",
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
		`"vulns":{"critical":0,"high":0,"medium":0,"low":0},"outcome":"clean",` +
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

// The finding golden-register actually carries, and the way out of it.
//
// GO-2026-5932 on golang.org/x/crypto has no severity, no fix upstream ever
// because the package is unmaintained by design, and an import scope that
// does not include anything this workspace uses. Upgrading cannot clear it,
// so acknowledging it is the only way past - which is why the error has to
// say all of that instead of a sentence somebody wrote once.
func TestAFindingSaysEverythingNeededToActOnIt(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"package": "golang.org/x/crypto", "ecosystem": "go", "prefix": "0",
		"current": "v0.55.0", "updatedAt": now,
		"vulns":   map[string]int{"critical": 0, "high": 1, "medium": 0, "low": 0},
		"outcome": "findings",
		"advisory": map[string]any{
			"vulnIds": []string{"GO-2026-5932"}, "severity": "unknown",
			"since":           "2026-08-11T00:00:00Z",
			"fixedIn":         []string{},
			"affectedImports": []string{"golang.org/x/crypto/openpgp", "golang.org/x/crypto/openpgp/armor"},
		},
	})

	f, root := register(t, map[string]string{"go/golang.org/x/crypto/0": string(raw)})

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"golang.org/x/crypto": {Track: "0"}})
	require.ErrorIs(t, err, resolvecontroller.ErrAdvisory)

	msg := err.Error()

	require.Contains(t, msg, "GO-2026-5932")
	require.Contains(t, msg, "severity  : not published by the feed",
		"38 percent of real records publish no severity; inventing one is worse")
	require.Contains(t, msg, "published : 2026-08-11",
		"the advisory's own date, not the moment the pipeline ran")
	require.Contains(t, msg, "golang.org/x/crypto/openpgp")
	require.Contains(t, msg, "no newer version fixes this. The feed names none.",
		"derived from an empty fixedIn, never asserted")
	require.Contains(t, msg, "acknowledge:")
	require.Contains(t, msg, "- id: GO-2026-5932")
	require.Contains(t, msg, "reason:")
}

// When a fix exists the error names it, because that is the easiest way out
// and the one a person should take. This line used to read "no fix upstream"
// on every advisory, whether or not anyone had looked.
func TestAFindingWithAFixNamesTheVersionThatFixesIt(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"package": "golang.org/x/net", "ecosystem": "go", "prefix": "0",
		"current": "v0.16.0", "updatedAt": now,
		"vulns":   map[string]int{"critical": 0, "high": 1, "medium": 0, "low": 0},
		"outcome": "findings",
		"advisory": map[string]any{
			"vulnIds": []string{"GHSA-4374-p667-p6c8"}, "severity": "high",
			"since": now, "fixedIn": []string{"0.17.0"},
		},
	})

	f, root := register(t, map[string]string{"go/golang.org/x/net/0": string(raw)})

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"golang.org/x/net": {Track: "0"}})
	require.ErrorContains(t, err, "upgrade. The feed names 0.17.0 as fixing this")
	require.NotContains(t, err.Error(), "no newer version fixes this")
}

// Acknowledging the finding you looked at clears it. Acknowledging it does
// not accept the next one.
func TestAnAcknowledgementClearsOnlyTheFindingItNames(t *testing.T) {
	one, _ := json.Marshal(map[string]any{
		"package": "p", "ecosystem": "go", "prefix": "1", "current": "v1.6.0", "updatedAt": now,
		"vulns":   map[string]int{"critical": 1, "high": 0, "medium": 0, "low": 0},
		"outcome": "findings",
		"advisory": map[string]any{
			"vulnIds": []string{"CVE-2026-1"}, "severity": "high", "since": now,
		},
	})

	f, root := register(t, map[string]string{"go/example.com/pkg/1": string(one)})

	ack := config.DependencySpec{Track: "1", Acknowledge: []config.Acknowledgement{{
		ID: "CVE-2026-1", Reason: "we do not call the affected path",
	}}}

	got, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": ack})
	require.NoError(t, err, "the finding was named on purpose, with a reason")
	require.Equal(t, "v1.6.0", got["example.com/pkg"])

	// A second finding appears. The first acknowledgement does not cover it.
	two, _ := json.Marshal(map[string]any{
		"package": "p", "ecosystem": "go", "prefix": "1", "current": "v1.6.0", "updatedAt": now,
		"vulns":   map[string]int{"critical": 1, "high": 0, "medium": 0, "low": 0},
		"outcome": "findings",
		"advisory": map[string]any{
			"vulnIds": []string{"CVE-2026-1", "CVE-2026-2"}, "severity": "high", "since": now,
		},
	})

	f2, root2 := register(t, map[string]string{"go/example.com/pkg/1": string(two)})

	_, _, err = newController().Resolve(context.Background(), f2, root2, "go",
		map[string]config.DependencySpec{"example.com/pkg": ack})
	require.ErrorIs(t, err, resolvecontroller.ErrAdvisory)
	require.ErrorContains(t, err, "CVE-2026-2")
	require.NotContains(t, err.Error(), "  CVE-2026-1\n", "the one that was named stays cleared")
}

// An acknowledgement that outlived its finding is named for removal, the same
// way a dead soft pin is. An accepted risk that no longer exists is a line
// nobody removes unless something says so.
func TestAnAcknowledgementIsNamedDeadWhenTheFindingGoes(t *testing.T) {
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})

	_, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1",
			Acknowledge: []config.Acknowledgement{{
				ID: "CVE-2026-1", Reason: "accepted last year",
			}},
		}})
	require.NoError(t, err)
	require.Contains(t, strings.Join(notes, "\n"),
		"acknowledges CVE-2026-1 and the register no longer reports it")
	require.Contains(t, strings.Join(notes, "\n"), "remove the acknowledgement")
}

// A package the feed never carried is not a package known to be safe. It
// warns, loudly, with the reason - and does not stop the build, because
// refusing to build when a feed is down protects nobody.
func TestAnUnmeasuredPackageWarnsAndDoesNotBlock(t *testing.T) {
	for _, outcome := range []string{"not-found", "unreachable"} {
		t.Run(outcome, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{
				"package": "p", "ecosystem": "go", "prefix": "1",
				"current": "v1.6.0", "updatedAt": now,
				"vulns":   map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0},
				"outcome": outcome,
				"reason":  "the feed carries no record for example.com/pkg in Go",
			})

			f, root := register(t, map[string]string{"go/example.com/pkg/1": string(raw)})

			got, notes, err := newController().Resolve(context.Background(), f, root, "go",
				map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
			require.NoError(t, err, "an unchecked package must not stop the build")
			require.Equal(t, "v1.6.0", got["example.com/pkg"])

			joined := strings.Join(notes, "\n")
			require.Contains(t, joined, "is unexamined, not known to be safe")
			require.Contains(t, joined, "The feed carries no record")

			// One line, not a paragraph. Fifty-six paragraphs is noise
			// nobody reads, which is the failure this note exists to stop.
			require.Len(t, notes, 1)
			require.NotContains(t, joined, "..")
			require.Less(t, len(notes[0]), 180, "one tight line: %q", notes[0])
		})
	}
}

// An expiry re-opens the decision. A risk accepted until something lands has
// to stop being accepted when it does not.
func TestAnExpiredAcknowledgementBlocksAgain(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"package": "p", "ecosystem": "go", "prefix": "1", "current": "v1.6.0", "updatedAt": now,
		"vulns":   map[string]int{"critical": 1, "high": 0, "medium": 0, "low": 0},
		"outcome": "findings",
		"advisory": map[string]any{
			"vulnIds": []string{"CVE-2026-1"}, "severity": "high", "since": now,
		},
	})

	f, root := register(t, map[string]string{"go/example.com/pkg/1": string(raw)})

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1",
			Acknowledge: []config.Acknowledgement{{
				ID: "CVE-2026-1", Reason: "until the rewrite lands", Expires: "2020-01-01",
			}},
		}})
	require.ErrorIs(t, err, resolvecontroller.ErrAdvisory)
	require.ErrorContains(t, err, "the acknowledgement expired")
	require.ErrorContains(t, err, "accepted until 2020-01-01")
}

// The reachability line, when an engine can answer.
//
// It is an enrichment and never a gate: the finding blocks either way. That
// ordering is the point - a reachability answer is a static approximation,
// and an approximation must not be the thing that unblocks a release.
func TestReachabilityEnrichesTheErrorAndNeverClearsIt(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"package": "golang.org/x/crypto", "ecosystem": "go", "prefix": "0",
		"current": "v0.55.0", "updatedAt": now,
		"vulns":   map[string]int{"critical": 0, "high": 1, "medium": 0, "low": 0},
		"outcome": "findings",
		"advisory": map[string]any{
			"vulnIds": []string{"GO-2026-5932"}, "severity": "unknown", "since": now,
			"fixedIn":         []string{},
			"affectedImports": []string{"golang.org/x/crypto/openpgp"},
		},
	})

	f, root := register(t, map[string]string{"go/golang.org/x/crypto/0": string(raw)})

	var asked []string

	answered := resolvecontroller.WithReachability(func(_ context.Context, dirs []string, in []byte) ([]byte, error) {
		asked = dirs

		require.Contains(t, string(in), "GO-2026-5932")
		require.Contains(t, string(in), "golang.org/x/crypto/openpgp")

		return []byte(`{"depth":"imports","answers":[{"id":"GO-2026-5932",
			"verdict":"not-reached",
			"reason":"your build imports none of the 1 package(s) this advisory covers"}]}`), nil
	})

	_, _, err := newController(answered).Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"golang.org/x/crypto": {Track: "0"}})

	require.ErrorIs(t, err, resolvecontroller.ErrAdvisory,
		"knowing the code is not reached does not clear the finding")
	require.ErrorContains(t, err, "your code does not reach it")
	require.ErrorContains(t, err, "does not make it safe to ignore")
	require.ErrorContains(t, err, "acknowledge:",
		"the way out is still to name it on purpose")

	// The engine is asked about the members that receive the dependency, not
	// about the register checkout. It used to be handed the register, so the
	// sentence "your code does not reach it" was computed over a repo the
	// operator was not asking about, and printed as a fact about theirs.
	require.NotEmpty(t, asked)

	for _, d := range asked {
		require.NotContains(t, d, "golden-register",
			"the register does not import the operator's dependencies")
	}

	require.Contains(t, asked[0], "golden-go", "a member that speaks go")
}

// No engine, one line fewer. Nothing fails and nothing is invented.
func TestAnUnavailableReachabilityEngineChangesNothingElse(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"package": "p", "ecosystem": "go", "prefix": "1", "current": "v1.6.0", "updatedAt": now,
		"vulns":   map[string]int{"critical": 1, "high": 0, "medium": 0, "low": 0},
		"outcome": "findings",
		"advisory": map[string]any{
			"vulnIds": []string{"CVE-2026-1"}, "severity": "high", "since": now,
			"affectedImports": []string{"example.com/pkg/inner"},
		},
	})

	f, root := register(t, map[string]string{"go/example.com/pkg/1": string(raw)})

	missing := resolvecontroller.WithReachability(func(context.Context, []string, []byte) ([]byte, error) {
		return nil, errors.New("forge: go-vulncheck is not provisioned")
	})

	_, _, err := newController(missing).Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})

	require.ErrorIs(t, err, resolvecontroller.ErrAdvisory)
	require.ErrorContains(t, err, "CVE-2026-1")
	require.NotContains(t, err.Error(), "your code")
}

// A reason that is only punctuation crashed the whole sync with a stack
// trace. sentence() sliced the first byte off a string that was empty after
// trimming, and split a multi-byte rune when it was not.
func TestASloppyReasonDoesNotCrashTheSync(t *testing.T) {
	for _, reason := range []string{" ", ".", ". ", "..", "  .  ", "élan is unmeasured"} {
		t.Run(reason, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{
				"package": "p", "ecosystem": "go", "prefix": "1",
				"current": "v1.6.0", "updatedAt": now,
				"vulns":   map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0},
				"outcome": "not-found", "reason": reason,
			})

			f, root := register(t, map[string]string{"go/example.com/pkg/1": string(raw)})

			require.NotPanics(t, func() {
				_, notes, err := newController().Resolve(context.Background(), f, root, "go",
					map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
				require.NoError(t, err)
				require.Len(t, notes, 1)
			})
		})
	}
}

// The quiet way past the gate was to delete the advisory block. Removing the
// whole track file fails loud; removing the block left outcome: findings and
// a non-zero vector behind, and resolved green with no note at all.
func TestARecordThatContradictsItselfIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  map[string]any
		want string
	}{
		{
			name: "findings with the advisory block deleted",
			doc: map[string]any{
				"vulns":   map[string]int{"critical": 2, "high": 0, "medium": 0, "low": 0},
				"outcome": "findings",
			},
			want: "records findings and names none",
		},
		{
			name: "findings that count nothing",
			doc: map[string]any{
				"vulns":   map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0},
				"outcome": "findings",
				"advisory": map[string]any{
					"vulnIds": []string{"CVE-1"}, "severity": "high", "since": now,
				},
			},
			want: "records findings and counts none",
		},
		{
			name: "an advisory naming no vulnerability",
			doc: map[string]any{
				"vulns":   map[string]int{"critical": 1, "high": 0, "medium": 0, "low": 0},
				"outcome": "findings",
				"advisory": map[string]any{
					"vulnIds": []string{}, "severity": "critical", "since": now,
				},
			},
			want: "naming no vulnerability",
		},
		{
			name: "clean that counts vulnerabilities",
			doc: map[string]any{
				"vulns":   map[string]int{"critical": 1, "high": 0, "medium": 0, "low": 0},
				"outcome": "clean",
			},
			want: "records clean and counts 1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := map[string]any{
				"package": "p", "ecosystem": "go", "prefix": "1",
				"current": "v1.6.0", "updatedAt": now,
				"vulns":   map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0},
				"outcome": "clean",
			}
			for k, v := range tc.doc {
				doc[k] = v
			}

			raw, _ := json.Marshal(doc)
			f, root := register(t, map[string]string{"go/example.com/pkg/1": string(raw)})

			_, _, err := newController().Resolve(context.Background(), f, root, "go",
				map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
			require.ErrorIs(t, err, resolvecontroller.ErrAdvisory)
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, err, "malformed")
		})
	}
}

// An expiry that cannot be read is not an expiry. Treating it as "never
// expires" turned a typo into a permanent acceptance, silently.
func TestAnUnreadableExpiryFailsClosed(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"package": "p", "ecosystem": "go", "prefix": "1", "current": "v1.6.0", "updatedAt": now,
		"vulns":   map[string]int{"critical": 0, "high": 1, "medium": 0, "low": 0},
		"outcome": "findings",
		"advisory": map[string]any{
			"vulnIds": []string{"CVE-2026-1"}, "severity": "high", "since": now,
		},
	})

	f, root := register(t, map[string]string{"go/example.com/pkg/1": string(raw)})

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {
			Track: "1",
			Acknowledge: []config.Acknowledgement{{
				ID: "CVE-2026-1", Reason: "typo'd the date", Expires: "not-a-date",
			}},
		}})
	require.ErrorIs(t, err, resolvecontroller.ErrAdvisory)
	require.ErrorContains(t, err, "is not a date")
}

// "accepted until 2026-08-21" means through that day. time.Parse gives
// midnight, so the acknowledgement used to die a day before the date it named
// - and the error still printed that date back at the operator.
func TestTheExpiryDateItselfIsStillAccepted(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"package": "p", "ecosystem": "go", "prefix": "1", "current": "v1.6.0", "updatedAt": now,
		"vulns":   map[string]int{"critical": 0, "high": 1, "medium": 0, "low": 0},
		"outcome": "findings",
		"advisory": map[string]any{
			"vulnIds": []string{"CVE-2026-1"}, "severity": "high", "since": now,
		},
	})

	f, root := register(t, map[string]string{"go/example.com/pkg/1": string(raw)})

	// now is 2026-08-21T12:00Z.
	for _, tc := range []struct {
		expires string
		blocked bool
	}{
		{"2026-08-19", true},
		{"2026-08-20", true},
		{"2026-08-21", false},
		{"2026-08-22", false},
	} {
		t.Run(tc.expires, func(t *testing.T) {
			_, _, err := newController().Resolve(context.Background(), f, root, "go",
				map[string]config.DependencySpec{"example.com/pkg": {
					Track: "1",
					Acknowledge: []config.Acknowledgement{{
						ID: "CVE-2026-1", Reason: "until the rewrite", Expires: tc.expires,
					}},
				}})

			if tc.blocked {
				require.ErrorIs(t, err, resolvecontroller.ErrAdvisory)
				require.ErrorContains(t, err, "the acknowledgement expired")
			} else {
				require.NoError(t, err, "the named date is included")
			}
		})
	}
}

// A finding on a toolchain binary was unrecoverable: the gate refused and the
// error told the operator to write yaml the schema then rejected.
func TestAToolchainBinaryCanBeAcknowledgedToo(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"package": "github.com/vektra/mockery/v3", "ecosystem": "go", "prefix": "3",
		"current": "v3.7.3", "updatedAt": now,
		"vulns":   map[string]int{"critical": 0, "high": 1, "medium": 0, "low": 0},
		"outcome": "findings",
		"advisory": map[string]any{
			"vulnIds": []string{"CVE-2026-7"}, "severity": "high", "since": now,
		},
	})

	f, root := register(t, map[string]string{"go/github.com/vektra/mockery/v3/3": string(raw)})

	_, _, err := newController().ResolveTool(context.Background(), f, root,
		"go:github.com/vektra/mockery/v3")
	require.ErrorIs(t, err, resolvecontroller.ErrAdvisory, "unacknowledged, it blocks")

	f.Toolchain = &config.Toolchain{Binaries: []config.ToolchainBinary{{
		Name: "mockery", Module: "github.com/vektra/mockery/v3",
		Track: "go:github.com/vektra/mockery/v3",
		Acknowledge: []config.Acknowledgement{{
			ID: "CVE-2026-7", Reason: "a build-time generator, not shipped",
		}},
	}}}

	got, _, err := newController().ResolveTool(context.Background(), f, root,
		"go:github.com/vektra/mockery/v3")
	require.NoError(t, err, "named on purpose, it resolves")
	require.Equal(t, "v3.7.3", got)
}

func TestAnUnreadableTrackFileIsNamed(t *testing.T) {
	f, root := register(t, nil)

	// A directory where the track file belongs. The register is a git
	// checkout somebody may have mangled, and the reply has to name the path
	// rather than report the package as absent, which would file a spurious
	// admission request.
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "golden-register", "index", "go", "p", "1.json"), 0o750))

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"p": {Track: "1"}})
	require.ErrorContains(t, err, "reading track index/go/p/1.json")
}

func TestAnUnlistableTrackDirectoryIsNamed(t *testing.T) {
	f, root := register(t, nil)

	// A file where the package's directory belongs, so the default track
	// cannot be chosen.
	dir := filepath.Join(root, "golden-register", "index", "go")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "p"), nil, 0o600))

	_, _, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"p": {}})
	require.ErrorContains(t, err, "listing tracks of go:p")
}

func TestResolveWithoutARegisterSaysSo(t *testing.T) {
	_, _, err := newController().Resolve(context.Background(), config.Factory{}, t.TempDir(), "go",
		map[string]config.DependencySpec{"p": {}})
	require.ErrorContains(t, err, "no register block")
}

func TestResolveNamesAnUncheckedOutRegister(t *testing.T) {
	f := config.Factory{Register: &config.Register{URL: "git@github.com:example/golden-register.git"}}

	_, _, err := newController().Resolve(context.Background(), f, t.TempDir(), "go",
		map[string]config.DependencySpec{"p": {}})
	// The remedy, not just the diagnosis. Nobody guesses "clone" from
	// "not checked out".
	require.ErrorContains(t, err, "run: forge-factory clone")
}

func TestResolveToolRefusesATrackThatIsNotAPair(t *testing.T) {
	f, root := register(t, nil)

	for _, name := range []string{"nocolon", ":pkg", "go:", ""} {
		_, _, err := newController().ResolveTool(context.Background(), f, root, name)
		require.ErrorContains(t, err, "is named <ecosystem>:<package>")
	}
}

// The register writes a reason for every unmeasured record, and a copy
// written by hand may not. The note still has to say something: "is
// unexamined." followed by nothing reads as a truncated message.
func TestAnUnmeasuredRecordWithNoReasonStillReads(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"package": "p", "ecosystem": "go", "prefix": "1",
		"current": "v1.6.0", "updatedAt": now,
		"vulns":   map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0},
		"outcome": "not-found",
	})

	f, root := register(t, map[string]string{"go/example.com/pkg/1": string(raw)})

	_, notes, err := newController().Resolve(context.Background(), f, root, "go",
		map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
	require.NoError(t, err)
	require.Len(t, notes, 1)
	require.Contains(t, notes[0], "The register does not say why.")
}

// The outcome is what tells a measured-clean package from one nobody ever
// asked about, and every rule downstream switches on it. An absent or
// misspelled one matched no case and resolved green with no note - the
// "clean when nothing was measured" bug again, reachable by editing one
// line of a file every consumer shares.
func TestATrackWithNoReadableOutcomeIsRefused(t *testing.T) {
	for name, outcome := range map[string]any{
		"absent":     nil,
		"empty":      "",
		"misspelled": "cleen",
		"invented":   "probably-fine",
	} {
		t.Run(name, func(t *testing.T) {
			doc := map[string]any{
				"package": "p", "ecosystem": "go", "prefix": "1",
				"current": "v1.6.0", "updatedAt": now,
				"vulns": map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0},
			}
			if outcome != nil {
				doc["outcome"] = outcome
			}

			raw, _ := json.Marshal(doc)
			f, root := register(t, map[string]string{"go/example.com/pkg/1": string(raw)})

			_, _, err := newController().Resolve(context.Background(), f, root, "go",
				map[string]config.DependencySpec{"example.com/pkg": {Track: "1"}})
			require.ErrorContains(t, err, "which is not one this can read")
			// The four it does read, so the reader can fix the file.
			require.ErrorContains(t, err, "findings, clean, not-found or unreachable")
		})
	}
}

func internalTrack(t *testing.T, module, prefix, current string) (string, string) {
	t.Helper()

	raw, _ := json.Marshal(map[string]any{
		"package": module, "ecosystem": "internal", "prefix": prefix,
		"current": current, "updatedAt": now,
		"vulns":   map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0},
		"outcome": "clean",
	})

	return "internal/" + module + "/" + prefix, string(raw)
}

// A shared spec one member imports from another is declared in no dependency
// list, so its version was decided by the tidy that follows: the highest
// published tag. That drifted in the real workspace, one member carrying a
// spec at v0.3.1 while the register's track said v0.3.0 - a version nothing
// ever adopted.
func TestMemberModulesResolveFromTheInternalTrack(t *testing.T) {
	k1, v1 := internalTrack(t, "github.com/example/spec-a", "0", "v0.3.0")
	k2, v2 := internalTrack(t, "github.com/example/spec-b", "1", "v1.4.2")
	// A lower track on the same module: the highest wins, as everywhere else.
	k3, v3 := internalTrack(t, "github.com/example/spec-b", "0", "v0.9.9")

	f, root := register(t, map[string]string{k1: v1, k2: v2, k3: v3})

	got, err := newController().ResolveMembers(context.Background(), f, root, "go")
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"github.com/example/spec-a": "v0.3.0",
		"github.com/example/spec-b": "v1.4.2",
	}, got)
}

func TestMemberModulesAreAGoQuestionOnly(t *testing.T) {
	k, v := internalTrack(t, "github.com/example/spec-a", "0", "v0.3.0")
	f, root := register(t, map[string]string{k: v})

	// The internal ecosystem is module paths the go command resolves.
	// Another language's members are named differently and have no tracks.
	for _, language := range []string{"rust", "python", "typescript"} {
		got, err := newController().ResolveMembers(context.Background(), f, root, language)
		require.NoError(t, err)
		require.Empty(t, got)
	}
}

func TestNoInternalTracksIsNotAFailure(t *testing.T) {
	// A register with no internal ecosystem is the first-run shape, and a
	// workspace whose members share nothing has no tracks to read.
	f, root := register(t, map[string]string{"go/example.com/pkg/1": track("v1.6.0")})

	got, err := newController().ResolveMembers(context.Background(), f, root, "go")
	require.NoError(t, err)
	require.Empty(t, got)

	// And a workspace with no register at all resolves nothing rather than
	// failing: the register block is optional.
	got, err = newController().ResolveMembers(context.Background(), config.Factory{}, root, "go")
	require.NoError(t, err)
	require.Empty(t, got)
}

// The register is a git checkout somebody can mangle. A track that exists
// and cannot be read is named, because somebody has to fix that file; a
// module with no track at all is simply not pinned.
func TestAMangledInternalTrackIsNamed(t *testing.T) {
	for name, mangle := range map[string]func(*testing.T, string){
		"a track file that is not json": func(t *testing.T, dir string) {
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "spec-b"), 0o750))
			require.NoError(t, os.WriteFile(
				filepath.Join(dir, "spec-b", "0.json"), []byte("{{{"), 0o600))
		},
		"a directory where a track file belongs": func(t *testing.T, dir string) {
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "spec-b", "0.json"), 0o750))
		},
	} {
		t.Run(name, func(t *testing.T) {
			good, raw := internalTrack(t, "github.com/example/spec-a", "0", "v0.3.0")
			f, root := register(t, map[string]string{good: raw})

			mangle(t, filepath.Join(root, "golden-register", "index", "internal",
				"github.com", "example"))

			_, err := newController().ResolveMembers(context.Background(), f, root, "go")
			require.Error(t, err)
		})
	}
}

func TestAnUncheckedOutRegisterIsNamed(t *testing.T) {
	f := config.Factory{Register: &config.Register{
		URL: "git@github.com:example/golden-register.git",
	}}

	_, err := newController().ResolveMembers(context.Background(), f, t.TempDir(), "go")
	require.ErrorContains(t, err, "run: forge-factory clone")
}

// A track directory holding no json at all - the shape a half-written
// register leaves behind.
func TestATrackDirectoryWithNoRecordIsSkipped(t *testing.T) {
	good, raw := internalTrack(t, "github.com/example/spec-a", "0", "v0.3.0")
	f, root := register(t, map[string]string{good: raw})

	require.NoError(t, os.MkdirAll(filepath.Join(root, "golden-register",
		"index", "internal", "github.com", "example", "spec-empty"), 0o750))

	got, err := newController().ResolveMembers(context.Background(), f, root, "go")
	require.NoError(t, err)
	require.Len(t, got, 1)
}

// A pinned register is read through git rather than the worktree, and git
// lists recursively. The two readers walk the tree differently, so both need
// driving or half of this is untested.
func TestMemberModulesReadThroughAPinnedRevision(t *testing.T) {
	k1, v1 := internalTrack(t, "github.com/example/spec-a", "0", "v0.3.0")
	k2, v2 := internalTrack(t, "github.com/example/deep/spec-b", "1", "v1.4.2")

	f, root := register(t, map[string]string{k1: v1, k2: v2})
	dir := filepath.Join(root, "golden-register")

	gitIn(t, dir, "init", "-q")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "index")
	gitIn(t, dir, "tag", "v0.1.0")

	// The worktree moves past the tag.
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "index", "internal", "github.com", "example", "spec-a", "0.json"),
		[]byte(v1[:len(v1)-1]+`,"x":1}`), 0o600))

	f.Register.Revision = "v0.1.0"

	got, err := newController().ResolveMembers(context.Background(), f, root, "go")
	require.NoError(t, err)

	// Both modules, including the deeper one, and at the tag's versions.
	require.Equal(t, map[string]string{
		"github.com/example/spec-a":      "v0.3.0",
		"github.com/example/deep/spec-b": "v1.4.2",
	}, got)
}
