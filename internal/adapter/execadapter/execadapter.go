package execadapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type Runner interface {
	Run(ctx context.Context, dir, name string, args ...string) (Result, error)
}

type OS struct{}

var _ Runner = OS{}

func New() OS {
	return OS{}
}

func (OS) Run(ctx context.Context, dir, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()

	var stdout, stderr strings.Builder

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}

	var exitErr *exec.ExitError
	if err != nil {
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()

			return res, nil
		}

		return res, fmt.Errorf("running %s in %s: %w", name, dir, err)
	}

	return res, nil
}
