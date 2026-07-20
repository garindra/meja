#!/usr/bin/env bash
# Prepare a release PR, then publish its verified main-branch commit.
set -euo pipefail

action="${1:-}"
version="${2:-}"

if [[ "$action" != "prepare" && "$action" != "publish" ]] || [[ -z "$version" ]]; then
  echo "usage: scripts/release.sh prepare|publish vX.Y.Z" >&2
  exit 1
fi

if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "version must look like v0.0.4" >&2
  exit 1
fi

if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "working tree is not clean" >&2
  exit 1
fi

git fetch origin --tags

if git rev-parse -q --verify "refs/tags/$version" >/dev/null ||
  git ls-remote --exit-code --tags origin "refs/tags/$version" >/dev/null 2>&1; then
  echo "tag $version already exists" >&2
  exit 1
fi

branch="$(git branch --show-current)"
if [[ "$branch" != "main" ]]; then
  echo "release $action must start on main (currently $branch)" >&2
  exit 1
fi

git fetch origin main
head="$(git rev-parse HEAD)"
origin_main="$(git rev-parse origin/main)"
if [[ "$head" != "$origin_main" ]]; then
  echo "main must exactly match origin/main" >&2
  exit 1
fi

if [[ "$action" == "prepare" ]]; then
  bare_version="${version#v}"
  perl -0pi -e "s{releases/download/v[0-9]+\\.[0-9]+\\.[0-9]+/meja_[0-9]+\\.[0-9]+\\.[0-9]+_}{releases/download/$version/meja_${bare_version}_}gx" README.md

  make check
  make race

  release_branch="release/$version"
  git switch -c "$release_branch"
  git add README.md
  git commit -m "docs: prepare $version release"
  git push -u origin "$release_branch"
  gh pr create --base main --head "$release_branch" --title "Release $version" --body "Prepare $version release. Publish the tag only after Linux and macOS CI pass."
  exit 0
fi

if ! rg -q "releases/download/$version/meja_${version#v}_" README.md; then
  echo "README install links do not point to $version" >&2
  exit 1
fi

ci="$(gh run list --workflow ci.yml --limit 20 --json headSha,status,conclusion,url \
  --jq "(map(select(.headSha == \"$head\"))[0] // empty) | [.status, .conclusion, .url] | @tsv")"
if [[ -z "$ci" ]]; then
  echo "no CI run found for main commit $head" >&2
  exit 1
fi
IFS=$'\t' read -r ci_status ci_conclusion ci_url <<<"$ci"
if [[ "$ci_status" != "completed" || "$ci_conclusion" != "success" ]]; then
  echo "CI is not green for main commit $head: $ci_status/$ci_conclusion $ci_url" >&2
  exit 1
fi

git tag -a "$version" -m "meja $version"
git push origin "$version"
