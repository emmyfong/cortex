# Cortex

Local-first knowledge graph and RAG system. Ingests documents, chunks them
semantically, embeds them locally, and builds a connected graph of concepts
without manual tagging.

**Status: Milestone 1 — infrastructure and schema.** The datastores, database
schema, service scaffolds, and dev loop are in place. The ingestion pipeline,
search, and graph arrive in later milestones.

## Prerequisites

| Tool | Purpose | Install |
|---|---|---|
| Go 1.26+ | API and worker | `winget install GoLang.Go` |
| Node 22+ | Web app and task scripts | [nodejs.org](https://nodejs.org) |
| Docker Desktop | Postgres and Redis | [docker.com](https://docker.com) |
| Ollama | Local embedding model | `winget install Ollama.Ollama` |

After installing Ollama, pull the embedding model:

```bash
ollama pull nomic-embed-text
```

Ollama runs natively on the host (not in Docker) so it can use the GPU without
container passthrough configuration.

## First run

```bash
npm run setup       # checks prerequisites, creates .env from .env.example
npm run dev         # starts datastores, migrates, runs api + worker + web
```

Then open <http://localhost:3000> — the status page reports each dependency's
health live.

`npm run setup` never overwrites an existing `.env`.

## Architecture

```
Next.js (3000) ──proxy /api/*──> Go API (8080) ──> Postgres + pgvector (5432)
                                      │                    ▲
                                      └──> Redis (6379) ───┤
                                                           │
                             Go worker (8081) ─────────────┘
                                      └──> Ollama (11434)
```

The Go services and Next.js run natively on the host; only Postgres and Redis
run in Docker. Everything therefore addresses `localhost`. If you later
containerize the Go services, they will need `host.docker.internal` to reach
Ollama.

The browser only ever talks to `localhost:3000`. Next.js rewrites `/api/*` to
the Go API server-side, so there is no CORS preflight and the API origin is
never baked into a client bundle.

### Ports

| Port | Service |
|---|---|
| 3000 | Next.js web |
| 5432 | Postgres (bound to 127.0.0.1) |
| 6379 | Redis (bound to 127.0.0.1) |
| 8080 | Go API |
| 8081 | Go worker health |
| 11434 | Ollama |

## Commands

| Command | Action |
|---|---|
| `npm run dev` | Full stack: infra, migrations, api, worker, web |
| `npm run infra:up` / `infra:down` | Start / stop datastores |
| `npm run infra:nuke` | Stop datastores **and delete their volumes** |
| `npm run db:migrate` | Apply pending migrations |
| `npm run db:rollback` | Roll back the most recent migration |
| `npm run db:status` | Show which migrations have run |
| `npm run db:reset` | Nuke volumes, restart, re-migrate |
| `npm run db:psql` | Open a psql shell |
| `npm run api` / `worker` / `web` | Run one service |
| `npm test` | Go test suite |
| `npm run test:short` | Unit tests only (skips anything needing a database) |
| `npm run test:cover` | Tests with coverage |
| `npm run lint` | `go vet` plus ESLint |

### `npm run test:race`

Requires a C compiler (`gcc`), which the Go race detector depends on and which
is not installed by default on Windows. Install mingw-w64 (`winget install
BrechtSanders.WinLibs.POSIX.UCRT`) to enable it. `npm test` works without it.

## Database

Schema lives in [backend/internal/db/migrations/](backend/internal/db/migrations/)
and is embedded into the migration binary, so it cannot drift from the code that
expects it.

| Table | Purpose |
|---|---|
| `sources` | Ingested documents and their raw markdown |
| `chunks` | Retrieval units with 768-dim embeddings |
| `concepts` | Graph nodes |
| `concept_connections` | Undirected graph edges |
| `concept_mentions` | Links a concept to the chunk that produced it |
| `jobs` | Durable ingestion job status |
| `master_notes` | Co-authored synthesis notes |

Three constraints are load-bearing and easy to break accidentally:

- **`canonical_edge_order`** on `concept_connections` requires
  `concept_a_id < concept_b_id`. This is what makes edges genuinely undirected —
  a plain `UNIQUE (a, b)` still admits both `(A,B)` and `(B,A)`. **Writers must
  sort the pair before inserting.** It also subsumes a self-loop check.
- **`connection_count`** is maintained by a trigger, not by application code.
  Do not increment it manually.
- **`idx_concepts_name_ci`** makes concept names unique case-insensitively, so
  an LLM emitting both "Battery Degradation" and "battery degradation" produces
  one node rather than two.

## Testing

`npm test` runs unit tests plus integration tests against a dedicated
`cortex_test` database, created automatically on first run. Development data is
never touched.

Integration tests **skip** rather than fail when no database is reachable, so
`npm run test:short` and a machine without Docker still get a clean run. If you
are expecting them to execute, confirm with:

```bash
cd backend && go test -v ./internal/db/
```

and check for `PASS` rather than `SKIP` on the schema tests.

## Configuration

All configuration comes from `.env` (gitignored). `.env.example` is the tracked
template and contains local-development defaults only.

Configuration is required, not defaulted — a missing `DATABASE_URL` fails at
startup rather than silently connecting somewhere unexpected. Postgres
credentials are also required by `docker-compose.yml`, so the container cannot
start with an unintended default password.

Passwords are redacted from logs and from HTTP responses. `Config` implements
`slog.LogValuer`, and database errors are stripped of connection strings before
being wrapped — drivers routinely embed the full DSN in error text.

## Roadmap

- **M2** — ingestion endpoint, parsers, header-aware chunker, Ollama embedding
  client, asynq task handlers, SSE progress stream, `/search`.
- **M3** — concept extraction, graph edges, force-graph UI, master notes.
