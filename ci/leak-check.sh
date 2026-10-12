#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Fails if anything about to be published names the internal GitLab server
# (its host or domain, from GitLab's CI_SERVER_HOST at run time, so it is never
# written into the repository). Matching is case-insensitive.
#
#   ci/leak-check.sh tree <dir>       files in a directory (not .git)
#   ci/leak-check.sh history <repo>   every commit: author, committer, message, file versions
#   ci/leak-check.sh text <file>      one file
set -eu

: "${CI_SERVER_HOST:?CI_SERVER_HOST is not set}"
host=$CI_SERVER_HOST
domain=$(echo "$host" | awk -F. 'NF >= 2 { print $(NF-1) "." $NF }')
[ -n "$domain" ] || domain=$host

# One pattern file: the host and domain, dots escaped.
patterns=$(mktemp)
printf '%s\n%s\n' "$host" "$domain" | sed 's/[.[\*^$]/\\&/g' >"$patterns"

# grep exits 1 for "no match"; anything else is an error,
# which must stop publishing rather than pass as clean.
search() {
  status=0
  "$@" >"$found" || status=$?
  if [ "$status" -gt 1 ]; then
    echo "BLOCKED: the leak check itself failed" >&2
    exit 2
  fi
}

found=$(mktemp)
history=$(mktemp)
case $1 in
  tree) search grep -rIliE --exclude-dir=.git -f "$patterns" "$2" ;;
  history)
    git -C "$2" log --all --format='commit %H%nauthor %an <%ae>%ncommitter %cn <%ce>%n%B' -p >"$history"
    search grep -iE -f "$patterns" "$history"
    ;;
  text) search grep -iE -f "$patterns" "$2" ;;
  *) echo "usage: $0 tree|history|text <path>" >&2; exit 2 ;;
esac

if [ -s "$found" ]; then
  echo "BLOCKED: internal information appears in what would be published ($1 $2):" >&2
  head -20 "$found" >&2
  exit 1
fi
echo "ok: no internal names in $1 $2"
