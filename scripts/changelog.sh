#!/usr/bin/env bash

# Prints the release notes for a tag, read from CHANGELOG.md. The release
# workflow hands the output to goreleaser, so what is under a release's
# heading in the file is what the release on GitHub says.
#
# A tag with no section fails the release rather than shipping with empty
# notes, and so does an entry still sitting under Unreleased: both mean the
# file was not brought up to date before the tag was cut, and the tag is
# cheaper to redo than a release is to amend.

set -e

tag=$1
if [ -z "$tag" ]; then
	echo "usage: $0 <tag>" >&2
	exit 1
fi

# The section is everything between the tag's heading and the next release
# heading. The heading itself is left out, since the release is already
# titled with the tag.
#
# The file is wrapped for reading, and GitHub renders a newline inside a
# release body as a line break, so a wrapped entry would show every wrap
# point. A line that starts neither an entry, a heading nor a paragraph
# continues the one above it and is joined back onto it.
section=$(awk -v tag="$tag" '
	/^## / { if (held != "") print held; held = ""; active = ($2 == tag); next }
	!active { next }
	held != "" && $0 != "" && !/^(- |#)/ { sub(/^[ \t]+/, ""); held = held " " $0; next }
	{ if (held != "") print held; held = $0 }
	END { if (held != "") print held }
' CHANGELOG.md | sed '/./,$!d')

if [ -z "$(printf %s "$section" | tr -d '[:space:]')" ]; then
	echo "CHANGELOG.md has no section for $tag" >&2
	exit 1
fi

unreleased=$(awk '
	/^## / { active = ($2 == "Unreleased") ; next }
	active && /^- / { print }
' CHANGELOG.md)

if [ -n "$unreleased" ]; then
	echo "CHANGELOG.md still lists changes under Unreleased; move them under $tag" >&2
	exit 1
fi

printf '%s\n' "$section"

# The commit list the notes replace is still one click away.
previous=$(git describe --tags --abbrev=0 "$tag^" 2>/dev/null || true)
if [ -n "$previous" ]; then
	printf '\n**Commits**: https://github.com/dsb-labs/takt/compare/%s...%s\n' "$previous" "$tag"
fi
