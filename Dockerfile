# csi-reclaimspace image. Built and pushed by CI on merges to main:
#   docker build --build-arg VERSION=1.0.0.0 -t csi-reclaimspace .
FROM golang:1.26.2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY internal ./internal
ARG VERSION=dev
# Hermetic build: go.sum is committed, so no `go mod tidy` / network resolution.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/csi-reclaimspace .

FROM alpine:3.22
COPY --from=build /out/csi-reclaimspace /usr/local/bin/csi-reclaimspace
# Runs as root: FITRIM needs CAP_SYS_ADMIN, which the pod grants (drop ALL, add SYS_ADMIN, DAC_READ_SEARCH).
ENTRYPOINT ["/usr/local/bin/csi-reclaimspace"]
