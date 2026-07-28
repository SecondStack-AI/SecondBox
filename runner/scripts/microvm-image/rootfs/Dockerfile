# syntax=docker/dockerfile:1

# BASE_IMAGE is deliberately unset. The caller supplies either a content-
# addressed OCI reference or the image ID imported from an explicit SecondBox
# Debian image definition.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

ARG APT_CHECK_VALID_UNTIL
ENV DEBIAN_FRONTEND=noninteractive

# apt and pip drop privileges to users that require conventional temporary
# directory permissions.
COPY config/verify-debian-snapshot-sources.sh /tmp/secondbox-build/config/verify-debian-snapshot-sources.sh
RUN chmod 1777 /tmp /var/tmp
RUN case "${APT_CHECK_VALID_UNTIL}" in \
      true) ;; \
      false) printf '%s\n' 'Acquire::Check-Valid-Until "false";' > /etc/apt/apt.conf.d/99secondbox-snapshot ;; \
      *) echo "SecondBox rootfs build failed: APT_CHECK_VALID_UNTIL must be true or false" >&2; exit 2 ;; \
    esac \
 && sh /tmp/secondbox-build/config/verify-debian-snapshot-sources.sh

COPY secondbox-apt-packages.txt /tmp/secondbox-build/secondbox-apt-packages.txt
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      $(grep -vE '^\s*#|^\s*$' /tmp/secondbox-build/secondbox-apt-packages.txt) \
 && rm -rf /var/lib/apt/lists/*

# Prefer the Debian Node.js runtime to any inherited /usr/local installation
# whose CPU assumptions may not match the Firecracker guest.
RUN rm -f /usr/local/bin/node /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack

COPY secondbox-python-requirements.txt /tmp/secondbox-build/secondbox-python-requirements.txt
RUN python3 -m pip install --no-cache-dir --break-system-packages \
      -r /tmp/secondbox-build/secondbox-python-requirements.txt

COPY config/ /tmp/secondbox-build/config/
COPY verify-secondbox-rootfs.sh /tmp/secondbox-build/verify-secondbox-rootfs.sh
COPY collect-secondbox-rootfs-provenance.py /tmp/secondbox-build/collect-secondbox-rootfs-provenance.py
RUN sh /tmp/secondbox-build/config/configure-imagemagick-policy.sh \
 && python3 -c "import matplotlib; matplotlib.use('Agg'); import matplotlib.pyplot as plt; plt.figure()" \
 && node -e "console.log(process.version)" \
 && npm --version \
 && /tmp/secondbox-build/verify-secondbox-rootfs.sh --live \
 && install -d -m 0755 /usr/share/secondbox/image-provenance \
 && python3 /tmp/secondbox-build/collect-secondbox-rootfs-provenance.py \
      --output-dir /usr/share/secondbox/image-provenance \
 && python3 -m pip freeze --all \
      > /usr/share/secondbox/image-provenance/rootfs-python.freeze \
 && rm -rf /tmp/secondbox-build
