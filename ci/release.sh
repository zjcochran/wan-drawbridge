#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Creates the next release after a merge to the default branch. The merged
# merge request's label (major, minor or patch) picks which part of the latest
# vX.Y.Z tag to bump; with no tag yet the base is v0.0.0. The GitLab release
# and its tag point at the merge commit.
#
# Runs in CI (see .gitlab-ci.yml). Needs RELEASE_TOKEN, a project access token
# with the api scope, because the job token cannot read merge requests.
#
# Writes release.env (RELEASE_TAG=vX.Y.Z, or empty when nothing was released)
# for the jobs that publish the release.
set -eu

echo "RELEASE_TAG=" >release.env

: "${RELEASE_TOKEN:?RELEASE_TOKEN is not set: add a project access token (api scope) as a CI/CD variable}"
api="$CI_API_V4_URL/projects/$CI_PROJECT_ID"
get() { curl -fsS -H "PRIVATE-TOKEN: $RELEASE_TOKEN" "$api$1"; }

git fetch --quiet --force --tags origin

# A retried job must not release the same commit twice.
existing=$(git tag --points-at "$CI_COMMIT_SHA" | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1 || true)
if [ -n "$existing" ]; then
  echo "$CI_COMMIT_SHA is already released as $existing."
  echo "RELEASE_TAG=$existing" >release.env
  exit 0
fi

# The merge request that produced this commit: by commit first, then among
# recent merges (matches merge, squash and fast-forward commits).
# API errors stop the job (set -e on the assignments) rather than looking like
# "no merge request".
resp=$(get "/repository/commits/$CI_COMMIT_SHA/merge_requests")
mr=$(printf '%s\n' "$resp" | jq -c --arg b "$CI_DEFAULT_BRANCH" '[.[] | select(.state == "merged" and .target_branch == $b)][0] // empty')
if [ -z "$mr" ]; then
  resp=$(get "/merge_requests?state=merged&target_branch=$CI_DEFAULT_BRANCH&order_by=updated_at&per_page=50")
  mr=$(printf '%s\n' "$resp" | jq -c --arg s "$CI_COMMIT_SHA" '[.[] | select(.merge_commit_sha == $s or .squash_commit_sha == $s or .sha == $s)][0] // empty')
fi
if [ -z "$mr" ]; then
  echo "No merged merge request produced $CI_COMMIT_SHA (a direct push?): no release."
  exit 0
fi
iid=$(printf '%s\n' "$mr" | jq -r .iid)
title=$(printf '%s\n' "$mr" | jq -r .title)

bump=$(printf '%s\n' "$mr" | jq -r '[.labels[] | select(. == "major" or . == "minor" or . == "patch")] | if length == 1 then .[0] else "" end')
if [ -z "$bump" ]; then
  echo "Merge request !$iid needs exactly one of the labels major, minor or patch. Add it, then retry this job." >&2
  exit 1
fi

prev=$(git tag -l 'v*' --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1 || true)
IFS=. read -r major minor patch <<EOF
${prev#v}
EOF
major=${major:-0} minor=${minor:-0} patch=${patch:-0}
case $bump in
  major) major=$((major + 1)) minor=0 patch=0 ;;
  minor) minor=$((minor + 1)) patch=0 ;;
  patch) patch=$((patch + 1)) ;;
esac
next="v$major.$minor.$patch"
echo "Releasing $next (${prev:-no earlier release} + $bump, from !$iid)."

range=$CI_COMMIT_SHA
[ -n "$prev" ] && range="$prev..$CI_COMMIT_SHA"
notes=$(
  printf '%s\n' "$mr" | jq -r '"## \(.title) (!\(.iid))\n" + (.description // "" | if . == "" then "" else "\n" + . + "\n" end)'
  echo "### Commits"
  echo
  git log --no-merges --format='- %s (%h)' "$range"
  if [ -n "$prev" ]; then
    printf '\nFull diff: %s/-/compare/%s...%s\n' "$CI_PROJECT_URL" "$prev" "$next"
  fi
)

jq -n --arg tag "$next" --arg ref "$CI_COMMIT_SHA" --arg name "$next" --arg notes "$notes" \
  '{tag_name: $tag, ref: $ref, name: $name, description: $notes}' |
  curl -fsS -X POST -H "PRIVATE-TOKEN: $RELEASE_TOKEN" -H "Content-Type: application/json" \
    --data @- "$api/releases" >/dev/null
echo "RELEASE_TAG=$next" >release.env
echo "Created release $next: $CI_PROJECT_URL/-/releases/$next"
echo "Title: $title"
