package resolvecontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/alexandremahdhaoui/forge-factory/pkg/config"
)

// ResolveMembers answers the version of every workspace member module the
// register carries an internal track for.
//
// A member's generated manifest names only the dependencies the factory
// declares. A shared spec that one member imports from another is declared
// nowhere, so its version was left to the tidy that follows, which resolves
// an undeclared import to the highest published tag. That is the same
// "whatever is latest today" this toolchain exists to remove, and it was
// already drifting quietly: forge-ci carried forge-revision-spec at v0.3.1
// while the register's track said v0.3.0 - a version nothing ever adopted.
//
// The internal track is the declared source of truth for an internal
// module's version. It is what `forge-register publish internal:` writes and
// what a remote run pins against. This makes the manifest follow it too.
//
// The caller decides which of these to write. Every runnable member has an
// internal track as well, carrying the pipeline's own dev label - a real
// revision, but not a tag any proxy can fetch - so writing them all would
// break the tidy rather than govern it.
func (c *Controller) ResolveMembers(
	ctx context.Context, f config.Factory, root, language string,
) (map[string]string, error) {
	out := map[string]string{}

	if f.Register == nil {
		return out, nil
	}

	// The internal ecosystem is Go-shaped: a module path the go command
	// resolves. Another language's members are named differently and have
	// no tracks here.
	if language != "go" {
		return out, nil
	}

	dir, err := c.registerDir(f, root)
	if err != nil {
		return nil, err
	}

	v := view{c: c, ctx: ctx, dir: dir, rev: f.Register.Revision, url: f.Register.URL}

	// A register with no internal ecosystem is the first-run shape, and a
	// workspace whose members share nothing has no tracks to read. Neither
	// is a failure, so a listing that answers nothing answers nothing.
	for _, module := range internalModules(v) {
		version, err := currentOfHighestTrack(v, module)
		if err != nil {
			return nil, err
		}

		if version != "" {
			out[module] = version
		}
	}

	return out, nil
}

// internalModules answers the module paths under index/internal. The tree is
// nested by path segment, so a module is the directory that holds the track
// files rather than more directories.
func internalModules(v view) []string {
	entries := listed(v, "index/internal")

	seen := map[string]bool{}

	// A git rev lists recursively and answers relative slash paths, so the
	// module is everything above the track file.
	for _, entry := range entries {
		if !strings.HasSuffix(entry, ".json") {
			continue
		}

		if module := path.Dir(entry); module != "." {
			seen[module] = true
		}
	}

	// A git rev lists recursively and is done. A worktree read answers one
	// level, so descend.
	if len(seen) == 0 {
		for _, entry := range entries {
			for _, module := range walkInternal(v, path.Join("index/internal", entry), entry) {
				seen[module] = true
			}
		}
	}

	return sortedModules(seen)
}

// listed answers a directory's entries, or nothing. A path that is not there
// and a path that cannot be read are the same answer here: no tracks to
// pin. A track file that exists and will not parse is a different thing and
// is reported.
func listed(v view, rel string) []string {
	entries, err := v.list(rel)
	if err != nil {
		return nil
	}

	return entries
}

// walkInternal descends until it reaches track files, answering the module
// paths it found. Bounded by the tree's own depth.
func walkInternal(v view, rel, module string) []string {
	var out []string

	for _, entry := range listed(v, rel) {
		if strings.HasSuffix(entry, ".json") {
			return []string{module}
		}

		out = append(out, walkInternal(v, path.Join(rel, entry), path.Join(module, entry))...)
	}

	return out
}

// currentOfHighestTrack reads the highest track's current version.
//
// A module with no track file, and a track file the reader cannot find,
// both answer empty: neither is a corrupt register, and a first run meets
// both. A track file that is not JSON is different and fails the sync,
// because a register nobody can parse is not a register a version should be
// resolved from. TestAMangledInternalTrackIsNamed pins that.
func currentOfHighestTrack(v view, module string) (string, error) {
	rel := path.Join("index/internal", module)

	entries := listed(v, rel)
	tracks := make([]string, 0, len(entries))

	for _, entry := range entries {
		if strings.HasSuffix(entry, ".json") {
			tracks = append(tracks, path.Base(entry))
		}
	}

	if len(tracks) == 0 {
		return "", nil
	}

	sort.Slice(tracks, func(i, j int) bool {
		return compareVersions(
			strings.TrimSuffix(tracks[i], ".json"),
			strings.TrimSuffix(tracks[j], ".json")) < 0
	})

	raw, found, err := v.read(path.Join(rel, tracks[len(tracks)-1]))
	if err != nil || !found {
		return "", err
	}

	var track struct {
		Current string `json:"current"`
	}

	if err := json.Unmarshal([]byte(raw), &track); err != nil {
		return "", fmt.Errorf("decoding the internal track of %s: %w", module, err)
	}

	return track.Current, nil
}

func sortedModules(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
