# addresstify

Australian address search and validation API backed by [G-NAF](https://data.gov.au/data/dataset/geocoded-national-address-file-g-naf) (Geocoded National Address File) and SQLite FTS5. No Elasticsearch, no external search infrastructure — a single Go binary querying a local SQLite database.

## Architecture

```
loader (one-shot)          →  gnaf.sqlite (Docker volume)  →  api (long-running)
downloads G-NAF (~1.7GB)      ~15M addresses                   Go HTTP server
DuckDB join + dedup           SQLite FTS5 index                ~50ms queries
exports to SQLite             ~2GB on disk                     256MB RAM
```

The loader uses DuckDB at build time to join five G-NAF tables (`ADDRESS_DETAIL`, `ADDRESS_DEFAULT_GEOCODE`, `STREET_LOCALITY`, `LOCALITY`, `STATE`), deduplicates by address, then exports to a SQLite database with an FTS5 full-text search index. The API queries SQLite directly — DuckDB is not required at runtime.

## Quick Start

**1. Build the database:**
```bash
# Full dataset — all states (~45 min, ~15M addresses)
make load

# Single state — much faster for development (~5 min)
make load-qld
# or any state:
docker compose -f deploy/docker-compose.yml --profile loader run --rm \
  -e GNAF_STATES=NSW loader
```

The loader skips re-downloading the zip if it's already present and the file size matches the expected value. Subsequent refreshes only re-download when the quarterly URL is updated.

**2. Start the API:**
```bash
make serve
# or
docker compose -f deploy/docker-compose.yml up api
```

**3. Test it:**
```bash
make test-search
make test-validate
make test-health
```

## API

### `GET /addresses?q=...&limit=N`

Full-text address search. Returns up to `limit` results (default 10, max 50).

Ranking:
1. Exact street number match, principal address (no unit/flat)
2. Exact street number match, sub-unit addresses
3. FTS5 BM25 relevance score

**Example:**
```bash
curl "http://localhost:8080/addresses?q=1+pitt+street+sydney&limit=5"
```

**Response:**
```json
{
  "results": [
    {
      "pid": "GANSW718250359",
      "full_address": "1 PITT STREET SYDNEY NSW 2000",
      "number": "1",
      "street_name": "PITT",
      "street_type": "STREET",
      "suburb": "SYDNEY",
      "state": "NSW",
      "postcode": "2000",
      "latitude": -33.86136649,
      "longitude": 151.20814219
    }
  ],
  "total": 1,
  "query": "1 pitt street sydney"
}
```

### `POST /addresses/validate`

Structured address validation. Returns the best G-NAF match and a confidence score (0.0–1.0). `matched` is `true` when confidence >= 0.5.

**Request:**
```json
{
  "number": "1",
  "street_name": "Pitt",
  "street_type": "Street",
  "suburb": "Sydney",
  "state": "NSW",
  "postcode": "2000"
}
```

All fields are optional — provide as many as you have.

**Response:**
```json
{
  "match": {
    "pid": "GANSW718250359",
    "full_address": "1 PITT STREET SYDNEY NSW 2000",
    "number": "1",
    "street_name": "PITT",
    "street_type": "STREET",
    "suburb": "SYDNEY",
    "state": "NSW",
    "postcode": "2000",
    "latitude": -33.86136649,
    "longitude": 151.20814219
  },
  "confidence": 0.91,
  "matched": true
}
```

### `GET /health`

Returns `200 ok`.

## Environment Variables

### API

| Variable | Default | Description |
|---|---|---|
| `GNAF_DB_PATH` | `/data/gnaf.sqlite` | Path to the SQLite database |
| `GNAF_ADDR` | `:8080` | HTTP listen address |
| `GNAF_CORS_ORIGIN` | _(disabled)_ | `Access-Control-Allow-Origin` value, e.g. `*` |

### Loader

| Variable | Default | Description |
|---|---|---|
| `GNAF_DB_PATH` | `/data/gnaf.sqlite` | Path to write the SQLite database |
| `GNAF_DATA_DIR` | `/data/gnaf-raw` | Directory for the downloaded zip |
| `GNAF_STATES` | _(all states)_ | Comma-separated state filter, e.g. `QLD,NSW` |
| `GNAF_URL` | _(current release)_ | Override the G-NAF download URL |

## Data Updates

G-NAF is updated quarterly by Geoscape. To refresh:

```bash
make load
```

The loader writes to a `.tmp` file and atomically renames it on completion — the API keeps serving during a rebuild with no downtime.

**Updating the release:** Each quarter, update `gnafURL` and `gnafExpectedBytes` in `cmd/loader/main.go` to point to the new release. The latest URL can be found at [data.gov.au](https://data.gov.au/data/dataset/geocoded-national-address-file-g-naf). Alternatively, override at runtime with `GNAF_URL` (the size mismatch will trigger a fresh download automatically).

## Known Limitations

- **Address ranges:** Some addresses in G-NAF are registered as ranges (e.g. `175-183 PITT STREET`). Searching for a number within the range (e.g. `177`) may return no results.
- **Search token order:** The search uses phrase prefix matching — tokens must appear left-to-right as typed. `"1 pitt street sydney"` matches but `"sydney pitt street 1"` does not.
- **G-NAF coverage:** G-NAF contains physical street addresses only. PO boxes, RMB addresses, and some rural properties may not be present.

## License

MIT — see [LICENSE](LICENSE).

**Data attribution:** This project downloads and uses G-NAF © Geoscape Australia, licensed by the Commonwealth of Australia under the [Open G-NAF End User Licence Agreement](https://data.gov.au/data/dataset/geocoded-national-address-file-g-naf/resource/09f74802-08b1-4214-a6ea-3591b2753d30). The G-NAF data itself is not included in this repository.
