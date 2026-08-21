# PulseChain config values for the mev-boost-relay fork

Task: PulseChain-enable the Vouchrun/mev-boost-relay fork (Phase 0). Atlas's fork
(`atlasbuilderxyz/mev-boost-relay`, AGPL-3.0) is used as a reference for the PulseChain
values only; the implementation follows upstream flashbots/mev-boost-relay's own network
structure (`common.NewEthNetworkDetails` + `-network`/`NETWORK` env). No commit lineage was
taken from Atlas. The VFD fee-recipient whitelist is NOT implemented (separate custom patch).

## Config values

| Value | Atlas value | Live beacon value | Value used | Notes |
|---|---|---|---|---|
| genesis_fork_version | `0x00000369` | `0x00000369` | `0x00000369` | matches live `/eth/v1/beacon/genesis` and `/eth/v1/config/spec` |
| genesis_validators_root | `0x3357ba0018a2582aeabe4ae847aa17d50a3a99aaeb66293c01f80a83aecd0c90` | `0x3357ba0018a2582aeabe4ae847aa17d50a3a99aaeb66293c01f80a83aecd0c90` | `0x3357ba0018a2582aeabe4ae847aa17d50a3a99aaeb66293c01f80a83aecd0c90` | used for proposer signing-domain computation; matches live |
| genesis_time | not set in relay | `1683785555` | not set in relay | relay fetches genesis time live from the beacon node (`/eth/v1/beacon/genesis`) — no config value needed |
| bellatrix_fork_version / epoch | `0x0000036b` / `2` | `0x0000036b` / `2` | `0x0000036b` | matches live `/eth/v1/config/spec` + `/eth/v1/config/fork_schedule` |
| capella_fork_version / epoch | `0x0000036c` / `3` | `0x0000036c` / `3` | `0x0000036c` | matches live; Capella is the latest active fork on PulseChain |
| deneb_fork_version / epoch | `0x0000036d` | `0xffffffff` / `18446744073709551615` (disabled) | `0xffffffff` | **Deviation from Atlas** — live spec reports Deneb disabled; the fork schedule has no Deneb entry, so this value is never matched at runtime (relay stays Capella-era). Atlas's `0x0000036d` is not on the network |
| electra_fork_version | `0x0000036e` | not defined (Deneb disabled) | `0x0000036e` | never active on PulseChain; Atlas value kept as placeholder |
| fulu_fork_version | `0x0000036f` | not defined (Deneb disabled) | `0x0000036f` | never active on PulseChain; Atlas value kept as placeholder |
| seconds_per_slot | `10` | `10` | `10` (operational) | relay reads `SEC_PER_SLOT` env var (default 12 in `common/common.go`). **Must set `SEC_PER_SLOT=10`** — no per-network default exists upstream |
| block gas limit | not set in relay | ~45,000,000 (elastic, ±1/1024) | not set in relay | current upstream relay has **no** block-gas-limit config (the plan doc's `RELAY_BLOCK_GAS_LIMIT` was illustrative). Gas limit comes from the builder's submitted block, validated by EL simulation |
| deposit contract / chain id | not set in relay | `0x3693693693693693693693693693693693693693` / `369` | not set in relay | relay's `GetSpec()` exposes `DEPOSIT_CONTRACT_ADDRESS`/`DEPOSIT_NETWORK_ID` from the live beacon node but does not consume them |

Live beacon source: `https://rpc-beacon.vouch.run/eth/v1/beacon/genesis`,
`/eth/v1/config/spec`, `/eth/v1/config/fork_schedule` (queried 2026-08-21, independently
re-verified against the sibling repo `repos/mev-boost/pulsechain-values.md`). Atlas values
extracted from commit `b1c0153` ("feat: Add PulseChain network support") on `atlas/main`.

## Files changed (branch `pulse`)

| File | Change |
|---|---|
| `common/types.go` | added `EthNetworkPulsechain`; added `GenesisForkVersionPulsechain`, `GenesisValidatorsRootPulsechain`, `BellatrixForkVersionPulsechain`, `CapellaForkVersionPulsechain`, `DenebForkVersionPulsechain`, `ElectraForkVersionPulsechain`, `FuluForkVersionPulsechain` constants; added `case EthNetworkPulsechain` in `NewEthNetworkDetails` |
| `common/types_test.go` | added `TestNewEthNetworkDetailsPulsechain` (fork versions + GVR + domains) |
| `pulsechain-values.md` | this report |

## Build & test results

- **Go toolchain:** `go1.24.13 windows/amd64` (portable ZIP, session-only PATH).
- **Build:** `go build ./...` — **PASSED** (exit 0).
- **Tests run (no external DBs needed):** `common`, `beaconclient`, `datastore`,
  `services/api`, `internal/investigations/...` — **ALL PASSED** (exit 0). Includes the new
  `TestNewEthNetworkDetailsPulsechain` (PASS).
- **Skipped — needs Postgres:** `database/` package (`database/database_test.go` connects to
  `TEST_DB_DSN` = `postgres://postgres:postgres@localhost:5432/postgres`). Not run; no
  Postgres available in this environment.
- **Self-skipping (integration-gated):** `datastore/memcached_test.go` skips unless
  `RUN_INTEGRATION_TESTS=1` + `MEMCACHED_URIS` are set (upstream behavior, unchanged).

## Relay topology prerequisites (per `knowledge/mev-stack-overview.md` Part II §3)

- **Postgres** — bid traces, validator registrations (revenue ledger).
- **Redis** — block-delivery cache; (optional Memcached as secondary).
- **Beacon nodes (1-2)** — Lighthouse-Pulse; SSE `head`/`payload_attributes` + genesis/fork
  schedule; relay fetches genesis time + fork schedule live from them.
- **Block-submission validation nodes (1-2)** — geth with the `flashbots` namespace, or the
  Vouch builder binary in `--builder.dry-run` mode; relay re-simulates every builder block.
- **Operational env for PulseChain:** `NETWORK=pulsechain`, `SEC_PER_SLOT=10`,
  `BEACON_URIS`, `REDIS_URI`, `POSTGRES_DSN`, `SECRET_KEY` (builder API signing key),
  `BLOCKSIM_URI` (validation EL). No gas-limit env exists upstream (block gas limit is
  carried by the submitted block itself).
