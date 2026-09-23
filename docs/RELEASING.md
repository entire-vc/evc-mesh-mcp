# Releasing evc-mesh-mcp

A release is cut by pushing a `v*` tag (for example `v1.0.0`). That runs
`.github/workflows/release.yml`.

## What a release produces

1. `go vet` and `go test -race ./...` against the tagged commit. Nothing ships
   if they fail.
2. Binaries for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`
   and `windows/amd64`, attached with `SHA256SUMS` to a GitHub Release.
3. A multi-arch (`linux/amd64`, `linux/arm64`) image,
   `ghcr.io/entire-vc/evc-mesh-mcp:<version>` and `:latest`.
4. An [MCPB](https://github.com/modelcontextprotocol/mcpb) bundle,
   `evc-mesh-mcp-<version>.mcpb`, attached to the same GitHub Release.
5. `server.json`, with the version, image tag, MCPB asset URL and its
   `fileSha256` filled in, published to the official MCP Registry
   (`registry.modelcontextprotocol.io`) with `mcp-publisher`, authenticated by
   GitHub OIDC. The registry checks the `io.modelcontextprotocol.server.name`
   label in `Dockerfile` against the pushed image.

### MCPB bundle layout

An MCPB manifest can branch per operating system (`platform_overrides`), not
per CPU architecture. The bundle therefore carries three binaries:

| OS | Binary |
|----|--------|
| Linux | `linux/amd64` |
| macOS | universal (`amd64` + `arm64`), so Apple Silicon runs natively |
| Windows | `windows/amd64` |

On Linux ARM, use the container image or `go install`.

## Cutting a release

```bash
git tag v1.0.0          # lightweight tag
git push origin v1.0.0
```

Then watch the run:

```bash
gh run list --repo entire-vc/evc-mesh-mcp --workflow release.yml --limit 3
```

To re-run the pipeline for an existing tag (for example after a transient
registry error), dispatch it against the tag:

```bash
gh workflow run release.yml --repo entire-vc/evc-mesh-mcp --ref v1.0.0
```

## After the first release

- **Container visibility.** The first push to `ghcr.io/entire-vc/evc-mesh-mcp`
  creates the package, and it may start out private. An organisation owner
  makes it public once in the package settings. Check with an anonymous
  `docker pull ghcr.io/entire-vc/evc-mesh-mcp` before relying on it.

## Glama

[Glama](https://glama.ai/mcp/servers) builds the listing from `Dockerfile`
and calls `initialize` and `tools/list` without credentials. The server
supports that: with `MESH_AGENT_KEY` unset it lists its tools and answers
every tool call with setup instructions. `glama.json` names the maintainers.
