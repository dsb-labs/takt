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

# The suite is signed here rather than by reprepro, whose gpgme asks a
# pinentry for the passphrase and dies on a runner with no terminal.
# Loopback mode reads it from the APT_SIGNING_PASSPHRASE variable
# instead, and an unset variable means a key that has none.
sign() {
	gpg --batch --yes --pinentry-mode loopback \
		${APT_SIGNING_PASSPHRASE:+--passphrase "$APT_SIGNING_PASSPHRASE"} "$@"
}

sign --armor --detach-sign \
	--output site/dists/stable/Release.gpg site/dists/stable/Release
sign --clearsign \
	--output site/dists/stable/InRelease site/dists/stable/Release

gpg --armor --export >site/key.asc
rm -rf site/conf site/db debs
