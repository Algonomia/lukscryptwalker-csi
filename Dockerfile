# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Install dependencies
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY cmd/ cmd/
COPY pkg/ pkg/

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o lukscryptwalker-csi ./cmd/main.go

# Runtime stage
FROM alpine:3.18

# Install required packages for LUKS operations
RUN apk add --no-cache \
    cryptsetup \
    util-linux \
    e2fsprogs \
    e2fsprogs-extra \
    xfsprogs \
    ca-certificates

# Create a non-root user
RUN addgroup -g 1001 lukscrypt && \
    adduser -u 1001 -G lukscrypt -s /bin/sh -D lukscrypt

# Copy binary from builder
COPY --from=builder /app/lukscryptwalker-csi /usr/local/bin/lukscryptwalker-csi

# Set permissions
RUN chmod +x /usr/local/bin/lukscryptwalker-csi

# Create directories
RUN mkdir -p /opt/local-path-provisioner && \
    chown -R lukscrypt:lukscrypt /opt/local-path-provisioner

# Note: User will be set by Kubernetes securityContext
# Controller runs as non-root, Node runs as root for privileged operations

ENTRYPOINT ["/usr/local/bin/lukscryptwalker-csi"]
