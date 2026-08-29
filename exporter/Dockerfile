# M4, cost exporter image.
#
# Built from the repository root rather than from exporter/, because the exporter depends
# on the shared pricing module through a local replace directive. A build context of
# exporter/ alone cannot see ../pricing, and the build fails at `go mod download` with a
# missing-module error that reads as a dependency problem rather than a context one.

FROM golang:1.24-alpine AS build

WORKDIR /src

# Both modules' manifests first, so dependency resolution is cached independently of
# source edits. The replace directive is relative, so the directory layout has to match
# the repository's.
COPY exporter/go.mod exporter/go.sum ./exporter/
COPY pricing/go.mod pricing/go.sum ./pricing/
RUN cd exporter && go mod download

COPY pricing/ ./pricing/
COPY exporter/ ./exporter/

ARG TARGETOS=linux
ARG TARGETARCH
RUN cd exporter && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -buildid=" -o /out/costexporter ./cmd/costexporter

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/costexporter /costexporter

USER 65532:65532

EXPOSE 9091
ENTRYPOINT ["/costexporter"]
