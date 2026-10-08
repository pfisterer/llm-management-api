## Stage 1: Builder image
FROM golang:1-alpine AS builder

RUN apk add --no-cache git make build-base

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY Makefile ./
COPY VERSION ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/

# `image` generates the embedded API description and builds the binary WITHOUT
# running the tests; those run in the checks workflow (see .github/workflows).
RUN make image

## Stage 2: Production image
FROM alpine:latest AS final

WORKDIR /app

COPY --from=builder /app/tmp/build/llm-management-api /app/

EXPOSE 8086

# Numeric non-root user so Kubernetes can enforce runAsNonRoot.
USER 65532:65532

CMD ["./llm-management-api"]
