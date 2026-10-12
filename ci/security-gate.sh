#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Fails the pipeline when the security scanners found something serious. GitLab
# Free shows these reports but cannot block a merge on them, so this job does.
#
#   SAST and container scanning: Critical or High findings fail.
#   Secret detection: any finding fails.
#   A missing or unreadable report fails too: the scanner did not run.
set -eu

failed=0
check() { # <report file> <jq filter selecting failing findings> <description>
  report=$1 filter=$2 what=$3
  if [ ! -f "$report" ]; then
    echo "FAIL $report is missing: the scanner did not produce a report."
    failed=1
    return
  fi
  if ! jq -e '.vulnerabilities | type == "array"' "$report" >/dev/null 2>&1; then
    echo "FAIL $report is not a readable scanner report."
    failed=1
    return
  fi
  total=$(jq '.vulnerabilities | length' "$report")
  bad=$(jq "[.vulnerabilities[] | select($filter)] | length" "$report")
  if [ "$bad" -gt 0 ]; then
    echo "FAIL $report: $bad of $total findings are $what:"
    jq -r ".vulnerabilities[] | select($filter) |
      \"  [\(.severity)] \(.name // .message // .identifiers[0].value // .id) at \" +
      (if .location.file then \"\(.location.file):\(.location.start_line // \"?\")\"
       else \"\(.location.dependency.package.name // \"?\") \(.location.dependency.version // \"\")\" end)" "$report"
    failed=1
  else
    echo "ok   $report: $total findings, none $what."
  fi
}

check gl-sast-report.json '.severity == "Critical" or .severity == "High"' "Critical or High"
check gl-secret-detection-report.json 'true' "secrets"
check gl-container-scanning-report.json '.severity == "Critical" or .severity == "High"' "Critical or High"

if [ "$failed" -ne 0 ]; then
  echo
  echo "Fix the findings above. For a false positive, mark it in the code (for example // nosemgrep with the reason) so the next scan no longer reports it."
  exit 1
fi
