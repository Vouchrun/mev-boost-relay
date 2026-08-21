# VFD Fee-Recipient Whitelist — Design

**Status:** Design only (no implementation). Branch `vfd-whitelist-design`, off `main`.
**Target:** Vouchrun/mev-boost-relay fork (upstream flashbots/mev-boost-relay base).
**Goal:** Protocol-level enforcement that all MEV income from relay-served blocks flows to the
Vouch protocol by refusing service to any validator/block whose fee recipient is not the
ValidatorFeeDepositor (VFD).

**VFD address (verified in `catalog/contracts.yaml`):**
`0x9325008eE3B5982c10010C8f12b6CD4943F48fA6`

> **Correction to the task brief:** the brief said to follow "upstream's CLI-flag pattern
> (urfave/cli)". `mev-boost-relay` uses **spf13/cobra**, not urfave/cli (urfave/cli is the
> mev-boost sidecar's framework; see `go.mod:27` `github.com/spf13/cobra v1.9.1`). The config
> pattern below follows the relay's actual cobra + env-var conventions.

---

## 1. Enforcement points in upstream code

All paths below are grounded in `services/api/service.go` (the API service) and
`services/housekeeper/housekeeper.go` (the background duty/registration pipeline).

### 1.1 How the registered fee recipient flows (context)

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
- **Proposed hook:** at the top of each loop, reject/drop any registration whose
  `reg.FeeRecipient` (JSON) / `signedValidatorRegistration.Message.FeeRecipient` (SSZ) is
  not in the whitelist. Placed **before** the cache lookup and BLS verification it is an
  O(1) address compare that also short-circuits signature work on spam.
- **Reject vs drop (design decision, see §3):** return a 400 with a clear message
  (`"fee recipient not whitelisted"`) and a dedicated metric, so operators can see rejected
  registrations; do **not** silently accept-and-ignore.

### 1.3 (b) Block submission — `POST /relay/v1/builder/blocks` (v1/v2/v3)

- Entry: `handleSubmitNewBlock` (`service.go:2343`). Validation sequence:
  `checkSubmissionSlotDetails` (`service.go:2514`) → `checkBuilderEntry` (`service.go:2520`)
  → **`checkSubmissionFeeRecipient`** (`service.go:2527`) → payload-attributes check
  (`service.go:2532+`) → simulation → `updateRedisBid` (`service.go:2284`).
- **Existing upstream check — this is the key anchor:**
  `checkSubmissionFeeRecipient` (`service.go:2092-2109`) already rejects any submission whose
  `bidTrace.ProposerFeeRecipient` does not match the **registered** recipient for that slot's
  proposer (`slotDuty.Entry.Message.FeeRecipient`), compared case-insensitively with
  `strings.EqualFold` (`service.go:2100`). On mismatch it returns 400
  `"fee recipient does not match"` (`service.go:2105`).
- **Proposed hook:** extend `checkSubmissionFeeRecipient` to also require
  `slotDuty.Entry.Message.FeeRecipient ∈ whitelist`. This is **defense-in-depth**: it covers
  registrations that were accepted before the whitelist was enabled (already sitting in the
  DB/Redis) and any registration that bypassed the §1.2 filter.

### 1.4 (c) getHeader / getPayload — `GET /eth/v1/builder/header/...`, `POST /eth/v1/builder/blinded_blocks`

- `handleGetHeader` (`service.go:1283`) serves the best bid for the slot via
  `api.redis.GetBestBid(slot, parentHash, proposerPubkey)` (`service.go:1397`).
- `innerHandleGetPayload` (`service.go:1516`) returns the stored payload for the signed
  block hash.
- **Transitively safe:** bids only enter Redis via `updateRedisBid` after passing
  `checkSubmissionFeeRecipient` (§1.3), and getPayload only serves payloads whose bid passed
  getHeader. So if registration (§1.2) and submission (§1.3) are whitelist-enforced, the
  served bid's recipient is VFD by construction.
- **Optional defense-in-depth (design option, not required):** in `handleGetHeader`, decode
  the winning `VersionedSignedBuilderBid`'s execution-payload-header `FeeRecipient` and
  require it ∈ whitelist before serving. Cost: an extra decode on the hot path; benefit:
  guards against a stale/non-whitelisted bid already resident in Redis. **Recommendation:
  skip in Phase 0** — the submission gate already guarantees it; revisit only if a
  pre-whitelist bid is observed in the wild.

### 1.5 (d) Does upstream verify feeRecipient matches the registered recipient?

