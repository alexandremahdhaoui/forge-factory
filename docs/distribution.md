# The tooling distribution

How a workspace gets its toolchain binaries: proven, pinned, verified, and
the same way everywhere - a laptop, a CI runner, an airgapped mirror.

## The model in one sentence

The register's internal track is the version authority; the pipeline
packages **one aggregated release per revision** (every cross-built binary
plus `index.json` pinning each binary's measured sha256); `forge-factory
sync` consumes it into a content-addressed store; and one shared precedence
rule decides every exec - never `@latest`.

## The store

```
~/.cache/forge/store/            (FORGE_STORE_DIR overrides)
  blobs/sha256/<hex>             immutable binaries, written once
  rev/<revision>/<os>-<arch>/bin symlinks into blobs, one view per revision
  rev/<revision>/index.json      the index that produced the view
```

Per workspace, sync links `<root>/.forge/bin` to the pinned view and keeps
one managed line in every member's `.envrc` putting it first on PATH. Two
workspaces on different revisions coexist; `~/go/bin` is never touched, so
nothing the store does can conflict with what a user installed - the pinned
view simply outranks it.

## Producing a distribution

The pipeline's release stage (forge-ci's `ci-artifact-release`) runs after
a revision is proven:

- every member is tagged with the next semver at the revision's sha;
- ONE release tagged `dist-<revision>` is created in the `releaseIn` repo,
  carrying every binary the runs built (plus `spec.assets` globs - the
  door for cross-built files) and `index.json`;
- every digest in the index is measured on the actual uploaded bytes.

Members cross-build through their `dist` test stage (`hack/build-dist.sh`):
`CGO_ENABLED=0`, `-trimpath -ldflags "-s -w"`, linux amd64+arm64, named by
the `<name>_<os>_<arch>` travel convention. The pipeline hands every target
`FORGE_CI_REVISION`, which the script stamps into the binaries - a released
binary knows the distribution it shipped with.

## Consuming one

```sh
forge-factory sync --tooling-from <dir-or-url>     # explicit
FORGE_DIST_MIRROR=<dir-or-url> forge-factory sync  # environment form
```

The source is anywhere the release's assets are reachable: the release's
own download URL
(`https://github.com/<owner>/<repo>/releases/latest/download`), a directory
someone copied them into, or an internal mirror. Every byte is verified
against the index before it lands; a tampered asset fails the whole apply
with nothing written. A warm store downloads nothing.

## Airgap

There is no bundle format because the release IS the bundle: mirror its
assets (`index.json` + binaries) to a directory by any means - a proxy, a
USB stick - and point `--tooling-from` at it. Identical verification,
identical store, zero network.

## CI

Today a workspace's `toolchainScript` compiles the toolchain from the
checkout and puts `$HOME/go/bin` on `$GITHUB_PATH`. That is what
`golden-register/forge-ci.yaml` does and what every rendered workflow runs.

Once a factory publishes an aggregated dist release, the same step becomes
a consumption of prebuilt, digest-verified binaries:

```sh
export FORGE_DIST_MIRROR=https://github.com/<owner>/<factory>/releases/latest/download
forge-factory sync --config ../forge-factory.yaml --root ..
echo "$(cd .. && pwd)/.forge/bin" >> "$GITHUB_PATH"
```

with `actions/cache` on `~/.cache/forge/store` so a warm runner installs
the toolchain without compiling or downloading anything, and a seed of
exactly one pinned binary: `go run …/forge-factory@<proven sha>`. The
consuming half is built and tested; nothing renders that step yet, and
`FORGE_DIST_MIRROR` has no producer. `golden-register/forge-ci.yaml`
carries the block commented out, waiting on dist-release being activated
in forge-self-factory's pipeline.

Until then this section describes a destination, not the current step.

## Resolution precedence

Everywhere the toolchain execs a companion (forge <-> forge-factory,
forge-ci's engines), one rule applies, canonical in forge's
`pkg/toolresolver`:

1. an explicit user override,
2. the workspace checkout - dev always wins,
3. the pinned store view,
4. PATH - only when nothing pins,
5. `go run module@pinnedVersion` - never `@latest`.

## The toolchain section

Third-party generators and linters - or a user's own engines - join the
same store through `forge-factory.yaml`:

```yaml
toolchain:
  binaries:
    - name: mockery
      module: github.com/vektra/mockery/v3
      track: go:github.com/vektra/mockery/v3   # register-governed
    - name: my-engine
      module: github.com/me/my-engines/cmd/my-engine
      version: v1.4.0                           # literal pin, register-less
```

Exactly one of `track` | `version` pins each entry: a track resolves from
the register's index under the same advisory and deprecation rules as
every dependency (a missing package files an admission request), a literal
version serves a workspace without a register. sync builds each pinned
(module, version) once - `go install` into the store, content-addressed
like a distributed binary - links it into `.forge/bin`, and reuses it
forever after. This is the one governed place tool versions live, instead
of an env var here and a code default there.
