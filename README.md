# disputes.online MCP server

A public, read-only [Model Context Protocol](https://modelcontextprotocol.io) server for
**disputes.online**. It lets an AI agent find open disputes and read how the stakes on them
are split — the crowd's implied odds.

**Endpoint:** `https://disputes.online/mcp` (Streamable HTTP, no authentication)

## Tools

| Tool | What it does |
|---|---|
| `search_disputes` | Disputes open to bets, filtered by language, category and country, sorted by pool size, number of bets or closing time |
| `search_disputes_by_tags` | Open disputes carrying any of the given tags |
| `get_dispute` | One dispute by id or page URL — open, or already settled, with the winning outcome |
| `list_categories` | The category tree, to find a `category_id` |

Every dispute comes with its page URL and, per outcome, the share of stakes
(`implied_probability`) and the payout multiplier. Amounts are in QOT, the platform's internal
currency. Lists default to English; pass `lang` for disputes written in another language.

There are no tools that place bets, create disputes or act for a user.

## Connecting

Claude Code:

```bash
claude mcp add --transport http disputes-online https://disputes.online/mcp
```

Any client that takes a JSON config:

```json
{ "mcpServers": { "disputes-online": { "type": "http", "url": "https://disputes.online/mcp" } } }
```

## How it works

The server has no database access. It calls the same public API the site uses
(`https://disputes.online/openapi.json`), so it can only say what the site already publishes.
It is stateless: any replica answers any request.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `APP_PORT` | `8098` | Port to listen on |
| `API_PATH` | `/mcp` | Path the protocol is served at |
| `DISPUTES_URL` | `http://disputes:8080` | Base URL of the dispute service |
| `CATEGORIES_URL` | `http://categories:8082` | Base URL of the category service |
| `HISTORY_URL` | `http://history:8095` | Base URL of the history service (settled disputes) |
| `PLATFORM_TIMEOUT` | `5s` | Timeout for one call to either service |
| `RATE_LIMIT_RPS` / `RATE_LIMIT_BURST` | `20` / `40` | Limit on the total request rate |

`GET /health` answers `ok`.

## Running locally

```bash
APP_PORT=8098 \
DISPUTES_URL=https://disputes.online/disputes \
CATEGORIES_URL=https://disputes.online/category \
HISTORY_URL=https://disputes.online/history \
go run ./cmd
```

Then point a client, or `npx @modelcontextprotocol/inspector`, at `http://localhost:8098/mcp`.

## Publishing to the MCP registry

`server.json` describes this server for the [official MCP registry](https://registry.modelcontextprotocol.io).
Its name, `io.github.peccancy/disputes-online`, is tied to the `peccancy` GitHub organisation, so
publishing takes an owner of that organisation:

```bash
brew install mcp-publisher
mcp-publisher login github
mcp-publisher publish
```

Bump `version` in `server.json` before publishing again; the registry refuses a version it already has.

## Tests

```bash
go test ./...
```
