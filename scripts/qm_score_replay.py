# -*- coding: utf-8 -*-
"""P1b：QuantMind 模型分数回放基线。

回答一个问题：universe 排名分数到底有没有前瞻预测力？口径与
backend/cmd/replay 完全一致——次日开盘买入、第 5 个交易日收盘卖出，
前复权价，不含交易成本。

数据来源：
- 分数：.runtime/qm-universe/YYYYMMDD.json（26 个交易日 × ~5000 只，rank/score）
- 日线：backend/.runtime/replay/klines/*.json（3335 只，2023-09 起，前复权）

输出：
- 按名次分段的前瞻收益（1-5 / 6-20 / 21-50 / 51-100 / 101-300 / 301-1000 / 1000+）
- 每日 Top30 的均值与胜率（稳定性）
- 位置门禁（当日涨幅>=6%、高出MA20>=12%、连涨>=5天）对 Top100 的影响
- 全市场基准与低分段对照

内存策略：逐票流式处理，不把全市场日线常驻内存（机器可用内存只有 ~0.4GB）。
"""

import json
import os
import sys
from statistics import mean

ROOT = r"D:\easy-stock-main"
UNI_DIR = os.path.join(ROOT, ".runtime", "qm-universe")
KLINE_DIR = os.path.join(ROOT, "backend", ".runtime", "replay", "klines")
HOLD = 5

# 位置门禁阈值：唯一来源是 policy.Limits（6% / 12% / 5 天），与生产一致
GATE_MAX_CHANGE = 6.0
GATE_MAX_MA20 = 12.0
GATE_MAX_UPDAYS = 5


def load_snapshots():
    snaps = []
    for name in sorted(os.listdir(UNI_DIR)):
        if not name.endswith(".json"):
            continue
        date = name[:4] + "-" + name[4:6] + "-" + name[6:8]
        with open(os.path.join(UNI_DIR, name), encoding="utf-8") as fh:
            rows = json.load(fh)
        snaps.append((date, rows))
    return snaps


