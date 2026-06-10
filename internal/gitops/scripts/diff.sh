#!/bin/bash
# Run git diff with provided arguments in the worktree
# Environment: PI_WORKTREE_PATH, PI_DIFF_ARGS

set -euo pipefail

cd "$PI_WORKTREE_PATH"

# Parse arguments
if [ -n "${PI_DIFF_ARGS:-}" ]; then
    # Handle special cases
    case "$PI_DIFF_ARGS" in
        --staged|-staged)
            git diff --staged
            ;;
        *)
            # shellcheck disable=SC2086
            git diff $PI_DIFF_ARGS
            ;;
    esac
else
    # Default: show all changes including untracked files
    # Use git add -N (intent to add) to make untracked files visible to git diff
    # This doesn't actually stage them, just marks them for tracking
    
    # Check if there are any untracked files
    if [ -n "$(git ls-files --others --exclude-standard)" ]; then
        # Create a temporary index to avoid modifying the real one
        export GIT_INDEX_FILE="$(mktemp -t git-index.XXXXXX)"
        trap "rm -f '$GIT_INDEX_FILE'" EXIT
        
        # Copy current index
        cp "$(git rev-parse --git-dir)/index" "$GIT_INDEX_FILE" 2>/dev/null || true
        
        # Add untracked files with intent-to-add flag
        git add -N . 2>/dev/null || true
        
        # Show diff
        git diff
        
        # Reset the intent-to-add flags to keep working directory clean
        git reset HEAD . 2>/dev/null || true
    else
        # No untracked files, just show normal diff
        git diff
    fi
fi
