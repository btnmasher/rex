FROM golang:1.27-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
COPY lib/jobruntime/go.mod lib/jobruntime/go.sum lib/jobruntime/
RUN go -C lib/jobruntime mod download && go mod download

COPY lib/jobruntime lib/jobruntime
COPY cmd cmd
COPY internal internal

RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/rex ./cmd/rex
RUN mkdir -p /out/data && chown 65532:65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build --chown=nonroot:nonroot /out/rex /rex
COPY --from=build --chown=nonroot:nonroot /out/data /data

WORKDIR /data

ENTRYPOINT ["/rex"]
