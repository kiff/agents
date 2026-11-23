#!/usr/bin/env bash
# This repository is public: its code, docs and tests say each rule as itself, never where it came
# from. So no coordination thread, no PM, no dated rule, and no team's own name for its lanes.
# Those names, and the reviewers' logins, are matched by the first 16 hex digits of their SHA-256,
# so this script doesn't name them either. Logins are allowed only in CODEOWNERS, where GitHub reads
# them. To add a name: printf %s NAME | sha256sum | cut -c1-16.
# It first runs on planted lines it must catch, so it never passes by matching nothing. Given paths,
# it reads only those. Fails with where.
set -euo pipefail
cd "$(dirname "$0")/.."

public_text() { # [paths...]
	python3 - "$@" <<'PY'
import hashlib, re, subprocess, sys

NAMES = set("""
3c32e58c6fc3cf62 20cca48ece26c362 f5068c4b858ead38 fe9c6bd7a66301f4 50077b782480811e
be896ac583d76627 afb42e3f1a4bce7e d419916f176e52d3 bd184cdc7aea9ef8 ab63a9b276c86bec
2ad06b22b301f873 3c00d0a9d4c2eb08 018a7f9527b114ab 94c6cdf43ac3f929 4a25962615e31ac6
""".split())
LOGINS = set("53dd1be914bd441c f66e74c6b8883a1f 3726111e555e250c".split())
OWNERS = ".github/CODEOWNERS"
MONTH = r"(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*"
SAID = re.compile(
    r"kiff/work\b|\bthe PM\b|\bPM's\b|\(PM[ ,)]|\bPM [0-9]{1,2}:[0-9]{2}"
    r"|\b(review|probe|ask|point|negatives|read) (on|of)\)"
    rf"|\bowner(?:'s|,) [0-9]{{1,2}} {MONTH}\b"
)
WORD = re.compile(r"(?<![@\w-])[a-z0-9]+(?:-[a-z0-9]+)*(?![\w-])")
SKIP = {"tools/public-text.sh"}

paths = sys.argv[1:] or subprocess.run(
    ["git", "ls-files", "-z"], capture_output=True, check=True).stdout.decode().split("\0")
found = []
for path in paths:
    if not path or path in SKIP:
        continue
    try:
        text = open(path, encoding="utf-8").read()
    except (UnicodeDecodeError, IsADirectoryError, FileNotFoundError):
        continue  # binary, a submodule or gone: no words to read
    for n, line in enumerate(text.splitlines(), 1):
        why = SAID.search(line) or next((w for w in WORD.finditer(line.lower())
            if (h := hashlib.sha256(w.group().encode()).hexdigest()[:16]) in NAMES
            or (h in LOGINS and path != OWNERS)), None)
        if why:
            found.append(f"{path}:{n}: {line.strip()[:160]}")
if found:
    print("Public text names a thread, a lane, the PM or a dated rule; say the rule itself:")
    print("\n".join(found))
    sys.exit(1)
print("Public text: clean")
PY
}

probe="$(mktemp)"
trap 'rm -f "$probe"' EXIT
printf '%s\n' "// as decided on kiff/work, owner's 22 Nov" "// $(printf 'QA%sKIFF' -) said so" > "$probe"
if [[ "$(public_text "$probe" | grep -c "^$probe:" || true)" -ne 2 ]]; then
	echo "the public-text check missed a planted line"
	exit 1
fi
public_text
