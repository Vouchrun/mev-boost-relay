# Relay Deploy Runbook — boost-relay.vouch.run bring-up

**Audience:** the owner, executing by hand on the voter/relay server.
**Goal:** bring up the full Vouchrun/mev-boost-relay fork (api + housekeeper + website) **after
the validation node is synced**, on PulseChain, with protocol fee-recipient enforcement
enabled.
**Source of truth:** this fork's `pulse` branch (PulseChain network support) plus the
protocol fee-recipient enforcement feature; verify every flag name against `cmd/` (api.go,
housekeeper.go, website.go, variables.go) before running — flag names below were checked
against the source.
**Deployment artifacts:** the concrete compose + Caddyfile live in **`ops/relay/`**
(`docker-compose.yml`, `Caddyfile`) — use those, not the inline skeletons below.

> **Prerequisite:** the block-validation node must be synced first (see
> `repos/go-pulse-builder/docs/validation-node-runbook.md`). The relay validates every builder block
> against it; a stale validation node means failed validations.

---

## 1. Prerequisites checklist

- [ ] **Validation node** synced and serving `flashbots` at `http://127.0.0.1:18546`
      (localhost-only) — the `repos/builder/ops/validation-node/docker-compose.yml` stack.
- [ ] **Beacon node (Lighthouse-Pulse)** of the validation stack running — the relay uses it
      **read-only** (SSE `head`/`payload_attributes`, `/eth/v1/beacon/...`) at
      `http://validation-consensus:5052` over the shared `mev-net` network.
- [ ] **Shared docker network `mev-net` created once** (both stacks join it):
      `docker network create mev-net`. See §2.
