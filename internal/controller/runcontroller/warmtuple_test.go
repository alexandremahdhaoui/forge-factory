// Copyright 2024 Alexandre Mahdhaoui
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	fullShaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fullShaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// TestWarmTupleKeyOnlyNamesFullyPinnedRequests: anything floating - a
// branch, a tag, a factory with no rev, the internal track - answers ""
// and keeps resolving.
func TestWarmTupleKeyOnlyNamesFullyPinnedRequests(t *testing.T) {
	t.Parallel()

	pinned := Request{
		Target:  "github.com/x/repo@" + fullShaA,
		Name:    "server",
		Factory: "git@github.com:x/factory.git@" + fullShaB,
	}
	if warmTupleKey(pinned) == "" {
		t.Error("a fully pinned request must name a tuple")
	}

	floats := []Request{
		{Target: "github.com/x/repo", Factory: "git@github.com:x/factory.git@" + fullShaB},
		{Target: "github.com/x/repo@v1.2.0", Factory: "git@github.com:x/factory.git@" + fullShaB},
		{Target: "github.com/x/repo@main", Factory: "git@github.com:x/factory.git@" + fullShaB},
		{Target: "github.com/x/repo@" + fullShaA, Factory: "git@github.com:x/factory.git"},
		{Target: "github.com/x/repo@" + fullShaA, Factory: ""},
		{Target: "github.com/x/repo@" + fullShaA, Factory: "git@github.com:x/factory.git@v0.2.0"},
	}
	for _, req := range floats {
		if key := warmTupleKey(req); key != "" {
			t.Errorf("a floating request must keep resolving, got key %q for %+v", key, req)
		}
	}
}

func TestWarmTupleKeySeparatesRunnables(t *testing.T) {
	t.Parallel()

	a := Request{Target: "github.com/x/repo@" + fullShaA, Name: "server", Factory: "git@github.com:x/f.git@" + fullShaB}
	b := Request{Target: "github.com/x/repo@" + fullShaA, Name: "client", Factory: "git@github.com:x/f.git@" + fullShaB}

	if warmTupleKey(a) == warmTupleKey(b) {
		t.Error("two runnables of one repo must not share a tuple")
	}
}

// TestAWarmTupleEntersAtTheInputsStep: with the marker and the context in
// place, the run execs with no git call at all - the mock git would fail
// the test on any unexpected use.
func TestAWarmTupleEntersAtTheInputsStep(t *testing.T) {
	r := newRig(t)
	req := Request{
		Target:   "github.com/x/repo@" + fullShaA,
		Name:     "server",
		Factory:  "git@github.com:x/factory.git@" + fullShaB,
		CacheDir: filepath.Join(t.TempDir(), "cache"),
		Args:     []string{"--flag"},
	}

	mark := warmTuple{RepoDir: "/cache/run/tuple/repo"}
	mark.Target.Name = "server"
	mark.Target.Src = "."

	raw, err := json.Marshal(mark)
	require.NoError(t, err)

	r.fs.files[warmTuplePath(req.CacheDir, warmTupleKey(req))] = string(raw)
	r.fs.dirs["/cache/run/tuple/repo"] = true

	r.exec.EXPECT().
		RunAttached(context.Background(), "/cache/run/tuple/repo",
			map[string]string{"FORGE_RUN_MATERIALIZED": "1"}, "forge", "run", "server", "--", "--flag").
		Return(0, nil)

	code, err := r.c.Run(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, 0, code)

	if !strings.Contains(r.out.String(), "warm tuple, entering at inputs") {
		t.Errorf("the entry must say what it skipped, got %q", r.out.String())
	}
}

// TestAMissingWarmMarkerFallsThroughToResolution: no marker means the
// normal path, which here fails at the clone because the mock git raises.
func TestAMissingWarmMarkerFallsThroughToResolution(t *testing.T) {
	r := newRig(t)
	req := Request{
		Target:   "github.com/x/repo@" + fullShaA,
		Name:     "server",
		Factory:  "git@github.com:x/factory.git@" + fullShaB,
		CacheDir: t.TempDir(),
	}

	// The clone step is the first git touch of a cold run.
	r.git.EXPECT().Clone(mock.Anything, "git@github.com:x/repo.git", mock.Anything).
		Return(errors.New("no network in this test"))

	_, err := r.c.Run(context.Background(), req)
	require.ErrorContains(t, err, "no network in this test")
}

