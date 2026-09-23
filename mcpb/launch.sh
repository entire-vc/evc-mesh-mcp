#!/bin/sh
# Default command of the MCPB bundle: runs the binary for this OS/arch.
#
# Clients that honour mcp_config.platform_overrides (Claude Desktop) start the
# per-OS binary directly and never run this. Clients that only read
# mcp_config.command (the Smithery CLI, for one) start this script, so the
# default must work on every POSIX host rather than point at the Linux binary.
dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
case "$(uname -s)/$(uname -m)" in
  Darwin/*) bin="$dir/evc-mesh-mcp-darwin-universal" ;;
  Linux/x86_64 | Linux/amd64) bin="$dir/evc-mesh-mcp-linux-amd64" ;;
  *)
    echo "evc-mesh-mcp: this bundle has no binary for $(uname -s)/$(uname -m)." >&2
    echo "Use the container image ghcr.io/entire-vc/evc-mesh-mcp or 'go install github.com/entire-vc/evc-mesh-mcp@latest'." >&2
    exit 1
    ;;
esac
exec "$bin" "$@"
