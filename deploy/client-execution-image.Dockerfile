FROM scratch

ARG ARTIFACT_VERSION
ARG SOURCE_REFERENCE

LABEL org.opencontainers.image.title="SecondBox client execution image" \
      org.opencontainers.image.version="${ARTIFACT_VERSION}" \
      org.opencontainers.image.base.name="${SOURCE_REFERENCE}"

COPY . /secondbox-runner-microvm/
