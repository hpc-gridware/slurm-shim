#!/bin/sh
# slurm-shim bootstrap: the curl-able path for a trial cluster (quickinstall's
# WITH_SLURM_SHIM=1), and for sites without rpm/deb.
#
#   curl -fsSL https://github.com/hpc-gridware/slurm-shim/releases/latest/download/install.sh | sh
#
# It moves bytes and hands off: detect the arch, download the matching release,
# VERIFY the checksum, unpack, then exec `slurm-shim install --apply`, which
# makes every decision. Requires $SGE_ROOT (source the cell's settings.sh) and a
# manager. Override the version with SLURM_SHIM_VERSION, exposure with
# SLURM_SHIM_EXPOSE (none|module|profile.d; default profile.d, since a trial
# cluster wants the commands on PATH), and skip the cluster step with
# SLURM_SHIM_NO_APPLY=1.
set -eu

REPO="${SLURM_SHIM_REPO:-hpc-gridware/slurm-shim}"
VERSION="${SLURM_SHIM_VERSION:-latest}"
EXPOSE="${SLURM_SHIM_EXPOSE:-profile.d}"

die() { echo "install.sh: $*" >&2; exit 1; }

[ -n "${SGE_ROOT:-}" ] || die "SGE_ROOT is not set; source \$SGE_ROOT/\$SGE_CELL/common/settings.sh first"
command -v curl >/dev/null 2>&1 || die "curl is required"

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac

if [ "$VERSION" = latest ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$VERSION"
fi
TARBALL="slurm-shim_linux_${ARCH}.tar.gz"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
cd "$WORK"
echo "downloading $BASE/$TARBALL"
curl -fsSL -o "$TARBALL" "$BASE/$TARBALL"
curl -fsSL -o SHA256SUMS "$BASE/SHA256SUMS"

# Verify before unpacking anything. Never run bytes whose hash did not match.
want="$(grep " $TARBALL\$" SHA256SUMS | awk '{print $1}')"
[ -n "$want" ] || die "$TARBALL is not listed in SHA256SUMS"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$TARBALL" | awk '{print $1}')"
else
  got="$(shasum -a 256 "$TARBALL" | awk '{print $1}')"
fi
[ "$got" = "$want" ] || die "checksum mismatch for $TARBALL: got $got want $want"
echo "checksum ok"

mkdir payload
tar -xzf "$TARBALL" -C payload
[ -x payload/bin/slurm-shim ] || die "tarball has no bin/slurm-shim"

if [ "${SLURM_SHIM_NO_APPLY:-0}" = 1 ]; then
  echo "payload unpacked at $WORK/payload (SLURM_SHIM_NO_APPLY=1: not configuring the cluster)"
  trap - EXIT
  exit 0
fi
exec payload/bin/slurm-shim install --apply --from "$WORK/payload" --expose "$EXPOSE" "$@"
