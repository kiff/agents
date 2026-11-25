#!/usr/bin/env bash
# This repository is public: its code, docs and tests say each rule as itself, never where it came
# from. So no coordination thread, no PM, no dated rule, and no team's own name for its lanes.
# Those names are matched by the first 16 hex digits of their SHA-256, so this script doesn't name
# them either, nor the reviewer logins outside CODEOWNERS, where GitHub reads them. Fails with where.
# Every run first proves the check on planted lines ("planted-lane" is a name kept for that), so a
# copy that lost a character fails here rather than passing on everything.
# --text checks what's on stdin instead: a pull request's title, body or review.
set -euo pipefail
cd "$(dirname "$0")/.."
# The script itself comes in on python's stdin, so the text to check goes in through the environment.
if [[ "${1:-}" == --text ]]; then
	PUBLIC_TEXT_IN="$(cat)"
	export PUBLIC_TEXT_IN
fi
python3 - "$@" <<'PY'
import hashlib, os, re, subprocess, sys

NAMES = set("""
3c32e58c6fc3cf62 20cca48ece26c362 f5068c4b858ead38 fe9c6bd7a66301f4 50077b782480811e
be896ac583d76627 afb42e3f1a4bce7e d419916f176e52d3 bd184cdc7aea9ef8 ab63a9b276c86bec
2ad06b22b301f873 3c00d0a9d4c2eb08 018a7f9527b114ab 94c6cdf43ac3f929 4a25962615e31ac6
d184eb5cf606a3eb d1cac6ef5a9ddbd1 a74b534355e685fc ea0edce134693c74 cb722454717333e5
0344d07edb285217
""".split())
# Reviewer logins: named only where GitHub reads them, in CODEOWNERS.
LOGINS = set("53dd1be914bd441c f66e74c6b8883a1f 3726111e555e250c".split())
MONTH = r"(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*"
SAID = re.compile(
    r"kiff/work\b|\bthe P[M]\b|\bPM's\b|\(PM[ ,)]|\bPM [0-9]{1,2}:[0-9]{2}"
    r"|\b(review|probe|ask|point|negatives|read) (on|of)\)"
    rf"|\bowner(?:'s|,) [0-9]{{1,2}} {MONTH}\b"
    # A status line's own words: the all-caps hand-off word with an arrow, or a board's line.
    r"|\bREADY\s*(?:→|->)|\bBOARD:"
)
WORD = re.compile(r"(?:^|(?<=[^\w-]))[a-z0-9]+(?:-[a-z0-9]+)*(?=[^\w-]|$)")
SELF, OWNERS = "tools/public-text.sh", ".github/CODEOWNERS"


def named(path, line):
    if SAID.search(line):
        return True
    for w in WORD.finditer(line.lower()):
        h = hashlib.sha256(w.group().encode()).hexdigest()[:16]
        if path != OWNERS and (h in NAMES or h in LOGINS):
            return True
    return False


PLANTED = [  # (path, line, whether it must fail)
    ("a.rs", "// planted-lane's note on a review", True),
    ("a.rs", "// as (Planted-Lane) said", True),
    ("a.rs", "// thanks @planted-lane, cc @Planted-Lane", True),
    ("a.md", "the rule from kiff/work, item 3", True),
    ("a.md", "as the " "PM said", True),  # split: older checks grep for the phrase
    ("a.md", "the " "PM's ruling", True),
    ("a.md", "(PM, at noon)", True),
    ("a.md", "the owner's 22 Nov rule", True),
    ("a.md", "the owner, 3 December", True),
    ("a.md", "tafs cloud's words, read at 4:30 PM", False),
    ("a.md", "planted-laneway and replanted-lane are other words", False),
    (OWNERS, "* @planted-lane", False),
    ("a.md", "as the " "PM 20:12 said", True),
    (OWNERS, "# one of the two (the owner, 22 Nov)", True),
    ("a.md", "READY → merge", True),
    ("a.md", "BOARD: two open", True),
    ("a.md", "The page is ready to merge; a circuit BOARD; READY_STATE", False),
    ("", "🤖 Generated with [Claude Code](https://claude.com/claude-code)", False),
]
wrong = [p for p in PLANTED if named(p[0], p[1]) != p[2]]
if wrong:
    print("The public-text check itself is broken: these planted lines came out wrong:")
    print("\n".join(f"{p}: {l} (should {'fail' if f else 'pass'})" for p, l, f in wrong))
    sys.exit(2)

if sys.argv[1:] == ["--text"]:
    found = [f"line {n}: {line.strip()[:160]}"
             for n, line in enumerate(os.environ.get("PUBLIC_TEXT_IN", "").splitlines(), 1) if named("", line)]
    if found:
        print("This pull request's text names a thread, a lane, a manager or a dated rule; say what changed and why:")
        print("\n".join(found))
        sys.exit(1)
    print(f"Public text: clean ({len(PLANTED)} planted lines checked first)")
    sys.exit(0)

files = subprocess.run(["git", "ls-files", "-z"], capture_output=True, check=True).stdout
found = []
for path in files.decode().split("\0"):
    if not path or path == SELF:
        continue
    try:
        text = open(path, encoding="utf-8").read()
    except (UnicodeDecodeError, IsADirectoryError, FileNotFoundError):
        continue  # binary, a submodule or gone: no words to read
    found += [f"{path}:{n}: {line.strip()[:160]}"
              for n, line in enumerate(text.splitlines(), 1) if named(path, line)]
if found:
    print("Public text names a thread, a lane, a manager or a dated rule; say the rule itself:")
    print("\n".join(found))
    sys.exit(1)
print(f"Public text: clean ({len(PLANTED)} planted lines checked first)")
PY
