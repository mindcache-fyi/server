# MindCache FYI — Server

The knowledge-capture backend for [MindCache FYI](https://mindcache.fyi). It
receives raw web/chat context captured by the [browser extension](https://github.com/mindcache-fyi/extension),
uses an LLM to distill it into lasting knowledge entries ("mindcaches"), and
serves them back through a simple REST API with a built-in API explorer.

## Features

- **LLM analysis** — captured content is summarized into structured knowledge
  using any OpenAI-compatible endpoint (OpenAI, DeepSeek, Ollama, vLLM, llama.cpp, ...)
- **Deduplication** — content-hash checks plus an LLM relevance judge keep your
  knowledge base free of duplicates, with in-flight request coalescing via
  singleflight
- **Pure Go** — no CGO required; SQLite via `modernc.org/sqlite`, so a single
  static binary is all you need
- **Pluggable storage** — blob storage driven by a URL
  ([`gocloud.dev/blob`](https://gocloud.dev/howto/blob/)): local filesystem by
  default, S3 / GCS / Azure Blob also supported
- **Built-in API docs** — Swagger UI at `/apidoc/`, OpenAPI spec at `/openapi.json`
- **Concurrency-safe** — bounded LLM concurrency via a semaphore

## Quick Start

### Pre-built binaries

Download a binary for your platform from
[GitHub Releases](https://github.com/mindcache-fyi/server/releases), then:

```bash
LLM_BASE_URL=http://localhost:11434/v1 ./server
```

The server listens on `http://localhost:9000` by default. In production mode
the database and blob storage default to `./mindcache.db` and `file://.` in the
current directory.

### Docker

```bash
docker run -p 9000:9000 \
  -e LLM_BASE_URL=http://host.docker.internal:11434/v1 \
  -v mindcache-data:/data \
  ghcr.io/mindcache-fyi/server
```

### From source

```bash
git clone https://github.com/mindcache-fyi/server.git
cd server
make build        # binary at bin/server
```

Requires Go 1.25+.

## Configuration

All configuration is via environment variables:

| Variable | Description | Default |
|---|---|---|
| `PORT` | Listen port | `9000` |
| `DB_PATH` | SQLite database path | `mindcache.db` |
| `STORAGE_URL` | Blob storage URL (`file://`, `s3://`, `gs://`, `azblob://`) | `file://.` |
| `LLM_BASE_URL` | OpenAI-compatible LLM endpoint | — |
| `LLM_API_KEY` | LLM API key | `local` |
| `LLM_MODEL` | LLM model name | — |
| `LLM_MAX_CONCURRENCY` | Max concurrent LLM calls | `1` |

Run `./server --help` for an overview.

## API

| Method | Path | Description |
|---|---|---|
| GET | `/health` | Health check |
| POST | `/v1/api/analyse` | Analyse captured content into mindcaches |
| GET | `/v1/api/list` | List mindcaches |
| GET | `/v1/api/get/{id}` | Get a mindcache with its full content |
| POST | `/v1/api/create` | Create a mindcache |
| POST | `/v1/api/update` | Update a mindcache |
| DELETE | `/v1/api/delete/{id}` | Delete a mindcache |

Start the server and open `http://localhost:9000/apidoc/` for the interactive
API explorer.

## Development

```bash
make dev          # development mode with hot config
make dev-air      # development mode with hot reload (requires air)
make test         # run tests with race detector
make lint         # run golangci-lint
make swagger      # regenerate Swagger docs from annotations
```

## Related Projects

- [mindcache-fyi/extension](https://github.com/mindcache-fyi/extension) — the browser
  extension that captures web context
- [mindcache-fyi/dashboard](https://github.com/mindcache-fyi/dashboard) — the web
  dashboard for browsing and managing your mindcaches
- [mindcache.fyi](https://mindcache.fyi) — website and documentation

## License

[MIT](./LICENSE)
