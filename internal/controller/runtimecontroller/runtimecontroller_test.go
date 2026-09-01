package runtimecontroller_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/lockadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/runtimecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/engineadaptermock"
	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// answer writes a wire-shaped value into an engine call's out parameter, the
// way the real MCP caller does: through json. The controller's wire structs
// stay unexported, exactly as they should.
func answer(t *testing.T, out, value any) {
	t.Helper()

	raw, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, out))
}

type harness struct {
	caller *engineadaptermock.MockCaller
	c      *runtimecontroller.Controller
	root   string
	store  string
}

// newHarness pins PATH and the process env: expose applies both to the
// running process on purpose, and the test must give them back.
func newHarness(t *testing.T) *harness {
	t.Helper()

	t.Setenv("PATH", os.Getenv("PATH"))

	caller := engineadaptermock.NewMockCaller(t)

	return &harness{
		caller: caller,
		c:      runtimecontroller.New(caller, fsadapter.New(), lockadapter.New()),
		root:   t.TempDir(),
		store:  t.TempDir(),
	}
}

func factoryWith(engines ...config.Engine) config.Factory {
	return config.Factory{Engines: engines}
}

func toyEngine() config.Engine {
	return config.Engine{Alias: "toygo", Engine: "forge://example.com/toy-provider"}
}

// description is the map form of the provider's answer, shaped as the wire.
func description(overrides map[string]any) map[string]any {
	d := map[string]any{
		"runtime": "toygo",
		"version": "1.0.0",
		"artifacts": []map[string]any{
			{"url": "https://example.com/toy.tar.gz", "sha256": strings.Repeat("a", 64), "unpack": "tar-gz"},
		},
		"bins": []string{"bin/toygo"},
	}

	for k, v := range overrides {
		d[k] = v
	}

	return d
}

func (h *harness) expectDescribe(t *testing.T, desc map[string]any) {
	t.Helper()

	h.caller.EXPECT().
		Call(mock.Anything, "forge://example.com/toy-provider", "describe", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			answer(t, out, desc)

			return nil
		}).Once()
}

// expectFetchAndInstall plays the default engines: the fetch answers a path,
// the install lays the runtime out into the staged prefix.
func (h *harness) expectFetchAndInstall(t *testing.T) {
	t.Helper()

	h.caller.EXPECT().
		Call(mock.Anything, runtimecontroller.DefaultFetchEngine, "fetch", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			dest := field(t, in, "dest")
			require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o750))
			require.NoError(t, os.WriteFile(dest, []byte("archive"), 0o600))
			answer(t, out, map[string]any{"path": dest, "sha256": strings.Repeat("a", 64)})

			return nil
		}).Once()

	h.caller.EXPECT().
		Call(mock.Anything, runtimecontroller.DefaultInstallEngine, "install", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			prefix := field(t, in, "prefix")
			require.NoError(t, os.MkdirAll(filepath.Join(prefix, "bin"), 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(prefix, "bin", "toygo"), []byte("#!/bin/sh\n"), 0o755)) //nolint:gosec // the fixture is an executable
			answer(t, out, map[string]any{"installed": []string{"bin/toygo"}})

			return nil
		}).Once()
}

// field reads one string key out of a wire input, through json, so the test
// never needs the unexported struct.
func field(t *testing.T, in any, key string) string {
	t.Helper()

	raw, err := json.Marshal(in)
	require.NoError(t, err)

	var m map[string]any

	require.NoError(t, json.Unmarshal(raw, &m))

	s, _ := m[key].(string)
	require.NotEmpty(t, s, "input carries no %q", key)

	return s
}

