# Protocol Fee-Recipient Enforcement — Design

**Status:** Design only (no implementation). Branch `vfd-whitelist-design`, off `main`.
**Target:** Vouchrun/mev-boost-relay fork (upstream flashbots/mev-boost-relay base).
**Goal:** A **public-capable** relay with protocol-level fee-recipient enforcement for Vouch
validators: all MEV income from Vouch-proposed blocks flows to the ValidatorFeeDepositor
(VFD), while external validators receive normal public-relay service with no recipient
constraint.

**VFD address (verified in `catalog/contracts.yaml`):**
`0x9325008eE3B5982c10010C8f12b6CD4943F48fA6`

> **Correction to the original task brief:** the brief said to follow "upstream's CLI-flag
> pattern (urfave/cli)". `mev-boost-relay` uses **spf13/cobra**, not urfave/cli (urfave/cli is
> the mev-boost sidecar's framework; see `go.mod:27` `github.com/spf13/cobra v1.9.1`). The
> config surface below follows the relay's actual cobra + env-var conventions.

---

## 1. Predicate & enforcement points

### 1.0 The predicate (supersedes the static whitelist)

```
IF proposerPubkey ∈ VouchValidatorRegistry
THEN fee_recipient MUST == VFD (0x9325008eE3B5982c10010C8f12b6CD4943F48fA6)
ELSE (external validator) any fee_recipient is allowed — normal public-relay service
```

Rationale:
- **Vouch pubkeys carry a protocol obligation:** every Vouch validator's income is protocol
  income — it must flow to VFD (the existing protocol rule; VFD is already the registered
  recipient for all Vouch validators). The relay enforces this at the protocol boundary so a
  misconfigured or malicious Vouch validator cannot redirect MEV away from the protocol.
- **External service is pure upside:** the relay takes no cut of bids, so serving external
  proposers costs nothing and adds public-relay value. When OUR builder (Phase 4) builds for
  external proposers, the builder margin routes to VFD — the enforcement predicate is what
  guarantees that path stays protocol-aligned.

### 1.1 Enforcement points (locations unchanged from the static-whitelist design)

All paths are grounded in `services/api/service.go` (the API service) and
`services/housekeeper/housekeeper.go` (the background duty/registration pipeline).

**How the registered fee recipient flows (context):**
1. `handleRegisterValidator` (`services/api/service.go:1093`, route
   `POST /eth/v1/builder/validators`, registered at `service.go:407`) parses registrations
   (JSON via `parseValidatorRegistrationsJSON` `service.go:1255`, or SSZ via
   `processValidatorRegistrationsSSZ` `service.go:3260`), verifies BLS signatures
   (`service.go:1211`), and pushes new registrations to the `validatorRegC` channel
   (`service.go:1230`, cap 450_000 at `service.go:343`) for async DB+Redis persistence.
2. The **housekeeper** builds proposer duties: `Housekeeper.UpdateProposerDutiesWithoutChecks`
   (`services/housekeeper/housekeeper.go:189`) fetches beacon proposer duties, loads the
   validators' **registered** fee recipients from the DB
   (`hk.db.GetValidatorRegistrationsForPubkeys`, `housekeeper.go:219`), and stores
   `{slot, validatorIndex, Entry: registration}` in Redis (`hk.redis.SetProposerDuties`,
   `housekeeper.go:250`).
3. The **API** loads those duties into `proposerDutiesMap[slot]` via
   `UpdateProposerDutiesWithoutChecks` (`service.go:957`, map built at `service.go:972-975`).
   Each entry carries `Entry.Message.FeeRecipient` — the validator's **registered** recipient.

### 1.2 (a) Validator registration — `POST /eth/v1/builder/validators`

- Entry: `handleRegisterValidator` (`service.go:1093`).
- Processing: `processValidatorRegistrationJSON` (`service.go:3200`) and
  `processValidatorRegistrationsSSZ` (`service.go:3260`). Both loop over registrations,
  check timestamp bounds (`service.go:3206-3211`), compare against the cached registration
  (`GetCachedValidatorRegistration`, `service.go:3214`; change detection at
  `service.go:3221-3233`), verify the pubkey is a known validator
  (`IsKnownValidator`, `service.go:3237`), then append to `newRegistrations`
  (`service.go:3246-3254`).
