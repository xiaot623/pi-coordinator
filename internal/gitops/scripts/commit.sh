#!/usr/bin/env bash
set -euo pipefail

fail() {
  echo "$*" >&2
  exit 1
}

require_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    fail "missing required env: $name"
  fi
}

has_remote_origin() {
  git -C "$PI_ORIGINAL_WORKSPACE" remote get-url origin >/dev/null 2>&1
}

resolve_base_branch() {
  local origin_head
  if origin_head=$(git -C "$PI_ORIGINAL_WORKSPACE" symbolic-ref --quiet refs/remotes/origin/HEAD 2>/dev/null); then
    origin_head="${origin_head#refs/remotes/origin/}"
    if [[ -n "$origin_head" ]]; then
      echo "$origin_head"
      return
    fi
  fi
  if git -C "$PI_ORIGINAL_WORKSPACE" show-ref --verify --quiet refs/heads/main || git -C "$PI_ORIGINAL_WORKSPACE" show-ref --verify --quiet refs/remotes/origin/main; then
    echo "main"
    return
  fi
  if git -C "$PI_ORIGINAL_WORKSPACE" show-ref --verify --quiet refs/heads/master || git -C "$PI_ORIGINAL_WORKSPACE" show-ref --verify --quiet refs/remotes/origin/master; then
    echo "master"
    return
  fi
  fail "could not determine the main branch (tried origin/HEAD, main, master)"
}

parse_commit_message() {
  local raw normalized subject body
  raw="${PI_COMMIT_MESSAGE_RAW:-}"
  normalized="${raw//$'\r'/}"
  if [[ "$normalized" == *$'\n'* ]]; then
    subject="${normalized%%$'\n'*}"
    body="${normalized#*$'\n'}"
  else
    subject="$normalized"
    body=""
  fi
  subject="$(printf '%s' "$subject" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
  if [[ -z "$subject" ]]; then
    fail "commit message subject is required: /commit <msg>"
  fi
  commit_message_file="$(mktemp)"
  printf '%s\n' "$subject" > "$commit_message_file"
  if [[ -n "$body" ]]; then
    printf '\n%s\n' "$body" >> "$commit_message_file"
  fi
}

cleanup() {
  if [[ -n "${commit_message_file:-}" && -f "$commit_message_file" ]]; then
    rm -f "$commit_message_file"
  fi
  if [[ -n "${restore_ref:-}" && -n "${current_ref:-}" && "$restore_ref" == "1" && "$current_ref" != "$base_branch" ]]; then
    if [[ "${current_ref_detached:-0}" == "1" ]]; then
      git -C "$PI_ORIGINAL_WORKSPACE" switch --detach "$current_ref" >/dev/null 2>&1 || true
    else
      git -C "$PI_ORIGINAL_WORKSPACE" switch "$current_ref" >/dev/null 2>&1 || true
    fi
  fi
}
trap cleanup EXIT

require_env PI_ORIGINAL_WORKSPACE
require_env PI_WORKTREE_PATH
require_env PI_WORKTREE_BRANCH
require_env PI_COMMIT_MESSAGE_RAW

if [[ ! -d "$PI_ORIGINAL_WORKSPACE" ]]; then
  fail "original workspace does not exist: $PI_ORIGINAL_WORKSPACE"
fi
if [[ ! -d "$PI_WORKTREE_PATH" ]]; then
  fail "worktree path does not exist: $PI_WORKTREE_PATH"
fi

git -C "$PI_WORKTREE_PATH" rev-parse --is-inside-work-tree >/dev/null 2>&1 || fail "worktree path is not a git worktree: $PI_WORKTREE_PATH"
git -C "$PI_ORIGINAL_WORKSPACE" rev-parse --is-inside-work-tree >/dev/null 2>&1 || fail "original workspace is not a git repository: $PI_ORIGINAL_WORKSPACE"

base_branch="$(resolve_base_branch)"
check_ref="refs/heads/$base_branch"
remote_ref=""
if has_remote_origin; then
  git -C "$PI_ORIGINAL_WORKSPACE" fetch origin --prune >/dev/null 2>&1 || fail "git fetch origin failed"
  if git -C "$PI_ORIGINAL_WORKSPACE" show-ref --verify --quiet "refs/remotes/origin/$base_branch"; then
    remote_ref="refs/remotes/origin/$base_branch"
    check_ref="$remote_ref"
  fi
fi

parse_commit_message
created_commit=0
if [[ -n "$(git -C "$PI_WORKTREE_PATH" status --short)" ]]; then
  git -C "$PI_WORKTREE_PATH" add -A
  git -C "$PI_WORKTREE_PATH" commit -F "$commit_message_file"
  created_commit=1
fi

if ! git -C "$PI_WORKTREE_PATH" merge-base --is-ancestor "$check_ref" HEAD; then
  fail "current branch is not based on $base_branch; run /rebase first"
fi
if [[ "$(git -C "$PI_WORKTREE_PATH" rev-parse "$check_ref")" == "$(git -C "$PI_WORKTREE_PATH" rev-parse HEAD)" ]]; then
  fail "current branch does not lead $base_branch; nothing to commit"
fi

if [[ -n "$(git -C "$PI_ORIGINAL_WORKSPACE" status --short)" ]]; then
  fail "original workspace has uncommitted changes; clean it before /commit"
fi

restore_ref=0
current_ref=""
current_ref_detached=0
if current_ref="$(git -C "$PI_ORIGINAL_WORKSPACE" symbolic-ref --quiet --short HEAD 2>/dev/null)"; then
  restore_ref=1
else
  current_ref="$(git -C "$PI_ORIGINAL_WORKSPACE" rev-parse HEAD)"
  current_ref_detached=1
  restore_ref=1
fi

if ! git -C "$PI_ORIGINAL_WORKSPACE" show-ref --verify --quiet "refs/heads/$base_branch"; then
  if [[ -n "$remote_ref" ]]; then
    git -C "$PI_ORIGINAL_WORKSPACE" branch --track "$base_branch" "origin/$base_branch" >/dev/null 2>&1 || fail "could not create local branch $base_branch from origin/$base_branch"
  else
    fail "local branch $base_branch does not exist"
  fi
fi

git -C "$PI_ORIGINAL_WORKSPACE" switch "$base_branch" >/dev/null
if [[ -n "$remote_ref" ]]; then
  git -C "$PI_ORIGINAL_WORKSPACE" merge --ff-only "origin/$base_branch" >/dev/null || fail "local $base_branch has diverged from origin/$base_branch"
fi
git -C "$PI_ORIGINAL_WORKSPACE" merge --ff-only "$PI_WORKTREE_BRANCH" >/dev/null || fail "could not fast-forward $base_branch from $PI_WORKTREE_BRANCH"

head_sha="$(git -C "$PI_ORIGINAL_WORKSPACE" rev-parse HEAD)"
echo "RESULT=ok"
echo "BASE_BRANCH=$base_branch"
echo "WORKTREE_BRANCH=$PI_WORKTREE_BRANCH"
echo "HEAD_SHA=$head_sha"
echo "CREATED_COMMIT=$created_commit"