func TestProvisionWalksTheWholeLifecycle(t *testing.T) {
	h := newHarness(t)
	h.expectDescribe(t, description(map[string]any{
		"env": map[string]string{"TOY_HOME": "{prefix}", "TOY_LIB": "{prefix}/lib"},
	}))
	h.expectFetchAndInstall(t)

	report, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.NoError(t, err)

	assert.Equal(t, []string{"toygo@1.0.0"}, report.Installed)
	assert.Empty(t, report.Reused)
	assert.Equal(t, []string{"toygo"}, report.Exposed)

	// The store landed atomically under runtimes/<name>@<version>.
	prefix := filepath.Join(h.store, "runtimes", "toygo@1.0.0")
	_, err = os.Stat(filepath.Join(prefix, "bin", "toygo"))
	require.NoError(t, err)

	// The workspace's .forge/bin links to it.
	target, err := os.Readlink(filepath.Join(h.root, ".forge", "bin", "toygo"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(prefix, "bin", "toygo"), target)

	// The env file composes the runtime's environment, {prefix} replaced.
	raw, err := os.ReadFile(filepath.Join(h.root, ".forge", "env"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), "export TOY_HOME=\""+prefix+"\"\n")
	assert.Contains(t, string(raw), "export TOY_LIB=\""+filepath.Join(prefix, "lib")+"\"\n")

	// And this very process sees both, so the commands that follow
	// provisioning in the same run already resolve the runtime.
	assert.Equal(t, prefix, os.Getenv("TOY_HOME"))
	assert.True(t, strings.HasPrefix(os.Getenv("PATH"),
		filepath.Join(h.root, ".forge", "bin")+string(os.PathListSeparator)))
}

func TestAnInstalledRuntimeIsReusedUntouched(t *testing.T) {
	h := newHarness(t)

	prefix := filepath.Join(h.store, "runtimes", "toygo@1.0.0")
	require.NoError(t, os.MkdirAll(filepath.Join(prefix, "bin"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(prefix, "bin", "toygo"), []byte("#!/bin/sh\n"), 0o755)) //nolint:gosec // the fixture is an executable

	// Only describe is called: the store already holds the runtime, so no
	// fetch and no install - the store is immutable and the reuse is the
	// converged state.
	h.expectDescribe(t, description(nil))

	report, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.NoError(t, err)

	assert.Empty(t, report.Installed)
	assert.Equal(t, []string{"toygo@1.0.0"}, report.Reused)
	assert.Equal(t, []string{"toygo"}, report.Exposed)
}

func TestNothingPinsAVersion(t *testing.T) {
	h := newHarness(t)

	_, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo"}})
	require.ErrorContains(t, err, "nothing pins a version")
}

func TestARuntimeWithNoEngineIsRefused(t *testing.T) {
	h := newHarness(t)

	_, err := h.c.Provision(context.Background(), factoryWith(), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.ErrorContains(t, err, "no engine is declared under that alias")
}

func TestAProviderAnsweringNoArtifactsIsRefused(t *testing.T) {
	h := newHarness(t)
	h.expectDescribe(t, description(map[string]any{"artifacts": []map[string]any{}}))

	_, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.ErrorContains(t, err, "answered no artifacts")
}

// A prerequisite is satisfied by another declared runtime that provides the
// capability - zig provides c-compiler for go - and nothing touches the host.
func TestAPrerequisiteProvidedByADeclaredRuntime(t *testing.T) {
	h := newHarness(t)

	needy := description(map[string]any{
		"prerequisites": []map[string]any{
			{"name": "c-compiler", "reason": "cgo (go test -race)", "verify": "cc"},
		},
	})
	provider := map[string]any{
		"runtime": "toycc", "version": "2.0.0",
		"artifacts": []map[string]any{
			{"url": "https://example.com/cc.tar.gz", "sha256": strings.Repeat("b", 64), "unpack": "tar-gz"},
		},
		"provides": []string{"c-compiler"},
	}

	h.expectDescribe(t, needy)
	h.caller.EXPECT().
		Call(mock.Anything, "forge://example.com/cc-provider", "describe", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			answer(t, out, provider)

			return nil
		}).Once()

	// Both prefixes pre-exist so nothing fetches; the point is the
	// prerequisite resolution.
	for _, p := range []string{"toycc@2.0.0", "toygo@1.0.0"} {
		require.NoError(t, os.MkdirAll(filepath.Join(h.store, "runtimes", p), 0o750))
	}

	f := factoryWith(toyEngine(), config.Engine{Alias: "toycc", Engine: "forge://example.com/cc-provider"})

	report, err := h.c.Provision(context.Background(), f, h.root, h.store, []runtimecontroller.Pin{
		{Name: "toygo", Version: "1.0.0"},
		{Name: "toycc", Version: "2.0.0"},
	})
	require.NoError(t, err)
	require.Len(t, report.Satisfied, 1)
	assert.Contains(t, report.Satisfied[0], "provided by the declared runtime toycc")
}

func TestAPrerequisiteVerifiedOnTheHost(t *testing.T) {
	h := newHarness(t)
	h.expectDescribe(t, description(map[string]any{
		"prerequisites": []map[string]any{
			// sh is on every machine this suite runs on.
			{"name": "posix-shell", "reason": "the tests need one", "verify": "sh"},
		},
	}))

	require.NoError(t, os.MkdirAll(filepath.Join(h.store, "runtimes", "toygo@1.0.0"), 0o750))

	report, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.NoError(t, err)
	require.Len(t, report.Satisfied, 1)
	assert.Contains(t, report.Satisfied[0], "found on the host")
}

// The refusal names the prerequisite, the reason, and every avenue - and it
// happens before anything is fetched, so a doomed provision downloads nothing.
func TestAMissingPrerequisiteFailsLoudBeforeFetching(t *testing.T) {
	h := newHarness(t)
	h.expectDescribe(t, description(map[string]any{
		"prerequisites": []map[string]any{
			{"name": "c-compiler", "reason": "cgo (go test -race)", "verify": "definitely-not-a-real-compiler"},
		},
	}))

	_, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.ErrorContains(t, err, "c-compiler")
	require.ErrorContains(t, err, "cgo (go test -race)")
	require.ErrorContains(t, err, "declare a runtime whose provides lists")

	// The store stayed empty: no fetch call was expected and none happened.
	_, statErr := os.Stat(filepath.Join(h.store, "runtimes"))
	assert.True(t, os.IsNotExist(statErr))
}

func TestAPrerequisiteNoRuntimeProvidesAndNoVerifyIsRefused(t *testing.T) {
	h := newHarness(t)
	h.expectDescribe(t, description(map[string]any{
		"prerequisites": []map[string]any{
			{"name": "kernel-module", "reason": "only a providing runtime can"},
		},
	}))

	_, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.ErrorContains(t, err, "cannot be verified on the host")
}

func TestSatisfyOverridesToTheHost(t *testing.T) {
	h := newHarness(t)
	h.expectDescribe(t, description(map[string]any{
		"prerequisites": []map[string]any{
			{"name": "posix-shell", "reason": "the tests need one", "verify": "sh"},
		},
	}))

	require.NoError(t, os.MkdirAll(filepath.Join(h.store, "runtimes", "toygo@1.0.0"), 0o750))

	f := factoryWith(toyEngine())
	f.Toolchain = &config.Toolchain{Satisfy: map[string]string{"posix-shell": "host"}}

	report, err := h.c.Provision(context.Background(), f, h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.NoError(t, err)
	require.Len(t, report.Satisfied, 1)
	assert.Contains(t, report.Satisfied[0], "satisfy: names the host")
}

func TestSatisfyNamingANonProviderIsRefused(t *testing.T) {
	h := newHarness(t)
	h.expectDescribe(t, description(map[string]any{
		"prerequisites": []map[string]any{
			{"name": "c-compiler", "reason": "cgo", "verify": "cc"},
		},
	}))

	f := factoryWith(toyEngine())
	f.Toolchain = &config.Toolchain{Satisfy: map[string]string{"c-compiler": "toyrust"}}

	_, err := h.c.Provision(context.Background(), f, h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.ErrorContains(t, err, "not a declared runtime providing it")
}

// A declared fetch engine wins over the default, and its spec - the mirror
// rewrites, an internal store's credentials - travels with the call.
func TestADeclaredFetchEngineAndItsSpecAreUsed(t *testing.T) {
	h := newHarness(t)
	h.expectDescribe(t, description(nil))

	h.caller.EXPECT().
		Call(mock.Anything, "forge://example.com/my-fetch", "fetch", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			raw, err := json.Marshal(in)
			require.NoError(t, err)
			assert.Contains(t, string(raw), `"rewrite"`)

			dest := field(t, in, "dest")
			require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o750))
			require.NoError(t, os.WriteFile(dest, []byte("archive"), 0o600))
			answer(t, out, map[string]any{"path": dest, "sha256": strings.Repeat("a", 64)})

			return nil
		}).Once()
	h.caller.EXPECT().
		Call(mock.Anything, runtimecontroller.DefaultInstallEngine, "install", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			answer(t, out, map[string]any{"installed": []string{}})

			return nil
		}).Once()

	f := factoryWith(toyEngine(), config.Engine{
		Alias:  "fetch",
		Engine: "forge://example.com/my-fetch",
		Spec:   map[string]any{"rewrite": []map[string]string{{"from": "https://example.com/", "to": "https://mirror/"}}},
	})

	_, err := h.c.Provision(context.Background(), f, h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.NoError(t, err)
}

// A failing install leaves nothing behind: the staged prefix never lands, so
// a later provision starts clean instead of reusing a half-written runtime.
func TestAFailedInstallLandsNothing(t *testing.T) {
	h := newHarness(t)
	h.expectDescribe(t, description(nil))

	h.caller.EXPECT().
		Call(mock.Anything, runtimecontroller.DefaultFetchEngine, "fetch", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			dest := field(t, in, "dest")
			require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o750))
			require.NoError(t, os.WriteFile(dest, []byte("archive"), 0o600))
			answer(t, out, map[string]any{"path": dest, "sha256": strings.Repeat("a", 64)})

			return nil
		}).Once()
	h.caller.EXPECT().
		Call(mock.Anything, runtimecontroller.DefaultInstallEngine, "install", mock.Anything, mock.Anything).
		Return(assert.AnError).Once()

	_, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.ErrorContains(t, err, "installing toygo@1.0.0")

	_, statErr := os.Stat(filepath.Join(h.store, "runtimes", "toygo@1.0.0"))
	assert.True(t, os.IsNotExist(statErr), "the staged prefix must never land")
}

