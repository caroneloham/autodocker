import importlib.util
from datetime import datetime, timedelta, timezone
import json
from pathlib import Path
import tempfile
import subprocess
import socket
import sys
import time
import types
import unittest
import urllib.error
import urllib.request
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("supervision", Path(__file__).with_name("supervision.py"))
monitor = importlib.util.module_from_spec(spec)
spec.loader.exec_module(monitor)


class SupervisionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.now = datetime(2026, 10, 19, 12, tzinfo=timezone.utc)
        self.receipt = Path(self.temp.name) / "backup.json"
        self.cfg = {"gpu": True, "disk_path": self.temp.name, "disk_warning": 85, "min_free_gib": 15, "temperature_warning": 85, "backup_status_file": str(self.receipt), "backup_max_hours": 30}

    def receipt_state(self, **overrides):
        receipt = {"status": "ok", "verified": True, "completed_at": (self.now - timedelta(hours=2)).isoformat(), **overrides}
        self.receipt.write_text(json.dumps(receipt), encoding="utf-8")
        return monitor.backup_check(self.cfg, self.now)["state"]

    def test_backup_healthy_only_after_external_verified_success(self):
        self.assertEqual(self.receipt_state(), "ok")
        self.assertEqual(self.receipt_state(verified=False), "warning")
        self.assertEqual(self.receipt_state(status="error"), "warning")

    def test_backup_stale_future_naive_and_invalid_are_not_healthy(self):
        for date, expected in [((self.now - timedelta(hours=31)).isoformat(), "warning"), ((self.now + timedelta(hours=1)).isoformat(), "unknown"), ("2026-10-19T12:00:00", "unknown"), (None, "unknown")]:
            with self.subTest(date=date):
                self.assertEqual(self.receipt_state(completed_at=date), expected)

    def test_missing_unconfigured_malformed_and_oversized_receipts(self):
        self.assertEqual(monitor.backup_check(self.cfg, self.now)["state"], "unknown")
        self.cfg["backup_status_file"] = ""
        self.assertEqual(monitor.backup_check(self.cfg, self.now)["state"], "not_configured")
        self.cfg["backup_status_file"] = str(self.receipt)
        for value in ("[1]", "{", "x" * 65537):
            self.receipt.write_text(value)
            self.assertEqual(monitor.backup_check(self.cfg, self.now)["state"], "unknown")

    def test_gpu_reports_all_devices_and_temperature_alert(self):
        response = types.SimpleNamespace(stdout="0, GPU A, 72, 20000, 24000, 69\n1, GPU B, 0, 23000, 24000, 88\n")
        with patch.object(monitor.subprocess, "run", return_value=response) as command:
            result = monitor.gpu_check(self.cfg)
        self.assertEqual(result["state"], "warning")
        self.assertEqual(len(result["devices"]), 2)
        self.assertEqual(result["devices"][0]["memory_used_mib"], 20000)
        self.assertEqual(command.call_args.kwargs["timeout"], 8)
        self.assertNotIn("shell", command.call_args.kwargs)

    def test_high_vram_alone_is_not_a_failure(self):
        with patch.object(monitor.subprocess, "run", return_value=types.SimpleNamespace(stdout="0, GPU A, 99, 23999, 24000, 69\n")):
            self.assertEqual(monitor.gpu_check(self.cfg)["state"], "ok")

    def test_gpu_missing_incomplete_nan_and_disabled(self):
        with patch.object(monitor.subprocess, "run", side_effect=FileNotFoundError):
            self.assertEqual(monitor.gpu_check(self.cfg)["state"], "unknown")
        for output in ("", "0, GPU, N/A, 100, 24000, 50", "0, GPU, nan, 100, 24000, 50", "0, GPU"):
            with patch.object(monitor.subprocess, "run", return_value=types.SimpleNamespace(stdout=output)):
                self.assertEqual(monitor.gpu_check(self.cfg)["state"], "unknown")
        self.cfg["gpu"] = False
        with patch.object(monitor.subprocess, "run") as command:
            self.assertEqual(monitor.gpu_check(self.cfg)["enabled"], False)
            command.assert_not_called()

    def test_disk_free_space_usage_and_missing_path(self):
        gib = 1024**3
        for used, free, state in ((80, 20, "ok"), (86, 14, "warning"), (45, 10, "warning")):
            with patch.object(monitor.shutil, "disk_usage", return_value=types.SimpleNamespace(total=100*gib, used=used*gib, free=free*gib)):
                self.assertEqual(monitor.disk_check(self.cfg)["state"], state)
        with patch.object(monitor.shutil, "disk_usage", side_effect=OSError):
            self.assertEqual(monitor.disk_check(self.cfg)["state"], "unknown")

    def test_global_state_cannot_hide_missing_backup(self):
        self.cfg.update(gpu=False, backup_status_file="")
        with patch.object(monitor, "disk_check", return_value=monitor.check("ok", "Disque")):
            self.assertEqual(monitor.collect(self.cfg, self.now)["state"], "unknown")

    def test_invalid_local_settings_rejected(self):
        for key, value in (("gpu", "true"), ("disk_warning", 200), ("min_free_gib", True), ("disk_path", ""), ("backup_max_hours", -1)):
            with self.subTest(key=key), self.assertRaises(ValueError):
                monitor.validate_settings({**self.cfg, key: value})

    def test_local_http_report_restricts_hosts_paths_and_caching(self):
        root = Path(self.temp.name)
        self.cfg.update(gpu=False, backup_status_file="")
        (root / "supervision.json").write_text(json.dumps(self.cfg), encoding="utf-8")
        (root / "client.html").write_text("<!doctype html><title>Portail test</title>", encoding="utf-8")
        (root / "supervision.py").write_text(Path(monitor.__file__).read_text(encoding="utf-8"), encoding="utf-8")
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        process = subprocess.Popen([sys.executable, str(root / "supervision.py"), "--serve", "--port", str(port)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        def stop_process():
            if process.poll() is None:
                process.kill()
            process.wait(timeout=5)
        self.addCleanup(stop_process)
        base = "http://127.0.0.1:" + str(port)
        for attempt in range(60):
            try:
                with urllib.request.urlopen(base + "/status.json", timeout=1) as response:
                    data = json.load(response)
                    self.assertEqual(response.headers["Cache-Control"], "no-store")
                    self.assertEqual(response.headers["X-Content-Type-Options"], "nosniff")
                    self.assertIsNone(response.headers.get("Access-Control-Allow-Origin"))
                    break
            except (OSError, urllib.error.URLError):
                if process.poll() is not None:
                    self.fail("Collector stopped before HTTP validation")
                time.sleep(0.05)
        else:
            self.fail("Collector did not expose a current report")
        self.assertNotEqual(data["state"], "ok")
        self.assertEqual(data["checks"][2]["state"], "not_configured")
        with urllib.request.urlopen(base, timeout=2) as response:
            self.assertIn(b"Portail test", response.read())
        for path, host, expected in (("/supervision.json", None, 404), ("/../supervision.json", None, 404), ("/status.json", "untrusted.example", 403)):
            headers = {"Host": host} if host else {}
            with self.assertRaises(urllib.error.HTTPError) as denied:
                urllib.request.urlopen(urllib.request.Request(base + path, headers=headers), timeout=2)
            self.assertEqual(denied.exception.code, expected)
            denied.exception.close()
        process.terminate()
        process.wait(timeout=5)


if __name__ == "__main__":
    unittest.main()
