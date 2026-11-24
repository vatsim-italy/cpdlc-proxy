# CPDLC FIR proxy

Shares one [Hoppie ACARS](https://www.hoppie.nl/acars/) station per FIR (for
example `LIRR`, `LIMM`) between all the sectors of that FIR. Sectors send
Hoppie-style requests to this proxy; only the proxy polls Hoppie, using its own
logon code. Pilots log on once to the FIR and stay connected across internal
sector changes.

A client needs more than a changed Hoppie URL to work with it: see the
[developer guide](docs/developer-guide.md), and §14 there for why existing ATC
clients don't work as they are.

The container serves plain HTTP on port 8080. Put a TLS-terminating reverse
proxy (the compose file uses Traefik) in front of it: logon codes cross the
wire, so do not expose the port directly.

## Image

`.github/workflows/image.yml` runs `go test` and pushes
`ghcr.io/<owner>/cpdlc-proxy` on every push: `latest` from the default branch,
plus a tag per branch name and `sha-<short>`, for `linux/amd64` and
`linux/arm64`. A new package on GHCR is private: either make it public (package
settings) or `docker login ghcr.io` on the server with a token that has
`read:packages`.

## Deploy

Needs Docker with Compose v2 and Traefik already running on a Docker network.
Only three files live on the server: `docker-compose.yml`, `config.json`, `.env`.

1. Copy `docker-compose.yml`, `config.example.json` and `.env.example` to the server.
2. `cp config.example.json config.json`, then edit `stations`: one entry per FIR
   with its Hoppie `station`, `facility` name and the sector callsign patterns.
   Airports that answer PDC requests get `"pdc": true`, and `"top_down"` names
   the CTR station that answers their PDCs while they are empty
   ([guide §11](docs/developer-guide.md#11-pdc)).
3. `cp .env.example .env`, then fill in:
   - `HOPPIE_LOGON`: the proxy's own Hoppie logon code.
   - `CPDLC_PROXY_HOST`: the public host name (DNS must point to the server).
   - `TRAEFIK_NETWORK`, `TRAEFIK_ENTRYPOINT`, `TRAEFIK_CERTRESOLVER`: match
     your Traefik setup (`docker network ls`, Traefik's static config).
   - `CPDLC_PROXY_TAG`: `latest`, or a branch name to run a branch's image.
4. If you build your own image, change the image owner in `docker-compose.yml`.
5. `docker compose up -d`
6. `docker compose logs -f` should show `listening on :8080, N stations`.

No proxied FIR may also be polled directly as its station (`LIRR`) by another
client: Hoppie hands each message to whoever polls first
([guide §1](docs/developer-guide.md#1-what-the-proxy-does)).

Each controller keeps their own Hoppie logon code; the proxy only checks it.

## Configuration

```json
{
  "hoppie_url": "https://www.hoppie.nl/acars/system/connect.html",
  "stale_seconds": 180,
  "stations": [
    { "station": "LIRR", "facility": "ROMA CTR", "sectors": ["LIRR_CTR", "LIRR_*_CTR"] },
    { "station": "LIRF", "pdc": true, "top_down": "LIRR", "sectors": ["LIRF_*", "LIRR_APP"] }
  ]
}
```

| Field | Meaning |
|---|---|
| `hoppie_url` | Hoppie endpoint. Default: Hoppie's public `connect.html`. |
| `poll_seconds` | Fixed Hoppie poll interval. Only for tests against a local simulator; leave it out in production (40 s). |
| `stale_seconds` | A Hoppie poll after a gap longer than this is an outage batch ([guide §10](docs/developer-guide.md#10-outages)). Default 180. |
| `station` | The Hoppie callsign the proxy polls and sends as. |
| `facility` | Name in `CURRENT ATC UNIT` when the proxy accepts a logon itself. Default: the station. |
| `sectors` | Controller callsign patterns (Go `path.Match` globs, matched against the upper-cased callsign) allowed to use this station. A callsign uses the first matching pattern in file order. |
| `pdc` | The station is an airport that answers PDC requests. |
| `top_down` | PDC station only: the CTR station whose sectors get its PDC requests while none of its own sectors is online. |

Flags: `-config` (default `config.json`), `-listen` (default `:8080`), `-state`
(state file; empty keeps no state across restarts). Environment:
`HOPPIE_LOGON` (required).

## Updating

`docker compose pull && docker compose up -d`. A config change alone needs
`docker compose restart`. To build locally instead: `docker build -t
cpdlc-proxy:latest .`

## State

State is saved to `/data/state.json` in the named volume `cpdlc-state` (on
SIGTERM, so `docker compose down` / `restart` keep it). A named volume is used
instead of `./data` because the container runs as uid 65532: a new named volume
takes the owner of the image's `/data`, while a bind mount would need
`chown 65532:65532 ./data` first. `docker compose down -v` deletes the state.

## Running without Docker

```sh
go build -o cpdlc-proxy .
HOPPIE_LOGON=<code> ./cpdlc-proxy -config config.json -listen 127.0.0.1:8080 -state state.json
```

## Tests

`go test ./...` runs the routing, MIN rewrite, transfer, outage and PDC
scenarios against an in-process fake Hoppie server.

## License

MIT, see [LICENSE](LICENSE).