- **Hook (new predicate):** at the top of each loop, evaluate the §1.0 predicate on
  `reg.Pubkey` (JSON) / `signedValidatorRegistration.Message.Pubkey` (SSZ) + `reg.FeeRecipient`
  / `Message.FeeRecipient`. If the pubkey is in the Vouch registry and the recipient is not
  VFD → reject (400, loud, metric). If the pubkey is not in the registry → allow any
  recipient. Placed **before** the cache lookup and BLS verification it is an O(1) registry
  membership test + address compare that also short-circuits signature work on spam.
- **Reject semantics (owner-approved decision Q1):** loud 400 with a clear message
  (`"fee recipient not whitelisted"`) plus a dedicated Prometheus counter; do **not**
  silently accept-and-ignore.

### 1.3 (b) Block submission — `POST /relay/v1/builder/blocks` (v1/v2/v3)

- Entry: `handleSubmitNewBlock` (`service.go:2343`). Validation sequence:
  `checkSubmissionSlotDetails` (`service.go:2514`) → `checkBuilderEntry` (`service.go:2520`)
  → **`checkSubmissionFeeRecipient`** (`service.go:2527`) → payload-attributes check
  (`service.go:2532+`) → simulation → `updateRedisBid` (`service.go:2284`).
- **Existing upstream check — the key anchor:**
  `checkSubmissionFeeRecipient` (`service.go:2092-2109`) already rejects any submission whose
  `bidTrace.ProposerFeeRecipient` does not match the **registered** recipient for that slot's
  proposer (`slotDuty.Entry.Message.FeeRecipient`), compared case-insensitively with
  `strings.EqualFold` (`service.go:2100`). On mismatch it returns 400
  `"fee recipient does not match"` (`service.go:2105`).
- **Hook (new predicate):** extend `checkSubmissionFeeRecipient` to evaluate the §1.0
  predicate on the slot's proposer pubkey (`slotDuty.Entry.Message.Pubkey`) + registered
  recipient (`slotDuty.Entry.Message.FeeRecipient`): registry-hit + non-VFD → 400;
  otherwise pass. This is **defense-in-depth AND the lock for the refresh window** (see §4):
  it covers registrations accepted before the registry sync picked the pubkey up, and any
  registration that bypassed the §1.2 filter.

### 1.4 (c) getHeader / getPayload — `GET /eth/v1/builder/header/...`, `POST /eth/v1/builder/blinded_blocks`

- `handleGetHeader` (`service.go:1283`) serves the best bid for the slot via
  `api.redis.GetBestBid(slot, parentHash, proposerPubkey)` (`service.go:1397`).
- `innerHandleGetPayload` (`service.go:1516`) returns the stored payload for the signed
  block hash.
- **Transitively safe:** bids only enter Redis via `updateRedisBid` after passing
  `checkSubmissionFeeRecipient` (§1.3), and getPayload only serves payloads whose bid passed
  getHeader. With §1.2 + §1.3 enforcing the predicate, the served bid's recipient is VFD for
  registry-hit proposers by construction.
- **getHeader defense-in-depth (owner-approved decision Q2):** **skip in Phase 0.** The
  submission gate already guarantees it; revisit only if a stale/non-conforming bid is
  observed in the wild.

### 1.5 (d) Does upstream verify feeRecipient matches the registered recipient?

**Yes.** `checkSubmissionFeeRecipient` (`service.go:2092-2109`) is exactly that check, at
block-submission time, comparing the bid's `ProposerFeeRecipient` against the slot duty's
registered `Entry.Message.FeeRecipient` (case-insensitive). The predicate is therefore not a
new verification mechanism — it is a **constraint on the registered recipient of Vouch
pubkeys**, layered on top of the existing comparison. With (a) + (b) in place, the existing
(d) check makes the whole path VFD-only for Vouch proposers and unrestricted for external
proposers.

---

## 2. Registry source: NodeDeposit

- **Contract:** `NodeDeposit` — mainnet address
  **`0x3f82615aE0C027d587FD0d04d9EaCc8f0BaCFf94`** (verified in
  `catalog/contracts.yaml:167`; repo `Vouchrun/pls-lsd-contracts`).
- **Registry today:** **34 nodes / 7,770 pubkeys** (20 trust nodes, 4,950 keys + 14 solo
  nodes, 2,820 keys — per `knowledge/mev-stack-overview.md` "Live Network Context").
