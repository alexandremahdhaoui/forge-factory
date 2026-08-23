# forge-factory

One file says what a workspace is made of and what version everything agrees on.
Every other list is generated from it.

Before this, membership was declared five times and none of the five agreed, and
bumping a shared dependency cost a pull request in every repo.

```sh
forge-factory sync
forge-factory bump go:github.com/stretchr/testify v1.12.0
forge-factory status
```

`docs/concepts.md` is the reference and `docs/decisions.md` holds the closed
questions. Both are generated from yaml. This file is the only hand written one.

## Running things

forge-factory materialises the dependency context a runnable needs and
hands execution to forge with one exec of `forge run <name>` inside it.
The context rules, ordered, first match wins, one stderr line names the
rule:

1. `--factory url[@rev]` overrides everything.
2. The enclosing workspace, when its factory claims the repo.
3. The runnable's own `factory:`. Mandatory, so nothing falls through.

A remote run resolves the version from the factory's register internal
track and pins every sha through the proving revision's record. The
factory's repos list is the trust boundary in every mode. The cache keeps
one full clone per repo URL under `~/.cache/forge-factory/git/` and one
worktree per resolved tuple under `~/.cache/forge-factory/run/`; nothing
is ever shallow, `--force` refreshes a tuple. A dev `@rev` runs visibly
UNPROVEN. `forge-factory bootstrap <factory-url> [dir]` places the
workspace files so `forge clone` stands a workspace up from nothing.

## Building it

```sh
forge test-all
```

Not `go test`. Four stages do not run under cargo or go and have caught real
breakage.
