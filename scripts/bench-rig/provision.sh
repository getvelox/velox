#!/usr/bin/env bash
# Provisions the Track B single-node benchmark rig in ap-south-1.
#
# Shape, and why:
#   app      c7g.large   (2 vCPU)  <- THE node under measurement
#   loadgen  c7g.xlarge  (4 vCPU)  <- deliberately BIGGER than the app, so the
#                                     generator is never the bottleneck. The
#                                     laptop attempt failed exactly here: load
#                                     generator, server and database shared 8
#                                     cores and the numbers measured contention.
#   rds      db.m7g.large, PostgreSQL 16, Single-AZ, 100GB gp3
#
# All three pinned to ap-south-1a. Cross-AZ traffic is billed per GB and is the
# only cost line that scales with throughput — same-AZ is free and is also the
# lower-latency choice a p99 measurement wants.
#
# Public subnet, no NAT gateway: NAT costs more per hour than an app node and
# has zero effect on the measured numbers, because benchmark traffic never
# leaves the VPC. Ingress is restricted by security group instead.
#
# No IAM instance profile — the velox-benchmark policy denies IAM writes by
# design, and the rig does not need one: instances build from the PUBLIC repo.
#
# Refuses to run without --yes, because this bills from the moment it succeeds.
set -euo pipefail

REGION="${AWS_REGION:-ap-south-1}"
AZ="${AZ:-ap-south-1a}"
# Overridable so the hardware question can be answered without editing this
# file mid-run. Defaults are the small rig: the point of a small app node is
# that the measurement is clearly about the software, not the machine.
APP_TYPE="${APP_TYPE:-c7g.large}"
GEN_TYPE="${GEN_TYPE:-c7g.xlarge}"
DB_CLASS="${DB_CLASS:-db.m7g.large}"
TAGS="Key=Project,Value=velox-bench"
OUT="${OUT:-$HOME/.velox-bench-rig}"
BRANCH="${BRANCH:-main}"

[ "${1:-}" = "--yes" ] || { echo "refusing to provision without --yes (this starts billing)"; exit 1; }

mkdir -p "$OUT"; chmod 700 "$OUT"
aws_() { aws --region "$REGION" "$@"; }
say() { printf '\n== %s\n' "$*"; }

VPC=$(aws_ ec2 describe-vpcs --filters Name=isDefault,Values=true --query 'Vpcs[0].VpcId' --output text)
SUBNET=$(aws_ ec2 describe-subnets --filters Name=vpc-id,Values="$VPC" Name=availability-zone,Values="$AZ" \
  --query 'Subnets[0].SubnetId' --output text)
SUBNET_B=$(aws_ ec2 describe-subnets --filters Name=vpc-id,Values="$VPC" Name=availability-zone,Values=ap-south-1b \
  --query 'Subnets[0].SubnetId' --output text)
AMI=$(aws_ ssm get-parameter --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-6.1-arm64 \
  --query 'Parameter.Value' --output text)
MYIP="$(curl -s https://checkip.amazonaws.com | tr -d '[:space:]')/32"
say "vpc=$VPC subnet=$SUBNET ($AZ) ami=$AMI ssh-from=$MYIP"
say "shape: app=$APP_TYPE loadgen=$GEN_TYPE db=$DB_CLASS"

say "key pair"
if ! aws_ ec2 describe-key-pairs --key-names velox-bench-key >/dev/null 2>&1; then
  aws_ ec2 create-key-pair --key-name velox-bench-key --tag-specifications "ResourceType=key-pair,Tags=[{$TAGS}]" \
    --query 'KeyMaterial' --output text > "$OUT/velox-bench-key.pem"
  chmod 600 "$OUT/velox-bench-key.pem"
fi
echo "  private key: $OUT/velox-bench-key.pem"

say "security group"
SG=$(aws_ ec2 describe-security-groups --filters Name=group-name,Values=velox-bench-sg Name=vpc-id,Values="$VPC" \
  --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null)
