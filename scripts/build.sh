#!/bin/sh
# Build bin/muster for `herdr plugin install`.
#
# Prefer a local Go toolchain, which produces an exact build of the cloned
# source. Without Go, fall back to the matching prebuilt release binary so
# installing works on a machine that has no Go at all.
set -eu

REPO="steig/muster"
OUT="bin/muster"

mkdir -p bin

if command -v go >/dev/null 2>&1; then
	go build -o "$OUT" .
	exit 0
fi

echo "muster: no Go toolchain found, downloading a prebuilt binary" >&2

case "$(uname -s)" in
Darwin) os="darwin" ;;
Linux) os="linux" ;;
*) echo "muster: unsupported OS $(uname -s); install Go and retry" >&2; exit 1 ;;
esac

case "$(uname -m)" in
arm64 | aarch64) arch="arm64" ;;
x86_64 | amd64) arch="amd64" ;;
*) echo "muster: unsupported architecture $(uname -m); install Go and retry" >&2; exit 1 ;;
esac

asset="muster_${os}_${arch}"
base="https://github.com/$REPO/releases/latest/download"

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		echo "muster: neither curl nor wget available; install Go and retry" >&2
		exit 1
	fi
}

fetch "$base/$asset" "$OUT"
fetch "$base/checksums.txt" bin/checksums.txt

# Verify before making it executable. This script downloads a binary over the
# network and hands it to herdr to run, which makes an unverified download the
# weakest link in the whole install. A missing or mismatched checksum is fatal
# rather than a warning: continuing would run the thing we just failed to
# vouch for.
expected=$(awk -v want="$asset" '$2 == want || $2 == "*"want {print $1}' bin/checksums.txt)
if [ -z "$expected" ]; then
	echo "muster: no checksum published for $asset; refusing to install it" >&2
	rm -f "$OUT" bin/checksums.txt
	exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$OUT" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
	actual=$(shasum -a 256 "$OUT" | awk '{print $1}')
else
	echo "muster: no sha256 tool to verify the download; install Go and retry" >&2
	rm -f "$OUT" bin/checksums.txt
	exit 1
fi

if [ "$expected" != "$actual" ]; then
	echo "muster: checksum mismatch for $asset" >&2
	echo "  expected $expected" >&2
	echo "  actual   $actual" >&2
	rm -f "$OUT" bin/checksums.txt
	exit 1
fi

rm -f bin/checksums.txt
chmod +x "$OUT"
