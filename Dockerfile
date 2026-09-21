# Multi-stage build producing a static binary on a distroless base.
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build

WORKDIR /src

# Dependencies first so the layer caches across source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/cx-metrics-gen .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/cx-metrics-gen /usr/local/bin/cx-metrics-gen

# Probes only. Metrics leave over OTLP, this is not a scrape target.
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/cx-metrics-gen"]
