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
	SchemeGo    = "go://"
	SchemeAlias = "alias://"

	defaultModule = "github.com/alexandremahdhaoui/forge-factory"
)

var (
	ErrScheme = errors.New("engine must start with go:// or alias://")
	ErrAlias  = errors.New("alias:// must be resolved before calling an engine")
)

type Command struct {
	Path string
	Args []string
}

type Resolver struct {
	SourceDir string
	LookPath  func(string) (string, error)
}

func NewResolver(sourceDir string) *Resolver {
	return &Resolver{SourceDir: sourceDir, LookPath: exec.LookPath}
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

	if r.LookPath != nil {
		if path, err := r.LookPath(name); err == nil {
			return Command{Path: path}, nil
		}
	}

	if r.SourceDir != "" {
		local := filepath.Join(r.SourceDir, "cmd", name)
		if info, err := os.Stat(local); err == nil && info.IsDir() {
			return Command{Path: "go", Args: []string{"run", "./cmd/" + name}}, nil
		}
	}

	if !strings.Contains(module, "/") {
		module = defaultModule + "/cmd/" + module
	}

	if version == "" {
		version = "latest"
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
