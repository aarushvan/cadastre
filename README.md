# cadastre

cadastre v0.1.0 indexes a local source tree and searches its declarations and
call graph. It runs offline, uses only the Go standard library, and exposes
the same index through a stdio MCP server.

## Build

Requires Go 1.23 or later.

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o cadastre .
./cadastre version
go test -race ./...
```

The Linux release binaries are statically linked. The macOS build uses
macOS system libraries with CGO disabled.

Release tags beginning with `v` produce `cadastre-darwin-arm64`,
`cadastre-linux-x86_64`, `cadastre-linux-arm64`, and `SHA256SUMS`.

## Use

Run these commands from a source checkout with `cadastre` on your PATH.

```sh
cadastre index .
cadastre index . --out .cadastre.idx --force --json
cadastre query "request decoder" --mode hybrid --limit 10
cadastre query "decodeRequest" --mode lexical --json
cadastre query "decodeRequest" --mode graph
cadastre callers decodeRequest --depth 3
cadastre callers decodeRequest --callees --depth 2 --json
cadastre stats --json
cadastre mcp --index .cadastre.idx
```

The default index is `.cadastre.idx` in the current working directory.
`query`, `callers`, `stats`, and `mcp` accept `--index` to read another file.
Quote multiword queries. Graph traversal accepts a name, qualified name such
as `Worker.Run`, or exact ID such as `worker.go:42:Worker.Run`. If a name
matches several symbols, traversal starts from all of them. Trace depth zero
returns an empty traversal.

| Search mode | Ranking |
| --- | --- |
| `lexical` | BM25 over symbol names, bodies, and indexed documentation |
| `hybrid` | BM25 and exact-name matches, signed-int8 cosine, and one-hop graph signals |
| `graph` | Named and lexical seeds, their one-hop neighbors, and degree for matching nodes |

The vectors are deterministic 128-dimensional signed int8 feature hashes.
Identifier components and character trigrams contribute signed counts,
which are absmax-quantized to the range -127 through 127.

Go declarations and call expressions are extracted with `go/parser` and
`go/ast`. Go edges resolve same-package functions, identifiable receiver
methods, and explicit imports inside the root Go module. Type inference and
dynamic dispatch can leave edges unresolved. TypeScript, TSX, JavaScript,
MJS, Python, Rust, C, C++, and headers use lexical extraction. Their edges are
best-effort name matches, and complex syntax or ambiguous names can leave
symbols or edges unresolved.

The walker honors basic root and nested `.gitignore` rules, including globs,
negation, and directory patterns. It skips `.git`, `node_modules`, `dist`,
`build`, `vendor`, symlinks, and binary files. The index is a single file with
a gzip-compressed JSON manifest followed by a gzip-compressed JSON payload.
Matching nanosecond mtimes and sizes reuse cached entries. A changed stamp
triggers a SHA-256 check before reparsing. A no-op reads the manifest and
source metadata, and leaves the index file untouched. Use `--force` when
source tools preserve both mtimes and sizes.

## MCP setup

Create the index with `cadastre index .` before starting the server. Replace
the binary and index paths in these examples with their absolute paths. The
server uses the index snapshot loaded at startup. Restart the MCP server
after re-indexing to pick up changes.

### Claude Code

Add this to `.mcp.json` at the project root. Approve the project-scoped
server if Claude Code asks.

```json
{
  "mcpServers": {
    "cadastre": {
      "type": "stdio",
      "command": "/usr/local/bin/cadastre",
      "args": ["mcp", "--index", "/home/me/project/.cadastre.idx"]
    }
  }
}
```

### Cursor

Use `.cursor/mcp.json` for the project or `~/.cursor/mcp.json` for your user
account.

```json
{
  "mcpServers": {
    "cadastre": {
      "type": "stdio",
      "command": "/usr/local/bin/cadastre",
      "args": ["mcp", "--index", "/home/me/project/.cadastre.idx"]
    }
  }
}
```

### Google Antigravity

In Antigravity IDE, open the MCP Store, select **Manage MCP Servers**, and
choose **View raw config**. Add the server to the `mcpServers` object in
`mcp_config.json`.

```json
{
  "mcpServers": {
    "cadastre": {
      "command": "/usr/local/bin/cadastre",
      "args": ["mcp", "--index", "/home/me/project/.cadastre.idx"]
    }
  }
}
```

| Tool | Arguments |
| --- | --- |
| `cadastre_search` | `query` (required), `mode` (default `hybrid`), `limit` (default `10`) |
| `cadastre_callers` | `symbol` (required), `depth` (default `3`), `callees` (default `false`) |
| `cadastre_symbol` | `symbol` (required) |
| `cadastre_stats` | Empty object |

Results are JSON in MCP text content. The server speaks newline-delimited
JSON-RPC 2.0 and advertises MCP `2024-11-05`. Clients must support that
revision. Stdio messages are limited to 1 MiB on input.

For client settings, see [Claude Code][claude], [Cursor][cursor], and
[Google Antigravity][antigravity].

[claude]: https://code.claude.com/docs/en/mcp
[cursor]: https://cursor.com/docs/mcp
[antigravity]: https://antigravity.google/docs/ide-mcp

## License

Choose [MIT](LICENSE-MIT) or [Apache-2.0](LICENSE-APACHE).
