# syntax=docker/dockerfile:1
#
# Extends the existing agent tool-VM rootfs with the standard package set for
# common coding + productivity workloads (see README.md). The base image is the
# CURRENT signed rootfs, imported by build-rootfs-source.sh; this Dockerfile adds
# packages/config and replaces known-bad base tool shims when needed.
ARG BASE_IMAGE=agentcy-rootfs:base
FROM ${BASE_IMAGE}

ARG APT_CHECK_VALID_UNTIL=true
ENV DEBIAN_FRONTEND=noninteractive

# The microVM builder recreates /tmp as mode 0755 (scripts/microvm-image/build.sh),
# but apt and pip drop privileges to a non-root user that needs a sticky,
# world-writable /tmp to create temp files (else apt-key fails and repositories
# read as "not signed"). Restore the conventional 1777 before installing.
RUN chmod 1777 /tmp /var/tmp 2>/dev/null || true
RUN if [ "${APT_CHECK_VALID_UNTIL}" = "false" ]; then \
      printf '%s\n' 'Acquire::Check-Valid-Until "false";' > /etc/apt/apt.conf.d/99agentcy-snapshot; \
    fi

# System packages + apt-packaged Python libraries.
COPY apt-std.txt /tmp/agentcy/apt-std.txt
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      $(grep -vE '^\s*#|^\s*$' /tmp/agentcy/apt-std.txt) \
 && rm -rf /var/lib/apt/lists/*

# The inherited rootfs may contain an upstream /usr/local Node build that runs
# in Docker but crashes under the PVM guest CPU/kernel shape. Prefer Debian's
# apt-packaged nodejs/npm for tool-VM workloads.
RUN rm -f /usr/local/bin/node /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack

# Pip-only importables not packaged in Debian, installed into the system
# interpreter (build-time --break-system-packages is deliberate; PEP 668).
COPY requirements-std.txt /tmp/agentcy/requirements-std.txt
COPY verify-standard-toolset.sh /tmp/agentcy/verify-standard-toolset.sh
RUN python3 -m pip install --no-cache-dir --break-system-packages \
      -r /tmp/agentcy/requirements-std.txt

# Config bakes: relax ImageMagick PDF/PS policy, matplotlib headless env, and
# pre-warm the matplotlib font cache in the default HOME location so the first
# plot is fast. The pyplot import also fails the build loudly if the standard
# Python stack cannot be imported.
COPY config/ /tmp/agentcy/config/
RUN sh /tmp/agentcy/config/imagemagick-relax-policy.sh \
 && install -D -m 0644 /tmp/agentcy/config/profile.d-agentcy-std.sh /etc/profile.d/agentcy-std.sh \
 && python3 -c "import matplotlib; matplotlib.use('Agg'); import matplotlib.pyplot as plt; plt.figure()" \
 && node -e "console.log(process.version)" \
 && npm --version \
 && (corepack --version >/dev/null 2>&1 || true) \
 && /tmp/agentcy/verify-standard-toolset.sh --live \
 && rm -rf /tmp/agentcy