func TestNoPinsIsANoOp(t *testing.T) {
	h := newHarness(t)

	report, err := h.c.Provision(context.Background(), config.Factory{}, h.root, h.store, nil)
	require.NoError(t, err)
	assert.Empty(t, report.Installed)
	assert.Empty(t, report.Reused)
}

func TestTheStoreResolvesFromTheEnvironment(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FORGE_STORE_DIR", h.store)

	require.NoError(t, os.MkdirAll(filepath.Join(h.store, "runtimes", "toygo@1.0.0"), 0o750))
	h.expectDescribe(t, description(nil))

	// StoreDir empty: FORGE_STORE_DIR decides, which is how CI points every
	// job at a cached store without a flag.
	report, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, "",
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"toygo@1.0.0"}, report.Reused)
}

func TestAFailedDescribeNamesTheRuntime(t *testing.T) {
	h := newHarness(t)

	h.caller.EXPECT().
		Call(mock.Anything, "forge://example.com/toy-provider", "describe", mock.Anything, mock.Anything).
		Return(assert.AnError).Once()

	_, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.ErrorContains(t, err, "describing runtime toygo@1.0.0")
}

func TestAFailedFetchNamesTheArtifact(t *testing.T) {
	h := newHarness(t)
	h.expectDescribe(t, description(nil))

	h.caller.EXPECT().
		Call(mock.Anything, runtimecontroller.DefaultFetchEngine, "fetch", mock.Anything, mock.Anything).
		Return(assert.AnError).Once()

	_, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.ErrorContains(t, err, "fetching https://example.com/toy.tar.gz for toygo@1.0.0")
}

