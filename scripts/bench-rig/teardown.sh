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
set -uo pipefail

REGION="${AWS_REGION:-ap-south-1}"
TAG_KEY="Project"
TAG_VAL="velox-bench"
CHECK_ONLY=0
[ "${1:-}" = "--check" ] && CHECK_ONLY=1

say() { printf '%s\n' "$*"; }
aws_() { aws --region "$REGION" "$@"; }

# ---- inventory ------------------------------------------------------------
instances=$(aws_ ec2 describe-instances \
  --filters "Name=tag:$TAG_KEY,Values=$TAG_VAL" \
            "Name=instance-state-name,Values=pending,running,stopping,stopped" \
  --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null | tr '\t' ' ')

dbs=$(aws_ rds describe-db-instances \
  --query "DBInstances[?starts_with(DBInstanceIdentifier, 'velox-bench')].DBInstanceIdentifier" \
  --output text 2>/dev/null | tr '\t' ' ')

sgs=$(aws_ ec2 describe-security-groups \
  --filters "Name=tag:$TAG_KEY,Values=$TAG_VAL" \
  --query 'SecurityGroups[].GroupId' --output text 2>/dev/null | tr '\t' ' ')

keys=$(aws_ ec2 describe-key-pairs \
  --filters "Name=tag:$TAG_KEY,Values=$TAG_VAL" \
  --query 'KeyPairs[].KeyName' --output text 2>/dev/null | tr '\t' ' ')

subnetgroups=$(aws_ rds describe-db-subnet-groups \
  --query "DBSubnetGroups[?starts_with(DBSubnetGroupName, 'velox-bench')].DBSubnetGroupName" \
  --output text 2>/dev/null | tr '\t' ' ')

say "inventory in $REGION:"
say "  instances:      ${instances:-<none>}"
say "  rds:            ${dbs:-<none>}"
say "  security groups:${sgs:+ $sgs}${sgs:-<none>}"
say "  key pairs:      ${keys:-<none>}"
say "  db subnet grps: ${subnetgroups:-<none>}"

if [ "$CHECK_ONLY" = "1" ]; then
  if [ -z "${instances}${dbs}${sgs}${keys}${subnetgroups}" ]; then
    say "CLEAN — nothing is running, nothing is billing"
  else
    say "NOT CLEAN — run ./teardown.sh to remove the above"
  fi
  exit 0
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