- [ ] **DNS** for `boost-relay.vouch.run` pointing at this server (CNAME chain, §2.2).
- [ ] **Router port-forward 443** → this server (§2.3).
- [ ] **TLS cert** for `boost-relay.vouch.run` (Let's Encrypt, automatic via Caddy — §5).
- [ ] **Relay signing key** — generate a BLS keypair (§4); the **public key** is embedded in
      the relay URL that sidecars use (`https://<pubkey>@boost-relay.vouch.run`).
- [ ] NodeDeposit / VFD addresses (for the enforcement flags): VFD
      `0x9325008eE3B5982c10010C8f12b6CD4943F48fA6`, NodeDeposit
      `0x3f82615aE0C027d587FD0d04d9EaCc8f0BaCFf94`.
- [ ] Disk/RAM: Postgres + Redis are small; budget ~2-4GB RAM for the three Go processes and
      headroom for the validation node's sync.

---

## 2. Network wiring

### 2.1 Shared docker network `mev-net`

The validation node publishes its RPC to **host loopback only** (`127.0.0.1:18546`), so a
container cannot reach it via the host. The relay stack therefore joins the **same external
docker network** as the validation stack and reaches it by container name:

| Endpoint | Container | Used for |
|---|---|---|
| `http://validation-node:18546` | validation-node | `-blocksim` (block validation) + `-vouch-registry-rpc` (registry enumeration) |
| `http://validation-consensus:5052` | validation-consensus | `-beacon-uris` (head / payload_attributes / beacon API) |

Create the network **once** before starting either stack:

```bash
docker network create mev-net
```

- `repos/mev-boost-relay/ops/relay/docker-compose.yml` — declares `mev-net` as
  `external: true` and attaches every relay service to it.
- `repos/builder/ops/validation-node/docker-compose.yml` — attaches `validation-node` and
  `validation-consensus` to `mev-net` (the existing loopback port publishes stay, for
  host-side `curl` checks).
- The relay api also gets `extra_hosts: host.docker.internal:host-gateway` for **optional
  host-loopback fallbacks** (production lighthouse `5052` / production geth `8545` on the
  host) if the mev-net endpoints are ever unavailable.

**Beacon node requirement (`-beacon-uris`):** the relay's beacon node(s) must emit the
`payload_attributes` SSE event (head tracking, duties, and the getPayload publish path all
depend on it). A Lighthouse beacon with **no attached VC** requires
`--always-prepare-payload` — otherwise no `payload_attributes` events are emitted and the
relay rejects builder submissions every slot (`checkSubmissionPayloadAttrs`; upstream
README, **"Beacon node setup"**). Also set `--prepare-payload-lookahead` for a full-slot
build window (e.g. `10000` ms for PulseChain's 10s slots, adapted from the README's
`12000` ms on 12s mainnet slots). On val002 the pilot colocates this role onto
`validation-consensus` (dual role with the builder's CL); in the production multi-host
deployment the relay operator runs a **dedicated synced Lighthouse** with these flags on
the relay host — the production/validator beacon is **never** a `-beacon-uris` target.

### 2.2 DNS

`boost-relay.vouch.run` must resolve to this server's public IP. The DNS chain (managed by
the owner at the registrar/DNS provider):

```
CNAME  boost-relay.vouch.run  ->  loop.vouch.run  ->  A  <server public IP>
```

Refer to the server's public IP generically (it is not committed anywhere in this repo).

### 2.3 Router

Port-forward **443/tcp** (and only 443) from the router to this server. Let's Encrypt is
served by Caddy via TLS-ALPN-01 on 443 — no port 80 is required or exposed.

### 2.4 LAN split-horizon (LAN-resident pilot validators)

Pilot validators that live on the same LAN as the relay should use a **split-horizon DNS**
entry so `boost-relay.vouch.run` resolves to the relay's **LAN address** (not the public IP):
otherwise their traffic hairpins out to the router and back. Recommended: add a LAN DNS
record (e.g. on the local resolver) for `boost-relay.vouch.run` → the relay server's LAN IP,
used only by LAN-resident sidecars.

---

## 3. Deployment artifacts (`ops/relay/`)

The concrete deployment lives in this repo at **`ops/relay/`**:

- **`ops/relay/docker-compose.yml`** — Postgres 16, Redis 7, the three relay processes
  (api + housekeeper + website, image `vouchrun/mev-boost-relay:pulse-<commit>`), and Caddy.
  All services join `mev-net` (§2.1). Only **443** is published publicly; api (9062),
  housekeeper (9064) and website/data (9060) are published to **host loopback only** —
  never expose them (or 18551/8000) to the public internet.
- **`ops/relay/Caddyfile`** — `boost-relay.vouch.run { reverse_proxy relay-api:9062 }` with
  automatic Let's Encrypt.

**Secrets** are passed via the environment (never in the compose file):

| Env var | Meaning |
|---|---|
| `RELAY_DB_PASSWORD` | Postgres password (also used in the relay DSN) |
| `RELAY_SECRET_KEY` | BLS secret key from §4 (32 bytes, `0x`-prefixed) |
| `RELAY_PUBKEY` | BLS public key from §4 (used in the website's relay URL) |

**DB migrations** are applied by the relay itself at startup (the `database` package runs
the embedded migrations) — no manual schema step.

---

## 4. Relay signing key

Run the fork's keypair generator (or use any BLS secret key generator):

```bash
cd /opt/mev/relay        # checkout of Vouchrun/mev-boost-relay, branch pulse
go run ./scripts/create-bls-keypair
# prints:  secret key: 0x...   public key: 0x...
```

- **`secret key`** → `RELAY_SECRET_KEY` (the `-secret-key` flag for the **api** process).
- **`public key`** → `RELAY_PUBKEY` and the `<pubkey>` in the relay URL given to sidecars:
  `https://<public_key>@boost-relay.vouch.run`. All three processes must agree on the network
  and the relay key so the website/data API match the api process.

Store the secret key in the environment/secret manager on the server; never in a git repo.

---

## 5. Relay processes + reverse proxy

Run the stack from `ops/relay/`:

```bash
cd ops/relay
export RELAY_DB_PASSWORD=... RELAY_SECRET_KEY=... RELAY_PUBKEY=...
docker compose up -d
```

All three processes set `SEC_PER_SLOT=10` (PulseChain's 10s slots; upstream default is 12 —
`common/common.go`) and `-network pulsechain` (the `pulse` branch registers
`EthNetworkPulsechain`). Flags in the compose (verified against `cmd/api.go`,
`cmd/housekeeper.go`, `cmd/website.go`):

- **api** (`relay-api`): `-listen-addr=0.0.0.0:9062`, `-beacon-uris=http://validation-consensus:5052`,
  `-redis-uri=redis:6379`, `-db=postgres://relay:${RELAY_DB_PASSWORD}@postgres:5432/relay?sslmode=disable`,
  `-secret-key=${RELAY_SECRET_KEY}`, `-blocksim=http://validation-node:18546`, plus the
  enforcement flags (`-protocol-fee-recipient`, `-vouch-registry-address`,
  `-registry-refresh-interval=5m`, `-vouch-registry-rpc=http://validation-node:18546`).
- **housekeeper** (`relay-housekeeper`): `-listen-addr=0.0.0.0:9064` (metrics/debug),
  `-beacon-uris=http://validation-consensus:5052`, `-redis-uri`, `-db`, `-network=pulsechain`.
- **website** (`relay-website`): `-listen-addr=0.0.0.0:9060`, `-redis-uri`, `-db`,
  `-relay-url=https://${RELAY_PUBKEY}@boost-relay.vouch.run`, `-link-data-api=https://boost-relay.vouch.run`.

Flag notes (verified against `cmd/`):
- `-blocksim` default is `http://localhost:8545` — the compose overrides it to the validation
  node's container endpoint.
- `-secret-key` — relay signing key from §4.
- `-known-validators` — optional comma-separated pubkey seed; the api also auto-refreshes
  known validators from the beacon node at startup and per head slot.
- `-builder-api` / `-data-api` / `-internal-api` / `-proposer-api` all default to enabled.
- Enforcement flags: `-protocol-fee-recipient`, `-vouch-registry-address`,
  `-registry-refresh-interval`, `-vouch-pubkeys-file` (optional static fallback),
  `-vouch-registry-rpc`. **All default-empty = enforcement disabled** (stock upstream
  behavior); set all of them to enable the Vouch predicate. Invalid hex in the address flags
  is fatal at startup — a typo must fail the process, not a mainnet slot.
- `-beacon-publish-uris` (api only, optional): if you want the relay to publish signed
  blinded blocks to a different beacon endpoint than the read endpoint. Defaults to the
  beacon-uris. Leave unset unless you have a separate publish endpoint.

**Reverse proxy / TLS** — Caddy (`ops/relay/Caddyfile`) terminates TLS for
`boost-relay.vouch.run` and proxies to `relay-api:9062` (container name on `mev-net`).
Let's Encrypt is automatic (TLS-ALPN-01 on 443). The relay URL format sidecars use:
`https://<relay_pubkey>@boost-relay.vouch.run`.

- **Never expose the validation node's RPC, the raw 9062, or the website/data ports to the
  public internet.** Only 443 is public.
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
   The enforcement is opt-in via the flags in §5; with all enforcement flags unset the
   relay behaves exactly like stock upstream.
4. **Logs:** check the api logs for `using genesis fork version` (PulseChain values:
   `0x00000369` / time `1683785555`), the registry sync (`vouch registry synced` with the
   pubkey count), and no fatal errors at startup.
5. **Registration signature domain sanity:** a wrong genesis fork version/root makes **every**
   registration fail — the `using genesis fork version: 0x00000369` log line is your cheap
   confirmation that `-network=pulsechain` took effect.

---

## 6.5 Monitoring (Tier 2 - MEV gate exporter)

A tiny loop service (`ops/monitoring/gate_exporter.py`, stdlib-only) runs in the
relay compose (`gate-exporter` service, loopback port **9701**) and evaluates the
pilot monitoring gates continuously, exposing them as Prometheus metrics.

**Gates it checks:**
- `mev_missed_proposals_total` - watched validators' missed proposer duties
  (beacon `duties/proposer` vs `beacon/blocks`; incremental slot cursor persisted
  to the `gate-state` volume; `WATCH_PUBKEYS` file required - empty = gate disabled).
- `mev_relay_delivered_recent` - blocks delivered via the relay's
  `/relay/v1/data/bidtraces/proposer_payload_delivered` within a recent slot window.
- `mev_relay_gas_drift_recent` - delivered blocks whose `gas_limit` < `GAS_LIMIT_MIN`.
- `mev_relay_enforcement_violations_recent` - delivered blocks whose
  `proposer_fee_recipient` != VFD.
- `mev_watched_validators`, `mev_eval_timestamp`, `mev_eval_errors_total`,
  `mev_exporter_up` (liveness).

**Configuration (env, all optional):** `BEACON_API`, `RELAY_API`, `VFD`,
`GAS_LIMIT_MIN` (default 44500000), `WATCH_PUBKEYS`, `MEV_EXPORTER_PORT`,
`EVAL_MIN_INTERVAL`, `STATE_FILE`. Endpoints are read-only; all HTTP timeouts <= 6s;
on any error the exporter keeps last-good values (only `mev_eval_errors_total`
moves) and never crashes the HTTP server.
Note: the exporter binds `MEV_EXPORTER_BIND` inside the container (compose sets
`0.0.0.0`); loopback-only exposure comes from the host-side publish
`127.0.0.1:9701:9700`, not from the container bind.

**Prometheus scrape snippet** (`ops/monitoring/prometheus-snippet.yml`): jobs for
the gate exporter (`127.0.0.1:9701`) and the mev-boost sidecar (`127.0.0.1:18551`;
the sidecar must be started with `-metrics` - already staged). Optionally scrape
the validation stack's own metrics (EL `127.0.0.1:6061`, beacon `127.0.0.1:5055`).

**Daily reconciliation (val002 cron, optional):** a once-daily job can re-verify
the gate deltas against the beacon/relay APIs directly (e.g. compare
`mev_missed_proposals_total` against a fresh beacon-slots sweep) and page on
divergence. **No Alertmanager is deployed** - alerts are log/CRON only until a
Tier-3 alerting decision is made.

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
      The compose pins `vouchrun/mev-boost-relay:pulse-<commit>` — update the tag when the
      pulse branch moves.
- [ ] Decide the public vs internal split for the data API and internal API endpoints (§5).
- [ ] Record the §7.1 10s timing-audit measurement (p95 submission latency vs the 10s
      budget) before the pilot.
- [ ] Establish the non-pilot control group and baseline missed-proposal rate (§7.4).
- [ ] Run and log the Postgres backup/restore drill (§7.2).
- [ ] Confirm the exact `-blocksim` and `-vouch-registry-rpc` reachability from inside the
      relay containers (should be `validation-node:18546` over `mev-net`; `host.docker.internal`
      is the optional fallback).

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