func TestSatisfyNamingTheProviderIsHonored(t *testing.T) {
	h := newHarness(t)

	needy := description(map[string]any{
		"prerequisites": []map[string]any{{"name": "jvm", "reason": "a jar runs on a JVM"}},
	})
	provider := map[string]any{
		"runtime": "toyjre", "version": "21",
		"artifacts": []map[string]any{
			{"url": "https://example.com/jre.tar.gz", "sha256": strings.Repeat("c", 64), "unpack": "tar-gz"},
		},
		"provides": []string{"jvm"},
	}

	h.expectDescribe(t, needy)
	h.caller.EXPECT().
		Call(mock.Anything, "forge://example.com/jre-provider", "describe", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			answer(t, out, provider)

			return nil
		}).Once()

	for _, p := range []string{"toygo@1.0.0", "toyjre@21"} {
		require.NoError(t, os.MkdirAll(filepath.Join(h.store, "runtimes", p), 0o750))
	}

	f := factoryWith(toyEngine(), config.Engine{Alias: "toyjre", Engine: "forge://example.com/jre-provider"})
	f.Toolchain = &config.Toolchain{Satisfy: map[string]string{"jvm": "toyjre"}}

	report, err := h.c.Provision(context.Background(), f, h.root, h.store, []runtimecontroller.Pin{
		{Name: "toygo", Version: "1.0.0"},
		{Name: "toyjre", Version: "21"},
	})
	require.NoError(t, err)
	require.Len(t, report.Satisfied, 1)
	assert.Contains(t, report.Satisfied[0], "(satisfy:)")
}

