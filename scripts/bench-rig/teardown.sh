#!/usr/bin/env bash
# Destroys every resource the Track B benchmark rig creates.
#
# Written and tested BEFORE provision.sh, and safe to run when nothing exists.
# A benchmark rig that outlives its run is a standing bill, and the failure mode
# is silence — nobody notices an idle instance. So teardown is the first script,
# not the last, and it deletes by TAG rather than by a list of ids held in a
# file that a crashed run would never have written.
#
#   ./teardown.sh          # delete everything tagged Project=velox-bench
#   ./teardown.sh --check  # report what exists, delete nothing
#
# Exit codes are load-bearing — the watchdog and any automation branch on them:
#   0  CLEAN    nothing exists (and every query genuinely succeeded)
#   1  NOT CLEAN something still exists and is billing
#   2  UNKNOWN  at least one inventory query FAILED, so nothing can be concluded
set -uo pipefail

REGION="${AWS_REGION:-ap-south-1}"
TAG_KEY="Project"
TAG_VAL="velox-bench"
CHECK_ONLY=0
case "${1:-}" in
  "") CHECK_ONLY=0 ;;
  --check) CHECK_ONLY=1 ;;
  *) printf 'usage: %s [--check]\n' "$0" >&2; exit 64 ;;
esac

say() { printf '%s\n' "$*"; }
aws_() { aws --region "$REGION" "$@"; }

# ---- inventory ------------------------------------------------------------
# EVERY query's exit status is checked. Previously each ended in `2>/dev/null`
# with no status check, so a failure — expired credentials, wrong region, a
# denied permission, throttling, no network — produced an EMPTY result that was
# then reported as "CLEAN — nothing is running, nothing is billing".
#
# That is the most dangerous sentence this script can print, because it is most
# likely to be wrong exactly when the operator cannot see the account, and
# watchdog.sh stands down permanently on reading it.
#
# Failures are recorded in a FILE, not a variable: every call below runs inside
# `$( )`, which is a subshell, so an incremented counter would be discarded on
# return and the script would conclude "clean" from queries it knows failed.
# The first version of this fix did exactly that, and also wrote its error text
# to stdout — where it was captured INTO the inventory variable, making an
# unreadable account look like a populated one.
FAILFILE=$(mktemp)
trap 'rm -f "$FAILFILE"' EXIT
q() {
  local desc=$1; shift
  local out err rc
  err=$(mktemp)
  out=$(aws_ "$@" 2>"$err"); rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s: %s\n' "$desc" "$(tail -1 "$err")" >>"$FAILFILE"
    rm -f "$err"
    return 1
  fi
  rm -f "$err"
  printf '%s' "$out" | tr '\t' ' '
}

instances=$(q instances ec2 describe-instances \
  --filters "Name=tag:$TAG_KEY,Values=$TAG_VAL" \
            "Name=instance-state-name,Values=pending,running,stopping,stopped" \
  --query 'Reservations[].Instances[].InstanceId' --output text)

dbs=$(q rds rds describe-db-instances \
  --query "DBInstances[?starts_with(DBInstanceIdentifier, 'velox-bench')].DBInstanceIdentifier" \
  --output text)

sgs=$(q security-groups ec2 describe-security-groups \
  --filters "Name=tag:$TAG_KEY,Values=$TAG_VAL" \
  --query 'SecurityGroups[].GroupId' --output text)

keys=$(q key-pairs ec2 describe-key-pairs \
  --filters "Name=tag:$TAG_KEY,Values=$TAG_VAL" \
  --query 'KeyPairs[].KeyName' --output text)

subnetgroups=$(q db-subnet-groups rds describe-db-subnet-groups \
  --query "DBSubnetGroups[?starts_with(DBSubnetGroupName, 'velox-bench')].DBSubnetGroupName" \
  --output text)

say "inventory in $REGION:"
say "  instances:      ${instances:-<none>}"
say "  rds:            ${dbs:-<none>}"
say "  security groups: ${sgs:-<none>}"
say "  key pairs:      ${keys:-<none>}"
say "  db subnet grps: ${subnetgroups:-<none>}"

# A failed query is never "clean". Report UNKNOWN and refuse to conclude.
# wc, not grep -c: grep exits 1 on zero matches, so `|| echo 0` appended a
# SECOND zero and the comparison died on "0\n0: integer expression expected"
# — falling straight through to CLEAN, which is the outcome this whole block
# exists to prevent.
FAILED_QUERIES=$(wc -l < "$FAILFILE" | tr -d " ")
if [ "$FAILED_QUERIES" -gt 0 ]; then
  say ""
  say "these inventory queries FAILED:"
  sed 's/^/  /' "$FAILFILE"
  say ""
  say "UNKNOWN — $FAILED_QUERIES inventory query/queries FAILED (see above)."
  say "This is NOT 'clean'. The account may be billing; this script could not look."
  say "Check credentials and region ($REGION), then re-run."
  exit 2
fi

if [ "$CHECK_ONLY" = "1" ]; then
  if [ -z "${instances}${dbs}${sgs}${keys}${subnetgroups}" ]; then
    say "CLEAN — nothing is running, nothing is billing"
    exit 0
  fi
  say "NOT CLEAN — run ./teardown.sh to remove the above"
  exit 1
fi

# ---- delete, most-expensive first ----------------------------------------
# RDS is the largest line, so it goes first and we do not wait for it before
# killing compute: every second of overlap is paid twice.
for db in $dbs; do
  say "deleting RDS $db"
  aws_ rds delete-db-instance --db-instance-identifier "$db" \
    --skip-final-snapshot --delete-automated-backups >/dev/null 2>&1 \
    || say "  (already going or gone)"
done

if [ -n "$instances" ]; then
  say "terminating instances: $instances"
  aws_ ec2 terminate-instances --instance-ids $instances >/dev/null 2>&1 || true
  say "waiting for termination..."
  aws_ ec2 wait instance-terminated --instance-ids $instances 2>/dev/null || true
fi

# Security groups cannot be deleted while an ENI still references them, and RDS
# holds its ENI until the instance is really gone.
for db in $dbs; do
  say "waiting for RDS $db to finish deleting (SG deletion depends on it)..."
  aws_ rds wait db-instance-deleted --db-instance-identifier "$db" 2>/dev/null || true
done

for g in $subnetgroups; do
  say "deleting db subnet group $g"
  aws_ rds delete-db-subnet-group --db-subnet-group-name "$g" >/dev/null 2>&1 || say "  (retry later)"
done

for sg in $sgs; do
  say "deleting security group $sg"
  aws_ ec2 delete-security-group --group-id "$sg" >/dev/null 2>&1 || say "  (still referenced; re-run)"
done

for k in $keys; do
  say "deleting key pair $k"
  aws_ ec2 delete-key-pair --key-name "$k" >/dev/null 2>&1 || true
done

say ""
say "re-checking..."
exec "$0" --check
