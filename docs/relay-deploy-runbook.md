# Relay Deploy Runbook — boost-relay.vouch.run bring-up

**Audience:** the owner, executing by hand on the voter/relay server.
**Goal:** bring up the full Vouchrun/mev-boost-relay fork (api + housekeeper + website) **after
the validation node is synced**, on PulseChain, with protocol fee-recipient enforcement
enabled.
**Source of truth:** this fork's `pulse` branch (PulseChain network support) plus the
protocol fee-recipient enforcement feature; verify every flag name against `cmd/` (api.go,
housekeeper.go, website.go, variables.go) before running — flag names below were checked
against the source.

> **Prerequisite:** the block-validation node must be synced first (see
> `repos/builder/docs/validation-node-runbook.md`). The relay validates every builder block
> against it; a stale validation node means failed validations.

---

## 1. Prerequisites checklist

- [ ] **Validation node** synced and serving `flashbots` at `http://127.0.0.1:18546`
      (localhost-only).
- [ ] **Beacon node (Lighthouse-Pulse)** already running on this host — the relay reuses it
      **read-only** (SSE `head`/`payload_attributes`, `/eth/v1/beacon/...`). Confirm it
      answers at its HTTP endpoint (default assumed `http://localhost:3500`; adjust with
      `-beacon-uris`).
