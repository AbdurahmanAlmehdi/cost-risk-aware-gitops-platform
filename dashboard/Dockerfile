# The demonstration dashboard — build component.
#
# Same shape as app/Dockerfile: static binary, distroless base, reproducible output. The
# one difference is that the web assets are compiled into the binary with go:embed rather
# than copied in. That is not a convenience — the manifest runs this with a read-only root
# filesystem and no shell, so there is no writable path to serve files from and no way to
# alter what is served without producing a new digest.

# --- build ------------------------------------------------------------------
FROM golang:1.24-alpine AS build

WORKDIR /src

# This module depends on nothing outside the standard library, which is why there is no
# go.sum to copy: the four reads and one patch it makes against the Kubernetes API are
# hand-rolled HTTP for exactly that reason (see kube.go).
COPY go.mod ./
RUN go mod download

COPY . .

# TARGETOS/TARGETARCH are supplied by BuildKit and make the same Dockerfile correct for a
# local arm64 cluster and an amd64 CI runner without a second file.
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -buildid=" -o /out/dashboard .

# --- runtime ----------------------------------------------------------------
# distroless static: no shell, no package manager, no libc. This one matters more than
# most — it is the image behind the only authenticated page the internet can reach, and
# the project deliberately refused a web terminal. A base with no shell means there is no
# shell to reach even if something else here were wrong.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/dashboard /dashboard

USER 65532:65532

EXPOSE 8080
ENTRYPOINT ["/dashboard"]
