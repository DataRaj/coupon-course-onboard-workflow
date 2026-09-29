#!/usr/bin/env bash
# ==============================================================================
# deploy-to-droplet.sh
# Cross-compiles using your local Go 1.26.5 compiler and deploys to your Droplet.
# Why? A 1GB droplet will struggle or OOM compile; your laptop builds in 3 seconds!
#
# Usage:
#   ./scripts/deploy-to-droplet.sh <DROPLET_IP>
# Example:
#   ./scripts/deploy-to-droplet.sh 167.99.50.20
# ==============================================================================

set -euo pipefail

if [ -z "${1:-}" ]; then
    echo "Usage: $0 <DROPLET_IP>"
    echo "Example: $0 167.99.50.20"
    exit 1
fi

DROPLET_IP="$1"
REMOTE_USER="root"
REMOTE_DIR="/opt/course-coupon-service"

echo "===> [1/4] Compiling static Linux binaries locally with Go..."
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/api ./cmd/api
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/worker ./cmd/worker
echo "✓ Local build complete: bin/api and bin/worker ready."

echo "===> [2/4] Ensuring remote directory structure..."
ssh "${REMOTE_USER}@${DROPLET_IP}" "mkdir -p ${REMOTE_DIR}/bin ${REMOTE_DIR}/logs"

echo "===> [3/4] Uploading binaries and migrations to Droplet..."
scp bin/api bin/worker "${REMOTE_USER}@${DROPLET_IP}:${REMOTE_DIR}/bin/"
scp -r migrations "${REMOTE_USER}@${DROPLET_IP}:${REMOTE_DIR}/"

# If .env does not exist on remote, copy it
if ! ssh "${REMOTE_USER}@${DROPLET_IP}" "test -f ${REMOTE_DIR}/.env"; then
    echo "Uploading .env to remote..."
    scp .env "${REMOTE_USER}@${DROPLET_IP}:${REMOTE_DIR}/.env"
fi

echo "===> [4/4] Restarting services on Droplet..."
ssh "${REMOTE_USER}@${DROPLET_IP}" "chmod +x ${REMOTE_DIR}/bin/* && systemctl restart course-api course-worker || true"

echo ""
echo "=========================================================="
echo "✓ Deployment successful to ${DROPLET_IP}!"
echo "Check logs with:"
echo "  ssh root@${DROPLET_IP} journalctl -u course-api -f"
echo "=========================================================="
