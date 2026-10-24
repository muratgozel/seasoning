#!/usr/bin/env bash
# Install or upgrade seasoning (macOS and Linux; amd64 and arm64).
#
#   curl -fsSL https://raw.githubusercontent.com/muratgozel/seasoning/main/scripts/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/muratgozel/seasoning/main/scripts/install.sh | bash -s -- v0.3.0
#
# Layout (shared with `seasoning update` and `make install`):
#   ${XDG_DATA_HOME:-~/.local/share}/seasoning/versions/<version>  one binary per version
#   ~/.local/bin/seasoning -> the active version                   symlink, swapped atomically
#
# Needs only curl, tar and sha256sum/shasum. Re-running upgrades. Env:
#   SEASONING_VERSION        tag to install (default: latest release; an argument wins)
#   SEASONING_REPO           GitHub owner/name (default: muratgozel/seasoning)
#   SEASONING_BIN_DIR        directory of the seasoning symlink (default: ~/.local/bin)
#   SEASONING_VERSIONS_DIR   directory of the version binaries
#   SEASONING_RELEASES_BASE  replaces https://github.com/<repo> (tests, mirrors)
#   SEASONING_INSTALL_HINTS  0 skips the closing setup hints (servers)
#
# internal/selfupdate implements the same steps in Go; keep them in sync.
set -euo pipefail

keep=3

die() {
  printf 'seasoning install: %s\n' "$1" >&2
  exit 1
}

info() {
  printf '%s\n' "$1"
}

usage() {
  printf 'usage: install.sh [vX.Y.Z]\n'
  printf 'Installs the latest seasoning release, or the given version.\n'
}

trim_slashes() {
  local s="$1"
  while [ "$s" != "/" ] && [ "${s%/}" != "$s" ]; do
    s="${s%/}"
  done
  printf '%s' "$s"
}

# Globals (not locals of main) because the EXIT trap runs after main returns.
tmp=""
staged=""
tmp_link=""
cleanup() {
  if [ -n "$tmp" ]; then rm -rf "$tmp"; fi
  if [ -n "$staged" ]; then rm -f "$staged"; fi
  if [ -n "$tmp_link" ]; then rm -f "$tmp_link"; fi
}

