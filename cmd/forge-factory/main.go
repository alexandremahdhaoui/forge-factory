package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/engineadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/gitadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/repoadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/controller/revisioncontroller"
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

	driver := clidriver.New(
		os.Stdout,
		fs,
		synccontroller.New(caller, fs, repoadapter.New(fs)),
		revisioncontroller.New(caller, git),
		statuscontroller.New(fs, git),
	)

	return driver.Run(ctx, args)
}
