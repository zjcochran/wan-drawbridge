#!/bin/sh
# Fails unless every package with code reaches the minimum statement coverage
# from its own tests. Reads a Go cover profile:
#
#   go test -coverprofile=coverage.out ./... && test/coverage.sh coverage.out 80
set -eu
profile=${1:-coverage.out}
min=${2:-80}
module=$(go list -m)

# Profile lines: <file>:<start>,<end> <statements> <hit count>
tail -n +2 "$profile" | awk -v min="$min" -v module="$module/" '
  {
    file = substr($1, 1, index($1, ":") - 1)
    pkg = file; sub("/[^/]*$", "", pkg); sub("^" module, "", pkg)
    key = $1                     # a block can appear more than once; count it once
    if (!(key in seen)) { seen[key] = 1; total[pkg] += $2 }
    if ($3 > 0 && !(key in hit)) { hit[key] = 1; covered[pkg] += $2 }
  }
  END {
    failed = 0
    for (p in total) {
      pct = 100 * covered[p] / total[p]
      status = pct + 0.0001 >= min ? "ok  " : "FAIL"
      if (status == "FAIL") failed = 1
      printf "%s %-24s %5.1f%%\n", status, p, pct | "sort -k2"
    }
    close("sort -k2")
    if (failed) { printf "\nEvery package needs at least %d%% coverage.\n", min; exit 1 }
  }'
