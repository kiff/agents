#!/bin/sh
# Copies every skill package under a kiff/tafs skills tree into an eval's
# skills directory, as -skills reads them (#101): each
# <anything>/<name>/<version>/SKILL.md, with the files beside it, lands at
# <out>/<name>/<version>/. A SKILL.md that isn't at a name/version depth (the
# kiff skill tafs pastes into its definitions, kiff/SKILL.md) is no package
# and is left out. Copies nothing, and makes no <out>, when there are none.
#
#	infra/copy-skill-packages.sh <tafs>/runtime/skills <out>/skills
set -eu
[ $# -eq 2 ] || { echo "usage: $0 <skills tree> <out dir>" >&2; exit 2; }
src=$(cd "$1" && pwd)
out=$2
n=0
for md in $(find "$src" -mindepth 3 -name SKILL.md | sort); do
  dir=$(dirname "$md")
  version=$(basename "$dir")
  name=$(basename "$(dirname "$dir")")
  mkdir -p "$out/$name"
  if [ -e "$out/$name/$version" ]; then
    echo "skill $name@$version is in the tree twice" >&2
    exit 1
  fi
  cp -R "$dir" "$out/$name/$version"
  n=$((n + 1))
done
[ "$n" -eq 0 ] || echo "copied $n skill packages to $out"
