#!/bin/sh
set -eu

# Every exported function must have a caller in production code. A ported
# function nobody calls still counts as ported, which is how half a panel goes
# missing while every gate reads green.
#
# String literals and comments are stripped first. A repo that emits code holds
# whole functions inside template literals, and reading those as definitions
# reported seven dead functions in golden-configgen that the compiler does not
# even see. Reading them as call sites is worse: it marks dead code as wired.

python3 - "$@" <<'PY'
import re
import subprocess
import sys
from pathlib import Path


def strip(src):
    """Remove comments and string literals, keeping newlines so lines still line up."""
    out = []
    i = 0
    n = len(src)

    while i < n:
        c = src[i]

        if c == '/' and i + 1 < n and src[i + 1] == '/':
            j = src.find('\n', i)
            i = n if j < 0 else j
        elif c == '/' and i + 1 < n and src[i + 1] == '*':
            j = src.find('*/', i + 2)
            j = n if j < 0 else j + 2
            out.append('\n' * src.count('\n', i, j))
            i = j
        elif c in '"`\'':
            quote = c
            j = i + 1

            while j < n:
                if src[j] == '\\' and quote != '`':
                    j += 2
                    continue

                if src[j] == quote:
                    j += 1
                    break

                j += 1

            out.append('""' + '\n' * src.count('\n', i, j))
            i = j
        else:
            out.append(c)
            i += 1

    return ''.join(out)


roots = [d for d in ('internal', 'pkg', 'cmd') if Path(d).is_dir()]

files = [
    p for r in roots for p in Path(r).rglob('*.go')
    if not p.name.endswith('_test.go') and 'mocks' not in p.parts
]

owned = [p for p in files if not p.name.startswith('zz_generated')]

bodies = {p: strip(p.read_text()) for p in files}
haystack = '\n'.join(bodies.values())

define = re.compile(r'^func\s+(?:\([^)]*\)\s*)?([A-Z]\w*)\s*\(', re.M)

fail = False

for path in owned:
    for name in sorted(set(define.findall(bodies[path]))):
        if name in ('Main', 'TestMain'):
            continue

        hits = len(re.findall(r'[^\w.]%s\s*\(' % name, haystack))
        hits += len(re.findall(r'\.%s\s*\(' % name, haystack))
        defs = len(re.findall(r'^func\s+(?:\([^)]*\)\s*)?%s\s*\(' % name, haystack, re.M))

        if hits - defs <= 0:
            print('%s: %s has no caller in production code' % (path, name), file=sys.stderr)
            fail = True

if fail:
    sys.exit(1)

print('every exported function has a production caller')
PY
