#!/bin/sh
# Build bin/worktender for `herdr plugin install`.
#
# Prefer a local Go toolchain, which produces an exact build of the cloned
# source. Without Go, fall back to the matching prebuilt release binary so
# installing works on a machine that has no Go at all.
set -eu

REPO="steig/worktender"

# WORKTENDER_BUILD_OUT is how `worktender update` asks for the binary somewhere
# other than its live path. It must never be built over: an update is normally
# run BY the binary being replaced, and herdr may be running an action through
# the same file. The update stages the build here and renames it into place.
OUT="${WORKTENDER_BUILD_OUT:-bin/worktender}"

mkdir -p "$(dirname "$OUT")"

if command -v go >/dev/null 2>&1; then
	go build -o "$OUT" .
	exit 0
fi

echo "worktender: no Go toolchain found, downloading a prebuilt binary" >&2

case "$(uname -s)" in
Darwin) os="darwin" ;;
Linux) os="linux" ;;
*) echo "worktender: unsupported OS $(uname -s); install Go and retry" >&2; exit 1 ;;
esac

case "$(uname -m)" in
arm64 | aarch64) arch="arm64" ;;
x86_64 | amd64) arch="amd64" ;;
*) echo "worktender: unsupported architecture $(uname -m); install Go and retry" >&2; exit 1 ;;
esac

asset="worktender_${os}_${arch}"

# Download the release matching the source that was cloned, not `latest`.
#
# `latest` is a mutable pointer: someone who clones tag v0.1.0, reads it, and
# installs would get whatever the newest release happened to be at that moment,
# which is not the thing they audited. The manifest sitting next to this script
# is part of that clone, so its version is the one pin available here — the
# commit is knowable too (herdr clones the repository) but GitHub keys release
# assets by tag, and the clone is shallow with no tags fetched.
#
# This does not make the download trustworthy; see the note on checksums below.
# It only makes it the release this source says it is.
version=$(awk -F'"' '/^version[[:space:]]*=/ {print $2; exit}' herdr-plugin.toml)
if [ -z "$version" ]; then
	echo "worktender: no version in herdr-plugin.toml; refusing to fall back to whatever \`latest\` points at" >&2
	exit 1
fi
base="https://github.com/$REPO/releases/download/v$version"

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		echo "worktender: neither curl nor wget available; install Go and retry" >&2
		exit 1
	fi
}

# An unverified download must never survive this script, and a cleanup line at
# the bottom cannot promise that: `set -e` inside fetch exits the shell before
# any line below it runs, so a failed checksums.txt fetch would leave bin/worktender
# on disk at exactly the path the manifest execs. A trap is the only form of the
# rule that holds on every exit path, and it is disarmed at the bottom once the
# binary has been verified.
trap 'rm -f "$OUT" bin/checksums.txt' EXIT

fetch "$base/$asset" "$OUT"
fetch "$base/checksums.txt" bin/checksums.txt

# Verify before making it executable. This script downloads a binary over the
# network and hands it to herdr to run, which makes an unverified download the
# weakest link in the whole install. A missing or mismatched checksum is fatal
# rather than a warning: continuing would run the thing we just failed to
# vouch for.
#
# Be clear about the ceiling on that. checksums.txt comes from the same release
# as the binary, so this proves the download arrived intact and says nothing
# about who published it — there is no signature and no attestation to check
# against. Whoever can publish a release here publishes both halves. That is the
# ordinary trust model for installing from GitHub, and the Go path above avoids
# it entirely by compiling the source that was cloned.
expected=$(awk -v want="$asset" '$2 == want || $2 == "*"want {print $1}' bin/checksums.txt)
if [ -z "$expected" ]; then
	echo "worktender: no checksum published for $asset; refusing to install it" >&2
	exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$OUT" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
	actual=$(shasum -a 256 "$OUT" | awk '{print $1}')
else
	echo "worktender: no sha256 tool to verify the download; install Go and retry" >&2
	exit 1
fi

if [ "$expected" != "$actual" ]; then
	echo "worktender: checksum mismatch for $asset" >&2
	echo "  expected $expected" >&2
	echo "  actual   $actual" >&2
	exit 1
fi

rm -f bin/checksums.txt
chmod +x "$OUT"
trap - EXIT
