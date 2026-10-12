#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Prints owner/name, in lower case, from a GitHub repository given as
# owner/name, ghcr.io/owner/name or https://github.com/owner/name(.git).
set -eu
r=$1
r=${r#https://}
r=${r#http://}
r=${r#github.com/}
r=${r#ghcr.io/}
r=${r%/}
r=${r%.git}
case $r in
  */*/* | /* | */ | "" | *[!A-Za-z0-9._/-]*) echo "not a GitHub repository: $1" >&2; exit 1 ;;
  */*) ;;
  *) echo "not a GitHub repository: $1" >&2; exit 1 ;;
esac
echo "$r" | tr '[:upper:]' '[:lower:]'
