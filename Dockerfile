FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:1.24.4 AS builder
ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /src

ENV GOOS=${TARGETOS}
ENV GOARCH=${TARGETARCH}
ENV CGO_ENABLED=0

# Build arguments
ARG RELEASE_TAG=dev
ARG GIT_COMMIT=unknown

# Copy go modules files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build both binaries
RUN go build -ldflags="-s -w -X main.version=${RELEASE_TAG} -X main.commit=${GIT_COMMIT}" \
    -o /installer ./cmd/installer

RUN go build -ldflags="-s -w -X main.version=${RELEASE_TAG} -X main.commit=${GIT_COMMIT}" \
    -o /gcp-ipam ./cmd/ipam

# Final stage - minimal runtime image
FROM debian:12-slim
WORKDIR /app

# Copy binaries from builder
COPY --from=builder --chown=nonroot:nonroot /installer /app/installer
COPY --from=builder --chown=nonroot:nonroot /gcp-ipam /app/gcp-ipam

# The installer will copy gcp-ipam to the host
ENTRYPOINT ["/app/installer"]
