# CPDLC FIR proxy

One Hoppie station per FIR (`LIRR`, `LIMM`) shared by all its sectors. Sectors
send Hoppie-style requests to this proxy; only the proxy polls Hoppie, using its
own logon code. Design and behaviour: [docs/cpdlc.md §14](../../docs/cpdlc.md#14-fir-proxy-planned).

The container serves plain HTTP on port 8080. TLS is terminated by Traefik on
the VPS; logon codes cross the wire, so do not expose the port directly.

## Image

`.github/workflows/cpdlc-proxy-image.yml` runs `go test` and pushes
`ghcr.io/vatsim-italy/cpdlc-proxy` on every push that changes this folder:
`latest` from `main`, plus a tag per branch name and `sha-<short>`, for
`linux/amd64` and `linux/arm64`.
A new package on GHCR is private: either make it public (package settings) or
`docker login ghcr.io` on the VPS with a token that has `read:packages`.

## Deploy on the VPS

Needs Docker with Compose v2 and Traefik already running on a Docker network.
Only three files live on the VPS: `docker-compose.yml`, `config.json`, `.env`.

1. Copy `docker-compose.yml`, `config.example.json` and `.env.example` to the VPS.
2. `cp config.example.json config.json`, then edit `stations`: one entry per FIR
   with its Hoppie `station`, `facility` name and the sector callsign patterns.
3. `cp .env.example .env`, then fill in:
   - `HOPPIE_LOGON`: the proxy's own Hoppie logon code.
   - `CPDLC_PROXY_HOST`: the public host name (DNS must point to the VPS).
   - `TRAEFIK_NETWORK`, `TRAEFIK_ENTRYPOINT`, `TRAEFIK_CERTRESOLVER`: match
     your Traefik setup (`docker network ls`, Traefik's static config).
   - `CPDLC_PROXY_TAG`: `latest`, or a branch name to run a branch's image.
4. `docker compose up -d`
5. `docker compose logs -f` should show `listening on :8080, N stations`.

No FIR may also be polled directly as its station (`LIRR`) by another client:
Hoppie hands each message to whoever polls first (§14.1).

## Plugin settings

In each sector's `settings.txt`:

```
CPDLC_PROXY_URL=https://<CPDLC_PROXY_HOST>/
```

Without the line the plugin uses `https://cpdlc.example.org/`. A `HOPPIE_URL`
without `CPDLC_PROXY_URL` talks to that Hoppie server directly.

Each controller keeps their own Hoppie logon code; the proxy only checks it.

## Updating

`docker compose pull && docker compose up -d`. A config change alone needs
`docker compose restart`. To build locally instead, `docker build -t
ghcr.io/vatsim-italy/cpdlc-proxy:latest .` in this folder.

## State

State is saved to `/data/state.json` in the named volume `cpdlc-state` (on
SIGTERM, so `docker compose down` / `restart` keep it). A named volume is used
instead of `./data` because the container runs as uid 65532: a new named volume
takes the owner of the image's `/data`, while a bind mount would need
`chown 65532:65532 ./data` first. `docker compose down -v` deletes the state.

## Tests

From this folder: `go test ./...`

End to end against `tools/hoppie-sim`, from the repo root in an x86 developer
prompt (needs `cl`, Go and Python):
`powershell -ExecutionPolicy Bypass -File tests\run-cpdlc-proxy-e2e.ps1`
