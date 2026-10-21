# -*- coding: utf-8 -*-
"""P1b 补数据：把 universe 快照 Top500 里日线缓存缺失的票补齐。

背景：replay 缓存的 3335 只是等距抽样，Top300 里缺 99 只非北交所票，
Top30 每日只有 60-70% 覆盖。等距抽样按代码顺序系统性跳票，与未来收益
无关联，但样本薄会让关键分段（1-5 名）的置信度打折。补齐后重跑基线。

纪律：与救援脚本一致——只读接口、0.3s 限速、失败不重试超过 1 次。
输出：直接写进 backend/.runtime/replay/klines/，与现有缓存同格式。
"""
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
UNI_DIR = ROOT / ".runtime" / "qm-universe"
KLINE_DIR = ROOT / "backend" / ".runtime" / "replay" / "klines"
ENV_FILE = ROOT / ".runtime" / "quantmind.env"
TOP_N = 500
SLEEP = 0.3
DAYS = 1200


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
    tok = d.get("access_token") or d.get("accessToken")
    if not tok:
        raise SystemExit(f"login failed: {code} {str(r)[:200]}")
    return tok


def fetch_kline(base, tok, symbol):
    query = urllib.parse.urlencode({
        "symbol": symbol, "market": "A", "period": "daily",
        "days": DAYS, "adjust": "qfq",
    })
    code, r = call(base, "GET", "/api/v1/market/kline?" + query, None, tok)
    if code != 200:
        return None
    d = r.get("data", r) if isinstance(r, dict) else {}
    rows = d.get("rows") or d.get("items") or (d.get("list") if isinstance(d, dict) else None)
    if not rows and isinstance(d, dict):
        # 兼容 data 直接是列表的形态
        if isinstance(r.get("data"), list):
            rows = r["data"]
    if not rows:
        return None
    bars = []
    for row in rows:
        t = row.get("time") or row.get("date") or row.get("datetime") or ""
        date = str(t)[:10].replace("/", "-")
        try:
            bars.append({
                "d": date,
                "o": float(row.get("open", 0)), "h": float(row.get("high", 0)),
                "l": float(row.get("low", 0)), "c": float(row.get("close", 0)),
                "v": float(row.get("volume", 0) or 0), "a": float(row.get("amount", 0) or 0),
            })
        except (TypeError, ValueError):
            continue
    return bars if len(bars) >= 30 else None


def main():
    env = load_env()
    base = env.get("A_STOCK_QUANTMIND_URL", "http://192.168.5.6:8000").rstrip("/")
    tok = login(base, env)

    cached = {f[:-5] for f in os.listdir(KLINE_DIR) if f.endswith(".json")}
    want = set()
    for name in os.listdir(UNI_DIR):
        if not name.endswith(".json"):
            continue
        rows = json.loads((UNI_DIR / name).read_text(encoding="utf-8"))
        for r in rows:
            if r.get("rank", 10**9) <= TOP_N and not r["code"].endswith(".BJ"):
                want.add(r["code"])
    missing = sorted(want - cached)
    print(f"Top{TOP_N} 去重后 {len(want)} 只，缓存已有 {len(want & cached)}，待补 {len(missing)}")
    ok = fail = 0
    for i, symbol in enumerate(missing):
        bars = fetch_kline(base, tok, symbol)
        if bars:
            (KLINE_DIR / (symbol + ".json")).write_text(
                json.dumps(bars, ensure_ascii=False), encoding="utf-8")
            ok += 1
        else:
            fail += 1
        if (i + 1) % 50 == 0:
            print(f"  … {i + 1}/{len(missing)}  成功 {ok}  失败 {fail}")
        time.sleep(SLEEP)
    print(f"完成：成功 {ok}，失败 {fail}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