# Everything runs from main, called on the last line: bash parses the whole
# function before running any of it, so a truncated `curl | bash` download is
# a syntax error instead of a half-run installer.
main() {
  tag="${SEASONING_VERSION:-}"
  case $# in
    0) ;;
    1)
      case "$1" in
        -h | --help)
          usage
          exit 0
          ;;
      esac
      tag="$1"
      ;;
    *) die "expected at most one argument (a version such as v0.3.0), got $#" ;;
  esac

  [ -n "${HOME:-}" ] || die "HOME is not set"

  repo="${SEASONING_REPO:-muratgozel/seasoning}"
  if ! [[ "$repo" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || [[ "$repo" == *..* ]]; then
    die "SEASONING_REPO='$repo' is not a GitHub owner/name (e.g. muratgozel/seasoning)"
  fi
  base="${SEASONING_RELEASES_BASE:-https://github.com/$repo}"
  while [ "${base%/}" != "$base" ]; do base="${base%/}"; done
  case "$base" in
    https://?* | http://?*) ;;
    *) die "SEASONING_RELEASES_BASE='$base' must be an http(s) URL such as https://github.com/$repo" ;;
  esac
  bin_dir="${SEASONING_BIN_DIR:-$HOME/.local/bin}"
  versions_dir="${SEASONING_VERSIONS_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/seasoning/versions}"

  # --- prerequisites ---------------------------------------------------------
  for tool in curl tar mktemp; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool is required but was not found on PATH"
  done
  if command -v sha256sum >/dev/null 2>&1; then
    sha_cmd=(sha256sum)
  elif command -v shasum >/dev/null 2>&1; then
    sha_cmd=(shasum -a 256)
  else
    die "sha256sum or shasum is required to verify the download but neither was found on PATH"
  fi

  case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *) die "unsupported operating system '$(uname -s)'; seasoning supports macOS and Linux only" ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) die "unsupported architecture '$(uname -m)'; seasoning supports amd64 and arm64 only" ;;
  esac

  curl_opts=(-fsS --connect-timeout 15 --retry 2)
  case "$base" in
    https://*) curl_opts+=(--proto '=https' --proto-redir '=https' --tlsv1.2) ;;
  esac

  # --- version ---------------------------------------------------------------
  if [ -z "$tag" ]; then
    # /releases/latest redirects to /releases/tag/<tag> (or to /releases when
    # nothing is published). Reading the Location header needs no API token and
    # has no rate limit.
    headers="$(curl "${curl_opts[@]}" -I --max-time 30 "$base/releases/latest")" ||
      die "could not look up the latest release at $base/releases/latest (see the error above); pass a version instead: ... | bash -s -- v0.3.0"
    location="$(printf '%s\n' "$headers" | tr -d '\r' | awk 'tolower($1) == "location:" { loc = $2 } END { print loc }')"
    case "$location" in
      */releases/tag/?*) tag="${location##*/releases/tag/}" ;;
      *) die "no published release found at $base/releases" ;;
    esac
  fi

  # A version-dir file name: semver without the leading "v".
  version_re='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'
  [[ "${tag#v}" =~ $version_re ]] || die "'$tag' is not a version such as v0.3.0"
  ver="${tag#v}"
  tag="v$ver"

  # --- download and verify ---------------------------------------------------
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/seasoning-install.XXXXXX")" || die "could not create a temporary directory"
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  asset="seasoning_${ver}_${os}_${arch}.tar.gz"
  url="$base/releases/download/$tag"
  info "Downloading seasoning $ver ($os/$arch)..."
  curl "${curl_opts[@]}" -L --max-time 60 --max-filesize 1048576 -o "$tmp/checksums.txt" "$url/checksums.txt" ||
    die "could not download $url/checksums.txt: release $tag does not exist or has no checksums (see $base/releases); refusing to install an unverified binary"
  curl "${curl_opts[@]}" -L --max-time 300 --max-filesize 104857600 -o "$tmp/$asset" "$url/$asset" ||
    die "could not download $url/$asset: release $tag may have no build for $os/$arch"

  # Fail closed: no entry or a mismatch never installs anything.
  expected="$(awk -v f="$asset" '($2 == f || $2 == "*" f) && length($1) == 64 { print tolower($1); exit }' "$tmp/checksums.txt")"
  [ -n "$expected" ] || die "checksums.txt of release $tag has no entry for $asset; refusing to install an unverified binary"
  actual="$("${sha_cmd[@]}" "$tmp/$asset" | awk '{ print tolower($1) }')"
  [ "$actual" = "$expected" ] ||
    die "checksum mismatch for $asset (expected $expected, got $actual): the download is corrupt or was tampered with; nothing was installed"

  # Extract only the binary at the archive root; nothing else in the archive is
  # written, so entry paths cannot escape the temp dir.
  mkdir "$tmp/x"
  tar -xzf "$tmp/$asset" -C "$tmp/x" seasoning || die "$asset has no 'seasoning' binary at its root"
  if [ -L "$tmp/x/seasoning" ] || [ ! -f "$tmp/x/seasoning" ]; then
    die "'seasoning' in $asset is not a regular file; refusing to install it"
  fi

  # --- install ---------------------------------------------------------------
  mkdir -p "$versions_dir" || die "cannot create $versions_dir"
  versions_dir="$(CDPATH='' cd -- "$versions_dir" && pwd)" # absolute symlink targets
  dest="$versions_dir/$ver"
  [ ! -d "$dest" ] || die "$dest is a directory; remove it and re-run"
  staged="$versions_dir/.seasoning-$ver.$$.tmp"
  cp "$tmp/x/seasoning" "$staged" || die "cannot write to $versions_dir"
  chmod 755 "$staged"
  # Stage next to the destination, then rename: atomic, and replacing a running
  # binary in place would fail with "text file busy".
  mv -f "$staged" "$dest" || die "cannot install $dest"
  staged=""
  "$dest" version </dev/null >/dev/null 2>&1 ||
    die "$dest does not run on this machine; the previously active version (if any) is unchanged"

  # --- activate --------------------------------------------------------------
  mkdir -p "$bin_dir" || die "cannot create $bin_dir"
  link="$bin_dir/seasoning"
  # `mv` onto a directory (or a symlink to one) would move into it instead of
  # replacing it, on both BSD and GNU mv.
  [ ! -d "$link" ] || die "$link is a directory; remove it and re-run"
  prev=""
  if [ -L "$link" ]; then
    prev_target="$(readlink "$link" || true)"
    case "$prev_target" in
      "$versions_dir"/*) prev="${prev_target##*/}" ;;
    esac
  fi
  tmp_link="$bin_dir/.seasoning.$$.tmp"
  rm -f "$tmp_link"
  ln -s "$dest" "$tmp_link" || die "cannot create a symlink in $bin_dir"
  # rename(2) swaps the link atomically; the seasoning command never disappears.
  mv -f "$tmp_link" "$link" || die "cannot update $link"
  tmp_link=""

  if [ -n "$prev" ] && [ "$prev" != "$ver" ]; then
    info "Updated $prev → $ver."
  fi
  info "Installed seasoning $ver: $link -> $dest"

  # --- prune -----------------------------------------------------------------
  # Keep the $keep most recent versions by mtime plus the active one. Only
  # version-named regular files are candidates, so a misconfigured
  # SEASONING_VERSIONS_DIR never loses unrelated files.
  #
  # SC2012: ls -t is the only portable (BSD + GNU) mtime sort; the names are
  # validated version strings below, so they contain no whitespace.
  n=0
  # shellcheck disable=SC2012
  while IFS= read -r name; do
    if [ "$name" != "dev" ] && ! [[ "$name" =~ $version_re ]]; then
      continue
    fi
    if [ -L "$versions_dir/$name" ] || [ ! -f "$versions_dir/$name" ]; then
      continue
    fi
    n=$((n + 1))
    if [ "$n" -le "$keep" ] || [ "$name" = "$ver" ]; then
      continue
    fi
    rm -f "$versions_dir/$name" || printf 'seasoning install: warning: could not remove old version %s\n' "$versions_dir/$name" >&2
  done < <(ls -1t "$versions_dir")

  # --- PATH hint -------------------------------------------------------------
  # Trailing slashes are cosmetic in a path but not in a string compare, so both
  # sides are normalized ("$HOME/.local/bin/" on PATH counts).
  bin_dir_norm="$(trim_slashes "$bin_dir")"
  path_norm=""
  saved_ifs="${IFS-}"
  set -f # PATH entries are literal, never globs
  IFS=:
  for entry in ${PATH:-}; do
    path_norm="${path_norm}:$(trim_slashes "$entry")"
  done
  set +f
  IFS="$saved_ifs"

  # SC2016: $PATH and $SHELL must stay literal: these lines are printed for the
  # user to paste into their rc file, not expanded here.
  # shellcheck disable=SC2016
  case "${path_norm}:" in
    *":$bin_dir_norm:"*) ;;
    *)
      printf '\n%s is not on your PATH. Add it:\n' "$bin_dir_norm"
      printf '  zsh:  echo '\''export PATH="%s:$PATH"'\'' >> ~/.zshrc\n' "$bin_dir_norm"
      printf '  bash: echo '\''export PATH="%s:$PATH"'\'' >> ~/.bashrc\n' "$bin_dir_norm"
      printf 'Then open a new terminal, or run: source ~/.zshrc (zsh) / source ~/.bashrc (bash).\n'
      ;;
  esac

  if [ "${SEASONING_INSTALL_HINTS:-1}" != 0 ]; then
    printf '\nNext: seasoning setup, then install the Claude Code plugin:\n'
    printf '  claude plugin marketplace add muratgozel/seasoning\n'
    printf '  claude plugin install seasoning@seasoning\n'
  fi
}

main "$@"
