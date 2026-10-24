import datetime as dt
import json
import tempfile
import unittest
from pathlib import Path

from capacity_planning import analyze, scenario


class CapacityPlanningTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.data = Path(self.temp.name)
        self.now = dt.datetime.fromisoformat("2026-10-24T10:00:00+08:00")

    def tearDown(self):
        self.temp.cleanup()

    def write(self, name, value):
        (self.data / name).write_text(json.dumps(value), encoding="utf-8")

    def test_forecast_uses_recent_burn_and_conservative_unit(self):
        buckets = []
        # 24 hourly buckets: 40 credits each = 960; previous day: 20 each = 480.
        for offset in range(48):
            stamp = self.now - dt.timedelta(hours=offset)
            buckets.append({"s": "h:" + stamp.strftime("%Y-%m-%dT%H"),
                            "q": 10, "e": 0, "t": 100,
                            "cr": 40 if offset < 24 else 20, "cn": 1})
        self.write("usage.json", {"saved": self.now.isoformat(), "buckets": buckets})
        self.write("state.json", {"accounts": {
            "a": {"credits": 100, "credits_total": 2500},
            "b": {"credits": 50, "credits_total": 5000},
            "c": {"credits": 0, "credits_total": 8000},
        }})
        self.write("credit-history.json", {"entries": [
            {"time": self.now.isoformat(), "delta": 200},
            {"time": self.now.isoformat(), "delta": -500},
        ]})
        report = analyze(self.data, forecast_days=7, now=self.now)
        self.assertEqual(report["usage"]["last_24h"]["credits"], 960)
        self.assertEqual(report["usage"]["previous_24h"]["credits"], 480)
        self.assertEqual(report["usage"]["daily_credits_forecast"], 960)
        self.assertEqual(report["usage"]["positive_replenishment_assumed_per_day"], 100)
        self.assertEqual(report["pool"]["new_account_credit_unit_p25"], 3750)
        self.assertEqual(report["coverage"]["credit_history_entries"], 2)
        self.assertIsNotNone(report["forecast"]["horizons"]["7"]["planned_50pct_replenishment"]["additional_accounts"])

    def test_expiry_reduces_usable_balance_and_scenarios_bound_plan(self):
        base = scenario(7, 1000, 150, 100, 2500, 0)
        expiring = scenario(7, 1000, 150, 100, 2500, 150)
        self.assertEqual(base["credits_usable_after_expiry"], 150)
        self.assertEqual(expiring["credits_usable_after_expiry"], 0)
        self.assertGreaterEqual(expiring["additional_accounts"], base["additional_accounts"])
        planned = scenario(7, 1000, 150, 100, 2500, 0)
        stable = scenario(7, 1000, 150, 200, 2500, 0)
        none = scenario(7, 1000, 150, 0, 2500, 0)
        self.assertLessEqual(stable["additional_accounts"], planned["additional_accounts"])
        self.assertLessEqual(planned["additional_accounts"], none["additional_accounts"])

    def test_empty_data_is_safe(self):
        report = analyze(self.data, now=self.now)
        self.assertEqual(report["traffic"]["requests"], 0)
        self.assertEqual(report["pool"]["accounts_total"], 0)
        self.assertIsNone(report["forecast"]["horizons"]["7"]["planned_50pct_replenishment"]["additional_accounts"])
        self.assertTrue(any(row["category"] == "data" for row in report["recommendations"]))

    def test_hidden_model_rejections_are_not_reported_as_outages(self):
        self.write("usage.json", {"saved": self.now.isoformat(), "buckets": []})
        self.write("state.json", {"accounts": [{"credits": 100, "credits_total": 2500}]})
        config = self.data / "config.json"
        self.write("config.json", {"pool": {"max_in_flight": 5},
                                   "models": {"hidden": ["kimi-k3-1"]}})
        (self.data / "request-logs").mkdir()
        event = {"time": self.now.isoformat(), "model": "cn:kimi-k3-1",
                 "status": 400, "ok": False, "outcome": "http_error"}
        (self.data / "request-logs" / "requests-2026-10-24.jsonl").write_text(
            json.dumps(event) + "\n", encoding="utf-8")
        report = analyze(self.data, now=self.now, config_path=config)
        self.assertEqual(report["pool"]["max_in_flight_per_account"], 5)
        hidden = [r for r in report["recommendations"] if "已按配置隐藏" in r["title"]]
        self.assertEqual(len(hidden), 1)
        self.assertEqual(hidden[0]["priority"], "P2")
        self.assertFalse(any(r["priority"] == "P1" and r["category"] == "model"
                             for r in report["recommendations"]))

    def test_disabled_and_cooling_accounts_do_not_count_available(self):
        self.write("usage.json", {"buckets": []})
        self.write("state.json", {"accounts": [
            {"credits": 100, "credits_total": 2500},
            {"credits": 100, "credits_total": 2500, "disabled": True},
            {"credits": 100, "credits_total": 2500, "until": "2026-10-25T04:00:00+08:00"},
            {"credits": 0, "credits_total": 2500},
        ]})
        report = analyze(self.data, now=self.now, max_in_flight=8)
        self.assertEqual(report["pool"]["accounts_credit_positive"], 2)
        self.assertEqual(report["pool"]["accounts_available_estimate"], 1)
        self.assertEqual(report["pool"]["in_flight_pool_limit_estimate"], 8)


if __name__ == "__main__":
    unittest.main()
