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

## Building it

```sh
forge test-all
```

Not `go test`. Four stages do not run under cargo or go and have caught real
breakage.
