package gitadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/execadapter"
)

type Git interface {
	Init(ctx context.Context, dir string) error
	Clone(ctx context.Context, url, dir string) error
	IsRepo(ctx context.Context, dir string) (bool, error)
	Add(ctx context.Context, dir, path string) error
	Commit(ctx context.Context, dir, message string) error
	HeadSHA(ctx context.Context, dir string) (string, error)
	RemoteSHA(ctx context.Context, url, ref string) (string, error)
	Dirty(ctx context.Context, dir string) (bool, error)
	Checkout(ctx context.Context, dir, sha string) error
	Fetch(ctx context.Context, dir string) error
	WorktreeHash(ctx context.Context, dir string) (string, error)
}

type CLI struct {
	runner execadapter.Runner
}

var _ Git = (*CLI)(nil)

func New(runner execadapter.Runner) *CLI {
	return &CLI{runner: runner}
}

func (g *CLI) run(ctx context.Context, dir, what string, args ...string) (execadapter.Result, error) {
	res, err := g.runner.Run(ctx, dir, "git", args...)
	if err != nil {
		return res, fmt.Errorf("%s: %w", what, err)
	}

	if res.ExitCode != 0 {
		return res, fmt.Errorf("%s: git exited %d: %s", what, res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	return res, nil
}

func (g *CLI) Init(ctx context.Context, dir string) error {
	_, err := g.run(ctx, dir, "initialising "+dir, "init", "-b", "main")

	return err
}

func (g *CLI) Clone(ctx context.Context, url, dir string) error {
	_, err := g.run(ctx, "", "cloning "+url, "clone", url, dir)

	return err
}

func (g *CLI) IsRepo(ctx context.Context, dir string) (bool, error) {
	res, err := g.runner.Run(ctx, dir, "git", "rev-parse", "--git-dir")
	if err != nil {
		return false, fmt.Errorf("inspecting %s: %w", dir, err)
	}

	return res.ExitCode == 0, nil
}

func (g *CLI) Add(ctx context.Context, dir, path string) error {
	_, err := g.run(ctx, dir, "staging "+path, "add", path)

	return err
}

func (g *CLI) Commit(ctx context.Context, dir, message string) error {
	_, err := g.run(ctx, dir, "committing in "+dir, "commit", "-m", message)

	return err
}

func (g *CLI) HeadSHA(ctx context.Context, dir string) (string, error) {
	res, err := g.run(ctx, dir, "reading HEAD of "+dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(res.Stdout), nil
}

func (g *CLI) RemoteSHA(ctx context.Context, url, ref string) (string, error) {
	if ref == "" {
		ref = "main"
	}

	res, err := g.run(ctx, "", "reading "+ref+" of "+url, "ls-remote", url, "refs/heads/"+ref)
	if err != nil {
		return "", err
	}

	fields := strings.Fields(res.Stdout)
	if len(fields) == 0 {
		return "", fmt.Errorf("reading %s of %s: no such ref", ref, url)
	}

	return fields[0], nil
}

func (g *CLI) WorktreeHash(ctx context.Context, dir string) (string, error) {
	status, err := g.run(ctx, dir, "reading status of "+dir, "status", "--porcelain")
	if err != nil {
		return "", err
	}

	if strings.TrimSpace(status.Stdout) == "" {
		return "", nil
	}

	diff, err := g.run(ctx, dir, "reading uncommitted changes in "+dir, "diff", "HEAD")
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256([]byte(status.Stdout + "\n" + diff.Stdout))

	return hex.EncodeToString(sum[:])[:12], nil
}

func (g *CLI) Dirty(ctx context.Context, dir string) (bool, error) {
	res, err := g.run(ctx, dir, "reading status of "+dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}

	return strings.TrimSpace(res.Stdout) != "", nil
}

// Checkout moves a repo onto one commit. It refuses a dirty worktree, because
// a checkout over uncommitted work destroys it with no undo.
func (g *CLI) Checkout(ctx context.Context, dir, sha string) error {
	dirty, err := g.Dirty(ctx, dir)
	if err != nil {
		return err
	}

	if dirty {
		return fmt.Errorf("checking out %s in %s: the worktree has uncommitted changes", sha, dir)
	}

	_, err = g.run(ctx, dir, "checking out "+sha, "checkout", "--detach", sha)

	return err
}

func (g *CLI) Fetch(ctx context.Context, dir string) error {
	_, err := g.run(ctx, dir, "fetching", "fetch", "--all", "--tags", "--quiet")

	return err
}
