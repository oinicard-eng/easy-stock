# -*- coding: utf-8 -*-
"""P1a：抢救 QuantMind universe 历史排名到本地。

背景：QuantMind 的每日全市场预测排名按日分片存储，归档默认 7 天硬删；
模型分数回放基线（P1b）需要更长的历史窗口，所以先把能拉的全部拉回本地。

输出：.runtime/qm-universe/YYYYMMDD.json，每行一条精简记录
      {rank, code, name, score, sector, isSt}
纪律：只读现成分数（不带 execute），limit=2000 分页，请求间 0.3s 限速，
      单 worker 的 api 进程不会被压到。
"""
import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import date, timedelta
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
OUT = ROOT / ".runtime" / "qm-universe"
ENV_FILE = ROOT / ".runtime" / "quantmind.env"
PAGE = 2000
SLEEP = 0.3
LOOKBACK_DAYS = 45


def load_env():
    env = {}
    for line in ENV_FILE.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            env[k.strip()] = v.strip()
    return env


def call(base, method, path, body=None, token=None, timeout=60):
    req = urllib.request.Request(base + path, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    data = json.dumps(body).encode() if body is not None else None
    try:
        with urllib.request.urlopen(req, data=data, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read().decode())
        except Exception:
            return e.code, {}
    except Exception as e:
        return 0, {"err": str(e)[:200]}


def login(base, env):
    code, r = call(base, "POST", "/api/v1/auth/login", {
        "tenant_id": env.get("A_STOCK_QUANTMIND_TENANT", ""),
        "username": env.get("A_STOCK_QUANTMIND_USER", ""),
        "password": env.get("A_STOCK_QUANTMIND_PASSWORD", ""),
    })
    d = r.get("data", r) if isinstance(r, dict) else {}
    tok = d.get("access_token") or d.get("accessToken") or (r.get("access_token") if isinstance(r, dict) else None)
    if not tok:
        raise SystemExit(f"login failed: {code} {str(r)[:200]}")
    return tok


def ready_model(base, tok):
    code, r = call(base, "GET", "/api/v1/models", None, tok)
    items = r.get("items", [])
    ready = [m for m in items if m.get("status") == "ready"]
    if not ready:
        raise SystemExit("no ready model")
    return ready[0]["model_id"]


def fetch_day(base, tok, mid_q, ds):
    """拉一整天全量排名（分页），返回精简记录列表；total=0 视为该日无数据。"""
    out = []
    offset = 0
    total = None
    while True:
        code, u = call(base, "GET",
                       f"/api/v1/research/universe?model_id={mid_q}&date={ds}&limit={PAGE}&offset={offset}",
                       None, tok)
        if code != 200:
            return None, f"http {code} at offset {offset}"
        data = u.get("data") or {}
        items = data.get("items") or []
        if total is None:
            total = (data.get("summary") or {}).get("total") or 0
            if total == 0:
                return [], "empty"
        out.extend({
            "rank": it.get("rank"),
            "code": it.get("code"),
            "name": it.get("name"),
            "score": it.get("score"),
            "sector": it.get("sector"),
            "isSt": it.get("isSt"),
        } for it in items)
        offset += PAGE
        if not items or offset >= total:
            break
        time.sleep(SLEEP)
    return out, f"{len(out)}/{total}"


def main():
    env = load_env()
    base = env.get("A_STOCK_QUANTMIND_URL", "http://192.168.5.6:8000").rstrip("/")
    tok = login(base, env)
    mid = ready_model(base, tok)
    mid_q = urllib.parse.quote(mid, safe="")
    print("model:", mid[:60], flush=True)

    OUT.mkdir(parents=True, exist_ok=True)
    today = date.today()
    ok_days, skipped = 0, []
    for i in range(LOOKBACK_DAYS, 0, -1):
        ds = (today - timedelta(days=i)).isoformat()
        dst = OUT / (ds.replace("-", "") + ".json")
        if dst.exists():
            print(ds, "cached", flush=True)
            ok_days += 1
            continue
        records, status = fetch_day(base, tok, mid_q, ds)
        if records is None:
            skipped.append((ds, status))
            print(ds, "FAIL", status, flush=True)
        elif not records:
            skipped.append((ds, status))
            print(ds, "empty", flush=True)
        else:
            dst.write_text(json.dumps(records, ensure_ascii=False), encoding="utf-8")
            ok_days += 1
            print(ds, "saved", status, flush=True)
        time.sleep(SLEEP)
    print(f"done: {ok_days} days saved, {len(skipped)} skipped/failed", flush=True)
    for ds, s in skipped:
        print("  skip", ds, s, flush=True)


if __name__ == "__main__":
    sys.exit(main())
