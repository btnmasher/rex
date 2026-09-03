# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
COPY lib/jobruntime/go.mod lib/jobruntime/go.sum lib/jobruntime/
RUN go -C lib/jobruntime mod download && go mod download

COPY lib/jobruntime lib/jobruntime
COPY cmd cmd
COPY internal internal

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -buildvcs=false -trimpath -ldflags='-s -w' -o /out/rex ./cmd/rex
RUN mkdir -p /out/data && chown 65532:65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=unknown

LABEL org.opencontainers.image.title="Rex ESI notification service" \
      org.opencontainers.image.description="EVE Online notification service for Discord" \
      org.opencontainers.image.source="https://github.com/btnmasher/rex" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$REVISION" \
      org.opencontainers.image.created="$CREATED"

COPY --from=build --chown=nonroot:nonroot /out/rex /rex
COPY --from=build --chown=nonroot:nonroot /out/data /data

WORKDIR /data

ENTRYPOINT ["/rex"]
