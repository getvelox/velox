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
USERDATA=$(cat <<UD
#!/bin/bash
set -x
dnf install -y git golang postgresql16 docker >/tmp/install.log 2>&1
systemctl enable --now docker >>/tmp/install.log 2>&1
cd /opt && git clone --depth 1 --branch $BRANCH https://github.com/getvelox/velox.git >/tmp/clone.log 2>&1
cd /opt/velox
git config --global --add safe.directory /opt/velox
# Amazon Linux ships Go with GOTOOLCHAIN=local, so go build REFUSES when go.mod
# requires a newer patch release than the packaged one — and GOSUMDB=off then
# blocks downloading the right toolchain. Both must be overridden or every
# binary silently fails to build while user-data still reports success.
# The container image is unaffected: the Dockerfile pins its own toolchain.
export GOTOOLCHAIN=auto GOSUMDB=sum.golang.org
go build -o /usr/local/bin/velox ./cmd/velox >/tmp/build-velox.log 2>&1
go build -o /usr/local/bin/velox-bench ./cmd/velox-bench >/tmp/build-bench.log 2>&1
go build -o /usr/local/bin/velox-bootstrap ./cmd/velox-bootstrap >/tmp/build-boot.log 2>&1
docker build -t velox:bench . >/tmp/build-image.log 2>&1
touch /tmp/READY
UD
)

launch() {
  local name="$1" type="$2"
  if [ -n "$(aws_ ec2 describe-instances --filters "Name=tag:Name,Values=$name" \
        "Name=instance-state-name,Values=pending,running" --query 'Reservations[].Instances[].InstanceId' --output text)" ]; then
    echo "  $name already running"; return
  fi
  aws_ ec2 run-instances --image-id "$AMI" --instance-type "$type" --count 1 \
    --key-name velox-bench-key --security-group-ids "$SG" --subnet-id "$SUBNET" \
    --associate-public-ip-address \
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

say "PROVISIONED — billing has started. Tear down with ./teardown.sh"
