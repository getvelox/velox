#!/usr/bin/env bash
# Writes the next migration file in internal/platform/migrate/sql/.
#
#   make migration name=add_invoice_purchase_order                 a normal file
#   make migration name=idx_invoices_purchase_order concurrent=1   an index build
#
# For an index build, the name is also the index's name.
#
# The number is one above the highest NNNNN_name.sql file in any worktree
# of this clone or on any local or remote branch. Run `git fetch origin`
# first, so origin/main is current. A file on another machine's unpushed
# branch is not seen; the -short tests (no gaps, no duplicate versions)
# catch a clash before merge. The rules: docs/dev/migrations.md.
set -euo pipefail

name="${1:-}"
concurrent="${2:-}"

usage() {
  echo "usage: make migration name=<lowercase_name> [concurrent=1]" >&2
  echo "  name: lowercase letters, digits and underscores, for example add_invoice_purchase_order" >&2
  exit 2
}
[[ "$name" =~ ^[a-z0-9_]+$ ]] || usage
case "$concurrent" in
  "" | 0) concurrent="" ;;
  1) ;;
  *) usage ;;
esac

root="$(git rev-parse --show-toplevel)"
rel="internal/platform/migrate/sql"

highest=180 # migration 180 is the v0.8.1 baseline
seen_in="the v0.8.1 baseline"
consider() { # $1: a path, $2: where it was seen
  local base="${1##*/}"
  [[ "$base" =~ ^([0-9]{5})_[a-z0-9_]+\.sql$ ]] || return 0
  local v=$((10#${BASH_REMATCH[1]}))
  if ((v > highest)); then
    highest=$v
    seen_in="$2"
  fi
}

while IFS= read -r wt; do
  for f in "$wt/$rel"/*.sql; do
    [[ -e "$f" ]] && consider "$f" "worktree $wt"
  done
done < <(git -C "$root" worktree list --porcelain | sed -n 's/^worktree //p')

# One git grep reads every branch's tip at once; the pathspec opens only
# five-digit files, so it stays fast with hundreds of branches.
commits="$(git -C "$root" for-each-ref --format='%(objectname)' refs/heads refs/remotes | sort -u)"
seen_commit=""
while IFS= read -r hit; do
  before=$highest
  consider "${hit#*:}" "branch"
  if ((highest > before)); then seen_commit="${hit%%:*}"; fi
done < <(
  # shellcheck disable=SC2086 # one argument per commit
  git -C "$root" grep -l -e '' $commits -- "$rel/[0-9][0-9][0-9][0-9][0-9]_*.sql" 2>/dev/null
)
if [[ "$seen_in" == "branch" ]]; then
  seen_in="branch $(git -C "$root" for-each-ref --points-at "$seen_commit" --format='%(refname:short)' refs/heads refs/remotes | head -n 1)"
fi

next="$(printf '%05d' $((highest + 1)))"
file="$rel/${next}_${name}.sql"

if [[ -n "$concurrent" ]]; then
  body="-- An index build: goose runs it without a transaction, and the file keeps
-- exactly this shape (docs/dev/migrations.md). Replace your_table (your_column).
-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = 0;
DROP INDEX CONCURRENTLY IF EXISTS ${name};
CREATE INDEX CONCURRENTLY ${name} ON your_table (your_column);
RESET lock_timeout;
-- +goose Down
SET lock_timeout = 0;
DROP INDEX CONCURRENTLY IF EXISTS ${name};
RESET lock_timeout;"
else
  body="-- +goose Up
-- TODO: what this migration changes, and why (docs/dev/migrations.md).


-- +goose Down
-- TODO: undo the Up.
"
fi

if [[ -e "$root/$file" ]]; then
  echo "$file already exists" >&2
  exit 1
fi
printf '%s\n' "$body" >"$root/$file"
echo "Wrote $file"
echo "The highest number before it was $(printf '%05d' "$highest"), in $seen_in."
