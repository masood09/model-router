# syntax=docker/dockerfile:1
#
# Multi-stage build: a static binary on distroless, running as non-root.
# Base images are pinned by digest; Dependabot keeps them current.

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/model-router ./cmd/model-router

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
LABEL org.opencontainers.image.title="model-router" \
      org.opencontainers.image.description="OpenAI-compatible proxy that switches local LLM models on demand" \
      org.opencontainers.image.source="https://github.com/masood09/model-router" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/model-router /usr/local/bin/model-router
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/model-router"]
CMD ["-config", "/etc/model-router/config.json"]