// satisfy: host on a prerequisite with no verify command has nothing to
// check, and saying so beats pretending.
func TestSatisfyHostWithNoVerifyIsRefused(t *testing.T) {
	h := newHarness(t)
	h.expectDescribe(t, description(map[string]any{
		"prerequisites": []map[string]any{{"name": "jvm", "reason": "a jar runs on a JVM"}},
	}))

	f := factoryWith(toyEngine())
	f.Toolchain = &config.Toolchain{Satisfy: map[string]string{"jvm": "host"}}

	_, err := h.c.Provision(context.Background(), f, h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.ErrorContains(t, err, "only a providing runtime can satisfy it")
}

// A second provision over a converged workspace reuses the store, re-links
// the same bins, and leaves PATH with one entry rather than stacking a
// second one.
func TestASecondProvisionConverges(t *testing.T) {
	h := newHarness(t)
	h.expectDescribe(t, description(nil))
	h.expectFetchAndInstall(t)

	pins := []runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}}

	_, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store, pins)
	require.NoError(t, err)

	pathAfterFirst := os.Getenv("PATH")

	h.expectDescribe(t, description(nil))

	report, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store, pins)
	require.NoError(t, err)
	assert.Equal(t, []string{"toygo@1.0.0"}, report.Reused)
	assert.Equal(t, pathAfterFirst, os.Getenv("PATH"), "the bin dir is prepended once, not stacked")
}

// With no override and no FORGE_STORE_DIR the store lives under the user
// cache - pointed at a tempdir here so the test touches nothing real.
func TestTheStoreDefaultsToTheUserCache(t *testing.T) {
	h := newHarness(t)

	cache := t.TempDir()
	t.Setenv("FORGE_STORE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", cache)

	require.NoError(t, os.MkdirAll(filepath.Join(cache, "forge", "store", "runtimes", "toygo@1.0.0"), 0o750))
	h.expectDescribe(t, description(nil))

	report, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, "",
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"toygo@1.0.0"}, report.Reused)
}

// A url with no usable base name still stages somewhere sane - the url is
// engine-answered data, not a path.
func TestAnAwkwardURLStillStages(t *testing.T) {
	h := newHarness(t)
	h.expectDescribe(t, description(map[string]any{
		"artifacts": []map[string]any{
			{"url": "https://example.com/dl:v1/", "sha256": strings.Repeat("a", 64), "unpack": "tar-gz"},
		},
	}))

	h.caller.EXPECT().
		Call(mock.Anything, runtimecontroller.DefaultFetchEngine, "fetch", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, in, out any) error {
			dest := field(t, in, "dest")
			assert.Equal(t, "0-artifact", filepath.Base(dest))
			require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o750))
			require.NoError(t, os.WriteFile(dest, []byte("a"), 0o600))
			answer(t, out, map[string]any{"path": dest, "sha256": strings.Repeat("a", 64)})

			return nil
		}).Once()
	h.caller.EXPECT().
		Call(mock.Anything, runtimecontroller.DefaultInstallEngine, "install", mock.Anything, mock.Anything).
		RunAndReturn(func(_ context.Context, _, _ string, _, out any) error {
			answer(t, out, map[string]any{"installed": []string{}})

			return nil
		}).Once()

	_, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.NoError(t, err)
}

