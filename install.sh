#!/bin/sh
set -eu

repo=https://github.com/nostalume/mountweave-cli

fail() {
    printf 'mweave install: %s\n' "$*" >&2
    exit 1
}

version=
bin_dir=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --version|--bin-dir)
            [ "$#" -ge 2 ] && [ -n "$2" ] || fail "$1 needs a value"
            case "$1" in
                --version) version=$2 ;;
                --bin-dir) bin_dir=$2 ;;
            esac
            shift 2
            ;;
        -h|--help)
            printf 'Usage: sh install.sh [--version TAG] [--bin-dir DIR]\n'
            exit 0
            ;;
        *) fail "unknown option $1" ;;
    esac
done

case $(uname -s) in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) fail 'this installer requires Linux or macOS' ;;
esac
case $(uname -m) in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) fail 'unsupported CPU architecture' ;;
esac

if [ -z "$bin_dir" ]; then
    bin_dir=${HOME:?Set HOME or use --bin-dir}/.local/bin
fi
case "$bin_dir" in
    /*) ;;
    *) bin_dir=$PWD/$bin_dir ;;
esac

download() {
    curl -q --proto '=https' --proto-redir '=https' -fsSL "$@"
}

if [ -z "$version" ]; then
    release=$(download -o /dev/null -w '%{url_effective}' "$repo/releases/latest") ||
        fail 'could not find the latest stable release'
    case "$release" in
        "$repo/releases/tag/"*) version=${release#"$repo/releases/tag/"} ;;
        *) fail 'no stable release found; pass a release tag with --version' ;;
    esac
fi
case "$version" in
    *[!0-9A-Za-z.-]*) fail 'expected a release tag such as v0.1.0' ;;
esac
printf '%s\n' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$' ||
    fail 'expected a release tag such as v0.1.0'

tmp=$(mktemp -d "${TMPDIR:-/tmp}/mweave.XXXXXXXX")
staged=
cleanup() {
    if [ -n "$staged" ]; then
        rm -f "$staged"
    fi
    rm -f "$tmp/SHA256SUMS" "$tmp/archive"
    rmdir "$tmp"
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

base=$repo/releases/download/$version
asset=mweave_${version}_${os}_${arch}.tar.gz
download -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" ||
    fail "could not download checksums for $version"
expected=$(awk -v asset="$asset" '
    { sub(/\r$/, "") }
    NF == 2 && $2 == asset {
        hash = tolower($1)
        if (length(hash) != 64 || hash ~ /[^0-9a-f]/) exit 1
        print hash
        matches++
    }
    END { if (matches != 1) exit 1 }
' "$tmp/SHA256SUMS") || fail "no unique SHA-256 checksum for $asset"
download -o "$tmp/archive" "$base/$asset" || fail "could not download $asset"
if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum < "$tmp/archive")
else
    actual=$(shasum -a 256 < "$tmp/archive")
fi
[ "${actual%% *}" = "$expected" ] || fail "SHA-256 mismatch for $asset"

mkdir -p "$bin_dir"
target=$bin_dir/mweave
[ ! -d "$target" ] || fail "$target is a directory"
staged=$(mktemp "$bin_dir/.mweave.XXXXXXXX")
tar -xOzf "$tmp/archive" mweave > "$staged" && [ -s "$staged" ] ||
    fail 'the archive does not contain the mweave executable'
chmod 755 "$staged"
mv -f "$staged" "$target" || fail "could not replace $target"
staged=

printf 'Installed mweave %s at %s\n' "$version" "$target"
case ":$PATH:" in
    *":$bin_dir:"*) ;;
    *) printf 'Add %s to PATH to use the mweave command.\n' "$bin_dir" ;;
esac
