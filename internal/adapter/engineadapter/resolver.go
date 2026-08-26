package engineadapter

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	SchemeGo    = "forge://"
	SchemeAlias = "alias://"

	defaultModule = "github.com/alexandremahdhaoui/forge-factory"
)

var (
	ErrScheme = errors.New("engine must start with forge:// or alias://")
	ErrAlias  = errors.New("alias:// must be resolved before calling an engine")
	// ErrNoVersion means nothing pins the go-run fallback: latest is never
	// a fallback, so a dev build off PATH fails loud instead of floating.
	ErrNoVersion = errors.New("nothing pins a version")
)

type Command struct {
	Path string
	Args []string
}

// Resolver mirrors the toolchain's shared precedence (canonical in forge's
// pkg/toolresolver): the source tree wins, then PATH, then a pinned go run.
type Resolver struct {
	SourceDir string
	// Version pins the go-run fallback when a URI carries no @version: the
	// running binary's own effective version, so every engine matches the
	// CLI that spawned it.
	Version  string
	LookPath func(string) (string, error)
}

func NewResolver(sourceDir, version string) *Resolver {
	return &Resolver{SourceDir: sourceDir, Version: version, LookPath: exec.LookPath}
}

func (r *Resolver) Resolve(uri string) (Command, error) {
	switch {
	case strings.HasPrefix(uri, SchemeAlias):
		return Command{}, fmt.Errorf("resolving %q: %w", uri, ErrAlias)
	case !strings.HasPrefix(uri, SchemeGo):
		return Command{}, fmt.Errorf("resolving %q: %w", uri, ErrScheme)
	}

	ref := strings.TrimPrefix(uri, SchemeGo)
	if ref == "" {
		return Command{}, fmt.Errorf("resolving %q: %w", uri, ErrScheme)
	}

	module, version := splitVersion(ref)
	name := filepath.Base(module)

	// The source tree wins over an installed binary: a dev naming a source
	// dir means it, and a stale install must not shadow it.
	if r.SourceDir != "" {
		local := filepath.Join(r.SourceDir, "cmd", name)
		if info, err := os.Stat(local); err == nil && info.IsDir() {
			return Command{Path: "go", Args: []string{"run", "./cmd/" + name}}, nil
		}
	}

	if r.LookPath != nil {
		if path, err := r.LookPath(name); err == nil {
			return Command{Path: path}, nil
		}
	}

	if !strings.Contains(module, "/") {
		module = defaultModule + "/cmd/" + module
	}

	// The go-run fallback is always pinned: the URI's own version, else the
	// running binary's. Latest is never a fallback.
	if version == "" {
		version = strings.TrimSuffix(strings.TrimSuffix(r.Version, "-dirty"), "+dirty")
	}

	if version == "" || version == "dev" {
		return Command{}, fmt.Errorf(
			"resolving %q: %w (dev build, not on PATH, no source dir); install %s or name an @version",
			uri, ErrNoVersion, name)
	}

	return Command{Path: "go", Args: []string{"run", module + "@" + version}}, nil
}

func splitVersion(ref string) (string, string) {
	at := strings.LastIndex(ref, "@")
	if at <= 0 {
		return ref, ""
	}

	return ref[:at], ref[at+1:]
}
