#!/usr/bin/env bash
# Tear down the OCS cluster. Pass -v to also delete the shared OCS install volume
# (required before starting with a different OCS_VERSION).
#   down.sh          -> stop containers, keep the install
#   down.sh -v       -> stop and wipe the install
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

[ -f "$(qi_dir)/docker-compose.yml" ] || die "no quickinstall checkout at $(qi_dir); nothing to tear down"
log "docker compose down $*"
compose down "$@"
