# syntax=docker/dockerfile:1
ARG GO_IMAGE=golang:1.27.1-alpine3.24@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -buildvcs=false \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/xunlei-api ./cmd/xunlei-api
RUN test -s /etc/ssl/certs/ca-certificates.crt \
    && mkdir -p /runtime/data /runtime/tmp \
    && chown 65532:65532 /runtime/data \
    && chmod 700 /runtime/data \
    && chmod 1777 /runtime/tmp

FROM scratch
ARG VERSION=dev
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="xunlei-api" \
      org.opencontainers.image.description="A small, authenticated API for an existing Xunlei Docker instance" \
      org.opencontainers.image.source="https://github.com/myth815/xunlei-api" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
COPY --from=build /out/xunlei-api /xunlei-api
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /runtime/data /data
COPY --from=build /runtime/tmp /tmp
USER 65532:65532
ENV LISTEN_ADDR=:8080 DATA_DIR=/data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/xunlei-api", "healthcheck"]
ENTRYPOINT ["/xunlei-api"]
