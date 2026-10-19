# go-http-api

HTTP API backed by AWS DynamoDB for managing items.

## Requirements

- Go 1.23+
- AWS DynamoDB table
- `aws-sdk-go-v2` dependencies (handled via Go modules)

## Environment Variables

| Variable | Required | Description | Default |
|----------|----------|-------------|---------|
| `DYNAMODB_TABLE` | yes | DynamoDB table name | - |
| `AWS_REGION` / `AWS_DEFAULT_REGION` | yes | AWS region | - |
| `PORT` | no | Listen port | `8080` |
| `RATE_LIMIT` | no | Requests per second per client (set to `0` to disable) | `10` |

## Building

```bash
go build .
```

## Running

```bash
DYNAMODB_TABLE=mytable AWS_REGION=us-east-1 ./go-http-api
# or with custom port
DYNAMODB_TABLE=mytable AWS_REGION=us-east-1 PORT=9090 ./go-http-api
```

## Docker

```bash
docker build -t go-http-api .
docker run -p 8080:8080 \
  -e DYNAMODB_TABLE=mytable \
  -e AWS_REGION=us-east-1 \
  go-http-api
```

The container uses a scratch image and exposes port `8080`.

## Endpoints

### GET /

Service info.

**Response** `200 OK`:
```json
{ "service": "go-http-api" }
```

---

### GET /health

Health check.

**Response** `200 OK`:
```json
{ "status": "ok" }
```

Content-Type: `application/json`

---

### POST /api/items

Create an item.

**Request body**:
```json
{ "name": "my item", "description": "optional" }
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Item name |
| `description` | string | no | Optional description (omitted from response when empty) |

Item ID is auto-generated: `<random-hex>-<unix-millis>`.

**Response** `201 Created`:
```json
{ "id": "<generated-id>", "name": "my item", "description": "optional" }
```

**Errors**:
- `400 Bad Request` — invalid JSON body
- `500 Internal Server Error` — DynamoDB PutItem failure

---

### GET /api/items

List items (paginated).

**Query parameters**:

| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `limit` | int | 20 | Max items per page, clamped to [1, 100] |
| `cursor` | string | none | JSON-encoded DynamoDB `LastEvaluatedKey` — pass to fetch next page |

**Response** `200 OK`:
```json
{
  "items": [
    { "id": "a1b2c3-...", "name": "Item Name", "description": "optional" }
  ],
  "nextKey": "{\"id\":{\"S\":\"...\"}}"
}
```

Pass `nextKey` as `?cursor=<value>` to fetch the next page.

**Errors**:
- `500 Internal Server Error` — DynamoDB Query failure

---

### GET /api/items/{id}

Get a single item by ID.

**Response** `200 OK`:
```json
{ "id": "...", "name": "Item Name", "description": "optional" }
```

**Errors**:
- `404 Not Found` — item does not exist
- `500 Internal Server Error` — DynamoDB GetItem failure

---

### DELETE /api/items/{id}

Delete an item by ID.

**Response** `204 No Content` — empty body

---

## Data Model

```json
{
  "id": "<generated-id>",
  "name": "string",
  "description": "string (optional, omitted when empty)"
}
```

- `id`: generated as `<random-hex>-<unix-millis>` (8 random hex bytes + timestamp in milliseconds)
- `name`: required on creation
- `description`: optional, excluded from JSON response when empty (`omitempty`)

## Rate Limiting

Per-client token bucket rate limiting on all endpoints, keyed by client IP (`RemoteAddr`). Returns `429 Too Many Requests` when exceeded. Stale entries are cleaned up every 30 seconds.

Controlled via `RATE_LIMIT` env var (requests/second per client, default `10`, set to `0` to disable).

## Auth & Middleware

No authentication, API keys, JWT tokens, or CORS middleware. All endpoints are publicly accessible.
