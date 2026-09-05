# MindCache FYI — Server

[![CI](https://github.com/mindcache-fyi/server/actions/workflows/ci.yml/badge.svg)](https://github.com/mindcache-fyi/server/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)

The knowledge-capture backend for [MindCache FYI](https://mindcache.fyi). It
receives raw web/chat context captured by the [browser extension](https://github.com/mindcache-fyi/extension),
uses an LLM to distill it into lasting knowledge entries ("mindcaches"), and
serves them back through a simple REST API with a built-in API explorer.

Everything runs on your own hardware with your own LLM keys — see the
[self-hosting guide](https://mindcache.fyi/guides/self-hosting/) for the
bigger picture, or the [quick start](https://mindcache.fyi/start/quick-start/)
to be up and running in minutes.

## Features

- **LLM analysis** — captured content is summarized into structured knowledge
  using any OpenAI-compatible endpoint (OpenAI, DeepSeek, Ollama, vLLM, llama.cpp, ...);
  two focused calls per capture (`staged` mode) or a single-call experimental
  `unified` mode via `ANALYSE_MODE`
- **Deduplication** — content-hash checks plus an LLM relevance judge keep your
  knowledge base free of duplicates, with in-flight request coalescing via
  singleflight
- **Full-text search** — a locally maintained index over all mindcache content
- **Optional embeddings** — with `EMBED_MODEL` set, retrieval-based matching
  narrows candidates by similarity before the LLM match call, so collections
  with hundreds of notes keep matching fast and accurate; falls back to the
  full list automatically when unconfigured or unreachable
- **Multi-machine sync** — share one blob bucket across machines; metadata
  sidecars are reconciled in the background (see below)
- **Pure Go** — no CGO required; SQLite via `modernc.org/sqlite`, so a single
  static binary is all you need
- **Pluggable storage** — blob storage driven by a URL
  ([`gocloud.dev/blob`](https://gocloud.dev/howto/blob/)): local filesystem by
  default, S3 / GCS / Azure Blob also supported
- **Built-in API docs** — Swagger UI at `/apidoc/`, OpenAPI spec at `/openapi.json`
- **Concurrency-safe** — bounded LLM concurrency via a semaphore

## Quick Start

### From source

```bash
git clone https://github.com/mindcache-fyi/server.git
cd server
make build        # binary at bin/server
```

Then run it against any OpenAI-compatible endpoint:

```bash
LLM_BASE_URL=http://localhost:11434/v1 ./bin/server
```

The server listens on `http://localhost:9000` by default. In production mode
the database and blob storage default to `./mindcache.db` and `file://.` in the
current directory.

### Docker

The repo ships a Dockerfile (multi-stage, distroless runtime). Build the
image yourself and run it:

```bash
docker build -t mindcache-server .
docker run -p 9000:9000 \
  -e LLM_BASE_URL=http://host.docker.internal:11434/v1 \
  -v mindcache-data:/data \
  mindcache-server
```

Requires Go 1.25+ to build from source.

## Configuration

All configuration is via environment variables:

| Variable | Description | Default |
|---|---|---|
| `PORT` | Listen port | `9000` |
| `DB_PATH` | SQLite database path | `mindcache.db` |
| `STORAGE_URL` | Blob storage URL (`file://`, `s3://`, `gs://`) — see [S3 storage credentials](#s3-storage-credentials) | `file://.` |
| `LLM_BASE_URL` | OpenAI-compatible LLM endpoint | — |
| `LLM_API_KEY` | LLM API key | `local` |
| `LLM_MODEL` | LLM model name | — |
| `LLM_MAX_CONCURRENCY` | Max concurrent LLM calls | `1` |
| `LLM_MAX_INPUT_CHARS` | Cap on conversation text sent to the LLM per call (≤0 disables) | `100000` |
| `ANALYSE_MODE` | Analysis pipeline: `staged` (two calls) or `unified` (experimental, one call per capture) | `staged` |
| `EMBED_BASE_URL` | Optional embeddings endpoint for retrieval-based matching | falls back to `LLM_BASE_URL` |
| `EMBED_API_KEY` | API key for the embeddings endpoint | falls back to `LLM_API_KEY` |
| `EMBED_MODEL` | Embedding model name — enables retrieval-based matching | empty (disabled) |
| `MATCH_CANDIDATE_K` | Retrieval candidates sent to the LLM per topic | `5` |
| `EMBED_MIN_COLLECTION` | Collection size above which retrieval is used | `30` |
| `SYNC_INTERVAL_SECONDS` | How often metadata is reconciled against the blob bucket | `60` |

Model and provider guidance lives in
[Choosing an LLM](https://mindcache.fyi/guides/choosing-an-llm/) in the docs.

### S3 storage credentials

`s3://` URLs carry no credentials — the AWS SDK resolves them from the
standard chain: the `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` environment
variables (optionally `AWS_SESSION_TOKEN` or `AWS_PROFILE`), a shared
`~/.aws` configuration, or the instance role on EC2/ECS. Everything else
about the bucket is configured through URL query parameters:

```bash
# AWS S3
STORAGE_URL='s3://my-bucket?region=us-east-1'

# Cloudflare R2
STORAGE_URL='s3://my-bucket?region=auto&endpoint=https://<account-id>.r2.cloudflarestorage.com'

# MinIO (path-style addressing is usually required)
STORAGE_URL='s3://my-bucket?region=us-east-1&endpoint=http://localhost:9000&use_path_style=true'
```

Public buckets can pass `anonymous=true` instead of credentials. `gs://`
likewise carries no credentials in the URL: it uses Google Application
Default Credentials (`GOOGLE_APPLICATION_CREDENTIALS` or the ambient
service account).

Run `./server --help` for an overview.

## Multi-machine sync

Several machines (e.g. one install per computer) can share a
single blob bucket — point every instance's `STORAGE_URL` at the same
`file://`, `s3://`, or `gs://` bucket while each keeps its own local SQLite
database.

Each mindcache's metadata (brief, source URLs, timestamps) is written as a
`meta.json` sidecar next to its `main.md` in the bucket, which is the source
of truth. Every instance periodically reconciles its local database against
those sidecars: new and updated mindcaches are adopted, deletions propagate,
and legacy content without a sidecar gets one seeded automatically. Conflict
resolution is last-write-wins, arbitrated by the sidecar's modification time
in the bucket rather than by machine clocks.

Notes:

- Sync is eventual; changes appear on other machines within one sync
  interval (`SYNC_INTERVAL_SECONDS`).
- Embeddings and the full-text index are derived locally on each machine.
- Concurrent updates of the same mindcache from two machines within the same
  interval: the later bucket write wins.

## API

| Method | Path | Description |
|---|---|---|
| GET | `/health` | Health check |
| POST | `/v1/api/analyse` | Analyse captured content into mindcaches |
| GET | `/v1/api/list` | List mindcaches |
| GET | `/v1/api/get/{id}` | Get a mindcache with its full content |
| GET | `/v1/api/search` | Full-text search across mindcaches (`?q=`) |
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
make lint         # run golangci-lint (auto-installs via go run if missing)
make hooks        # opt in to the pre-commit lint hook
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