// TestACorruptOrHomelessMarkerFallsThrough: a bad marker or a vanished
// context is a miss, never an error - the marker is convenience, not
// authority.
func TestACorruptOrHomelessMarkerFallsThrough(t *testing.T) {
	for name, mutate := range map[string]func(r *rig, path string){
		"corrupt json": func(r *rig, path string) {
			r.fs.files[path] = "not json {"
		},
		"context gone": func(r *rig, path string) {
			mark := warmTuple{RepoDir: "/cache/run/tuple/repo"}
			raw, _ := json.Marshal(mark)
			r.fs.files[path] = string(raw)
			// No dir recorded: the worktree was pruned.
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			req := Request{
				Target:   "github.com/x/repo@" + fullShaA,
				Name:     "server",
				Factory:  "git@github.com:x/factory.git@" + fullShaB,
				CacheDir: filepath.Join(t.TempDir(), "cache"),
			}

			mutate(r, warmTuplePath(req.CacheDir, warmTupleKey(req)))

			r.git.EXPECT().Clone(mock.Anything, "git@github.com:x/repo.git", mock.Anything).
				Return(errors.New("cold path reached"))

			_, err := r.c.Run(context.Background(), req)
			require.ErrorContains(t, err, "cold path reached")
		})
	}
}

// TestAWarmTupleStillChecksItsInputs: the early entry skips resolution,
// never the inputs gate.
func TestAWarmTupleStillChecksItsInputs(t *testing.T) {
	r := newRig(t)
	req := Request{
		Target:   "github.com/x/repo@" + fullShaA,
		Name:     "server",
		Factory:  "git@github.com:x/factory.git@" + fullShaB,
		CacheDir: filepath.Join(t.TempDir(), "cache"),
	}

	mark := warmTuple{RepoDir: "/cache/run/tuple/repo"}
	mark.Target.Name = "server"
	mark.Target.Src = "."

	raw, err := json.Marshal(mark)
	require.NoError(t, err)

	r.fs.files[warmTuplePath(req.CacheDir, warmTupleKey(req))] = string(raw)
	r.fs.dirs["/cache/run/tuple/repo"] = true
	r.fs.files["/cache/run/tuple/repo/zz_generated.runnable.yaml"] = "inputs:\n  env:\n    - SURELY_UNSET_VARIABLE_XYZ\n"

	_, err = r.c.Run(context.Background(), req)
	require.ErrorIs(t, err, ErrMissingInput)
}

// TestAForcedRunIgnoresTheWarmTuple: --force means re-materialise, so the
// marker must not short-circuit it.
func TestAForcedRunIgnoresTheWarmTuple(t *testing.T) {
	r := newRig(t)
	req := Request{
		Target:   "github.com/x/repo@" + fullShaA,
		Name:     "server",
		Factory:  "git@github.com:x/factory.git@" + fullShaB,
		CacheDir: filepath.Join(t.TempDir(), "cache"),
		Force:    true,
	}

	mark := warmTuple{RepoDir: "/cache/run/tuple/repo"}
	raw, _ := json.Marshal(mark)
	r.fs.files[warmTuplePath(req.CacheDir, warmTupleKey(req))] = string(raw)
	r.fs.dirs["/cache/run/tuple/repo"] = true

	r.git.EXPECT().Clone(mock.Anything, "git@github.com:x/repo.git", mock.Anything).
		Return(errors.New("cold path reached"))

	_, err := r.c.Run(context.Background(), req)
	require.ErrorContains(t, err, "cold path reached")
}

// TestWriteWarmTupleRoundTrips: what a successful materialisation writes,
// the next run reads.
func TestWriteWarmTupleRoundTrips(t *testing.T) {
	r := newRig(t)
	cacheDir := t.TempDir()

	target := runnable{Name: "server", Src: "./cmd/server"}
	r.c.writeWarmTuple(cacheDir, "some-key", "/ctx/repo", target)

	raw, err := r.fs.ReadFile(warmTuplePath(cacheDir, "some-key"))
	require.NoError(t, err)

	var mark warmTuple
	require.NoError(t, json.Unmarshal(raw, &mark))
	require.Equal(t, "/ctx/repo", mark.RepoDir)
	require.Equal(t, "server", mark.Target.Name)
	require.Equal(t, "./cmd/server", mark.Target.Src)
}

// The hint decorates only failures inside a register-resolved context:
// a tuple the caller pinned is the caller's to debug.
func TestStaleTupleHintDecoratesOnlyRegisterResolvedRuns(t *testing.T) {
	t.Parallel()

	require.NoError(t, staleTupleHint(nil, "m", "v1"))
	require.NoError(t, staleTupleHint(nil, "m", ""))

	base := errors.New("resolving go dependencies: boom")
	require.Same(t, base, staleTupleHint(base, "m", ""))

	hinted := staleTupleHint(base, "github.com/o/m", "v0.1.0-dev.r1.gabc")
	require.ErrorIs(t, hinted, base)
	require.ErrorContains(t, hinted, "the tuple the register last proved")
	require.ErrorContains(t, hinted, "github.com/o/m at v0.1.0-dev.r1.gabc")
	require.ErrorContains(t, hinted, "forge-register status")
}