- **Enumeration — CRITICAL:** enumerate validators via the **`Deposited`/`Staked` events**
  (filter client-side; `node` is not indexed today) or the **per-index getter
  `pubkeysOfNode(node, i)`** (iterate until empty). **NEVER `getPubkeysOfNode(node)`** — it
  returns the node's entire `bytes[]` and EVM **memory-expansion pricing is quadratic**
  (`3w + w²/512` gas), crossing the **45M block-gas wall at ~4,500 pubkeys per node** for
  default-gas RPC calls (cold eth_call; explicit-gas eth_call works to ~30k). This is the
  documented off-chain enumeration wall — cite
  `knowledge/validator-scaling-limits.md` (§4.3, "Enumeration: `getPubkeysOfNode` — the
  first wall (~4,500)"; RPC behavior measured live on rpc.vouch.run and g4mm4).
- **ABI facts (from `pls-lsd-contracts` `NodeDeposit.sol` artifact):**
  - `getPubkeysOfNode(address) -> bytes[]` — **do not use** (quadratic wall).
  - `pubkeysOfNode(address, uint256) -> bytes` — per-index getter (recommended).
  - `pubkeyInfoOf(bytes) -> uint8,address,uint256,uint256`.
  - Events: `Deposited(node, nodeType, pubkey, validatorSignature, amount)`,
    `Staked(node, pubkey)`, `EtherDeposited(from, amount)`, `SetPubkeyStatus(pubkey, status)`.
- **Future-proofing (per validator-scaling-limits.md §6.3):** in any future contract upgrade,
  make `node` **indexed** in `Deposited`/`Staked` and add a `getPubkeysLengthOfNode(address)`
  view — not required for this design.

---

## 3. Registry caching

- **Sync job → Redis set:** a background sync (in the API service, or a small dedicated
  goroutine) enumerates the registry and stores the Vouch pubkey set in Redis under a
  network-scoped key (e.g. `boost-relay/<network>:vouch-registry`), mirroring the existing
  `SetValidatorRegistrationData` pattern. The in-memory membership set on the API side is
  loaded from Redis at startup and refreshed on a timer.
- **Refresh interval:** default **5 minutes** (`-registry-refresh-interval`), configurable.
- **Last-known-good on RPC failure:** if an enumeration/refresh fails (RPC error, timeout),
  keep serving from the previous snapshot (last-known-good) and log + alert. Never clear the
  set on failure.
- **Fail-closed vs fail-open:**
  - **Known Vouch pubkeys (registry-hit): fail-CLOSED** — if the pubkey is in the last-known
    good registry set, a non-VFD recipient is rejected (both gates).
  - **Unknown pubkeys (registry-miss): fail-OPEN** — an unknown pubkey is treated as
    external and served normally. This keeps the relay public-capable even if the registry
    sync is stale or the set is empty (e.g. cold start before first successful sync).
- **Cold start:** until the first successful sync, the set is empty → all proposers are
  treated as external (fail-open). The `-vouch-pubkeys-file` static fallback (§5) closes the
  cold-start gap for operators who want enforcement from boot.

---

## 4. Refresh-window edge case

A new Vouch validator can register with a foreign (non-VFD) recipient **before** the registry
sync picks its pubkey up (the sync runs on a 5-minute cadence; the validator's registration
arrives at any time).

- **Registration gate is fast rejection, not the lock:** during the window, the §1.2
  registration gate may not yet know the pubkey (registry-miss → allowed). This is
  acceptable by design — the registration is accepted but flagged for follow-up.
- **Submission gate is the lock:** the §1.3 submission gate re-evaluates the predicate at
  **block time** against the refreshed registry. Once the sync has picked the pubkey up
  (≤ refresh interval later), any block whose registered recipient is non-VFD is rejected
  with 400 — the validator gets no MEV service for that slot and the rejection is visible in
  logs/metrics. Worst case exposure is bounded by the refresh interval (default 5 min), and
  the validator's own fee-recipient misconfiguration is what triggers it.
- **State this explicitly:** the design accepts a bounded window (≤ refresh interval) during
  which a not-yet-synced Vouch pubkey could slip a foreign-recipient registration through the
  registration gate; the submission gate is the authoritative enforcement point and closes the
  window at block time. The `-vouch-pubkeys-file` fallback can shrink the window to zero for
  known fleets.

---

## 5. Config surface (cobra pattern)

Pattern source (relay's actual conventions):
- Cobra flag + env default: `-network` / `NETWORK`
  (`cmd/variables.go:10`, `cmd/api.go:66`).
- Comma-separated slice flag: `-known-validators` (`cmd/api.go:75-76`,
  `apiCmd.Flags().StringSliceVar(&apiKnownValidators, "known-validators", nil, ...)`).
- Feature toggles read raw env in `NewRelayAPI` (`service.go:347-390`).

**Proposed flags (all default-empty → enforcement disabled = stock upstream behavior):**

| Flag | Env | Default | Purpose |
|---|---|---|---|
| `-protocol-fee-recipient` | `PROTOCOL_FEE_RECIPIENT` | empty | The protocol fee recipient (VFD). Empty → enforcement disabled. |
| `-vouch-registry-address` | `VOUCH_REGISTRY_ADDRESS` | empty | NodeDeposit contract address (mainnet `0x3f82615aE0C027d587FD0d04d9EaCc8f0BaCFf94`). Empty → enforcement disabled. |
| `-registry-refresh-interval` | `REGISTRY_REFRESH_INTERVAL` | `5m` | Registry sync cadence (only meaningful when enforcement enabled). |
| `-vouch-pubkeys-file` | `VOUCH_PUBKEYS_FILE` | empty | Static fallback: a file of Vouch pubkeys (one per line) merged into the registry set at startup — closes the cold-start/refresh window for known fleets. Empty → not used. |

- **Wiring:** `cmd/api.go` — `apiCmd.Flags().StringVar(...)` for each; pass into
  `RelayAPIOpts` (`service.go:152-180`) as `ProtocolFeeRecipient`,
  `VouchRegistryAddress`, `RegistryRefreshInterval`, `VouchPubkeysFile`.
- **Parsing:** at `NewRelayAPI` (`service.go:278`): `utils.HexToAddress` the recipient and
  registry address; invalid hex → fatal at startup (fail loud, not silent). Registry pubkeys
  are normalized to lowercase hex (the codebase's `NewPubkeyHex` convention).
- **Storage:** `RelayAPI` struct fields next to the feature flags (`service.go:253-262`):
  `protocolFeeRecipient *bellatrix.ExecutionAddress`, `vouchRegistry map[PubkeyHex]struct{}`
  (in-memory, loaded from Redis + static file), plus the sync job handle.
- **Housekeeper needs no config:** it only reads registrations from the DB; enforcement
  stays in the API service.

---

## 6. Resolved decisions (owner-approved)

| # | Question | Decision |
|---|---|---|
| Q1 | Reject vs silently-drop at registration (§1.2) | **Loud 400 + Prometheus counter** (e.g. `validator_registration_fee_recipient_rejected_total`). Validator clients log the 400; operators see the metric. No silent filtering. |
| Q2 | getHeader defense-in-depth (§1.4) | **Skip in Phase 0** — transitively safe via the submission gate; revisit only if a stale/non-conforming bid is observed. |
| Q3 | 10s-slot timing audit (§7 below) | **Pre-pilot acceptance gate owned by the relay-deploy task**, not this patch. The predicate adds O(1) per check (microseconds); the existing simulation/publish path must be measured against the 10s budget before the pilot. |
| Q4 | Validator flips a previously-VFD registration to non-VFD | **Reject-and-keep-old**: the new registration is rejected (400), the previous VFD registration stays in DB/Redis and remains the slot duty's recipient — the validator keeps receiving MEV to VFD. **No tombstone** needed; the cached-registration change detection (`service.go:3221`, `service.go:3283`) is unaffected because the non-VFD registration never reaches the cache. |

---

## 7. 10s slot timing & external-validator service notes

### 7.1 10s slot timing (PulseChain, `SEC_PER_SLOT=10`)

- Slot timing is `common.SecondsPerSlot` (`common/common.go:15`), default 12, set to 10 for
  PulseChain via the `SEC_PER_SLOT` env var. The relay computes slot boundaries from live
  genesis time (`service.go:1306`, `service.go:2476`).
- **Predicate latency cost: negligible.** Both hooks are O(1):
  - §1.2: one registry membership test (map lookup) + address compare per registration,
    before BLS (BLS verification dominates).
  - §1.3: one map lookup inside `checkSubmissionFeeRecipient`, which already does a
    `proposerDutiesMap` lookup + `strings.EqualFold` (`service.go:2093-2100`). Same cost
    class — microseconds.
- **Headroom vs 12s Ethereum:** the validation path budget is set by
  `getHeaderRequestCutoffMs` (default 3000, `service.go:113` — 3s into slot) and the
  getPayload retry timeout (100ms, `service.go:112`). At 10s slots these are 30% of the slot
  vs 25% at 12s — tighter, but the predicate adds nothing measurable to the p95 of
  decode→simulate→publish. **The 10s audit itself is a pre-pilot acceptance gate owned by
  the relay-deploy task (decision Q3).**

### 7.2 External-validator service

- **No recipient constraint:** external proposers are served with normal public-relay
  behavior — any registered fee recipient is accepted; bids are not filtered.
- **Revenue path:** the relay takes no cut of bids. The protocol-aligned upside of serving
  external proposers arrives with **Phase 4 (our builder)**: when the Vouch builder builds
  for external proposers, the builder margin routes to VFD. The §1.0 predicate is what keeps
  that builder-margin path protocol-aligned (the builder's payment target is the proposing
  validator's registered recipient; for Vouch proposers that is enforced to be VFD).
- **Ops cost of going public — deployment checklist:**
  - [ ] **Rate limiting** on the proposer API (`registerValidator`, `getHeader`,
    `getPayload`) — the relay is now a public target.
  - [ ] **DoS surface review**: payload size caps (upstream `apiMaxPayloadBytes`), request
    timeouts, connection limits; verify the public endpoints are behind the existing
    hardening.
  - [ ] **Status page / data API** public exposure (bid traces, `/relay/v1/data/...`) —
    decide what is public vs internal; the internal API must stay private.
  - [ ] **Missed-slot blame**: with external proposers, a relay outage or slow validation
    becomes a public-support issue — define the SLO, alerting, and a public status page
    before opening up.
  - [ ] **Monitoring**: rejections (Vouch non-VFD) and external traffic volume on the
    existing metrics; alert on any Vouch-registry rejection spike.

---

## 8. Test plan

Unit tests (no Postgres/Redis — follow `common/types_test.go` and the in-process
`miniredis`+`database.MockDB` pattern used by `services/api/service_test.go:56-104`):

1. **Predicate unit** — a pure helper `enforceFeeRecipient(pubkey, recipient, registry, vfd)`:
   - registry-hit + recipient == VFD → allow.
   - registry-hit + recipient != VFD → reject.
   - registry-miss (external) + any recipient → allow.
2. **Registration JSON path** — `processValidatorRegistrationJSON` with a registry-hit pubkey
   + non-VFD recipient → rejected (400 + metric); registry-hit + VFD → accepted; registry-miss
   + any recipient → accepted.
3. **Registration SSZ path** — same assertions via `processValidatorRegistrationsSSZ`.
4. **Submission gate** — `checkSubmissionFeeRecipient` with a slot duty whose proposer is a
   registry-hit with a non-VFD registered recipient → 400; registry-hit + VFD → passes;
   registry-miss + any recipient → passes. (Extends the existing
   `TestCheckSubmissionFeeRecipient` at `services/api/service_test.go:742`.)
5. **Cache-stale behavior** — registry set is empty / stale (last-known-good) → known Vouch
   pubkeys still rejected at submission (fail-closed for known), unknown pubkeys allowed
   (fail-open); RPC failure during refresh keeps the previous set and logs.
6. **Refresh-window edge case** — simulate a pubkey absent from the registry at registration
   (allowed, non-VFD accepted), then present at submission (rejected 400) — proving the
   submission gate is the lock.
7. **Disabled (all flags empty)** — everything accepts; upstream behavior unchanged. This is
   the regression guard: `TestRegisterValidator` (`service_test.go:544`) and
   `TestCheckSubmissionFeeRecipient` (`service_test.go:742`) use `testAddress` with the
   default empty config.
8. **Integration (needs Postgres/Redis — skipped in this environment):** full
   register → housekeeper duties → submit flow with enforcement on/off, plus the registry
   sync job populating Redis and the cold-start (empty set) path.

**Regression gate:** `go build ./...` + the non-DB unit suite (`common`, `beaconclient`,
`datastore`, `services/api`) must stay green with enforcement defaulting to disabled.
