# M1 — build component.
#
# One image carries both binaries and the manifests choose between them by command.
# Two images would double the build, the push, and the number of digests M2 has to
# reason about, for two programs that share every dependency.

# --- build ------------------------------------------------------------------
FROM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies are copied and downloaded before the source so that editing application
# code does not invalidate the module cache layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# TARGETOS/TARGETARCH are supplied by BuildKit and make the same Dockerfile correct
# for a local arm64 kind cluster and an amd64 CI runner without a second file.
ARG TARGETOS=linux
ARG TARGETARCH
# CGO is disabled so the result is a static binary that runs on a distroless base.
# -trimpath and the empty build ID keep the output reproducible: the same source
# produces the same digest, which is what makes digest-pinning meaningful.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -buildid=" -o /out/api    ./cmd/api && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -buildid=" -o /out/worker ./cmd/worker

# --- runtime ----------------------------------------------------------------
# distroless static: no shell, no package manager, no libc. This is not decoration —
# M2's policy sub-gate requires runAsNonRoot and a read-only root filesystem, and this
# base is what lets the workload satisfy its own platform's rules.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/api    /api
COPY --from=build /out/worker /worker

# 65532 is distroless's `nonroot` user. Declared here as well as in the manifest so the
# image is safe by default even if it is ever run outside this platform.
USER 65532:65532

EXPOSE 8080
ENTRYPOINT ["/api"]
