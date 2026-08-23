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
	RunEnv(ctx context.Context, dir string, env map[string]string, name string, args ...string) (Result, error)
	// RunAttached wires the command to the caller's stdio and answers its
	// exit code. It is how a run hands the terminal to the program.
	RunAttached(ctx context.Context, dir string, env map[string]string, name string, args ...string) (int, error)
}

type OS struct{}

var _ Runner = OS{}

func New() OS {
	return OS{}
}

func (o OS) Run(ctx context.Context, dir, name string, args ...string) (Result, error) {
	return o.RunEnv(ctx, dir, nil, name, args...)
}

// RunEnv adds to the inherited environment rather than replacing it, so a
// command still finds its toolchain on PATH.
func (OS) RunEnv(
	ctx context.Context,
	dir string,
	env map[string]string,
	name string,
	args ...string,
) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()

	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}

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

func (OS) RunAttached(
	ctx context.Context,
	dir string,
	env map[string]string,
	name string,
	args ...string,
) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()

	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err == nil {
		return 0, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}

	return 0, fmt.Errorf("running %s: %w", name, err)
}
