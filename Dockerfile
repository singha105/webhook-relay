# Multi-stage build. The final image carries a static binary and nothing else:
# no shell, no package manager, no Go toolchain. A distroless base is not a
# nice-to-have here — it is what keeps the CVE surface of a deployed webhook
# receiver close to zero.

# ---- build ----------------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

# Copy the module files alone first so `go mod download` is cached and only
# re-runs when dependencies actually change, not on every source edit.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 for a static binary that runs on a base with no libc.
# -trimpath strips local filesystem paths from the binary.
# -s -w drop the symbol table and DWARF data; we do not debug in production
# images, and it cuts the binary roughly in half.
ARG TARGET=api
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/app ./cmd/${TARGET}

# ---- runtime --------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/app /app

# Non-root by default. The distroless nonroot tag runs as uid 65532.
USER nonroot:nonroot

EXPOSE 8080
ENTRYPOINT ["/app"]
