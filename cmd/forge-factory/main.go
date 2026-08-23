package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/engineadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/gitadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/repoadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/clonecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/resolvecontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/revisioncontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/runcontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/statuscontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/synccontroller"
	"github.com/alexandremahdhaoui/forge-factory/internal/driver/clidriver"
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
	fs := fsadapter.New()
	git := gitadapter.New(execadapter.New())
	caller := engineadapter.NewMCPCaller(".", version, os.Stderr)

	sync := synccontroller.New(caller, fs, repoadapter.New(fs), execadapter.New(),
		resolvecontroller.New(fs, git, time.Now))

	driver := clidriver.New(
		os.Stdout,
		fs,
		clonecontroller.New(fs, git),
		sync,
		revisioncontroller.New(caller, git),
		statuscontroller.New(fs, git),
		runcontroller.New(fs, git, execadapter.New(), sync, os.Stderr),
		os.Exit,
	)

	return driver.Run(ctx, args)
}