def main():
    snapshots = load_snapshots()
    if not snapshots:
        print("没有 universe 快照")
        return 1
    snap_dates = [d for d, _ in snapshots]
    snap_index = {d: i for i, d in enumerate(snap_dates)}
    print(f"快照 {len(snapshots)} 天：{snap_dates[0]} ~ {snap_dates[-1]}")

    # 结果容器：ret[(date, code)] = 前瞻收益%；pos[(date, code)] = (chg, ma20gap, updays)
    rets = {}
    poss = {}
    covered = {}          # date -> 该日快照里能算出前瞻收益的票数
    symbols_seen = set()

    files = sorted(f for f in os.listdir(KLINE_DIR) if f.endswith(".json"))
    print(f"日线缓存 {len(files)} 只，逐票计算…")

    for idx, fname in enumerate(files):
        code = fname[:-5]
        try:
            with open(os.path.join(KLINE_DIR, fname), encoding="utf-8") as fh:
                bars = json.load(fh)
        except Exception:
            continue
        if len(bars) < 30:
            continue
        dates = [b["d"] for b in bars]
        # 同一只票的日期单调，用 dict 反查快照日的位置
        pos_of = {d: i for i, d in enumerate(dates)}
        for snap_date in snap_dates:
            pos = pos_of.get(snap_date)
            if pos is None or pos < 21:
                continue
            entry = pos + 1
            exit_ = pos + HOLD
            if exit_ >= len(bars):
                continue
            open_px = bars[entry]["o"]
            close_px = bars[exit_]["c"]
            if open_px <= 0 or close_px <= 0:
                continue
            cur = bars[pos]["c"]
            prev = bars[pos - 1]["c"]
            if cur <= 0 or prev <= 0:
                continue
            ma20 = mean([bars[j]["c"] for j in range(pos - 20, pos + 1)])
            if ma20 <= 0:
                continue
            updays = 0
            j = pos
            while j > 0 and bars[j]["c"] > bars[j - 1]["c"]:
                updays += 1
                j -= 1
            key = (snap_date, code)
            rets[key] = (close_px / open_px - 1) * 100.0
            poss[key] = ((cur / prev - 1) * 100.0, (cur / ma20 - 1) * 100.0, updays)
        if (idx + 1) % 500 == 0:
            print(f"  … {idx + 1}/{len(files)}", file=sys.stderr)

    # 覆盖率
    for snap_date, rows in snapshots:
        n = sum(1 for r in rows if (snap_date, r["code"]) in rets)
        covered[snap_date] = n

    # 分段定义
    buckets = [
        ("1-5名", lambda r: r["rank"] <= 5),
        ("6-20名", lambda r: 5 < r["rank"] <= 20),
        ("21-50名", lambda r: 20 < r["rank"] <= 50),
        ("51-100名", lambda r: 50 < r["rank"] <= 100),
        ("101-300名", lambda r: 100 < r["rank"] <= 300),
        ("301-1000名", lambda r: 300 < r["rank"] <= 1000),
        ("1001名以后", lambda r: r["rank"] > 1000),
    ]

    print()
    print(f"=== QuantMind 分数前瞻收益（次日开盘买入，{HOLD} 日收盘卖出，不含成本）===")
    print(f"{'分段':<10} {'笔数':>8} {'均值%':>9} {'胜率%':>8} {'中位%':>9}")
    stats = {}
    for name, pred in buckets:
        vals = []
        for snap_date, rows in snapshots:
            for r in rows:
                if not pred(r):
                    continue
                v = rets.get((snap_date, r["code"]))
                if v is not None:
                    vals.append(v)
        if not vals:
            continue
        vals_sorted = sorted(vals)
        med = vals_sorted[len(vals_sorted) // 2]
        win = sum(1 for v in vals if v > 0) / len(vals) * 100
        stats[name] = (len(vals), mean(vals), win)
        print(f"{name:<10} {len(vals):>8} {mean(vals):>+9.3f} {win:>8.1f} {med:>+9.3f}")

    # 全市场基准
    all_vals = list(rets.values())
    all_mean = mean(all_vals)
    all_win = sum(1 for v in all_vals if v > 0) / len(all_vals) * 100
    print(f"{'全市场基准':<10} {len(all_vals):>8} {all_mean:>+9.3f} {all_win:>8.1f}")

    # Top-N 每日组合视角（更接近实际建仓）
    print()
    print("=== 每日 Top-N 组合（等权，次日开盘买入）===")
    print(f"{'N':>6} {'天数':>6} {'日均收益%':>10} {'胜率%':>8} {'正收益天数占比%':>14}")
    for topn in (10, 30, 50, 100):
        day_means = []
        for snap_date, rows in snapshots:
            vals = [rets[(snap_date, r["code"])] for r in rows
                    if r["rank"] <= topn and (snap_date, r["code"]) in rets]
            if vals:
                day_means.append(mean(vals))
        if not day_means:
            continue
        pos_days = sum(1 for v in day_means if v > 0)
        print(f"{topn:>6} {len(day_means):>6} {mean(day_means):>+10.3f} "
              f"{'-':>8} {pos_days / len(day_means) * 100:>14.1f}")

    # 每日 Top30 明细（稳定性）
    print()
    print("=== 每日 Top30 明细 ===")
    for snap_date, rows in snapshots:
        vals = [rets[(snap_date, r["code"])] for r in rows
                if r["rank"] <= 30 and (snap_date, r["code"]) in rets]
        if not vals:
            print(f"{snap_date}  前瞻窗口未走完，跳过")
            continue
        print(f"{snap_date}  n={len(vals):>3}  均值 {mean(vals):+.3f}%  "
              f"最佳 {max(vals):+.2f}%  最差 {min(vals):+.2f}%")

    # 位置门禁对 Top100 的影响
    print()
    print("=== 位置门禁（>=6%涨幅 / >=12%MA20 / >=5连涨）对 Top100 的影响 ===")
    kept_vals, blocked_vals = [], []
    n_blocked = 0
    for snap_date, rows in snapshots:
        for r in rows:
            if r["rank"] > 100:
                continue
            key = (snap_date, r["code"])
            if key not in rets:
                continue
            chg, ma20gap, updays = poss[key]
            if chg >= GATE_MAX_CHANGE or ma20gap >= GATE_MAX_MA20 or updays >= GATE_MAX_UPDAYS:
                blocked_vals.append(rets[key])
                n_blocked += 1
            else:
                kept_vals.append(rets[key])
    if kept_vals:
        print(f"门禁放行 {len(kept_vals)} 笔：均值 {mean(kept_vals):+.3f}%  "
              f"胜率 {sum(1 for v in kept_vals if v > 0) / len(kept_vals) * 100:.1f}%")
    if blocked_vals:
        print(f"门禁拦截 {len(blocked_vals)} 笔：均值 {mean(blocked_vals):+.3f}%  "
              f"胜率 {sum(1 for v in blocked_vals if v > 0) / len(blocked_vals) * 100:.1f}%")

    # 覆盖率
    print()
    print("=== 每日快照覆盖率（快照票数 / 日线可算票数）===")
    for snap_date, rows in snapshots:
        n = covered[snap_date]
        print(f"{snap_date}  快照 {len(rows):>5}  可算前瞻 {n:>5}  ({n / len(rows) * 100:.0f}%)")

    print()
    print(f"注意：窗口仅 {len(snapshots)} 个快照日，最后 1-2 天前瞻窗口未走完；"
          f"结论只对 2026-09~10 这段行情负责。来回成本约 0.2~0.3%（佣金+印花税+滑点），"
          f"均值低于该水平不构成可执行信号。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
