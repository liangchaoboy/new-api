"""Exercise B against two stock A instances in disposable loopback directories.

python3 e2e/supplier_customer.py /path/to/reseller-new-api /path/to/stock-new-api
"""

import argparse
from contextlib import ExitStack
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import secrets
import socket
import sqlite3
import subprocess
import tempfile
import threading
import time
from urllib.error import HTTPError
from urllib.request import Request, urlopen


def request(base, path, method="GET", body=None, bearer="", expect_success=True):
    headers = {"Content-Type": "application/json", "Origin": base}
    if bearer:
        headers["Authorization"] = "Bearer " + bearer
    data = None if body is None else json.dumps(body).encode()
    try:
        with urlopen(
            Request(base + path, data=data, headers=headers, method=method), timeout=20
        ) as response:
            result = json.load(response)
    except HTTPError as error:
        if expect_success:
            raise RuntimeError(f"{method} {path} returned HTTP {error.code}") from None
        return {"success": False}
    if expect_success and result.get("success") is False:
        raise RuntimeError(f"{method} {path} was rejected")
    return result


def wait(check, label, seconds=60):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        value = check()
        if value:
            return value
        time.sleep(0.5)
    raise RuntimeError(f"Timed out waiting for {label}")


class Instance:
    def __init__(self, stack, binary, directory, log_dsn=None):
        directory.mkdir()
        self.db = directory / "fixture.sqlite"
        with socket.socket() as port:
            port.bind(("127.0.0.1", 0))
            address = str(port.getsockname()[1])
        self.url = "http://127.0.0.1:" + address
        self.password = secrets.token_urlsafe(24)
        env = {k: v for k, v in os.environ.items() if k in {"HOME", "PATH", "SYSTEMROOT", "TMPDIR"}}
        env.update(
            HOST="127.0.0.1",
            PORT=address,
            SQLITE_PATH=str(self.db),
            SESSION_SECRET=secrets.token_hex(32),
            CRYPTO_SECRET=secrets.token_hex(32),
            DEFAULT_LANGUAGE="en",
            GIN_MODE="release",
            GLOBAL_API_RATE_LIMIT_ENABLE="false",
            CRITICAL_RATE_LIMIT_ENABLE="false",
            GOMAXPROCS="2",
        )
        if log_dsn:
            env["LOG_SQL_DSN"] = log_dsn
        log = stack.enter_context((directory / "app.log").open("w"))
        process = subprocess.Popen([str(binary)], env=env, cwd=directory, stdout=log, stderr=log)
        stack.callback(process.wait, timeout=30)
        stack.callback(process.terminate)

        def ready():
            if process.poll() is not None:
                raise RuntimeError("Fixture exited before setup")
            try:
                return request(self.url, "/api/setup")
            except OSError:
                return False

        wait(ready, "fixture startup")
        self.username = "fixtureadmin"
        request(
            self.url,
            "/api/setup",
            "POST",
            {
                "username": self.username,
                "password": self.password,
                "confirmPassword": self.password,
            },
        )
        self.admin = self.login(self.username, self.password)

    def login(self, username, password):
        return request(
            self.url, "/api/user/login", "POST", {"username": username, "password": password}
        )["data"]["access_token"]

    def api(self, path, method="GET", body=None, user=None):
        return request(self.url, path, method, body, user or self.admin).get("data")

    def supplier(self, username):
        password = secrets.token_urlsafe(24)
        self.api("/api/user/", "POST", {"username": username, "password": password, "role": 1})
        return self.login(username, password)

    def target(self, url, username, password):
        result = self.api(
            "/api/supply/targets",
            "POST",
            {
                "url": url + "/sign-in",
                "username": username,
                "password": password,
                "access_method": "saved_account",
            },
        )
        target = next(t for t in self.api("/api/supply/targets") if t["id"] == result["id"])
        assert target["url"] == url and target["mode"] == "customer_account"
        assert (
            target["account_api"] == "official_channels" and target["usage_api"] == "channel_logs"
        )
        assert "secret" not in target and "password" not in target
        with sqlite3.connect(self.db) as db:
            (ciphertext,) = db.execute(
                "SELECT secret FROM supply_targets WHERE id=?", (target["id"],)
            ).fetchone()
            assert ciphertext and password not in ciphertext
        return target["id"]

    def row(self, resource):
        return next(r for r in self.api("/api/supply/resources") if r["id"] == resource)

    def assign(self, resource, target):
        self.api(
            f"/api/supply/resources/{resource}/assignment",
            "PUT",
            {
                "target": target,
                "scope": "instance",
                "customer_group": "default",
                "revision": self.row(resource)["revision"],
            },
        )

    def confirmed(self, resource):
        def read():
            row = self.row(resource)
            return row if row["revision"] == row["applied"] else None

        return wait(read, "channel readback " + resource)

    def usage(self, resource, user=None, target=""):
        day = int(time.time()) // 86400 * 86400
        return self.api(
            f"/api/supply/resources/{resource}/usage?from={day}&to={day}&target={target}", user=user
        )

    def inference_key(self):
        self.api(
            "/api/token/", "POST", {"name": "fixture", "expired_time": -1, "unlimited_quota": True}
        )
        token = self.api("/api/token/")["items"][0]
        key = self.api(f"/api/token/{token['id']}/key", "POST", {})["key"]
        with sqlite3.connect(self.db) as db:
            db.execute("UPDATE users SET quota=100000000 WHERE username=?", (self.username,))
        return key if key.startswith("sk-") else "sk-" + key


