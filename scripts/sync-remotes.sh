#!/usr/bin/env bash
# 先推 AtomGit（origin），成功后再推 GitHub（github）。GitHub 永远不领先。
set -euo pipefail

branch="${1:-main}"

git push --follow-tags origin "$branch"
git push --follow-tags github "$branch"
