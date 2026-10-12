#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Applies git filter-repo --replace-text rules to standard input, so release
# notes get exactly the replacements the history gets.
#
#   apply-rules.py <rules file> < text > text
#
# Rule lines: "literal==>replacement" or "regex:pattern==>replacement"
# (Python re, \1 for groups); blank lines are ignored.
import re
import sys

text = sys.stdin.read()
with open(sys.argv[1], encoding="utf-8") as f:
    for line in f.read().splitlines():
        if not line.strip():
            continue
        pattern, _, replacement = line.partition("==>")
        if pattern.startswith("regex:"):
            text = re.sub(pattern[len("regex:"):], replacement, text)
        else:
            text = text.replace(pattern, replacement)
sys.stdout.write(text)
