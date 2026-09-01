// Package installcontroller materializes fetched archives into one prefix.
// It is the INSTALLED arrow of the runtime lifecycle, and its contract is
// containment: everything it writes lands under the prefix it was given,
// and a path that would escape refuses the whole install.
//
// It understands exactly the formats the pinned distributions publish -
// tar, tar-gz, zip and a bare file - with strip and picks applied as the
// description declares. Nothing here knows what a runtime is.
package installcontroller

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/alexandremahdhaoui/forge-factory/internal/types/runtimetypes"
)

// ErrEscape means an archive entry or a pick would write outside the
// prefix. That is an attack shape, not a layout choice.
var ErrEscape = errors.New("a path escapes the install prefix")

// Fetched pairs an artifact with the local path its verified bytes sit at.
type Fetched struct {
	Artifact runtimetypes.Artifact
	Path     string
}

type Controller struct{}

func New() *Controller { return &Controller{} }

// Install lays every archive out under prefix and answers the top-level
// entries it created, sorted-free and prefix-relative for the caller to
// report.
func (c *Controller) Install(archives []Fetched, prefix string) ([]string, error) {
	cleanPrefix, err := filepath.Abs(prefix)
	if err != nil {
		return nil, fmt.Errorf("resolving the prefix: %w", err)
	}

	installed := []string{}

	for _, a := range archives {
		laid, err := c.one(a, cleanPrefix)
		if err != nil {
			return nil, fmt.Errorf("installing %s: %w", path.Base(a.Artifact.URL), err)
		}

		installed = append(installed, laid...)
	}

	return installed, nil
}

func (c *Controller) one(a Fetched, prefix string) ([]string, error) {
	switch a.Artifact.Unpack {
	case "file":
		return c.file(a, prefix)
	case "zip":
		return c.zip(a, prefix)
	case "tar", "tar-gz":
		return c.tar(a, prefix)
	default:
		return nil, fmt.Errorf("unpack kind %q is not one this engine lays out (tar, tar-gz, zip, file)", a.Artifact.Unpack)
	}
}

// file writes a single executable. An at-only pick renames it; otherwise it
// keeps the url's base name.
func (c *Controller) file(a Fetched, prefix string) ([]string, error) {
	name := path.Base(a.Artifact.URL)
	if len(a.Artifact.Picks) == 1 && a.Artifact.Picks[0].From == "" && a.Artifact.Picks[0].At != "" {
		name = a.Artifact.Picks[0].At
	}

	dest, err := contained(prefix, name)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(a.Path)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, err
	}

	if err := os.WriteFile(dest, data, 0o755); err != nil {
		return nil, err
	}

	return []string{name}, nil
}

func (c *Controller) zip(a Fetched, prefix string) ([]string, error) {
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return nil, err
	}

	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("opening the zip: %w", err)
	}

	laid := map[string]bool{}

	for _, f := range reader.File {
		rel, ok := route(f.Name, a.Artifact)
		if !ok {
			continue
		}

		dest, err := contained(prefix, rel)
		if err != nil {
			return nil, err
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return nil, err
			}

			continue
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, err
		}

		src, err := f.Open()
		if err != nil {
			return nil, err
		}

		body, err := io.ReadAll(src)
		_ = src.Close()

		if err != nil {
			return nil, err
		}

		if err := os.WriteFile(dest, body, f.Mode().Perm()|0o444); err != nil {
			return nil, err
		}

		laid[top(rel)] = true
	}

	return keys(laid), nil
}

func (c *Controller) tar(a Fetched, prefix string) ([]string, error) {
	f, err := os.Open(a.Path)
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	var stream io.Reader = f

	if a.Artifact.Unpack == "tar-gz" {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("opening the gzip stream: %w", err)
		}

		defer func() { _ = gz.Close() }()

		stream = gz
	}

	reader := tar.NewReader(stream)
	laid := map[string]bool{}

	for {
		hdr, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("reading the tar: %w", err)
		}

		rel, ok := route(hdr.Name, a.Artifact)
		if !ok {
			continue
		}

		dest, err := contained(prefix, rel)
		if err != nil {
			return nil, err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return nil, err
			}

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return nil, err
			}

			body, err := io.ReadAll(reader)
			if err != nil {
				return nil, err
			}

			mode := os.FileMode(hdr.Mode).Perm() | 0o444
			if err := os.WriteFile(dest, body, mode); err != nil {
				return nil, err
			}

		case tar.TypeSymlink:
			// A link target is resolved relative to the link, so an
			// absolute target or one climbing out of the prefix escapes
			// containment exactly like a path would. The judgement is on
			// where the target RESOLVES, not on its shape: the JRE ships
			// legal/jdk.localedata/LICENSE -> ../java.base/LICENSE, which
			// climbs one directory and stays squarely inside the prefix.
			target := hdr.Linkname
			if filepath.IsAbs(target) {
				return nil, fmt.Errorf("%w: symlink %s -> %s", ErrEscape, rel, target)
			}

			resolved := filepath.Clean(filepath.Join(filepath.Dir(dest), filepath.FromSlash(target)))
			if resolved != prefix && !strings.HasPrefix(resolved, prefix+string(filepath.Separator)) {
				return nil, fmt.Errorf("%w: symlink %s -> %s", ErrEscape, rel, target)
			}

			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return nil, err
			}

			_ = os.Remove(dest)

			if err := os.Symlink(target, dest); err != nil {
				return nil, err
			}

		case tar.TypeXGlobalHeader:
			continue

		default:
			return nil, fmt.Errorf("entry %s has type %d, which this engine does not lay out", rel, hdr.Typeflag)
		}

		laid[top(rel)] = true
	}

	return keys(laid), nil
}

// route strips and picks one entry path, answering where it lands relative
// to the prefix and whether it lands at all.
func route(name string, a runtimetypes.Artifact) (string, bool) {
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	if clean == "." || clean == "/" {
		return "", false
	}

	parts := strings.Split(clean, "/")
	if len(parts) <= a.Strip {
		return "", false
	}

	rel := path.Join(parts[a.Strip:]...)

	if len(a.Picks) == 0 {
		return rel, true
	}

	for _, p := range a.Picks {
		if p.From == "" {
			continue
		}

		if rel == p.From {
			if p.At == "" {
				return ".", true
			}

			return p.At, true
		}

		if strings.HasPrefix(rel, p.From+"/") {
			return path.Join(p.At, strings.TrimPrefix(rel, p.From+"/")), true
		}
	}

	return "", false
}

// contained joins rel under prefix and refuses anything that climbs out.
func contained(prefix, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrEscape, rel)
	}

	return filepath.Join(prefix, clean), nil
}

func top(rel string) string {
	if i := strings.IndexByte(rel, '/'); i > 0 {
		return rel[:i]
	}

	return rel
}

func keys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}

	return out
}