def run(reseller, stock, test_clickhouse=False):
    with ExitStack() as stack:
        work = Path(stack.enter_context(tempfile.TemporaryDirectory()))
        center = Instance(stack, reseller, work / "B")
        log_dsn = "clickhouse://default@127.0.0.1:9000/supplier_e2e" if test_clickhouse else None
        customers = [
            Instance(stack, stock, work / "A1", log_dsn),
            Instance(stack, stock, work / "A2"),
        ]
        targets = [center.target(a.url, a.username, a.password) for a in customers]
        suppliers = [center.supplier(name) for name in ("supplier1", "supplier2")]
        models = ["gpt-4o", "gpt-4o-mini"]
        keys = {model: secrets.token_hex(20) for model in models}

        class Provider(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                model = json.loads(self.rfile.read(int(self.headers["Content-Length"])))["model"]
                if (
                    model not in keys
                    or self.headers.get("Authorization") != "Bearer " + keys[model]
                ):
                    self.send_error(401)
                    return
                response = json.dumps(
                    {
                        "id": "fixture",
                        "model": model,
                        "object": "chat.completion",
                        "choices": [
                            {
                                "index": 0,
                                "message": {"role": "assistant", "content": "ok"},
                                "finish_reason": "stop",
                            }
                        ],
                        "usage": {"prompt_tokens": 12, "completion_tokens": 4, "total_tokens": 16},
                    }
                ).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(response)))
                self.end_headers()
                self.wfile.write(response)

        provider = stack.enter_context(ThreadingHTTPServer(("127.0.0.1", 0), Provider))
        threading.Thread(target=provider.serve_forever, daemon=True).start()
        stack.callback(provider.shutdown)
        configs = [
            {
                "name": "fixture " + model,
                "models": model,
                "key": keys[model],
                "type": 1,
                "base_url": f"http://127.0.0.1:{provider.server_port}",
            }
            for model in models
        ]
        resources = [
            center.api("/api/supply/resources", "POST", cfg, user)["id"]
            for cfg, user in zip(configs, suppliers)
        ]
        for rid in resources:
            center.assign(rid, targets[0])
        initial = [center.confirmed(r)["effective"]["channel_id"] for r in resources]
        assert initial[0] != initial[1]
        for user, rid in zip(suppliers, resources):
            assert [r["id"] for r in center.api("/api/supply/resources", user=user)] == [rid]
        other_path = f"/api/supply/resources/{resources[0]}"
        day = int(time.time()) // 86400 * 86400
        for suffix in ("/history", f"/usage?from={day}&to={day}"):
            assert (
                request(center.url, other_path + suffix, bearer=suppliers[1], expect_success=False)[
                    "success"
                ]
                is False
            )
        api_keys = [a.inference_key() for a in customers]

        def infer(customer, model, ok=True):
            response = request(
                customers[customer].url,
                "/v1/chat/completions",
                "POST",
                {"model": model, "messages": [{"role": "user", "content": "fixture"}]},
                api_keys[customer],
                expect_success=ok,
            )
            assert bool(response.get("choices")) == ok

        def count(rid, target, user, expected):
            def check():
                result = center.usage(rid, user, target)
                return result["complete"] and sum(r["records"] for r in result["rows"]) == expected

            wait(check, "customer usage")
            rows = center.usage(rid, user, target)["rows"]
            assert sum(r["input_tokens"] for r in rows) == expected * 12
            assert sum(r["output_tokens"] for r in rows) == expected * 4
            assert all(r["quota"] >= 0 and r["channel_id"] > 0 for r in rows)

        for _ in range(9):
            infer(0, models[0])
        infer(0, models[1])
        count(resources[0], targets[0], suppliers[0], 9)
        count(resources[1], targets[0], suppliers[1], 1)
        keys[models[0]] = secrets.token_hex(20)
        center.api(
            other_path,
            "PUT",
            {
                **configs[0],
                "name": "rotated",
                "key": keys[models[0]],
                "revision": center.row(resources[0])["revision"],
            },
            suppliers[0],
        )
        assert center.confirmed(resources[0])["effective"]["channel_id"] == initial[0]
        infer(0, models[0])  # The customer now uses the rotated supplier credential.
        center.assign(resources[0], targets[1])
        center.confirmed(resources[0])
        infer(0, models[0], False)
        for _ in range(6):
            infer(1, models[0])
        for user in (center.admin, suppliers[0]):
            count(resources[0], targets[0], user, 10)
            count(resources[0], targets[1], user, 6)
            count(resources[0], "", user, 16)
        center.api(
            other_path + "?revision=" + str(center.row(resources[0])["revision"]),
            "DELETE",
            user=suppliers[0],
        )
        assert center.confirmed(resources[0])["archived"]
        infer(1, models[0], False)
        count(resources[0], "", suppliers[0], 16)
        with sqlite3.connect(center.db) as db:
            assert db.execute("SELECT count(*) FROM logs WHERE type=2").fetchone()[0] == 0
        print(
            "PASS B-issued supplier accounts, customer registration, automatic channel CRUD and reassignment"
        )
        print(
            "PASS C1 usage A1=10 A2=6 total=16, C2=1, ownership isolation, archived history, B relay calls=0"
        )


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("reseller", type=Path)
    parser.add_argument("stock", type=Path)
    parser.add_argument(
        "--test-clickhouse",
        action="store_true",
        help="Use an empty disposable loopback ClickHouse database named supplier_e2e for A1 logs",
    )
    args = parser.parse_args()
    run(args.reseller.resolve(), args.stock.resolve(), args.test_clickhouse)
