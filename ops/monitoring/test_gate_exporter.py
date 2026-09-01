#!/usr/bin/env python3
"""Unit tests for the MEV gate exporter's core logic.

Stdlib-only (unittest). Mocks the beacon/relay HTTP endpoints; does not need a
live node. Run with:  python -m unittest ops.monitoring.test_gate_exporter
or:                 python -m pytest ops/monitoring/test_gate_exporter.py
"""

import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import gate_exporter as ge  # noqa: E402

WATCHED_PUBKEY = "0x" + "ab" * 48
OTHER_PUBKEY = "0x" + "cd" * 48
DEFAULT_METRICS = dict(ge.METRICS)


class GateExporterTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        ge.STATE_FILE = os.path.join(self.tmp.name, "state.json")
        ge.WATCH_FILE = os.path.join(self.tmp.name, "watch.txt")
        ge.RELAY_WINDOW_EPOCHS = 3
        ge.METRICS.clear()
        ge.METRICS.update(DEFAULT_METRICS)
        ge.DUTIES_CACHE.clear()
        ge._relay_payload = []  # default: no relay deliveries
        # default mocks: every slot is watched and has a block
        ge._get_json = self._mock_get_json
        ge._probe = self._mock_probe

    def tearDown(self):
        self.tmp.cleanup()

    def _mock_get_json(self, url):
        if "/eth/v1/beacon/headers/head" in url:
            return {"data": {"header": {"message": {"slot": 100}}}}
        if "/eth/v1/validator/duties/proposer/" in url:
            epoch = int(url.rsplit("/", 1)[-1])
            slots = range(epoch * 32, epoch * 32 + 32)
            return {"data": [{"slot": s, "pubkey": WATCHED_PUBKEY} for s in slots]}
        if "/relay/v1/data/bidtraces/" in url:
            return ge._relay_payload
        raise AssertionError("unexpected URL: %s" % url)

    def _mock_probe(self, url):
        # every block endpoint returns 200 with data (block present)
        return 200, {"data": {"execution_optimistic": False}}

    # -- helpers ----------------------------------------------------------

    def _write_watch(self, pubkeys):
        with open(ge.WATCH_FILE, "w") as f:
            f.write("\n".join(pubkeys) + "\n")

    # -- render -----------------------------------------------------------

    def test_render_includes_all_metrics(self):
        body = ge._render()
        for key in DEFAULT_METRICS:
            self.assertIn("# TYPE %s " % key, body)
            self.assertIn("%s " % key, body)
        self.assertIn("# HELP mev_missed_proposals_total", body)
        self.assertIn("# TYPE mev_missed_proposals_total counter", body)
        self.assertTrue(body.endswith("\n"))

    # -- cursor persistence ------------------------------------------------

    def test_cursor_roundtrip(self):
        self.assertEqual(ge._load_cursor(), 0)  # missing file -> 0
        ge._save_cursor(12345)
        self.assertEqual(ge._load_cursor(), 12345)

    def test_save_cursor_creates_dir(self):
        ge.STATE_FILE = os.path.join(self.tmp.name, "sub", "dir", "state.json")
        ge._save_cursor(7)
        self.assertEqual(ge._load_cursor(), 7)

    # -- watched set -------------------------------------------------------

    def test_watched_parsing(self):
        self.assertEqual(ge._watched(), set())  # no file -> disabled
        self._write_watch([WATCHED_PUBKEY.upper(), OTHER_PUBKEY, ""])
        self.assertEqual(ge._watched(), {WATCHED_PUBKEY, OTHER_PUBKEY})

    def test_watched_missing_file(self):
        self.assertEqual(ge._watched(), set())

    # -- missed-proposals gate ----------------------------------------------

    def test_missed_proposals_disabled_without_watch_file(self):
        """No WATCH_PUBKEYS file -> gate disabled: no sweep, no count, cursor untouched."""
        ge._save_cursor(90)
        ge._run_evaluation()
        self.assertEqual(ge.METRICS["mev_missed_proposals_total"], 0)
        self.assertEqual(ge.METRICS["mev_watched_validators"], 0)
        self.assertEqual(ge._load_cursor(), 90)

    def test_missed_proposals_cursor_math(self):
        self._write_watch([WATCHED_PUBKEY])
        ge._save_cursor(90)
        ge._run_evaluation()
        # slots 91..100 all watched and all blocks present -> 0 missed,
        # cursor advanced to head (100)
        self.assertEqual(ge.METRICS["mev_missed_proposals_total"], 0)
        self.assertEqual(ge._load_cursor(), 100)

    def test_missed_proposals_detects_miss(self):
        self._write_watch([WATCHED_PUBKEY])

        def probe(url):
            if url.endswith("/eth/v1/beacon/blocks/95"):
                return 404, None  # missed slot
            return 200, {"data": {"execution_optimistic": False}}

        ge._probe = probe
        ge._save_cursor(90)
        ge._run_evaluation()
        self.assertEqual(ge.METRICS["mev_missed_proposals_total"], 1)
        self.assertEqual(ge._load_cursor(), 100)

    def test_missed_proposals_skips_unwatched(self):
        self._write_watch([WATCHED_PUBKEY])

        def get_json(url):
            if "/relay/v1/data/bidtraces/" in url:
                return self._mock_get_json(url)
            if "/eth/v1/beacon/headers/head" in url:
                return {"data": {"header": {"message": {"slot": 100}}}}
            if "/eth/v1/validator/duties/proposer/" in url:
                epoch = int(url.rsplit("/", 1)[-1])
                slots = range(epoch * 32, epoch * 32 + 32)
                # every other slot is a non-watched proposer
                return {"data": [{"slot": s, "pubkey": OTHER_PUBKEY if s % 2 else WATCHED_PUBKEY} for s in slots]}
            raise AssertionError(url)

        def probe(url):
            return 404, None  # all blocks "missed"

        ge._get_json = get_json
        ge._probe = probe
        ge._save_cursor(90)
        ge._run_evaluation()
        # only the watched slots (even ones: 92,94,96,98,100) count as missed
        self.assertEqual(ge.METRICS["mev_missed_proposals_total"], 5)

    def test_missed_proposals_keeps_cursor_on_beacon_error(self):
        """A beacon outage mid-window must not skip the unchecked range."""
        self._write_watch([WATCHED_PUBKEY])
        calls = {"n": 0}

        def probe(url):
            calls["n"] += 1
            if calls["n"] >= 3:
                return 0, None  # beacon unreachable
            return 200, {"data": {"execution_optimistic": False}}

        ge._probe = probe
        ge._save_cursor(90)
        ge._run_evaluation()
        # loop broke at the third checked slot; cursor must stay at the last
        # successfully processed slot (92), NOT jump to head (100)
        self.assertEqual(ge._load_cursor(), 92)

    # -- relay gates ---------------------------------------------------------

    def test_relay_gates_counts(self):
        base = head_slot = 100
        window = ge.RELAY_WINDOW_EPOCHS * ge.SLOTS_PER_EPOCH
        recent = base - window + 10
        ge._relay_payload = [
            {"slot": recent + 0, "gas_limit": 45000000, "proposer_fee_recipient": ge.VFD},
            {"slot": recent + 1, "gas_limit": 30000000, "proposer_fee_recipient": ge.VFD},   # gas drift
            {"slot": recent + 2, "gas_limit": 45000000, "proposer_fee_recipient": "0x" + "ff" * 20},  # violation
            {"slot": 2, "gas_limit": 45000000, "proposer_fee_recipient": ge.VFD},  # outside window
        ]
        ge._run_evaluation()
        self.assertEqual(ge.METRICS["mev_relay_delivered_recent"], 3)
        self.assertEqual(ge.METRICS["mev_relay_gas_drift_recent"], 1)
        self.assertEqual(ge.METRICS["mev_relay_enforcement_violations_recent"], 1)

    def test_relay_gates_keep_last_good_on_error(self):
        ge._relay_payload = [
            {"slot": 95, "gas_limit": 45000000, "proposer_fee_recipient": ge.VFD},
        ]
        ge._run_evaluation()
        good = dict(ge.METRICS)

        def get_json(url):
            if "/relay/v1/data/bidtraces/" in url:
                raise OSError("relay down")
            return self._mock_get_json(url)

        ge._get_json = get_json
        with self.assertRaises(OSError):
            ge._run_evaluation()  # the eval loop would catch this
        # relay values unchanged (last-good), errors incremented, liveness kept
        self.assertEqual(ge.METRICS["mev_relay_delivered_recent"], good["mev_relay_delivered_recent"])
        self.assertEqual(ge.METRICS["mev_relay_gas_drift_recent"], good["mev_relay_gas_drift_recent"])
        self.assertEqual(ge.METRICS["mev_exporter_up"], 1)


if __name__ == "__main__":
    unittest.main()
