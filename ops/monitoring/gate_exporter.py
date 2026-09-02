#!/usr/bin/env python3
"""MEV gate exporter (on-scrape model).

A tiny loop service: evaluates the pilot monitoring gates against the local
beacon API and the relay, then serves them as Prometheus metrics on loopback.

Gates (plan §7):
  mev_missed_proposals_total        missed proposer duties for watched validators
  mev_relay_delivered_recent        blocks delivered via the relay (recent window)
  mev_relay_gas_drift_recent        delivered blocks with undersized gas limit
  mev_relay_enforcement_violations_recent  delivered blocks whose fee recipient is not VFD
  mev_watched_validators            watched set size (0 = gate disabled)
  mev_eval_timestamp                last successful evaluation (unix)
  mev_eval_errors_total             evaluation errors since start
  mev_exporter_up                   liveness (1 when the loop is alive)

Configuration (environment variables, all optional):
  BEACON_API        consensus client API        (default http://validation-consensus:5052)
  RELAY_API         relay api (data endpoints)  (default http://relay-api:9062)
  VFD               expected fee recipient      (default 0x9325008eE3B5982c10010C8f12b6CD4943F48fA6)
  MEV_EXPORTER_PORT listen port                 (default 9700)
  MEV_EXPORTER_BIND bind host                   (default 127.0.0.1 loopback)
  WATCH_PUBKEYS     optional file of validator pubkeys (one per line) to watch
  GAS_LIMIT_MIN     minimum acceptable gas limit (default 44500000)
  EVAL_MIN_INTERVAL minimum seconds between evaluations (default 20)
  STATE_FILE        cursor persistence path     (default /data/mev_exporter_state.json)
  RELAY_WINDOW_EPOCHS  how many epochs back the relay "recent" window counts (default 3)

Read-only: no secrets, no keys, no external access beyond the configured endpoints.
Robust: the HTTP server never crashes; evaluations keep last-good metric values on
error (only mev_eval_errors_total and mev_exporter_up change); all HTTP timeouts <= 6s.
"""

import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib import request as urlreq
from urllib.error import HTTPError, URLError

BEACON_API = os.environ.get("BEACON_API", "http://validation-consensus:5052")
RELAY_API = os.environ.get("RELAY_API", "http://relay-api:9062")
VFD = os.environ.get("VFD", "0x9325008eE3B5982c10010C8f12b6CD4943F48fA6").lower()
PORT = int(os.environ.get("MEV_EXPORTER_PORT", "9700"))
BIND_HOST = os.environ.get("MEV_EXPORTER_BIND", "127.0.0.1")
WATCH_FILE = os.environ.get("WATCH_PUBKEYS", "")
GAS_LIMIT_MIN = int(os.environ.get("GAS_LIMIT_MIN", "44500000"))
EVAL_MIN_INTERVAL = max(int(os.environ.get("EVAL_MIN_INTERVAL", "20")), 15)
STATE_FILE = os.environ.get("STATE_FILE", "/data/mev_exporter_state.json")
RELAY_WINDOW_EPOCHS = max(int(os.environ.get("RELAY_WINDOW_EPOCHS", "3")), 1)

SLOTS_PER_EPOCH = 32
HTTP_TIMEOUT = 6  # seconds, hard cap
EVAL_LOCK = threading.Lock()
DUTIES_CACHE = {}  # epoch -> [(slot, pubkey)]
METRICS = {
    "mev_missed_proposals_total": 0,
    "mev_relay_delivered_recent": 0,
    "mev_relay_gas_drift_recent": 0,
    "mev_relay_enforcement_violations_recent": 0,
    "mev_watched_validators": 0,
    "mev_eval_timestamp": 0,
    "mev_eval_errors_total": 0,
    "mev_exporter_up": 1,
}


def _get_json(url):
    req = urlreq.Request(url, headers={"Accept": "application/json"})
    with urlreq.urlopen(req, timeout=HTTP_TIMEOUT) as resp:
        return json.loads(resp.read().decode())


def _probe(url):
    """Returns (status, data-dict-or-None) without raising on transport errors."""
    req = urlreq.Request(url, headers={"Accept": "application/json"})
    try:
        with urlreq.urlopen(req, timeout=HTTP_TIMEOUT) as resp:
            try:
                return resp.status, json.loads(resp.read().decode())
            except Exception:
                return resp.status, None
    except HTTPError as e:
        try:
            return e.code, None
        except Exception:
            return 0, None
    except (URLError, OSError, ValueError):
        return 0, None


def _load_cursor():
    try:
        with open(STATE_FILE) as f:
            return int(json.load(f).get("last_checked_slot", 0))
    except Exception:
        return 0


def _save_cursor(slot):
    try:
        os.makedirs(os.path.dirname(STATE_FILE) or ".", exist_ok=True)
        with open(STATE_FILE, "w") as f:
            json.dump({"last_checked_slot": int(slot)}, f)
    except Exception:
        pass


def _watched():
    """Watched pubkey set (lowercased). Empty set means the gate is disabled."""
    if not WATCH_FILE:
        return set()
    try:
        with open(WATCH_FILE) as f:
            return {line.strip().lower() for line in f if line.strip()}
    except Exception:
        return set()


def _epoch_duties(epoch):
    if epoch in DUTIES_CACHE:
        return DUTIES_CACHE[epoch]
    url = "%s/eth/v1/validator/duties/proposer/%d" % (BEACON_API, epoch)
    data = _get_json(url)
    duties = [(int(d["slot"]), d["pubkey"].lower()) for d in data.get("data", [])]
    DUTIES_CACHE[epoch] = duties
    if len(DUTIES_CACHE) > 8:
        for old in sorted(DUTIES_CACHE)[:-8]:
            del DUTIES_CACHE[old]
    return duties