**Yes.** `checkSubmissionFeeRecipient` (`service.go:2092-2109`) is exactly that check, at
block-submission time, comparing the bid's `ProposerFeeRecipient` against the slot duty's
registered `Entry.Message.FeeRecipient` (case-insensitive). The whitelist is therefore not a
new verification mechanism — it is a **constraint on the registered recipient itself**,
layered on top of the existing comparison. This is the elegant property of the design: with
(a) + (b) in place, the existing (d) check makes the whole path VFD-only.

---

## 2. Config surface

Pattern source (relay's actual conventions):
- Cobra flag + env default: `-network` / `NETWORK`
  (`cmd/variables.go:10`, `cmd/api.go:66`).
- Comma-separated slice flag: `-known-validators` (`cmd/api.go:75-76`,
  `apiCmd.Flags().StringSliceVar(&apiKnownValidators, "known-validators", nil, ...)`).
- Feature toggles read raw env in `NewRelayAPI` (`service.go:347-390`, e.g.
  `REGISTER_VALIDATOR_CONTINUE_ON_INVALID_SIG`).

**Proposed (Phase 0, static):**

| Item | Proposal |
|---|---|
| Flag | `-fee-recipient-whitelist` (repeated / comma-separated hex addresses) |
| Env | `FEE_RECIPIENT_WHITELIST` (comma-separated) |
| Default | empty → **whitelist disabled, upstream behavior preserved** |
| Wiring | `cmd/api.go`: `apiCmd.Flags().StringSliceVar(&apiFeeRecipientWhitelist, "fee-recipient-whitelist", defaultFeeRecipientWhitelist, ...)`; pass into `RelayAPIOpts` (`service.go:152-180`) as `FeeRecipientWhitelist []string` |
| Parsing | at `NewRelayAPI` (`service.go:278`): `utils.HexToAddress` each entry → `map[[20]byte]struct{}` (or `[]bellatrix.ExecutionAddress`); invalid hex → fatal at startup (fail loud, not silent) |
| Storage | `RelayAPI` struct field next to the feature flags (`service.go:253-262`), e.g. `feeRecipientWhitelist map[bellatrix.ExecutionAddress]struct{}` |

Housekeeper needs **no** config: it only reads registrations from the DB, and non-VFD
registrations never enter the DB once §1.2 is enforced. (If §1.2 is later relaxed, the
housekeeper is still agnostic — enforcement stays in the API service.)

---

## 3. Failure modes & edge cases

1. **Validator re-registers a different (non-VFD) fee recipient** — §1.2 rejects the new
   registration (400 + metric). The previous VFD registration stays in DB/Redis and remains
   the slot duty's recipient, so the validator keeps receiving MEV to VFD. This is the
   anti-redirection property the whitelist exists for.
2. **Empty whitelist = disabled** — default. No enforcement anywhere; all existing upstream
   behavior and tests are preserved. Deployment is therefore a non-breaking opt-in.
3. **Case-insensitive hex compare** — addresses are parsed once at startup to 20 raw bytes
   (`utils.HexToAddress`); membership is a byte-array compare, inherently case-insensitive.
   (The existing check at `service.go:2100` already uses `strings.EqualFold` for the
   bid-vs-registration compare.)
4. **Register spam** — the whitelist check runs before cache lookup and BLS verification
   (cheapest possible position). Rejection is a 400 with a distinct message
   (`"fee recipient not whitelisted"`) plus a Prometheus counter (e.g.
   `validator_registration_whitelist_rejected_total`). Upstream has no registration rate
   limiter; the whitelist does not introduce one (out of scope).
5. **Zero fee recipient (`0x0000...0`)** — not whitelisted → rejected. Correct.
6. **External (non-Vouch) validators** — rejected by default. The comma-separated list lets
   the operator whitelist additional recipients (e.g. a future B2B relay for other
   validators) without a code change.
7. **Relay restarted with whitelist enabled after non-VFD registrations already exist** —
   §1.3 (submission gate) rejects their blocks with 400, so they cannot get MEV service;
   the rejection is visible in logs/metrics and is the trigger for operator follow-up. This
   is why §1.3 is required, not optional.
8. **SSZ vs JSON registration paths** — both `processValidatorRegistrationJSON` and
   `processValidatorRegistrationsSSZ` must enforce the whitelist identically (they share the
   same semantic; a gap in one is a bypass).

---

## 4. 10s slot timing implications (PulseChain, `SEC_PER_SLOT=10`)

- Slot timing is `common.SecondsPerSlot` (`common/common.go:15`), default 12, set to 10 for
  PulseChain via the `SEC_PER_SLOT` env var. The relay computes slot boundaries from live
  genesis time (`service.go:1306`, `service.go:2476`).
- **Whitelist latency cost: negligible.** Both hooks are O(1):
  - §1.2: one map lookup per registration, before BLS (BLS verification dominates).
  - §1.3: one map lookup inside `checkSubmissionFeeRecipient`, which already does a
    `proposerDutiesMap` lookup + `strings.EqualFold` (`service.go:2093-2100`). The whitelist
    membership test is the same cost class — microseconds.
- **Headroom vs 12s Ethereum:** the validation path budget is set by
  `getHeaderRequestCutoffMs` (default 3000, `service.go:113` — 3s into slot) and the
  getPayload retry timeout (100ms, `service.go:112`). At 10s slots these are 30% of the slot
  vs 25% at 12s — tighter but the whitelist adds nothing measurable to the p95 of
  decode→simulate→publish.
- **Open question / follow-up (per plan §7.2):** the 10s concern is the *existing*
  simulation+publish path, not the whitelist. Before the PulseChain pilot, measure p95
  submission→publish latency against the 10s budget and verify the 3s getHeader cutoff and
  getPayload timeouts are safe at 10s (this is a relay-wide timing audit, independent of the
  whitelist).

---

## 5. Test plan

Unit tests (no Postgres/Redis — follow `common/types_test.go` and the in-process
`miniredis`+`database.MockDB` pattern used by `services/api/service_test.go:56-104`):

1. **Whitelist parsing** — comma-separated input; mixed-case hex normalizes to the same
  membership; invalid hex → startup error (fail loud).
2. **Registration JSON path** — non-VFD `FeeRecipient` → rejected (400 / filtered out +
  metric); VFD → accepted. (`processValidatorRegistrationJSON`)
3. **Registration SSZ path** — same assertions via `processValidatorRegistrationsSSZ`.
4. **Submission gate** — `checkSubmissionFeeRecipient` with a slot duty whose registered
  recipient is non-whitelisted → 400; whitelisted → passes. (Extends the existing
  `TestCheckSubmissionFeeRecipient` at `services/api/service_test.go:742`.)
5. **Disabled (empty whitelist)** — all of the above accept everything; upstream behavior
  unchanged. This is the regression guard that existing tests keep passing:
   `TestRegisterValidator` (`service_test.go:544`), `TestCheckSubmissionFeeRecipient`
   (`service_test.go:742`) use `testAddress` as the recipient with the default empty
   whitelist.
6. **Integration (needs Postgres/Redis — skipped in this environment):** full
   register → housekeeper duties → submit flow with whitelist on/off, verifying a
   non-VFD-registered proposer's block is rejected at submission and never served by
   getHeader.

**Regression gate:** `go build ./...` + the non-DB unit suite (`common`, `beaconclient`,
`datastore`, `services/api`) must stay green with the whitelist defaulting to empty.

---

## 6. Phasing

**Phase 0 — static whitelist (this design, config-driven):**
- `-fee-recipient-whitelist` / `FEE_RECIPIENT_WHITELIST` with VFD as the sole entry.
- Enforce at §1.2 (registration) + §1.3 (submission). Empty = disabled.
- Metrics + clear 400 messages; operator alerting on rejections.

**Future — on-chain lookup (sketch only, NOT in Phase 0):**
- The whitelist is **recipient-based** ("only these recipients may receive MEV"). The
  on-chain registry (`NodeDeposit.getPubkeysOfNode`) is **pubkey-based** ("these pubkeys are
  Vouch validators"). They are complementary, not interchangeable.
- Sketch: a periodic reconciler that (1) reads Vouch pubkeys from `NodeDeposit`, (2) reads
  their latest registered fee recipients from the relay DB/Redis, (3) alerts (and optionally
  rejects at submission) on any pubkey whose registered recipient ≠ VFD — catching drift
  before a slot is lost. This does not replace the static whitelist; it adds detection of
  Vouch-internal misconfiguration.
- Alternative sketch: derive the allowed-recipient set dynamically from the registry
  (pubkey → registered recipient) instead of a static list — deferred; requires a trusted
  RPC and cache invalidation design.

---

## Open questions

- **Reject vs silently-drop at registration (§1.2):** this design rejects with 400 + metric
  (visible to operators). Confirm the coordinator prefers 400 (validator clients log it) over
  silent filtering.
- **getHeader defense-in-depth (§1.4):** recommended to skip in Phase 0 (transitively safe).
  Confirm.
- **10s timing audit (§4):** the whitelist is timing-neutral, but the relay's existing
  simulation/publish path must be measured against the 10s budget before the pilot (plan
  §7.2) — out of scope for the whitelist patch itself.
- **Fee-recipient change detection** (`service.go:3221`, `service.go:3283`): confirm the
  desired behavior when a validator flips a previously-VFD registration to non-VFD — this
  design rejects it and keeps the old VFD registration; verify the cached-registration
  semantics don't need a tombstone.
