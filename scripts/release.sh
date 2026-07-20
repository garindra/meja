#!/usr/bin/env bash
# Prepare and publish a versioned release. The pushed tag starts the GitHub
# Actions release workflow after Linux and macOS checks have passed.
set -euo pipefail

version="${1:?usage: scripts/release.sh vX.Y.Z}"

if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "version must look like v0.0.4" >&2
  exit 1
fi

if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "working tree is not clean" >&2
  exit 1
fi

git fetch origin --tags

if git rev-parse -q --verify "refs/tags/$version" >/dev/null; then
  echo "tag $version already exists" >&2
  exit 1
fi

bare_version="${version#v}"
perl -0pi -e "s{releases/download/v[0-9]+\\.[0-9]+\\.[0-9]+/meja_[0-9]+\\.[0-9]+\\.[0-9]+_}{releases/download/$version/meja_${bare_version}_}gx" README.md

make check
make race

git add README.md
git commit -m "docs: prepare $version release"
git tag -a "$version" -m "meja $version"
git push origin main
git push origin "$version"
