# Images pinned by digest for scripts/gen-baseline.sh and
# scripts/ci/migration-convergence.sh (ADR-125). Sourced, not run.
#
# VELOX_V081_IMAGE is the release whose 178 migrations the baseline
# replaces. It stays v0.8.1 until the next baseline.
# shellcheck disable=SC2034
POSTGRES16_IMAGE="postgres:16.15-alpine@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea"
POSTGRES14_IMAGE="postgres:14.24-alpine@sha256:4ea9e5ed06591da7ea23eb65465e8d3187fe79f4d5ec3ae976d29a33b013e77a"
VELOX_V081_IMAGE="ghcr.io/getvelox/velox:v0.8.1@sha256:58fd137ef55bfa373174a19e342c8e0ee668b83afe37508f80681c3e37df1db5"