def _check_missed_proposals(head_slot):
    """Incrementally walks watched proposer duties and counts missed blocks.

    Returns the highest slot fully processed (the next cursor), or the last
    fully processed slot if the beacon became unreachable mid-window.
    """
    cursor = _load_cursor()
    watched = _watched()
    if not watched:
        return cursor  # gate disabled: no sweep, no count
    first = max(cursor + 1, head_slot - (RELAY_WINDOW_EPOCHS * SLOTS_PER_EPOCH))
    last_done = cursor
    for slot in range(first, head_slot + 1):
        if slot <= cursor:
            continue
        epoch = slot // SLOTS_PER_EPOCH
        try:
            duties = _epoch_duties(epoch)
        except Exception:
            break  # beacon unreachable: resume from this slot next cycle
        proposer = next((pk for d_slot, pk in duties if d_slot == slot), None)
        if proposer is None or (watched and proposer not in watched):
            last_done = slot
            continue
        status, data = _probe("%s/eth/v1/beacon/blocks/%d" % (BEACON_API, slot))
        if status == 0:
            break  # beacon unreachable: resume from this slot next cycle
        # A slot is "missed" when there is no canonical block for it: the
        # beacon blocks endpoint returns 404 for finalized misses, or 200 with
        # data == null for a recent skipped slot.
        block_present = status == 200 and data is not None and data.get("data")
        if not block_present:
            METRICS["mev_missed_proposals_total"] += 1
        last_done = slot
    return last_done


def _check_relay(head_slot):
    """Evaluates the relay delivered-traces gates over a recent slot window."""
    window_start = head_slot - (RELAY_WINDOW_EPOCHS * SLOTS_PER_EPOCH)
    url = "%s/relay/v1/data/bidtraces/proposer_payload_delivered?limit=200" % RELAY_API
    data = _get_json(url)
    if not isinstance(data, list):
        raise ValueError("relay delivered-traces returned a non-list payload")
    delivered = gas_drift = violations = 0
    for rec in data:
        try:
            slot = int(rec.get("slot", 0))
        except (TypeError, ValueError):
            continue
        if slot < window_start:
            continue
        delivered += 1
        try:
            gas_limit = int(rec.get("gas_limit", 0))
        except (TypeError, ValueError):
            gas_limit = 0
        if 0 < gas_limit < GAS_LIMIT_MIN:
            gas_drift += 1
        recipient = str(rec.get("proposer_fee_recipient", "")).lower()
        if recipient and recipient != VFD:
            violations += 1
    METRICS["mev_relay_delivered_recent"] = delivered
    METRICS["mev_relay_gas_drift_recent"] = gas_drift
    METRICS["mev_relay_enforcement_violations_recent"] = violations


def _run_evaluation():
    head = _get_json("%s/eth/v1/beacon/headers/head" % BEACON_API)
    head_slot = int(head["data"]["header"]["message"]["slot"])

    cursor = _check_missed_proposals(head_slot)
    _save_cursor(cursor)

    watched = _watched()
    METRICS["mev_watched_validators"] = len(watched)

    # Relay gates are best-effort: if the relay/API is down, _check_relay
    # raises and the eval-loop increments mev_eval_errors_total while the relay
    # metrics keep their last-good values.
    _check_relay(head_slot)

    METRICS["mev_eval_timestamp"] = int(time.time())
    METRICS["mev_exporter_up"] = 1


def _eval_loop():
    while True:
        try:
            _run_evaluation()
        except Exception:
            METRICS["mev_eval_errors_total"] += 1
            METRICS["mev_exporter_up"] = 1  # still alive; endpoints may be down
        time.sleep(max(EVAL_MIN_INTERVAL, 15))


class MetricsHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/metrics":
            self.send_response(404)
            self.end_headers()
            return
        with EVAL_LOCK:
            body = _render()
        payload = body.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


def _render():
    help_map = {
        "mev_missed_proposals_total": ("counter", "Missed proposer duties for watched validators"),
        "mev_relay_delivered_recent": ("gauge", "Blocks delivered via the relay (recent window)"),
        "mev_relay_gas_drift_recent": ("gauge", "Delivered blocks with undersized gas limit"),
        "mev_relay_enforcement_violations_recent": ("gauge", "Delivered blocks whose fee recipient is not VFD"),
        "mev_watched_validators": ("gauge", "Watched validator count (0 = gate disabled)"),
        "mev_eval_timestamp": ("gauge", "Last successful evaluation (unix seconds)"),
        "mev_eval_errors_total": ("counter", "Evaluation errors since start"),
        "mev_exporter_up": ("gauge", "1 when the exporter loop is alive"),
    }
    lines = []
    for key in sorted(METRICS):
        lines.append("# HELP %s %s" % (key, help_map.get(key, ("gauge", ""))[1]))
        lines.append("# TYPE %s %s" % (key, help_map.get(key, ("gauge", ""))[0]))
        lines.append("%s %s" % (key, METRICS[key]))
    return "\n".join(lines) + "\n"


def main():
    server = ThreadingHTTPServer((BIND_HOST, PORT), MetricsHandler)
    server.serve_forever()


if __name__ == "__main__":
    thread = threading.Thread(target=_eval_loop, daemon=True)
    thread.start()
    main()
