#!/usr/bin/env bash
# Flag writing that doesn't match the project style in Markdown files.
#   Em and en dashes are errors.
#   Likely Oxford commas are warnings, since the check is a heuristic.
# Usage: check-prose.sh [file ...]   (defaults to every tracked Markdown file)
# Exits 1 if any errors are found.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

if [ "$#" -eq 0 ]; then
  set -- $(git ls-files '*.md')
fi

status=0
for file in "$@"; do
  case "$file" in *.md) ;; *) continue ;; esac
  [ -f "$file" ] || continue

  # Skip fenced code blocks so examples aren't policed.
  prose=$(awk '/^```/{code=!code; print ""; next} {print code ? "" : $0}' "$file")

  # Em dash and en dash as raw UTF-8, so this works on the old bash macOS ships.
  em=$(printf '\342\200\224'); en=$(printf '\342\200\223')
  dashes=$(printf '%s\n' "$prose" | grep -n -e "$em" -e "$en" || true)
  if [ -n "$dashes" ]; then
    status=1
    printf '%s: em or en dash, use a full stop, comma or brackets instead\n' "$file"
    printf '%s\n' "$dashes" | sed 's/^/  line /'
  fi

  oxford=$(printf '%s\n' "$prose" | grep -nE '[^,]+, [^,]+, (and|or) ' || true)
  if [ -n "$oxford" ]; then
    printf '%s: possible Oxford comma, check these read right without it\n' "$file"
    printf '%s\n' "$oxford" | sed 's/^/  line /'
  fi
done

exit "$status"