- [ ] **DNS A-record** `boost-relay.vouch.run` → this server's public IP.
- [ ] **TLS cert** for `boost-relay.vouch.run` (Let's Encrypt via the reverse proxy — §5).
- [ ] **Relay signing key** — generate a BLS keypair (§3); the **public key** is embedded in
      the relay URL that sidecars use (`https://<pubkey>@boost-relay.vouch.run`).
- [ ] NodeDeposit / VFD addresses (for the enforcement flags): VFD
      `0x9325008eE3B5982c10010C8f12b6CD4943F48fA6`, NodeDeposit
      `0x3f82615aE0C027d587FD0d04d9EaCc8f0BaCFf94`.
- [ ] Disk/RAM: Postgres + Redis are small; budget ~2-4GB RAM for the three Go processes and
      headroom for the validation node's sync.

---

## 2. Postgres 16 + Redis 7 — minimal compose

```yaml
services:
  redis:
    image: redis:7-alpine
    restart: unless-stopped
    command: ["redis-server", "--appendonly", "yes"]
    volumes: [redis-data:/data]

  postgres:
    image: postgres:16-alpine
    restart: unless-stopped
    environment:
      POSTGRES_DB: relay
      POSTGRES_USER: relay
      POSTGRES_PASSWORD: ${RELAY_DB_PASSWORD}
    volumes: [pg-data:/var/lib/postgresql/data]

volumes:
  redis-data:
  pg-data:
```

Postgres DSN used by the relay (all three processes):
`postgres://relay:${RELAY_DB_PASSWORD}@postgres:5432/relay?sslmode=disable`

> **DB migrations** are applied by the relay itself at startup (the `database` package runs
> the embedded migrations) — no manual schema step.

---

## 3. Relay signing key

Run the fork's keypair generator (or use any BLS secret key generator):

```bash
cd /opt/mev/relay        # checkout of Vouchrun/mev-boost-relay, branch pulse
go run ./scripts/create-bls-keypair
# prints:  secret key: 0x...   public key: 0x...
```

- **`secret key`** → the `-secret-key` flag for the **api** process (32 bytes, `0x`-prefixed).
- **`public key`** → the `<pubkey>` in the relay URL given to sidecars:
  `https://<public_key>@boost-relay.vouch.run`. All three processes must agree on the network
  and the relay key so the website/data API match the api process.

Store the secret key in the environment/secret manager on the server; never in a git repo.

---

## 4. Relay processes

Run all three as containers in one compose project, sharing the network with Postgres/Redis.
**All processes must set `SEC_PER_SLOT=10`** (PulseChain's 10s slots; upstream default is 12 —
`common/common.go`) and `-network pulsechain` (the `pulse` branch registers
`EthNetworkPulsechain`).

### 4.1 api (the proposer/builder/data API)

```yaml
  relay-api:
    image: vouchrun/mev-boost-relay:pulsechain-<commit>
    restart: unless-stopped
    environment:
      SEC_PER_SLOT: "10"
    command:
      - api
      - -network=pulsechain
      - -listen-addr=0.0.0.0:9062
      - -beacon-uris=http://lighthouse-pulse:3500
      - -redis-uri=redis:6379
      - -db=postgres://relay:${RELAY_DB_PASSWORD}@postgres:5432/relay?sslmode=disable
      - -secret-key=${RELAY_SECRET_KEY}
      - -blocksim=http://host.docker.internal:18546        # the validation node (§1)
      # protocol fee-recipient enforcement (Vouch registry predicate)
      - -protocol-fee-recipient=0x9325008eE3B5982c10010C8f12b6CD4943F48fA6
      - -vouch-registry-address=0x3f82615aE0C027d587FD0d04d9EaCc8f0BaCFf94
      - -registry-refresh-interval=5m
      - -vouch-registry-rpc=http://host.docker.internal:18546   # eth RPC for registry enumeration
    ports:
      - "127.0.0.1:9062:9062"   # behind the reverse proxy; not exposed publicly directly
```

Flag notes (verified against `cmd/api.go`):
- `-network=pulsechain` — required (pulse branch).
- `-blocksim` = the **validation node** URL (default `http://localhost:8545` — you MUST
  override it to the validation node's localhost port, `18546` in the validation runbook).
- `-secret-key` — relay signing key from §3.
- `-known-validators` — optional comma-separated pubkey seed; the api also auto-refreshes
  known validators from the beacon node at startup and per head slot.
- `-builder-api` / `-data-api` / `-internal-api` / `-proposer-api` all default to enabled.
- Enforcement flags: `-protocol-fee-recipient`, `-vouch-registry-address`,
  `-registry-refresh-interval`, `-vouch-pubkeys-file` (optional static fallback),
  `-vouch-registry-rpc`. **All default-empty = enforcement disabled** (stock upstream
  behavior); set all of them to enable the Vouch predicate. Invalid hex in the address flags
  is fatal at startup — a typo must fail the process, not a mainnet slot.

### 4.2 housekeeper (proposer duties, registrations, housekeeping)

```yaml
  relay-housekeeper:
    image: vouchrun/mev-boost-relay:pulsechain-<commit>
    restart: unless-stopped
    environment:
      SEC_PER_SLOT: "10"
    command:
      - housekeeper
      - -network=pulsechain
      - -beacon-uris=http://lighthouse-pulse:3500
      - -redis-uri=redis:6379
      - -db=postgres://relay:${RELAY_DB_PASSWORD}@postgres:5432/relay?sslmode=disable
      - -listen-addr=0.0.0.0:9064
```

### 4.3 website (status page / bid-trace browser; optional but recommended)

```yaml
  relay-website:
    image: vouchrun/mev-boost-relay:pulsechain-<commit>
    restart: unless-stopped
    environment:
      SEC_PER_SLOT: "10"
    command:
      - website
      - -network=pulsechain
      - -listen-addr=0.0.0.0:9060
      - -redis-uri=redis:6379
      - -db=postgres://relay:${RELAY_DB_PASSWORD}@postgres:5432/relay?sslmode=disable
      - -relay-url=https://${RELAY_PUBKEY}@boost-relay.vouch.run
      - -link-data-api=https://boost-relay.vouch.run
```

Flag notes (verified against `cmd/housekeeper.go`, `cmd/website.go`): housekeeper exposes
metrics/debug on `-listen-addr` (default `localhost:9064`); the api on 9062; the website on
9060.

> **`-beacon-publish-uris`** (api only, optional): if you want the relay to publish signed
> blinded blocks to a different beacon endpoint than the read endpoint. Defaults to the
> beacon-uris. Leave unset unless you have a separate publish endpoint.

---

## 5. Reverse proxy / TLS

Expose only the **api** (9062) through a reverse proxy with TLS for `boost-relay.vouch.run`
(nginx/caddy). The relay URL format sidecars use:
`https://<relay_pubkey>@boost-relay.vouch.run`.

```nginx
server {
    listen 443 ssl;
    server_name boost-relay.vouch.run;
    ssl_certificate     /etc/letsencrypt/live/boost-relay.vouch.run/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/boost-relay.vouch.run/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:9062;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_read_timeout 60s;
    }
}
server { listen 80; server_name boost-relay.vouch.run; return 301 https://$host$request_uri; }
```

- **Never expose the validation node's RPC or the raw 9062 to the public internet.**
- Decide which data API endpoints are public (`/relay/v1/data/...` on the api) vs internal;
  the `-internal-api` endpoints must stay behind the proxy/internal network.

---

## 6. Health verification

1. **Status endpoint** (proposer API):
   ```
   curl -s -o /dev/null -w "%{http_code}" https://boost-relay.vouch.run/eth/v1/builder/status
   # expect 200
   ```
   Also `curl -s https://boost-relay.vouch.run/livez` and `/readyz` (both 200 once the
   service is ready; readiness depends on known validators being refreshed — see the api
   logs).
2. **Data API** (bid traces — the revenue ledger):
   ```
   curl -s "https://boost-relay.vouch.run/relay/v1/data/bidtraces/proposer_payload_delivered?limit=1"
   # expect JSON array (possibly empty before the first delivered block)
   ```
3. **Registration dry-run:** after the pilot sidecars are configured, submit a test
   `registerValidator` from a validator client; confirm:
   - a **Vouch validator with a non-VFD recipient is rejected** (HTTP 400,
     `fee recipient not allowed for vouch validator`) — the enforcement predicate works;
   - a **Vouch validator with VFD recipient is accepted** (200);
   - an **external validator with any recipient is accepted** (200).
   The enforcement is opt-in via the flags in §4.1; with all enforcement flags unset the
   relay behaves exactly like stock upstream.
4. **Logs:** check the api logs for `using genesis fork version` (PulseChain values:
   `0x00000369` / time `1683785555`), the registry sync (`vouch registry synced` with the
   pubkey count), and no fatal errors at startup.
5. **Registration signature domain sanity:** a wrong genesis fork version/root makes **every**
   registration fail — the `using genesis fork version: 0x00000369` log line is your cheap
   confirmation that `-network=pulsechain` took effect.

---

## 7. Pre-pilot acceptance gates (plan §7)

These are executable gates, not review sign-off (§7.1). Run before the pilot:

- [ ] **10s-slot timing audit (§7.2):** the relay defaults assume 12s slots. Measure the p95
      of the validation path (block submission → publish) against the 10s budget on the live
      node; verify `getHeaderRequestCutoffMs` (default 3000, `service.go`) and the getPayload
      timeouts are safe at 10s. **This gate is owned by this deploy task, not the
      enforcement patch.** TODO-OPERATOR: record the measured p95 submission latency and
      confirm it stays inside a 10s slot with margin.
- [ ] **Registration signature domain validated against live PulseChain genesis values
      (§7.2)** — covered by the §6.5 log check.
- [ ] **Data API bid traces return correct rows for delivered pilot blocks (§7.2)** — the
      revenue ledger; verify against the first delivered pilot block.
- [ ] **Postgres backup/restore drill (§7.2)** — bid traces are accounting records; prove the
      backup and a restore.
- [ ] **Missed-proposal baseline (§7.4):** pilot validators' missed-proposal rate must be
      ≤ the non-pilot fleet baseline (the §6.1 status + proposer-duties monitoring feed this;
      establish the control group before enabling MEV-Boost on pilot nodes).
- [ ] **Fee-recipient enforcement (§7.2/§7.4):** 100% of relay-delivered blocks pay the
      registered recipient (VFD for Vouch validators) — verified from bid traces + execution
      traces; the §6.3 registration dry-run is the pre-pilot proof the predicate is live.

---

## 8. Failure drill (relay killed → validators fall back)

1. `docker compose stop relay-api` (kill the relay mid-slot).
2. On the pilot validator host: confirm the beacon client falls back to **local block
   building** with **zero missed proposals** (mev-boost sidecar fails closed — no relay bid →
   local block). Check the beacon/VC logs for the slot.
3. Restart the relay: `docker compose start relay-api`; confirm it reconnects to Postgres,
   Redis, the beacon node and the validation node, and serves `/eth/v1/builder/status` again.
4. Record the outcome; a missed proposal during the drill fails the §7.4 baseline gate.

> Local fallback is inherent: when no relay bid is available the validator builds locally as
> today (priority fees → VFD). No keys or fee-recipient changes are involved in the drill.

---

## 9. Rollback

- **Relay:** `docker compose stop` (all relay processes) → validators fall back to local
  building (as in §8). To fully remove: `docker compose down` + delete the Postgres/Redis
  volumes (bid traces are the only data of value — back them up first, §7).
- **Validation node:** see the validation-node runbook §7 (stop container, delete datadir).
  Production geth is untouched throughout.
- **DNS/TLS:** point `boost-relay.vouch.run` away or remove the cert if rolling back the
  public endpoint.

---

## TODO-OPERATOR items

- [ ] Confirm exact image tags for the relay fork from a committed state (no `latest`).
- [ ] Decide the public vs internal split for the data API and internal API endpoints (§5).
- [ ] Record the §7.1 10s timing-audit measurement (p95 submission latency vs the 10s
      budget) before the pilot.
- [ ] Establish the non-pilot control group and baseline missed-proposal rate (§7.4).
- [ ] Run and log the Postgres backup/restore drill (§7.2).
- [ ] Confirm the exact `-blocksim` and `-vouch-registry-rpc` reachability from inside the
      relay containers (`host.docker.internal` vs a shared network — depends on the compose
      network setup on this host).

---

## Registry warm-start (optional)

The relay's enforcement predicate is **fail-open for unknown pubkeys**: until the live
`-vouch-registry-rpc` sync has populated the registry, every validator is treated as
external and no enforcement happens. If you want enforcement active **before the first live
sync** (or to bootstrap offline), generate a static pubkeys file with the
`export-vouch-pubkeys` tool and pass it via `-vouch-pubkeys-file`.

**When to use it:**
- **Enforcement active before first live sync** — the static file seeds the registry at
  startup, so Vouch validators are enforced from boot (closes the cold-start fail-open
  window).
- **Offline bootstrap** — no EL RPC needed at relay runtime; the file is the only registry
  source.

**Generate the file** (run against any PulseChain EL RPC, e.g. the validation node or
`rpc.vouch.run`):

```bash
go run ./cmd/export-vouch-pubkeys \
  --registry-address 0x3f82615aE0C027d587FD0d04d9EaCc8f0BaCFf94 \
  --rpc http://127.0.0.1:18546 \
  --out /opt/mev/vouch-pubkeys.txt
# prints: vouch pubkeys exported: <N> nodes enumerated, <M> pubkeys written to ..., elapsed ...
```

- Enumeration uses the **per-index `pubkeysOfNode(node,i)` getter across all nodes** — never
  `getPubkeysOfNode` (quadratic memory wall, ~4,500 pubkeys at 45M gas).
- The tool **exits non-zero and writes nothing** on any RPC failure — a partial file is
  never produced; the operator must know the file is incomplete.
- Output format is exactly what `-vouch-pubkeys-file` expects: one lowercase `0x`-prefixed
  hex pubkey per line (deduped, sorted).

**Ops note — the file goes stale:** validators are added (and withdrawn) continuously, so a
static file drifts from the on-chain registry. It is a **warm-start/fallback only**; the
live `-vouch-registry-rpc` sync is the **primary** source and refreshes every
`-registry-refresh-interval` (default 5m). Regenerate the file whenever you need a fresh
snapshot, and keep `-vouch-registry-rpc` configured so the live sync takes over.
