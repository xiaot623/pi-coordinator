#!/usr/bin/env bash
set -euo pipefail

system_summary() {
  local os_name arch version pretty
  os_name="$(uname -s 2>/dev/null || echo Unknown)"
  arch="$(uname -m 2>/dev/null || echo unknown)"
  case "$os_name" in
    Darwin)
      version="$(sw_vers -productVersion 2>/dev/null || true)"
      pretty="macOS"
      [[ -n "$version" ]] && pretty="$pretty $version"
      ;;
    Linux)
      pretty="Linux"
      if [[ -r /etc/os-release ]]; then
        version="$(awk -F= '/^PRETTY_NAME=/{gsub(/^"|"$/, "", $2); print $2; exit}' /etc/os-release)"
        [[ -n "$version" ]] && pretty="$version"
      fi
      ;;
    *)
      pretty="$os_name"
      ;;
  esac
  printf '%s · %s\n' "$pretty" "$arch"
}

plural_files() {
  local count="$1"
  if [[ "$count" == "1" ]]; then
    printf '1 file\n'
    return
  fi
  printf '%s files\n' "$count"
}

path="${PI_DETAIL_PATH:-}"

echo "RESULT=ok"
echo "SYSTEM_SUMMARY=$(system_summary)"

if [[ -z "$path" || ! -d "$path" ]]; then
  echo "GIT_AVAILABLE=0"
  echo "GIT_SUMMARY=-"
  exit 0
fi

if ! git -C "$path" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "GIT_AVAILABLE=0"
  echo "GIT_SUMMARY=-"
  exit 0
fi

branch="$(git -C "$path" symbolic-ref --quiet --short HEAD 2>/dev/null || echo HEAD)"
head="$(git -C "$path" rev-parse --short HEAD 2>/dev/null || echo -)"
changed_files="$(git -C "$path" status --porcelain 2>/dev/null | wc -l | tr -d ' ')"
dirty="0"
summary="$branch @ $head · clean"
if [[ "$changed_files" != "0" ]]; then
  dirty="1"
  summary="$branch @ $head · dirty · $(plural_files "$changed_files")"
  summary="${summary%$'\n'}"
fi

repo_root="$(git -C "$path" rev-parse --show-toplevel 2>/dev/null || true)"

echo "GIT_AVAILABLE=1"
echo "REPO_ROOT=$repo_root"
echo "GIT_BRANCH=$branch"
echo "GIT_HEAD=$head"
echo "GIT_DIRTY=$dirty"
echo "GIT_CHANGED_FILES=$changed_files"
echo "GIT_SUMMARY=$summary"
