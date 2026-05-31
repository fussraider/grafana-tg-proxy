# Phase 1: Build the statically linked Go binary
FROM golang:1.26-alpine AS builder

# Install CA certificates to secure outbound SSL/TLS handshakes
RUN apk --no-cache add ca-certificates

# Set up project workspace
WORKDIR /app

# Copy dependency specifications and cache them
COPY go.mod go.sum ./
RUN go mod download

# Copy application source code
COPY . .

# Compile static binary with CGO disabled and stripped debug layouts
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /app/proxy .

# Phase 2: Create the minimal execution container
FROM scratch

# Copy CA root certificates from builder stage
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# Copy compiled static binary
COPY --from=builder /app/proxy /proxy

# Expose transparent proxy port and Prometheus metrics port
EXPOSE 8080
EXPOSE 9090

# Run proxy binary
ENTRYPOINT ["/proxy"]