if [ "$SG" = "None" ] || [ -z "$SG" ]; then
  SG=$(aws_ ec2 create-security-group --group-name velox-bench-sg --description "velox track B bench rig" \
    --vpc-id "$VPC" --tag-specifications "ResourceType=security-group,Tags=[{$TAGS}]" \
    --query 'GroupId' --output text)
  aws_ ec2 authorize-security-group-ingress --group-id "$SG" --protocol tcp --port 22 --cidr "$MYIP" >/dev/null
  # App and DB ports are reachable only from inside the group, never the internet.
  aws_ ec2 authorize-security-group-ingress --group-id "$SG" --protocol tcp --port 8080 --source-group "$SG" >/dev/null
  aws_ ec2 authorize-security-group-ingress --group-id "$SG" --protocol tcp --port 5432 --source-group "$SG" >/dev/null
fi
echo "  sg=$SG"

say "RDS (longest lead time — started first, ~6-10 min)"
DBPASS_FILE="$OUT/db-password"
[ -f "$DBPASS_FILE" ] || { openssl rand -hex 20 > "$DBPASS_FILE"; chmod 600 "$DBPASS_FILE"; }
DBPASS=$(cat "$DBPASS_FILE")
if ! aws_ rds describe-db-subnet-groups --db-subnet-group-name velox-bench-subnets >/dev/null 2>&1; then
  # RDS requires >=2 AZs in a subnet group even for a Single-AZ instance; the
  # instance itself is pinned to $AZ below, which is what actually matters.
  aws_ rds create-db-subnet-group --db-subnet-group-name velox-bench-subnets \
    --db-subnet-group-description "velox bench" --subnet-ids "$SUBNET" "$SUBNET_B" \
    --tags "$TAGS" >/dev/null
fi
if ! aws_ rds describe-db-instances --db-instance-identifier velox-bench-db >/dev/null 2>&1; then
  aws_ rds create-db-instance --db-instance-identifier velox-bench-db \
    --db-instance-class "$DB_CLASS" --engine postgres --engine-version 16.14 \
    --master-username velox --master-user-password "$DBPASS" \
    --allocated-storage 100 --storage-type gp3 \
    --db-subnet-group-name velox-bench-subnets --vpc-security-group-ids "$SG" \
    --availability-zone "$AZ" --no-multi-az --no-publicly-accessible \
    --backup-retention-period 0 --no-auto-minor-version-upgrade \
    --tags "$TAGS" >/dev/null
fi
echo "  velox-bench-db creating (password in $DBPASS_FILE)"

