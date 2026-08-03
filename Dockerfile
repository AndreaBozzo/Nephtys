# ---------- Build ----------
# Pinned to the *build host's* platform. Go cross-compiles natively, which is
# far cheaper than emulating the target architecture: without this, the arm64
# leg of a multi-arch buildx run executes the entire Go toolchain under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

# Supplied automatically by BuildKit, and deliberately left without defaults.
# These are *predefined* platform args: giving one a default makes the default
# win, so `ARG TARGETARCH=amd64` silently cross-compiled every leg of a
# multi-arch build to amd64 while buildx still labelled one of them arm64. A
# plain `docker build` needs no default either — BuildKit fills these in from
# the host platform.
ARG TARGETOS
ARG TARGETARCH

# Stamped into the binary so `nephtys --version` reports a release rather than a
# commit hash. The runtime fallback in main.go reads VCS info from the build,
# which needs both the git binary in this stage and a full .git in the context —
# and .dockerignore deliberately excludes the latter.
ARG VERSION=dev

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The -X value is quoted inside the ldflags string because `go` re-splits that
# string on whitespace before handing it to the linker: an unquoted VERSION
# containing a space would send its tail to the linker as a bogus flag.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X 'main.version=${VERSION}'" -o /nephtys ./cmd/nephtys

# ---------- Runtime ----------
# The :nonroot variant runs as uid 65532 rather than root. Nephtys binds :3002,
# an unprivileged port, and writes nothing to disk, so it never needs root.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /nephtys /nephtys

EXPOSE 3002
USER nonroot:nonroot
ENTRYPOINT ["/nephtys"]
