# syntax=docker/dockerfile:1.4
# -------- Build Stage --------
FROM golang:1.24.3 as builder

# Set Go environment
ENV CGO_ENABLED=0 \
    GO111MODULE=on \
    GOPROXY=https://proxy.golang.org

WORKDIR /app

# Copy go.mod/go.sum and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Generate gRPC/protobuf code if needed
# (optional if already generated)
# RUN go generate ./...

# Build the binary
RUN go build -o nvmeof-csi ./cmd/

# -------- Runtime Stage --------
FROM almalinux:9

# Install nvme-cli
RUN dnf install -y nvme-cli && \
    dnf clean all && \
    rm -rf /var/cache/dnf

WORKDIR /

# Copy binary from builder
COPY --from=builder /app/nvmeof-csi /nvmeof-csi

# # Set non-root user
# USER nonroot:nonroot

# Run the CSI driver
ENTRYPOINT ["/nvmeof-csi"]