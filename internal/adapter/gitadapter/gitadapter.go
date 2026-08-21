package gitadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
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
	LatestTag(ctx context.Context, dir string) (string, error)
	Fetch(ctx context.Context, dir string) error
	WorktreeHash(ctx context.Context, dir string) (string, error)
	Show(ctx context.Context, dir, rev, path string) (string, bool, error)
	LsTree(ctx context.Context, dir, rev, path string) ([]string, error)
	AheadBehind(ctx context.Context, dir, ref string) (int, int, error)
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

// LatestTag is the highest semver tag a checkout carries. A repo with no tag
// answers empty rather than failing, because most members never carry one.
// Show reads one file as it exists at a revision. A path the revision does
// not carry is found false, never an error - the same shape the state
// transport gives a missing record.
func (g *CLI) Show(ctx context.Context, dir, rev, path string) (string, bool, error) {
	res, err := g.runner.Run(ctx, dir, "git", "show", rev+":"+path)
	if err != nil {
		return "", false, fmt.Errorf("reading %s at %s: %w", path, rev, err)
	}

	if res.ExitCode != 0 {
		stderr := strings.ToLower(res.Stderr)
		if strings.Contains(stderr, "does not exist") ||
			strings.Contains(stderr, "exists on disk, but not in") {
			return "", false, nil
		}

		return "", false, fmt.Errorf("reading %s at %s: git exited %d: %s",
			path, rev, res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	return res.Stdout, true, nil
}

// LsTree lists the entry names under one directory as it exists at a
// revision. A directory the revision does not carry lists to nothing.
func (g *CLI) LsTree(ctx context.Context, dir, rev, path string) ([]string, error) {
	res, err := g.runner.Run(ctx, dir, "git", "ls-tree", "--name-only", rev, path+"/")
	if err != nil {
		return nil, fmt.Errorf("listing %s at %s: %w", path, rev, err)
	}

	if res.ExitCode != 0 {
		return nil, fmt.Errorf("listing %s at %s: git exited %d: %s",
			path, rev, res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	var names []string

	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if i := strings.LastIndex(line, "/"); i >= 0 {
			line = line[i+1:]
		}

		names = append(names, line)
	}

	return names, nil
}

// AheadBehind counts the commits a checkout is ahead of and behind a ref.
func (g *CLI) AheadBehind(ctx context.Context, dir, ref string) (int, int, error) {
	res, err := g.run(ctx, dir, "counting divergence",
		"rev-list", "--left-right", "--count", "HEAD..."+ref)
	if err != nil {
		return 0, 0, err
	}

	fields := strings.Fields(res.Stdout)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("counting divergence against %s: unexpected output %q", ref, res.Stdout)
	}

	ahead, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("counting divergence against %s: %w", ref, err)
	}

	behind, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("counting divergence against %s: %w", ref, err)
	}

	return ahead, behind, nil
}

func (g *CLI) LatestTag(ctx context.Context, dir string) (string, error) {
	res, err := g.run(ctx, dir, "listing tags", "tag", "--sort=-v:refname")
	if err != nil {
		return "", err
	}

	for _, line := range strings.Split(res.Stdout, "\n") {
		if tag := strings.TrimSpace(line); tag != "" {
			return tag, nil
		}
	}

	return "", nil
}
