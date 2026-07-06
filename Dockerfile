FROM golang:1.26.4 AS builder

RUN apt-get update \
 && apt-get install -y --no-install-recommends git tzdata \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /github.com/armadakv/armada
# Copy the source
COPY . .

# Build
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build GOMODCACHE=/go/pkg/mod GOCACHE=/root/.cache/go-build CGO_ENABLED=0 VERSION=${VERSION} make armada

# Runtime
FROM gcr.io/distroless/static-debian13

ARG VERSION=dev
LABEL org.opencontainers.image.authors="Armada Developers <armadakv@github.com>"
LABEL org.opencontainers.image.base.name="gcr.io/distroless/static-debian13"
LABEL org.opencontainers.image.description="Armada is a distributed key-value store. It is Kubernetes friendly with emphasis on high read throughput and low operational cost."
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.source="https://github.com/armadakv/armada"
LABEL org.opencontainers.image.version="${VERSION}"

WORKDIR /
COPY --from=builder /usr/share/zoneinfo/ /usr/share/zoneinfo/
COPY --from=builder --chown=65532:65532 /github.com/armadakv/armada/armada /usr/local/bin/

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/armada"]
