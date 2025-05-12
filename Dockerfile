# syntax = docker/dockerfile:1.2
FROM golang:1.24.3-alpine3.21 as builder

RUN apk add --update --no-cache build-base tzdata \
 && addgroup -g 1000 -S armada && adduser -u 1000 -S armada -G armada

WORKDIR /github.com/armadakv/armada
# Copy the source
COPY . .

# Build
ARG VERSION
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build GOMODCACHE=/go/pkg/mod GOCACHE=/root/.cache/go-build VERSION=${VERSION} make armada

# Runtime
FROM alpine:3.21

ARG VERSION
LABEL org.opencontainers.image.authors="Armada Developers <armadakv@github.com>"
LABEL org.opencontainers.image.base.name="docker.io/library/alpine:3.21"
LABEL org.opencontainers.image.description="Armada is a distributed key-value store. It is Kubernetes friendly with emphasis on high read throughput and low operational cost."
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.source="https://github.com/armadakv/armada"
LABEL org.opencontainers.image.version="${VERSION}"

WORKDIR /
COPY --from=builder /etc/passwd /etc/
COPY --from=builder /usr/share/zoneinfo/ /usr/share/zoneinfo/
COPY --from=builder --chown=1000:1000 /github.com/armadakv/armada/armada /usr/local/bin/

USER armada

ENTRYPOINT ["armada"]
