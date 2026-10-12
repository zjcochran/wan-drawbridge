#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Publishes the default branch, every vX.Y.Z tag and every GitLab release to
# the GitHub repository, after a merge. Nothing runs on GitHub.
#
# GitHub gets a sanitized copy of the history: in a scratch repository, every
# file version and commit message has the GitLab project's URL, host and path
# replaced with the GitHub ones, plus the rules in PUBLISH_REPLACEMENTS (a
# GitLab CI/CD variable, file or text, in git filter-repo --replace-text
# format, holding anything private that older commits contain). Every author
# and committer becomes GHCR_USER with GitHub's no-reply address,
# Claude-Session trailers are removed, and the binary files listed in
# ci/publish-strip-blobs.txt (old screenshots) are dropped; binary files cannot
# be checked by the text guard, so screenshots must only show fictional data. The rewrite is deterministic, so a commit has
# the same GitHub hash on every run and each sync is a fast-forward. Before
# anything is pushed, ci/leak-check.sh fails the job if the internal host or
# domain still appears anywhere.
#
# It never force-pushes unless GITHUB_FORCE_SYNC=true, so a GitHub repository
# that has diverged fails the job instead of being overwritten.
#
# Needs GHCR_TOKEN (classic GitHub token: repo, write:packages), GHCR_REPO
# (owner/name) and RELEASE_TOKEN (to read GitLab releases), plus git-filter-repo.
# GITHUB_API_URL and GITHUB_PUSH_URL override the GitHub endpoints (for tests).
set -eu

: "${GHCR_TOKEN:?GHCR_TOKEN is not set: add the GitHub token as a CI/CD variable}"
: "${GHCR_REPO:?GHCR_REPO is not set}"
: "${RELEASE_TOKEN:?RELEASE_TOKEN is not set}"
: "${GHCR_USER:?GHCR_USER is not set}"
repo=$(ci/github-repo.sh "$GHCR_REPO")
case $GHCR_USER in
  *[!A-Za-z0-9-]*) echo "GHCR_USER must be a GitHub user name" >&2; exit 1 ;;
esac
github_url="https://github.com/$repo"
api=${GITHUB_API_URL:-https://api.github.com}
remote=${GITHUB_PUSH_URL:-"https://x-access-token:${GHCR_TOKEN}@github.com/${repo}.git"}
here=$(pwd)

github_api() {
  curl -fsS -H "Authorization: Bearer $GHCR_TOKEN" -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" "$@"
}

# Replacements, most specific first, built from GitLab's predefined variables
# so the internal names are never stored in the repository.
rules=$(mktemp)
cat >"$rules" <<EOF
$CI_PROJECT_URL/-/==>$github_url/
$CI_PROJECT_URL==>$github_url
$CI_SERVER_HOST/$CI_PROJECT_PATH==>github.com/$repo
$CI_PROJECT_PATH==>$repo
EOF
# PUBLISH_REPLACEMENTS: a File variable (a path) or the rules themselves.
if [ -n "${PUBLISH_REPLACEMENTS:-}" ]; then
  if [ -f "$PUBLISH_REPLACEMENTS" ]; then cat "$PUBLISH_REPLACEMENTS"; else printf '%s\n' "$PUBLISH_REPLACEMENTS"; fi >>"$rules"
fi

# 1. A sanitized copy of the default branch and the version tags.
git fetch --quiet --force --tags origin
work=$(mktemp -d)
git init --quiet --bare "$work"
git push --quiet "$work" "$CI_COMMIT_SHA:refs/heads/$CI_DEFAULT_BRANCH" 'refs/tags/v*:refs/tags/v*'
grep -v '^#' "$here/ci/publish-strip-blobs.txt" >"$work.strip"
git -C "$work" filter-repo --force --quiet --replace-text "$rules" --replace-message "$rules" \
  --strip-blobs-with-ids "$work.strip" \
  --name-callback "return b'$GHCR_USER'" \
  --email-callback "return b'$GHCR_USER@users.noreply.github.com'" \
  --message-callback 'import re
return re.sub(rb"(?m)^Claude-Session:.*\n?", b"", message)'
"$here/ci/leak-check.sh" history "$work"

force=""
[ "${GITHUB_FORCE_SYNC:-}" = "true" ] && force="--force" && echo "GITHUB_FORCE_SYNC=true: overwriting GitHub."
# shellcheck disable=SC2086 # $force is empty or one flag
git -C "$work" push --quiet $force "$remote" \
  "refs/heads/$CI_DEFAULT_BRANCH:refs/heads/$CI_DEFAULT_BRANCH" 'refs/tags/v*:refs/tags/v*'
echo "Pushed $CI_DEFAULT_BRANCH ($(git -C "$work" rev-parse --short "$CI_DEFAULT_BRANCH") on GitHub)" \
  "and tags $(git -C "$work" tag -l 'v*' --sort=v:refname | tr '\n' ' ')"

# 2. Releases: create the ones GitHub does not have, oldest first, so the
# newest ends up as GitHub's latest release. Notes get the same replacements.
# Responses are fetched first so an API error stops the job (set -e).
github_releases=$(github_api "$api/repos/$repo/releases?per_page=100")
have=$(printf '%s\n' "$github_releases" | jq -r '.[].tag_name')
gitlab_releases=$(curl -fsS -H "PRIVATE-TOKEN: $RELEASE_TOKEN" \
  "$CI_API_V4_URL/projects/$CI_PROJECT_ID/releases?per_page=100&sort=asc")
# Parsed into a variable first so a jq error stops the job too.
# The notes go through the same rules as the history.
releases=$(printf '%s\n' "$gitlab_releases" | python3 "$here/ci/apply-rules.py" "$rules" |
  jq -c '.[] | {tag_name, name, body: (.description // "")}')
notes=$(mktemp)
list=$(mktemp)
printf '%s\n' "$releases" >"$list"
while read -r release; do
  [ -n "$release" ] || continue
  tag=$(printf '%s\n' "$release" | jq -r .tag_name)
  if printf '%s\n' "$have" | grep -qxF "$tag"; then
    continue
  fi
  printf '%s\n' "$release" >"$notes"
  "$here/ci/leak-check.sh" text "$notes" >/dev/null
  github_api -X POST --data @"$notes" "$api/repos/$repo/releases" >/dev/null
  echo "Created GitHub release $tag"
done <"$list"
echo "GitHub releases are in sync: $github_url/releases"
