#!/usr/bin/env bash

# Builds the apt repository the release workflow deploys to GitHub Pages.
# The debs the last releases published are the pool of record: the site is
# rebuilt from them on every run, so it can be deleted and rebuilt
# identically, and pruning old versions is the release limit below.
#
# Needs gh authenticated to the repository, reprepro, and an imported gpg
# signing key. Writes the finished site into ./site.

set -e

releases=5

rm -rf site debs
mkdir -p site/conf debs

# A release older than the packaging carries no debs, which is why a
# download finding none is reported rather than failing the build.
for tag in $(gh release list --limit "$releases" --json tagName --jq '.[].tagName'); do
	gh release download "$tag" --pattern '*.deb' --dir debs ||
		echo "no debs in $tag"
done

# reprepro reads conf/ and writes db/ inside the site directory. Both are
# its own bookkeeping rather than part of the repository, so they are
# removed once the repository is built.
cp packaging/apt/distributions site/conf/distributions
reprepro -b site includedeb stable debs/*.deb
gpg --armor --export >site/key.asc
rm -rf site/conf site/db debs
