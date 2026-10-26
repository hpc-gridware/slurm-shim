#!/usr/bin/env bash
# Install OpenMPI into the running test cluster, for test/e2e/35_mpi_pmix.sh.
#
#   install-openmpi.sh               OpenMPI 4.1 from the distro on every node
#   OPENMPI5=1 install-openmpi.sh    also build OpenMPI 5.0.x into the shared
#                                    /opt/ocs/openmpi5 (about 10 minutes, once)
#   APPTAINER=1 install-openmpi.sh   also install Apptainer on every node, for
#                                    docs/recipes/openmpi/openmpi-apptainer.sh
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
  pkgs="openmpi4 openmpi4-devel gcc gcc-c++ make perl tar bzip2"
  [ "${APPTAINER:-0}" = 1 ] && pkgs="$pkgs apptainer"
  # mpi-selector puts the system default on PATH in login shells; interactive
  # non-login shells (docker exec ... bash) read /etc/bash.bashrc.local on
  # openSUSE, as for slurm-shim in install-shim.sh. Jobs inherit it from the
  # submitting shell.
  node_sh "$n" "zypper -q --non-interactive in -y $pkgs >/dev/null
    mpi-selector --system --set openmpi4 --yes >/dev/null
    grep -qs mpi-selector /etc/bash.bashrc.local || \
      printf '. /etc/profile.d/mpi-selector.sh\n' >> /etc/bash.bashrc.local" &
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
cat <<EOF

  OpenMPI 4.1 is on PATH for every new shell: docker exec -it -u $JOB_USER $MASTER bash
  (mpicc, mpirun; the default comes from mpi-selector, see mpi-selector --list).
EOF
if [ "${OPENMPI5:-0}" = 1 ]; then
  cat <<EOF
  OpenMPI $OPENMPI5_VERSION instead: export PATH=$OPENMPI5_PREFIX/bin:\$PATH
EOF
fi
if [ "${APPTAINER:-0}" = 1 ]; then
  cat <<EOF
  Apptainer: as root, apptainer build openmpi.sif openmpi.def (docs/recipes/openmpi)
EOF
fi
cat <<EOF
  Try: docs/recipes/openmpi, or make e2e (35_mpi_pmix)

EOF
