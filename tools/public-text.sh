#!/usr/bin/env bash
# This repository is public: its code, docs and tests say each rule as itself, never where it came
# from. So no coordination thread, no PM, no dated rule, and no team's own name for its lanes.
# Those names are matched by the first 16 hex digits of their SHA-256, so this script doesn't name
# them either. CODEOWNERS is read for everything but names: it works by logins. Fails with where.
set -euo pipefail
cd "$(dirname "$0")/.."
python3 - <<'PY'
import hashlib, re, subprocess, sys

NAMES = set("""
3c32e58c6fc3cf62 20cca48ece26c362 f5068c4b858ead38 fe9c6bd7a66301f4 50077b782480811e
be896ac583d76627 afb42e3f1a4bce7e d419916f176e52d3 bd184cdc7aea9ef8 ab63a9b276c86bec
2ad06b22b301f873 3c00d0a9d4c2eb08 018a7f9527b114ab 94c6cdf43ac3f929
""".split())
MONTH = r"(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*"
SAID = re.compile(
    r"kiff/work|\bthe PM\b|\bPM's\b|\(PM[ ,)]"
    rf"|\bowner(?:'s|,) [0-9]{{1,2}} {MONTH}\b"
)
WORD = re.compile(r"(?<![@\w-])[a-z0-9]+(?:-[a-z0-9]+)*(?![\w-])")
SELF, OWNERS = "tools/public-text.sh", ".github/CODEOWNERS"

files = subprocess.run(["git", "ls-files", "-z"], capture_output=True, check=True).stdout
found = []
for path in files.decode().split("\0"):
    if not path or path == SELF:
        continue
    try:
        text = open(path, encoding="utf-8").read()
    except (UnicodeDecodeError, IsADirectoryError, FileNotFoundError):
        continue  # binary, a submodule or gone: no words to read
    for n, line in enumerate(text.splitlines(), 1):
        why = SAID.search(line)
        if not why and path != OWNERS:
            why = next((w for w in WORD.finditer(line.lower())
                        if hashlib.sha256(w.group().encode()).hexdigest()[:16] in NAMES), None)
        if why:
            found.append(f"{path}:{n}: {line.strip()[:160]}")
if found:
    print("Public text names a thread, a lane, the PM or a dated rule; say the rule itself:")
    print("\n".join(found))
    sys.exit(1)
print("Public text: clean")
PY
