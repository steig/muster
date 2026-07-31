#!/bin/sh
# Build bin/herdr-wt for `herdr plugin install`.
#
# Prefer a local Go toolchain, which produces an exact build of the cloned
# source. Without Go, fall back to the matching prebuilt release binary so
# installing works on a machine that has no Go at all.
set -eu

REPO="steig/herdr-wt"
OUT="bin/herdr-wt"

mkdir -p bin

if command -v go >/dev/null 2>&1; then
	go build -o "$OUT" .
	exit 0
fi

echo "herdr-wt: no Go toolchain found, downloading a prebuilt binary" >&2

case "$(uname -s)" in
Darwin) os="darwin" ;;
Linux) os="linux" ;;
*) echo "herdr-wt: unsupported OS $(uname -s); install Go and retry" >&2; exit 1 ;;
esac

case "$(uname -m)" in
arm64 | aarch64) arch="arm64" ;;
x86_64 | amd64) arch="amd64" ;;
*) echo "herdr-wt: unsupported architecture $(uname -m); install Go and retry" >&2; exit 1 ;;
esac

url="https://github.com/$REPO/releases/latest/download/herdr-wt_${os}_${arch}"

if command -v curl >/dev/null 2>&1; then
	curl -fsSL "$url" -o "$OUT"
elif command -v wget >/dev/null 2>&1; then
	wget -qO "$OUT" "$url"
else
	echo "herdr-wt: neither curl nor wget available; install Go and retry" >&2
	exit 1
fi

chmod +x "$OUT"
