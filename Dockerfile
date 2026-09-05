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
# Build BOTH binaries, always.
#
# The Helm chart points the api and worker Deployments at a single image ref,
# and the release pipeline publishes one image. With only the TARGET binary
# baked in, that meant the worker Deployment ran whichever binary the build
# happened to produce -- in practice the API, so the cluster never delivered
# anything. The pods were stuck in ImagePullBackOff, so nothing surfaced it.
#
# One image containing both is the right shape here: it keeps a single tag to
# sign, attest, and roll back, and makes the role an explicit `command` on each
# Deployment rather than a property of how the image was built.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" -o /out/api ./cmd/api
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# TARGET still selects the DEFAULT entrypoint, so `docker run <image>` and the
# compose stack keep working unchanged.
ARG TARGET=api
RUN cp /out/${TARGET} /out/app

# ---- runtime --------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/api /api
COPY --from=build /out/worker /worker
COPY --from=build /out/app /app

# Non-root by default. The distroless nonroot tag runs as uid 65532.
USER nonroot:nonroot

EXPOSE 8080
ENTRYPOINT ["/app"]
