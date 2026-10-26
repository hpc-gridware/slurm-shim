#!/usr/bin/env bash
# Install OpenMPI into the running test cluster, for test/e2e/35_mpi_pmix.sh.
#
#   install-openmpi.sh               OpenMPI 4.1 from the distro on every node
#   OPENMPI5=1 install-openmpi.sh    also build OpenMPI 5.0.x into the shared
#                                    /opt/ocs/openmpi5 (about 10 minutes, once)
#
# The quickinstall image (openSUSE Leap 15.6) packages OpenMPI 4.1 only; 5.x
# is built from the release tarball, with its bundled PRRTE and PMIx.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_cluster
[ "$BACKEND" = docker ] || die "install-openmpi.sh installs packages into the Docker cluster only; on BACKEND=$BACKEND install OpenMPI yourself and set OPENMPI_HOMES"

OPENMPI5_VERSION="${OPENMPI5_VERSION:-5.0.9}"
# SHA256 of the 5.0.9 tarball as published on open-mpi.org; set it with any
# other OPENMPI5_VERSION.
OPENMPI5_SHA256="${OPENMPI5_SHA256:-dfb72762531170847af3e4a0f21d77d7b23cf36f67ce7ce9033659273677d80b}"
OPENMPI5_PREFIX=/opt/ocs/openmpi5

pids=()
for n in "${NODES[@]}"; do
  log "installing OpenMPI 4.1 on $n"
  node_sh "$n" "zypper -q --non-interactive in -y openmpi4 openmpi4-devel gcc gcc-c++ make perl tar bzip2 >/dev/null" &
  pids+=("$!")
done
for p in "${pids[@]}"; do
  wait "$p" || die "zypper failed on a node (see above)"
done

if [ "${OPENMPI5:-0}" = 1 ]; then
  if node_sh "$MASTER" "[ -x $OPENMPI5_PREFIX/bin/mpirun ]"; then
    log "OpenMPI 5 already in $OPENMPI5_PREFIX"
  else
    log "building OpenMPI $OPENMPI5_VERSION into $OPENMPI5_PREFIX (shared by every node)"
    node_sh "$MASTER" "set -e
      d=\$(mktemp -d); cd \$d
      curl -fsSL -o ompi.tar.bz2 'https://download.open-mpi.org/release/open-mpi/v5.0/openmpi-$OPENMPI5_VERSION.tar.bz2'
      echo '$OPENMPI5_SHA256  ompi.tar.bz2' | sha256sum -c --quiet
      tar xjf ompi.tar.bz2 && cd 'openmpi-$OPENMPI5_VERSION'
      ./configure --prefix=$OPENMPI5_PREFIX --disable-mpi-fortran --disable-oshmem >configure.log 2>&1 || { tail -20 configure.log; exit 1; }
      make -j\$(nproc) >make.log 2>&1 || { tail -20 make.log; exit 1; }
      make install >install.log 2>&1
      cd / && rm -rf \$d"
  fi
fi
log "OpenMPI ready; run: make e2e (35_mpi_pmix)"
