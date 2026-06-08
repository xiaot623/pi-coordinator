#!/usr/bin/env bash
set -euo pipefail

plural_changed() {
  local count="$1"
  if [[ "$count" == "1" ]]; then
    printf '1 file changed\n'
    return
  fi
  printf '%s files changed\n' "$count"
}

path="${PI_DETAIL_PATH:-}"

echo "RESULT=ok"

if [[ -z "$path" || ! -d "$path" ]]; then
  echo "GIT_AVAILABLE=0"
  exit 0
fi

if ! git -C "$path" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "GIT_AVAILABLE=0"
  exit 0
fi

repo_root="$(git -C "$path" rev-parse --show-toplevel 2>/dev/null || true)"
branch="$(git -C "$path" symbolic-ref --quiet --short HEAD 2>/dev/null || echo HEAD)"
head="$(git -C "$path" rev-parse --short HEAD 2>/dev/null || echo -)"

status_output="$(git -C "$path" status --porcelain 2>/dev/null || true)"
staged="$(printf '%s\n' "$status_output" | awk 'BEGIN{c=0} {x=substr($0,1,1); if (x != "" && x != " " && x != "?") c++} END{print c+0}')"
unstaged="$(printf '%s\n' "$status_output" | awk 'BEGIN{c=0} {y=substr($0,2,1); if (y != "" && y != " ") c++} END{print c+0}')"
untracked="$(printf '%s\n' "$status_output" | awk 'BEGIN{c=0} /^\?\?/ {c++} END{print c+0}')"

numstat_file="$(mktemp)"
cleanup() {
  rm -f "$numstat_file"
}
trap cleanup EXIT

if git -C "$path" rev-parse --verify HEAD >/dev/null 2>&1; then
  git -C "$path" diff --numstat --find-renames HEAD > "$numstat_file"
else
  empty_tree="$(git -C "$path" hash-object -t tree /dev/null)"
  git -C "$path" diff --cached --numstat --find-renames "$empty_tree" > "$numstat_file"
fi

tracked_files="$(awk 'NF{c++} END{print c+0}' "$numstat_file")"
tracked_additions="$(awk 'NF{a=$1; if (a == "-") a=0; sum+=a} END{print sum+0}' "$numstat_file")"
tracked_deletions="$(awk 'NF{d=$2; if (d == "-") d=0; sum+=d} END{print sum+0}' "$numstat_file")"
changed_files=$((tracked_files + untracked))

echo "GIT_AVAILABLE=1"
echo "REPO_ROOT=$repo_root"
echo "BRANCH=$branch"
echo "HEAD=$head"
echo "WORKING_TREE=$staged staged · $unstaged unstaged · $untracked untracked"
echo "DIFF_STAT=$(plural_changed "$changed_files" | tr -d '\n'), +$tracked_additions -$tracked_deletions"

while IFS=$'\t' read -r added deleted file_path; do
  [[ -z "${file_path:-}" ]] && continue
  [[ "$added" == "-" ]] && added=0
  [[ "$deleted" == "-" ]] && deleted=0
  printf 'FILE=%s\t%s\t%s\n' "$file_path" "$added" "$deleted"
done < "$numstat_file"

while IFS= read -r line; do
  [[ "$line" == '?? '* ]] || continue
  file_path="${line#?? }"
  printf 'FILE=%s\t0\t0\tuntracked\n' "$file_path"
done <<< "$status_output"