// raceLock simulates the losing side of two concurrent provisions: by the
// time the lock is held, the winner has already landed the prefix.
type raceLock struct{ prefix string }

func (r raceLock) Lock(string) (func(), error) {
	if err := os.MkdirAll(r.prefix, 0o750); err != nil {
		return nil, err
	}

	return func() {}, nil
}

func TestTheLoserOfAStoreRaceReusesTheWinnersInstall(t *testing.T) {
	h := newHarness(t)

	prefix := filepath.Join(h.store, "runtimes", "toygo@1.0.0")
	c := runtimecontroller.New(h.caller, fsadapter.New(), raceLock{prefix: prefix})

	h.expectDescribe(t, description(nil))

	report, err := c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"toygo@1.0.0"}, report.Reused, "no fetch, no install: the winner's install stands")
}

func TestAnUnwritableBinDirFailsTheExpose(t *testing.T) {
	h := newHarness(t)

	require.NoError(t, os.MkdirAll(filepath.Join(h.store, "runtimes", "toygo@1.0.0"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(h.root, ".forge"), 0o750))
	// .forge/bin exists as a FILE, so the expose cannot create the dir.
	require.NoError(t, os.WriteFile(filepath.Join(h.root, ".forge", "bin"), []byte("x"), 0o600))

	h.expectDescribe(t, description(nil))

	_, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.ErrorContains(t, err, "exposing")
}

func TestAVersionWithHostileCharactersStoresSafely(t *testing.T) {
	h := newHarness(t)

	// The version string comes from config and lands in a directory name;
	// path-meaningful characters are flattened.
	require.NoError(t, os.MkdirAll(filepath.Join(h.store, "runtimes", "toygo@1.0-rc-2"), 0o750))
	h.expectDescribe(t, description(map[string]any{"version": "1.0@rc:2"}))

	report, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0@rc:2"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"toygo@1.0@rc:2"}, report.Reused)
}

func TestABlockedBinLinkFailsTheExpose(t *testing.T) {
	h := newHarness(t)

	require.NoError(t, os.MkdirAll(filepath.Join(h.store, "runtimes", "toygo@1.0.0"), 0o750))
	// A non-empty DIRECTORY sits where the link must go, which nothing may
	// silently delete.
	require.NoError(t, os.MkdirAll(filepath.Join(h.root, ".forge", "bin", "toygo"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(h.root, ".forge", "bin", "toygo", "keep"), []byte("x"), 0o600))

	h.expectDescribe(t, description(nil))

	_, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.ErrorContains(t, err, "exposing toygo")
}

func TestAnUnresolvableUserCacheIsAnError(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FORGE_STORE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")

	_, err := h.c.Provision(context.Background(), factoryWith(toyEngine()), h.root, "",
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.ErrorContains(t, err, "resolving the store dir")
}

type brokenLock struct{}

func (brokenLock) Lock(string) (func(), error) { return nil, assert.AnError }

func TestAnUnlockableStoreIsAnError(t *testing.T) {
	h := newHarness(t)
	c := runtimecontroller.New(h.caller, fsadapter.New(), brokenLock{})

	h.expectDescribe(t, description(nil))

	_, err := c.Provision(context.Background(), factoryWith(toyEngine()), h.root, h.store,
		[]runtimecontroller.Pin{{Name: "toygo", Version: "1.0.0"}})
	require.ErrorContains(t, err, "locking the store for toygo@1.0.0")
}
