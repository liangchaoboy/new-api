"""Linux migration matrix. Requires Docker, nsenter, curl and local test binaries."""

import argparse
import os
import json
import time
import sqlite3
import secrets
import subprocess as sp
import socket
import tempfile
from pathlib import Path


def run(args, **kw):
    return sp.run(args, text=True, capture_output=True, check=True, **kw).stdout.strip()


def engine(which, root, binaries, tests):
    mysql = which == "mysql"
    pg = which == "postgres"
    name = "supply-" + which + "-" + secrets.token_hex(4)
    pw = secrets.token_hex(18)
    prefix = []
    env = dict(
        PATH=os.defpath + ":/usr/local/bin",
        SESSION_SECRET=secrets.token_hex(32),
        CRYPTO_SECRET=secrets.token_hex(32),
        HOST="127.0.0.1",
        GIN_MODE="release",
        DEFAULT_LANGUAGE="en",
        TZ="UTC",
        GOMAXPROCS="2",
    )
    dbname = "template1" if pg else "mysql"
    dbfile = None

    def sql(q):
        if which == "sqlite":
            with sqlite3.connect(dbfile) as conn:
                return "\n".join("|".join(str(v) for v in r) for r in conn.execute(q))
        if mysql:
            return run(
                [
                    "docker",
                    "exec",
                    "-e",
                    "MYSQL_PWD=" + pw,
                    name,
                    "mysql",
                    "-N",
                    "-B",
                    "-uroot",
                    dbname,
                    "-e",
                    q,
                ]
            )
        return run(
            [
                "docker",
                "exec",
                name,
                "psql",
                "-U",
                "postgres",
                "-d",
                dbname,
                "-At",
                "-v",
                "ON_ERROR_STOP=1",
                "-c",
                q,
            ]
        )

    cid = None
    try:
        if mysql or pg:
            args = ["docker", "run", "--rm", "-d", "--network", "none", "--name", name]
            if mysql:
                args += [
                    "--env",
                    "MYSQL_ROOT_PASSWORD=" + pw,
                    "mysql:8.4",
                    "--innodb-buffer-pool-size=64M",
                    "--performance-schema=OFF",
                ]
            else:
                args += ["--env", "POSTGRES_PASSWORD=" + pw, "pgvector/pgvector:0.8.0-pg15"]
            cid = run(args)
            pid = run(["docker", "inspect", "--format", "{{.State.Pid}}", cid])
            prefix = ["nsenter", "-t", pid, "-n", "--"]
            for _ in range(90):
                try:
                    sql("SELECT 1")
                    if (
                        not mysql
                        or run(
                            [
                                "docker",
                                "exec",
                                "-e",
                                "MYSQL_PWD=" + pw,
                                name,
                                "mysql",
                                "--protocol=tcp",
                                "-h127.0.0.1",
                                "-uroot",
                                "-N",
                                "-e",
                                "SELECT 1",
                            ]
                        )
                        == "1"
                    ):
                        break
                except sp.CalledProcessError:
                    pass
                time.sleep(1)
            else:
                raise RuntimeError("disposable database did not become ready")
        for scenario in ("fresh", "upgrade", "receiver_upgrade"):
            work = root / (which + "-" + scenario + "-" + name[-8:])
            work.mkdir()
            database = "supply_" + scenario
            if which != "sqlite":
                dbname = "mysql" if mysql else "template1"
                sql("CREATE DATABASE " + database + (" CHARACTER SET utf8mb4" if mysql else ""))
                dbname = database
                env["SQL_DSN"] = (
                    "root:" + pw + "@tcp(127.0.0.1:3306)/" + database + "?parseTime=true"
                    if mysql
                    else "postgres://postgres:"
                    + pw
                    + "@127.0.0.1:5432/"
                    + database
                    + "?sslmode=disable"
                )
            else:
                dbfile = work / "test.db"
                env["SQLITE_PATH"] = str(dbfile)
            with socket.socket() as sock:
                sock.bind(("127.0.0.1", 0))
                port = sock.getsockname()[1]
            env["PORT"] = str(port)
            seq = (
                ["branch", "branch"]
                if scenario == "fresh"
                else [
                    ("previous" if scenario == "receiver_upgrade" else "release"),
                    "branch",
                    "branch",
                ]
            )
            for i, label in enumerate(seq):
                exe = binaries[label]
                log = work / f"{i}-{label}.log"
                with log.open("w") as output:
                    proc = sp.Popen(
                        prefix + [str(exe)], cwd=work, env=env, stdout=output, stderr=sp.STDOUT
                    )
                    try:
                        for _ in range(120):
                            if proc.poll() is not None:
                                raise ValueError("startup exited; see " + str(log))
                            r = sp.run(
                                prefix
                                + [
                                    "curl",
                                    "-fsS",
                                    "--max-time",
                                    "3",
                                    "http://127.0.0.1:" + str(port) + "/api/status",
                                ],
                                capture_output=True,
                            )
                            if r.returncode == 0 and json.loads(r.stdout).get("success"):
                                break
                            time.sleep(0.4)
                        else:
                            raise ValueError("startup timed out " + str(log))
                    finally:
                        proc.terminate()
                        proc.wait(timeout=30)
                if i == 0:
                    keycol = '"key"' if pg else "`key`"
                    sql(
                        "INSERT INTO channels (name,"
                        + keycol
                        + ",status,type) VALUES ('baseline','fixture',1,1)"
                    )
                    if mysql:
                        idxq = "SHOW INDEX FROM channels"
                    elif pg:
                        idxq = "SELECT indexname FROM pg_indexes WHERE tablename='channels' ORDER BY indexname"
                    else:
                        idxq = "SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='channels' ORDER BY name"

                    def index_names():
                        rows = sql(idxq).splitlines()
                        return {r.split("\t")[2] for r in rows} if mysql else set(rows)

                    baseline = index_names()
                    if scenario == "receiver_upgrade":
                        sql(
                            "INSERT INTO supply_targets(id,name,url,issuer,secret) VALUES ('legacy-target','existing','https://example.org',12,'legacy-fixture')"
                        )
                    gcol = '"group"' if pg else "`group`"
                    channel = int(sql("SELECT id FROM channels WHERE name='baseline'"))
                    sql(
                        f"UPDATE channels SET {gcol}='fixture', models='fixture-model' WHERE id={channel}"
                    )
                    boolval = "true" if pg else "1"
                    ability = f"INSERT INTO abilities ({gcol},model,channel_id,enabled) VALUES ('fixture','fixture-model',{channel},{boolval})"
                    sql(ability)
                    user = "INSERT INTO users (username,aff_code,password) VALUES ('matrix-legacy','mxleg1','disabled-fixture')"
                    sql(user)
                for duplicate in [ability, user]:
                    try:
                        sql(duplicate)
                    except sqlite3.IntegrityError as e:
                        if "unique" not in str(e).lower():
                            raise
                    except sp.CalledProcessError as e:
                        if not any(word in e.stderr.lower() for word in ("duplicate", "unique")):
                            raise
                    else:
                        raise ValueError("legacy uniqueness lost")
                assert (
                    sql(
                        f"SELECT count(*) FROM abilities WHERE {gcol}='fixture' AND model='fixture-model' AND channel_id={channel}"
                    )
                    == "1"
                )
                assert (
                    sql(
                        "SELECT count(*) FROM users WHERE username='matrix-legacy' AND aff_code='mxleg1'"
                    )
                    == "1"
                )
                if label == "branch":
                    assert (
                        sql(
                            "SELECT COUNT(*) FROM channels WHERE name='baseline' AND assigned_user_id IS NULL AND status=1"
                        )
                        == "1"
                    )
                    assert baseline <= index_names()
                    if scenario == "receiver_upgrade":
                        assert (
                            sql(
                                "SELECT COUNT(*) FROM supply_targets WHERE id='legacy-target' AND issuer=12 AND secret='legacy-fixture' AND (mode IS NULL OR mode='') AND address_ref IS NULL"
                            )
                            == "1"
                        )
                print(which, scenario, i + 1, label, "healthy", flush=True)
            if scenario == "fresh":
                logfile = work / "routing.log"
                with logfile.open("w") as output:
                    rc = sp.run(
                        prefix
                        + [
                            str(tests),
                            "-test.run",
                            "^(TestAssignedChannelSelectionAndRevocation|TestSupplyReceiptAndUsage|TestSupplyReviewSafety|TestDirectSupplyCustomerHandoff|TestDirectSupplyOrdinaryAccountRegistration|TestCustomerSupplyUncertainWriteAndScopedUsage)$",
                            "-test.v",
                        ],
                        env={**env, "TEST_CHANNEL_SQL_DSN": env.get("SQL_DSN", "")},
                        cwd=work,
                        stdout=output,
                        stderr=sp.STDOUT,
                    ).returncode
                for line in logfile.read_text().splitlines():
                    if "database=" in line and "version=" in line:
                        print(line, flush=True)
                if rc:
                    raise ValueError("routing test failed " + str(logfile))
                print(which, "routing pass", flush=True)
    finally:
        if cid:
            run(["docker", "stop", cid])


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for label in ("branch", "release", "previous", "controller_tests"):
        parser.add_argument(label, type=Path)
    args = parser.parse_args()
    binaries = {k: getattr(args, k).resolve(strict=True) for k in ("branch", "release", "previous")}
    tests = args.controller_tests.resolve(strict=True)
    os.umask(0o077)
    root = Path(tempfile.mkdtemp(prefix="new-api-supplier-matrix-"))
    print("Isolated migration logs:", root, flush=True)
    for dialect in ("sqlite", "mysql", "postgres"):
        engine(dialect, root, binaries, tests)
