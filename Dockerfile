# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /build

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Build binary
COPY . .
ARG VERSION
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=${VERSION}" -o unbeholden-lnurl ./cmd/unbeholden-lnurl

# Runtime stage
FROM gcr.io/distroless/static:nonroot

WORKDIR /app

COPY --from=builder /build/unbeholden-lnurl /app/unbeholden-lnurl

USER nonroot:nonroot

ENTRYPOINT ["/app/unbeholden-lnurl"]
CMD ["-config", "/app/config.yaml"]
