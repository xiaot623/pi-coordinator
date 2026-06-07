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

rebase_target_ref() {
  local branch="$1"
  if git -C "$PI_WORKTREE_PATH" show-ref --verify --quiet "refs/remotes/origin/$branch"; then
    echo "refs/remotes/origin/$branch"
    return
  fi
  if git -C "$PI_WORKTREE_PATH" show-ref --verify --quiet "refs/heads/$branch"; then
    echo "refs/heads/$branch"
    return
  fi
  fail "could not resolve rebase target for branch $branch"
}

require_env PI_ORIGINAL_WORKSPACE
require_env PI_WORKTREE_PATH

if [[ ! -d "$PI_ORIGINAL_WORKSPACE" ]]; then
  fail "original workspace does not exist: $PI_ORIGINAL_WORKSPACE"
fi
if [[ ! -d "$PI_WORKTREE_PATH" ]]; then
  fail "worktree path does not exist: $PI_WORKTREE_PATH"
fi

git -C "$PI_WORKTREE_PATH" rev-parse --is-inside-work-tree >/dev/null 2>&1 || fail "worktree path is not a git worktree: $PI_WORKTREE_PATH"

base_branch="$(resolve_base_branch)"
if has_remote_origin; then
  git -C "$PI_WORKTREE_PATH" fetch origin --prune >/dev/null 2>&1 || fail "git fetch origin failed"
fi
target_ref="$(rebase_target_ref "$base_branch")"

stashed=0
if [[ -n "$(git -C "$PI_WORKTREE_PATH" status --short)" ]]; then
  stash_label="pi:/rebase:${PI_SESSION_ID:-unknown}"
  git -C "$PI_WORKTREE_PATH" stash push -u -m "$stash_label" >/dev/null
  stashed=1
fi

git -C "$PI_WORKTREE_PATH" rebase "$target_ref"

if [[ "$stashed" == "1" ]]; then
  git -C "$PI_WORKTREE_PATH" stash pop >/dev/null || fail "rebase succeeded but git stash pop reported conflicts; resolve them in $PI_WORKTREE_PATH"
fi

head_sha="$(git -C "$PI_WORKTREE_PATH" rev-parse HEAD)"
current_branch="$(git -C "$PI_WORKTREE_PATH" symbolic-ref --quiet --short HEAD 2>/dev/null || true)"

echo "RESULT=ok"
echo "BASE_BRANCH=$base_branch"
echo "TARGET_REF=$target_ref"
echo "WORKTREE_BRANCH=$current_branch"
echo "HEAD_SHA=$head_sha"
echo "STASHED=$stashed"
