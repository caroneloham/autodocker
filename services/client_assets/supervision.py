#!/usr/bin/env python3
"""Collecteur AutoDocker pour l'hôte cible. Bibliothèque standard, aucune action de maintenance."""
import argparse
import csv
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import math
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parent
MAX_RECEIPT = 65536


def read_json(path, limit=MAX_RECEIPT):
    with Path(path).open("rb") as stream:
        data = stream.read(limit + 1)
    if len(data) > limit:
        raise ValueError("Fichier JSON trop volumineux")
    value = json.loads(data)
    if not isinstance(value, dict):
        raise ValueError("Objet JSON attendu")
    return value


def check(state, label, **values):
    return {"state": state, "label": label, **values}


def disk_check(cfg):
    try:
        disk = shutil.disk_usage(cfg["disk_path"])
        used = 100 * disk.used / disk.total
        free = disk.free / 1024**3
        state = "warning" if used >= cfg["disk_warning"] or free < cfg["min_free_gib"] else "ok"
        return check(state, "Disque", path=cfg["disk_path"], used_percent=round(used, 1), free_gib=round(free, 1))
    except (OSError, ValueError, ZeroDivisionError):
        return check("unknown", "Disque", message="Chemin inaccessible sur cet hôte")


def gpu_check(cfg):
    if not cfg["gpu"]:
        return check("not_configured", "GPU", enabled=False, message="GPU non demandé")
    try:
        result = subprocess.run(["nvidia-smi", "--query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu", "--format=csv,noheader,nounits"], capture_output=True, text=True, timeout=8, check=True)
        gpus = []
        for row in csv.reader(result.stdout.splitlines()):
            if len(row) != 6:
                raise ValueError("Mesure GPU incomplète")
            index, name, *numbers = [value.strip() for value in row]
            usage, used, total, temperature = map(float, numbers)
            if not all(math.isfinite(v) for v in (usage, used, total, temperature)) or not 0 <= usage <= 100 or not 0 <= used <= total or total <= 0 or not 0 <= temperature <= 130:
                raise ValueError("Mesure GPU invalide")
            gpus.append({"index": index, "name": name, "utilization_percent": usage, "memory_used_mib": used, "memory_total_mib": total, "temperature_c": temperature})
        if not gpus:
            raise ValueError("Aucun GPU retourné")
        state = "warning" if any(g["temperature_c"] >= cfg["temperature_warning"] for g in gpus) else "ok"
        return check(state, "GPU NVIDIA", devices=gpus)
    except (OSError, ValueError, subprocess.SubprocessError):
        return check("unknown", "GPU NVIDIA", message="Mesures indisponibles : vérifier nvidia-smi et les droits sur l'hôte")


def backup_check(cfg, now):
    path = cfg["backup_status_file"]
    if not path:
        return check("not_configured", "Sauvegarde", message="Relier le reçu du système de sauvegarde client")
    try:
        receipt = read_json(path)
        if receipt.get("status") != "ok" or receipt.get("verified") is not True:
            return check("warning", "Sauvegarde", message="Le système externe ne déclare pas une sauvegarde vérifiée réussie")
        completed = datetime.fromisoformat(receipt["completed_at"].replace("Z", "+00:00"))
        if completed.tzinfo is None:
            raise ValueError("Fuseau absent")
        age = (now - completed).total_seconds() / 3600
        if age < -5 / 60:
            raise ValueError("Date future")
        return check("warning" if age > cfg["backup_max_hours"] else "ok", "Sauvegarde", age_hours=round(max(0, age), 2), completed_at=completed.isoformat(), message="Résultat déclaré par le système de sauvegarde externe")
    except (OSError, ValueError, KeyError, TypeError, AttributeError, OverflowError):
        return check("unknown", "Sauvegarde", message="Reçu absent, illisible ou horodatage invalide")


def validate_settings(cfg):
    if not isinstance(cfg.get("gpu"), bool):
        raise ValueError("gpu doit être un booléen")
    for key in ("disk_path", "backup_status_file"):
        if not isinstance(cfg.get(key), str) or len(cfg[key]) > 500 or any(ord(c) < 32 for c in cfg[key]):
            raise ValueError("Chemin invalide : " + key)
    if not cfg["disk_path"]:
        raise ValueError("Disque manquant")
    for key, low, high in (("disk_warning", 50, 95), ("min_free_gib", 1, 10000), ("temperature_warning", 40, 95), ("backup_max_hours", 1, 8760)):
        if type(cfg.get(key)) is not int or not low <= cfg[key] <= high:
            raise ValueError("Seuil invalide : " + key)
    return cfg


def collect(cfg, now=None):
    now = now or datetime.now(timezone.utc)
    checks = [disk_check(cfg), gpu_check(cfg), backup_check(cfg, now)]
    enabled = [c for c in checks if c.get("enabled", True)]
    states = {c["state"] for c in enabled}
    state = "warning" if "warning" in states else "unknown" if states & {"unknown", "not_configured"} else "ok"
    return {"version": 1, "checked_at": now.isoformat(), "state": state, "checks": checks}


def write_report(report):
    handle, name = tempfile.mkstemp(prefix=".supervision-", dir=ROOT)
    try:
        with os.fdopen(handle, "w", encoding="utf-8") as stream:
            json.dump(report, stream, ensure_ascii=False, allow_nan=False)
        os.replace(name, ROOT / "supervision-status.json")
    finally:
        if os.path.exists(name):
            os.unlink(name)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--once", action="store_true")
    mode.add_argument("--serve", action="store_true")
    parser.add_argument("--port", type=int, default=18798)
    args = parser.parse_args()
    if not 1024 <= args.port <= 65535:
        parser.error("Port compris entre 1024 et 65535")
    cfg = validate_settings(read_json(ROOT / "supervision.json"))
    if args.once:
        report = collect(cfg)
        write_report(report)
        print(json.dumps(report, ensure_ascii=False))
        return {"ok": 0, "warning": 1, "unknown": 2}[report["state"]]

    page = (ROOT / "client.html").read_bytes()
    lock = threading.Lock()
    cached = None
    def update():
        nonlocal cached
        while True:
            try:
                report = collect(validate_settings(read_json(ROOT / "supervision.json")))
                write_report(report)
                with lock:
                    cached = json.dumps(report, ensure_ascii=False, allow_nan=False).encode("utf-8")
            except (OSError, ValueError, KeyError, TypeError) as error:
                print("Collecte impossible : " + str(error), file=sys.stderr)
                # Preserve the previous timestamp; the browser marks a stale report.
            time.sleep(60)

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.headers.get("Host") not in ("127.0.0.1:" + str(args.port), "localhost:" + str(args.port)):
                self.send_error(403)
                return
            path = urlsplit(self.path).path
            with lock:
                report = cached
            if path in ("/", "/client.html"):
                data, mime, status = page, "text/html; charset=utf-8", 200
            elif path == "/status.json":
                data, mime, status = report or b'{"error":"Collecte en cours"}', "application/json", 200 if report else 503
            else:
                self.send_error(404)
                return
            self.send_response(status)
            self.send_header("Content-Type", mime)
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.send_header("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        def log_message(self, *args):
            pass

    server = HTTPServer(("127.0.0.1", args.port), Handler)
    threading.Thread(target=update, daemon=True).start()
    print("Supervision locale : http://127.0.0.1:" + str(args.port), flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, TypeError, KeyError) as error:
        print("Supervision indisponible : " + str(error), file=sys.stderr)
        sys.exit(2)
