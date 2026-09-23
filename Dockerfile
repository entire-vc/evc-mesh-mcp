# syntax=docker/dockerfile:1

# EVC Mesh MCP server — container image.
#
# Two stages: a golang:alpine builder that produces a static binary, and a
# distroless static runtime with no shell, no package manager, and a non-root
# user. stdio is the default transport (MESH_MCP_TRANSPORT unset) because
# that is how MCP clients (Claude Desktop config, `docker run -i --rm ...`,
# the registry's own package runner) invoke it; SSE mode is opt-in via
# MESH_MCP_TRANSPORT=sse for the shared-server deployment described in
# README.md.
#
# BUILD_SHA and VERSION are threaded through -ldflags into internal/mcp.BuildSHA
# and internal/mcp.Version; the server reports them in serverInfo.version and
# `--version`.

ARG GO_VERSION=1.25
ARG BUILD_SHA=dev
ARG VERSION=dev

FROM golang:${GO_VERSION}-alpine AS builder
ARG BUILD_SHA
ARG VERSION

WORKDIR /src

# Layer caching: dependency download only re-runs when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 + static ldflags -> a binary with zero dynamic dependencies,
# required for the distroless "static" runtime base below (no libc at all).
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X github.com/entire-vc/evc-mesh-mcp/internal/mcp.BuildSHA=${BUILD_SHA} -X github.com/entire-vc/evc-mesh-mcp/internal/mcp.Version=${VERSION}" \
    -o /out/evc-mesh-mcp .

# distroless/static: no shell, no package manager — nothing an attacker who
# reaches the container can use besides the one binary it ships.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
ARG BUILD_SHA

LABEL org.opencontainers.image.title="evc-mesh-mcp" \
      org.opencontainers.image.description="Tasks, comments, shared memory and handoffs for teams of people and AI agents, over MCP." \
      org.opencontainers.image.source="https://github.com/entire-vc/evc-mesh-mcp" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.vendor="Entire VC" \
      org.opencontainers.image.revision="${BUILD_SHA}" \
      io.modelcontextprotocol.server.name="io.github.entire-vc/evc-mesh-mcp"

# distroless/static:nonroot already runs as uid 65532 ("nonroot") — no
# separate USER directive needed, and there is no shell here to add one with
# anyway.
COPY --from=builder /out/evc-mesh-mcp /usr/local/bin/evc-mesh-mcp

# SSE mode's default port (README.md); irrelevant for stdio but documents the
# contract for anyone running --transport sse.
EXPOSE 8081

ENTRYPOINT ["/usr/local/bin/evc-mesh-mcp"]
