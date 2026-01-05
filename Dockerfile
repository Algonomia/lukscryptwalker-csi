# Build stage
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

# Set up build arguments for cross-compilation and version
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git gcc musl-dev

# Copy go mod files first for better caching
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY cmd/ cmd/
COPY pkg/ pkg/

# Build the binary with proper cross-compilation and version injection
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -ldflags="-w -s -extldflags '-static' \
    -X github.com/lukscryptwalker-csi/pkg/version.Version=${VERSION} \
    -X github.com/lukscryptwalker-csi/pkg/version.GitCommit=${GIT_COMMIT} \
    -X github.com/lukscryptwalker-csi/pkg/version.BuildDate=${BUILD_DATE}" \
    -a -installsuffix cgo \
    -o lukscryptwalker-csi ./cmd/main.go

# Runtime stage
FROM alpine:3.18

# Install required packages for LUKS operations and rclone mount (via librclone)
RUN apk add --no-cache \
    cryptsetup \
    util-linux \
    e2fsprogs \
    e2fsprogs-extra \
    xfsprogs \
    ca-certificates \
    fuse3

# Configure FUSE to allow non-root mounts with allow_other option
RUN echo "user_allow_other" >> /etc/fuse.conf

# Create a non-root user
RUN addgroup -g 1001 lukscrypt && \
    adduser -u 1001 -G lukscrypt -s /bin/sh -D lukscrypt

# Copy binary from builder
COPY --from=builder /app/lukscryptwalker-csi /usr/local/bin/lukscryptwalker-csi

# Set permissions
RUN chmod +x /usr/local/bin/lukscryptwalker-csi

# Create directories
RUN mkdir -p /opt/local-path-provisioner && \
    chown -R lukscrypt:lukscrypt /opt/local-path-provisioner && \
    mkdir -p /var/lib/lukscrypt-cache/backing /var/lib/lukscrypt-cache/mounts

# Note: User will be set by Kubernetes securityContext
# Controller runs as non-root, Node runs as root for privileged operations

ENTRYPOINT ["/usr/local/bin/lukscryptwalker-csi"]
