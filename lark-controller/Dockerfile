FROM golang:1.26.1-alpine@sha256:2389ebfa5b7f43eeafbd6be0c3700cc46690ef842ad962f6c5bd6be49ed82039 AS builder

ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64}

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/lark-controller ./cmd/lark-controller \
    && go build -trimpath -ldflags="-s -w" -o /out/lark-correction ./cmd/lark-correction \
    && go build -trimpath -ldflags="-s -w" -o /out/lark-config ./cmd/lark-config

FROM debian:bookworm-slim@sha256:f06537653ac770703bc45b4b113475bd402f451e85223f0f2837acbf89ab020a AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata wget \
    && rm -rf /var/lib/apt/lists/* \
    && install -d -o 10001 -g 10001 /var/lib/lark-controller

USER 10001:10001
WORKDIR /var/lib/lark-controller

FROM runtime AS controller
COPY --from=builder /out/lark-controller /usr/local/bin/lark-controller
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=5s --start-period=10s --retries=5 \
    CMD wget -q -T 3 -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/lark-controller"]

FROM runtime AS correction
COPY --from=builder /out/lark-correction /usr/local/bin/lark-correction
ENTRYPOINT ["/usr/local/bin/lark-correction"]

FROM node:22-bookworm-slim@sha256:d649c27dae7ba0137b3cef5dd75baa422c08dc3d9e3fc0c23dfb172dc3cc6436 AS config

ARG LARK_CLI_VERSION=1.0.80
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && npm install --global --omit=dev "@larksuite/cli@${LARK_CLI_VERSION}" \
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/* \
    && install -d -o 10001 -g 10001 /var/lib/lark-controller
COPY --from=builder /out/lark-config /usr/local/bin/lark-config
USER 10001:10001
WORKDIR /var/lib/lark-controller
ENTRYPOINT ["/usr/local/bin/lark-config"]
