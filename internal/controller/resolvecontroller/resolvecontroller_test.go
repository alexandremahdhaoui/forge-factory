package resolvecontroller_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/resolvecontroller"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

var now = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

func newController() *resolvecontroller.Controller {
	return resolvecontroller.New(fsadapter.New(), func() time.Time { return now })
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

	filed, err2 := filepath.Glob(filepath.Join(root, "golden-register", "requests", "go", "example.com", "absent", "*.json"))
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
    engine: go://example.com/lang-go
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
    engine: go://example.com/lang-go
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

	filed, _ := filepath.Glob(filepath.Join(root, "golden-register", "requests", "go", "example.com", "pkg", "*.json"))
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
