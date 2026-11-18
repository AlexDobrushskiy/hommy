#!/bin/bash
# Lints a hommy vault (master spec §6): every path in AGENTS.md's routing table exists, index.md lists every file that
# is not a README (files inside inbox/ and archive/ excepted), no folder is empty, every context/ file starts with
# `## TL;DR`. Hidden files and folders are ignored.
#   scripts/vault-lint.sh <vault>      prints `ok` (exit 0), or one `vault-lint: <path>: <problem>` line each (exit 1)
set -euo pipefail
if [[ $# -ne 1 || $1 == -h || $1 == --help ]]; then
  sed -n '2,6s/^# \{0,1\}//p' "$0" >&2
  exit 2
fi
[[ -d $1 ]] || { echo "vault-lint: $1 is not a directory" >&2; exit 2; }
exec /usr/bin/python3 - "$1" <<'PY'
import os
import re
import sys

root = sys.argv[1]
problems = []


def bad(path, what):
    problems.append(f"vault-lint: {path}: {what}")


def read(rel):
    with open(os.path.join(root, rel), encoding="utf-8", errors="replace") as f:
        return f.read()


files, dirs = [], []
for d, subdirs, names in os.walk(root):
    subdirs[:] = sorted(s for s in subdirs if not s.startswith("."))
    rel = os.path.relpath(d, root)
    if rel != ".":
        dirs.append(rel)
        if not any(not n.startswith(".") for n in names) and not subdirs:
            bad(rel + "/", "empty folder (add a README.md saying what lands here)")
    files += [os.path.normpath(os.path.join(rel, n)) for n in sorted(names) if not n.startswith(".")]

# 1. the routing table: the first table after a heading that names "routing"
if not os.path.isfile(os.path.join(root, "AGENTS.md")):
    bad("AGENTS.md", "missing")
else:
    lines = read("AGENTS.md").splitlines()
    in_section, rows = False, []
    for line in lines:
        if line.startswith("#"):
            if rows:
                break
            in_section = "routing" in line.lower()
            continue
        if in_section and line.lstrip().startswith("|"):
            rows.append(line)
        elif rows:
            break
    if len(rows) < 3:
        bad("AGENTS.md", "no routing table (a table under a heading with 'routing')")
    path_re = re.compile(r"(?<![\w./-])((?:[\w.-]+/)+(?:[\w.-]+\.md)?|[\w.-]+\.md)(?![\w/-])")
    for row in rows[2:]:
        cells = [c.strip() for c in row.strip().strip("|").split("|")]
        target = cells[-1] if len(cells) > 1 else ""
        target = re.sub(r"`[^`]*<[^`]*`", "", target)
        found = path_re.findall(target)
        if not found:
            bad("AGENTS.md", f"routing row without a path: {cells[0][:60]}")
        for p in found:
            if not os.path.exists(os.path.join(root, p)):
                bad("AGENTS.md", f"routing table names {p}, which does not exist")

# 2. index.md lists every non-README file
if not os.path.isfile(os.path.join(root, "index.md")):
    bad("index.md", "missing")
else:
    index = read("index.md")
    for f in files:
        top = f.split(os.sep)[0]
        if f == "index.md" or os.path.basename(f) == "README.md" or (top in ("inbox", "archive") and os.sep in f):
            continue
        if not re.search(r"(?<![\w/.-])" + re.escape(f) + r"(?![\w/.-])", index):
            bad("index.md", f"does not list {f}")

# 3. every context/ file starts with ## TL;DR
for f in files:
    if f.startswith("context" + os.sep) and f.endswith(".md"):
        first = next((l.strip() for l in read(f).splitlines() if l.strip()), "")
        if first != "## TL;DR":
            bad(f, "does not start with ## TL;DR")

if problems:
    print("\n".join(problems))
    sys.exit(1)
print("ok")
PY
