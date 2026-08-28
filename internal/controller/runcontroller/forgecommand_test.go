package runcontroller

import (
	"io"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/alexandremahdhaoui/forge-factory/internal/mocks/execadaptermock"
)

func TestDepVersionReadsTheVersionForgeWasBuiltAgainst(t *testing.T) {
	const forge = "github.com/alexandremahdhaoui/forge"

	for name, tc := range map[string]struct {
		deps []*debug.Module
		want string
	}{
		"a released dependency": {
			[]*debug.Module{{Path: "other", Version: "v9.9.9"}, {Path: forge, Version: "v0.42.0"}},
			"v0.42.0",
		},
		// A replace wins, because the replacement is the code that is
		// actually linked in. Running the replaced version would run
		// something this binary was never built against.
		"a replaced dependency": {
			[]*debug.Module{{Path: forge, Version: "v0.42.0",
				Replace: &debug.Module{Path: forge, Version: "v0.41.0"}}},
			"v0.41.0",
		},
		// A workspace build records no usable version, and the caller must
		// keep the bare name so its failure names the missing install.
		// Pinning "(devel)" would ask the proxy for a version that is not
		// there.
		"a workspace build":    {[]*debug.Module{{Path: forge, Version: "(devel)"}}, ""},
		"no version at all":    {[]*debug.Module{{Path: forge}}, ""},
		"forge is not a dep":   {[]*debug.Module{{Path: "other", Version: "v1.0.0"}}, ""},
		"nothing at all":       {nil, ""},
		"a nil entry survives": {[]*debug.Module{nil, {Path: forge, Version: "v0.42.0"}}, "v0.42.0"},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, depVersion(tc.deps))
		})
	}
}

// A controller with nothing wired but the boundary this asks about. The
// shared rig answers LookPath already, and a second answer over the top of
// it proves only which one mockery picked.
func forgeCommandRig(t *testing.T, installed bool) *Controller {
	t.Helper()

	runner := execadaptermock.NewMockRunner(t)
	runner.EXPECT().LookPath("forge").Return("/usr/bin/forge", installed).Once()

	return New(nil, nil, runner, nil, nil, io.Discard)
}

func TestForgeCommandPrefersTheInstalledForge(t *testing.T) {
	name, args := forgeCommandRig(t, true).forgeCommand()
	require.Equal(t, "forge", name)
	require.Nil(t, args)
}

func TestForgeCommandFallsBackToAPinnedGoRun(t *testing.T) {
	name, args := forgeCommandRig(t, false).forgeCommand()

	// Whatever this test binary was built against decides which branch runs.
	// Both are correct; neither may ever be "@latest", which would make the
	// version of the tool depend on the day it ran.
	if v := forgeDepVersion(); v == "" {
		require.Equal(t, "forge", name)
		require.Nil(t, args)

		return
	}

	require.Equal(t, "go", name)
	require.Len(t, args, 2)
	require.Equal(t, "run", args[0])
	require.NotContains(t, args[1], "@latest")
}
