package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/engineadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/gitadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/lockadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/repoadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/clonecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/resolvecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/revisioncontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/runcontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/runtimecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/statuscontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/synccontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/toolingcontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/driver/clidriver"
	"github.com/alexandremahdhaoui/forge/pkg/engineversion"
)

var version = "dev"

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		if errors.Is(err, clidriver.ErrUsage) {
			fmt.Fprint(os.Stderr, clidriver.Usage())
			os.Exit(2)
		}

		fmt.Fprintf(os.Stderr, "forge-factory: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	// The enclosing workspace's pinned tooling joins PATH for this whole
	// process tree, so engines and the forge boundary resolve provisioned
	// binaries with no .envrc sourced.
	if wd, err := os.Getwd(); err == nil {
		ensureWorkspaceBin(wd)
	}

	fs := fsadapter.New()
	git := gitadapter.New(execadapter.New())
	// The effective version pins engine go-run fallbacks: a go-install build
	// carries its module version in build info even when ldflags stamped
	// nothing, so every engine matches the CLI that spawned it.
	caller := engineadapter.NewMCPCaller(".", engineversion.GetEffectiveVersion(version), os.Stderr)

	sync := synccontroller.New(caller, fs, repoadapter.New(fs), execadapter.New(),
		// The reachability engine is wired here, in the composition root,
		// and nowhere else. It enriches a failing resolution with whether
		// the vulnerable code is in the build at all; when it is not
		// provisioned the error simply carries one line fewer.
		resolvecontroller.New(fs, git, time.Now,
			resolvecontroller.WithReachability(resolvecontroller.RunVulncheckEngine)))

	driver := clidriver.New(
		os.Stdout,
		fs,
		clonecontroller.New(fs, git),
		sync,
		revisioncontroller.New(caller, fs, git),
		statuscontroller.New(fs, git),
		runcontroller.New(fs, git, execadapter.New(), sync, lockadapter.New(), os.Stderr),
		toolingcontroller.New(fs, execadapter.New(), lockadapter.New()),
		runtimecontroller.New(caller, fs, lockadapter.New()),
		os.Exit,
	)

	return driver.Run(ctx, args)
}

// ensureWorkspaceBin puts the enclosing workspace's pinned tooling on this
// process's PATH: it walks up from wd to the first directory carrying
// forge-factory.yaml alongside a .forge/bin directory and prepends that bin
// dir once. Aligned copy of forge's toolresolver.EnsureWorkspaceBin until
// forge tags a release carrying the package - see FOLLOWUP.
func ensureWorkspaceBin(wd string) {
	dir := wd

	for {
		factory := filepath.Join(dir, "forge-factory.yaml")
		bin := filepath.Join(dir, ".forge", "bin")

		if info, err := os.Stat(factory); err == nil && !info.IsDir() {
			if binInfo, err := os.Stat(bin); err == nil && binInfo.IsDir() {
				current := os.Getenv("PATH")

				for _, entry := range filepath.SplitList(current) {
					if entry == bin {
						return
					}
				}

				_ = os.Setenv("PATH", bin+string(os.PathListSeparator)+current)

				return
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}

		dir = parent
	}
}