# Both run modes are built, because they answer different questions and only
# one of them is the number a self-hoster actually gets.
#
#   container  - the image our Dockerfile produces, which is what deploy/compose
#                runs. THIS IS THE HEADLINE NUMBER: it is what someone
#                following our own deployment docs will see.
#   bare binary - the same code with the container layer removed. Not what we
#                ship, so not the headline; it isolates what containerisation
#                costs, which is a question self-hosters legitimately have.
#
# Publishing only the bare-binary figure would be the same error as quoting the
# in-process ingest number instead of the HTTP one: measuring the configuration
# that flatters us rather than the one the reader will run.
#
# NOTE the published figure still excludes nginx. deploy/compose puts nginx in
# front of velox, which is one more hop; this measures the app container
# directly and the writeup says so.
# THE HEREDOC IS QUOTED (<<'UD'). Nothing below expands here — it is a script
# for the instance, not for this shell. That distinction is load-bearing and was
# learned the expensive way: while unquoted, `$(uname -m)` evaluated on the
# OPERATOR'S MAC, which answers "arm64" where the instance answers "aarch64", so
# the k6 architecture was decided on the wrong machine and the wrong binary was
# baked in. Quoting makes the safe behaviour the default, so a future edit that
# adds a `$(...)` cannot silently reintroduce it.
#
# The branch is the one value that genuinely comes from here, so it is
# substituted explicitly after the heredoc, where the exception is visible.
USERDATA=$(cat <<'UD'
#!/bin/bash
set -x
# HOME first, before ANY command that needs it. cloud-init runs user-data with
# no HOME at all: Go then fails with "module cache not found: neither GOMODCACHE
# nor GOPATH is set", and `git config --global` cannot find ~/.gitconfig either.
# docs/benchmarks/sustained-throughput.md has prescribed this since the first
# rig run; provision.sh set only half of it, and set it too late.
# A container test cannot catch this — Docker supplies HOME=/root for you.
export HOME=/root GOPATH=/root/go
export GOTOOLCHAIN=auto GOSUMDB=sum.golang.org
FAIL=0
# Every step is checked. The previous version redirected each failure into its
# own log and then touched READY unconditionally, so an instance whose builds
# had all failed still advertised itself as provisioned.
run() { local log=$1; shift; "$@" >"$log" 2>&1 || { echo "FAILED: $* (see $log)" >>/tmp/failures.log; FAIL=1; }; }

run /tmp/install.log dnf install -y git golang postgresql16 postgresql16-contrib docker tar gzip procps-ng   # -contrib = pgbench (db-ceiling.sh); procps-ng = vmstat (measure.sh capture)
run /tmp/docker.log systemctl enable --now docker
cd /opt || exit 1
run /tmp/clone.log git clone --depth 1 --branch __BRANCH__ https://github.com/getvelox/velox.git
cd /opt/velox || { echo "FAILED: clone produced no /opt/velox" >>/tmp/failures.log; touch /tmp/NOT_READY; exit 1; }
git config --global --add safe.directory /opt/velox
# Amazon Linux ships Go with GOTOOLCHAIN=local, so go build REFUSES when go.mod
# requires a newer patch release than the packaged one — and GOSUMDB=off then
# blocks downloading the right toolchain. Both must be overridden or every
# binary silently fails to build while user-data still reports success.
# The container image is unaffected: the Dockerfile pins its own toolchain.
#
# HOME and GOPATH are equally required and are the half that was missing here:
# cloud-init runs user-data with NO HOME, and Go then refuses with
# "module cache not found: neither GOMODCACHE nor GOPATH is set", failing all
# three builds. docs/benchmarks/sustained-throughput.md has documented this
# since the first rig run; provision.sh simply never applied it. It is invisible
# to a container test, because Docker sets HOME=/root for you.
run /tmp/build-velox.log go build -o /usr/local/bin/velox ./cmd/velox
run /tmp/build-seed.log  go build -o /usr/local/bin/velox-bench-seed ./cmd/velox-bench-seed
run /tmp/build-boot.log  go build -o /usr/local/bin/velox-bootstrap ./cmd/velox-bootstrap
run /tmp/build-image.log docker build -t velox:bench .

# k6 generates the load; velox-bench-seed only creates fixtures and mints a key.
# Installed on both instances because the loadgen is the one that needs it and
# the app node is where you end up debugging.
#
# The TARBALL is the only option on Graviton: k6 publishes no arm64 rpm or deb
# for any release, only linux-arm64.tar.gz. (The previous rpm URL was a 404 on
# every architecture.) The version is pinned to the one
# scripts/bench-rig/calibrate/ was proven against — running a k6 other than the
# calibrated one would invalidate that proof.
# if/elif rather than the more natural `case`, deliberately: macOS ships bash
# 3.2, whose parser loses track of `$( ... )` when the heredoc inside it
# contains an unbalanced `)` — which every `case` branch label has. The operator
# runs this script from a Mac, so a `case` here makes provision.sh fail to parse
# before it ever reaches AWS.
K6_VERSION=v2.2.0
K6_ARCH=unknown
if [ "$(uname -m)" = aarch64 ]; then
  K6_ARCH=arm64
elif [ "$(uname -m)" = x86_64 ]; then
  K6_ARCH=amd64
else
  echo "FAILED: unsupported arch $(uname -m)" >>/tmp/failures.log
  FAIL=1
fi
run /tmp/k6-dl.log curl -fsSL "https://github.com/grafana/k6/releases/download/${K6_VERSION}/k6-${K6_VERSION}-linux-${K6_ARCH}.tar.gz" -o /tmp/k6.tgz
run /tmp/k6-tar.log tar xzf /tmp/k6.tgz -C /tmp
run /tmp/k6-inst.log install -m 0755 "/tmp/k6-${K6_VERSION}-linux-${K6_ARCH}/k6" /usr/local/bin/k6
# Prove it EXECUTES on this architecture rather than merely landing on disk —
# an amd64 binary on aarch64 installs fine and then fails with "No such file or
# directory", which is what the old fallback produced.
run /tmp/k6-ver.log /usr/local/bin/k6 version

if [ "$FAIL" = "0" ]; then touch /tmp/READY; else touch /tmp/NOT_READY; fi
UD
)
# The single intentional local substitution — see the note above the heredoc.
USERDATA=${USERDATA//__BRANCH__/$BRANCH}

# --monitoring Enabled = EC2 detailed monitoring: 1-minute CloudWatch CPU points
# instead of 5-minute, so measure.sh's capture has more than one point per run.
# ~$0.003/hr per instance; the on-box vmstat sampler is still the primary source.
launch() {
  local name="$1" type="$2"
  if [ -n "$(aws_ ec2 describe-instances --filters "Name=tag:Name,Values=$name" \
        "Name=instance-state-name,Values=pending,running" --query 'Reservations[].Instances[].InstanceId' --output text)" ]; then
    echo "  $name already running"; return
  fi
  aws_ ec2 run-instances --image-id "$AMI" --instance-type "$type" --count 1 \
    --key-name velox-bench-key --security-group-ids "$SG" --subnet-id "$SUBNET" \
    --associate-public-ip-address \
    --monitoring Enabled=true \
    --user-data "$USERDATA" \
    --tag-specifications "ResourceType=instance,Tags=[{$TAGS},{Key=Name,Value=$name}]" \
    --query 'Instances[0].InstanceId' --output text
}

say "EC2"
APP=$(launch velox-bench-app "$APP_TYPE")
GEN=$(launch velox-bench-loadgen "$GEN_TYPE")
echo "  app=$APP loadgen=$GEN"

say "waiting for instances to run"
aws_ ec2 wait instance-running --instance-ids $APP $GEN
aws_ ec2 describe-instances --instance-ids $APP $GEN \
  --query 'Reservations[].Instances[].[Tags[?Key==`Name`]|[0].Value,InstanceId,PrivateIpAddress,PublicIpAddress]' --output text | tee "$OUT/instances.txt"

# Same-AZ is REQUESTED above (--availability-zone for RDS, a subnet in $AZ for
# the instances) but nothing so far has confirmed it happened. Verify, because
# the request can be honoured partially: the RDS subnet group must span two AZs
# to be created at all, so a capacity shortfall or a later failover can put the
# database in ap-south-1b while the app nodes sit in 1a.
#
# This is the one cost line that scales with throughput — every DB round trip
# becomes billable inter-AZ traffic, plausibly exceeding all the compute
# combined at 12k ev/s — and it also raises the per-event latency floor the
# whole benchmark is measuring. A rig that quietly spread across AZs would
# still produce confident-looking numbers, which is the failure mode worth
# spending ten lines to prevent.
say "verifying same-AZ placement"
INST_AZS=$(aws_ ec2 describe-instances --filters "Name=tag:Project,Values=velox-bench" \
  "Name=instance-state-name,Values=running,pending" \
  --query 'Reservations[].Instances[].Placement.AvailabilityZone' --output text | tr '\t' '\n' | sort -u)
DB_AZ=$(aws_ rds describe-db-instances --db-instance-identifier velox-bench-db \
  --query 'DBInstances[0].AvailabilityZone' --output text 2>/dev/null)
echo "  instances: $(echo "$INST_AZS" | tr '\n' ' ') | rds: ${DB_AZ:-<still creating>}"
if [ "$(echo "$INST_AZS" | wc -l | tr -d ' ')" != "1" ]; then
  echo "  FATAL: instances span multiple AZs — cross-AZ traffic would be billed AND measured" >&2
  echo "  tear down with ./teardown.sh before measuring anything" >&2
  exit 1
fi
# RDS reports its AZ only once it leaves 'creating'; an empty value here is not
# a pass, so say so rather than printing a checkmark over an unchecked thing.
if [ -z "$DB_AZ" ] || [ "$DB_AZ" = "None" ]; then
  echo "  RDS AZ not yet assigned — RE-CHECK before measuring:" >&2
  echo "    aws --region $REGION rds describe-db-instances --db-instance-identifier velox-bench-db --query 'DBInstances[0].AvailabilityZone' --output text" >&2
elif [ "$DB_AZ" != "$(echo "$INST_AZS" | tr -d ' ')" ]; then
  echo "  FATAL: app nodes are in $INST_AZS but RDS is in $DB_AZ" >&2
  echo "  every DB round trip would be billed inter-AZ; tear down and re-provision" >&2
  exit 1
else
  echo "  confirmed: instances and RDS all in $DB_AZ"
fi

say "PROVISIONED — billing has started. Tear down with ./teardown.sh"
