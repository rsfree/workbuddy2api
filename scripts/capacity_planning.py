#!/usr/bin/env python3
"""Read-only WorkBuddy2API capacity planning from existing operational data.

No third-party packages and no upstream/model requests. The tool reads usage.json,
state.json, credit-history.json and request-logs/requests-*.jsonl, then writes a
machine-readable JSON report and a self-contained HTML report.

Example:
  python3 scripts/capacity_planning.py --data-dir ./data \
    --output-dir ./data/capacity-report --forecast-days 7 --max-in-flight 8
"""

from __future__ import annotations

import argparse
import collections
import datetime as dt
import glob
import html
import json
import math
import os
import statistics
from pathlib import Path


HOUR = dt.timedelta(hours=1)
DAY = dt.timedelta(days=1)


def parse_time(value):
    if not value:
        return None
    try:
        if isinstance(value, (int, float)):
            return dt.datetime.fromtimestamp(value, dt.timezone.utc)
        parsed = dt.datetime.fromisoformat(str(value).replace("Z", "+00:00"))
        if parsed.tzinfo is None:
            parsed = parsed.replace(tzinfo=dt.timezone.utc)
        return parsed
    except (ValueError, TypeError, OSError):
        return None


def load_json(path, default):
    try:
        with open(path, encoding="utf-8") as handle:
            return json.load(handle)
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        return default


def percentile(values, fraction):
    values = sorted(values)
    if not values:
        return 0.0
    if len(values) == 1:
        return float(values[0])
    position = fraction * (len(values) - 1)
    lower = int(math.floor(position))
    upper = int(math.ceil(position))
    if lower == upper:
        return float(values[lower])
    weight = position - lower
    return values[lower] * (1 - weight) + values[upper] * weight


def rounded(value, digits=2):
    return round(float(value), digits)


def usage_bucket_time(scope):
    if not scope:
        return None
    kind, _, stamp = scope.partition(":")
    fmt = "%Y-%m-%dT%H" if kind == "h" else "%Y-%m-%d"
    try:
        return dt.datetime.strptime(stamp, fmt).replace(tzinfo=dt.timezone.utc)
    except ValueError:
        return None


def empty_usage():
    return {"account_attempts": 0, "attempt_errors": 0, "credits": 0.0,
            "tokens": 0, "credit_samples": 0}


def sum_usage(buckets, since, until):
    """Hour buckets only: rolling forecasts must not double-count folded day buckets."""
    out = empty_usage()
    for bucket in buckets:
        scope = bucket.get("s", "")
        if not scope.startswith("h:"):
            continue
        # Usage bucket strings are local naive values. Convert their wall time to the
        # report timezone before comparing; offsets cancel when using the same zone.
        stamp = parse_time(scope[2:] + ":00+00:00")
        if stamp is None:
            continue
        bucket_wall = stamp.replace(tzinfo=None)
        # usage.json scopes are wall-clock strings in the server's local timezone.
        # Compare them with the report cutoff's wall time, not a UTC conversion.
        since_naive = since.replace(tzinfo=None)
        until_naive = until.replace(tzinfo=None)
        if not (since_naive < bucket_wall <= until_naive):
            continue
        out["account_attempts"] += int(bucket.get("q", 0))
        out["attempt_errors"] += int(bucket.get("e", 0))
        out["credits"] += float(bucket.get("cr", 0))
        out["tokens"] += int(bucket.get("t", 0))
        out["credit_samples"] += int(bucket.get("cn", 0))
    return out


def normalize_usage_window(window):
    return {
        "account_attempts": window["account_attempts"],
        "attempt_errors": window["attempt_errors"],
        "credits": rounded(window["credits"]),
        "tokens": window["tokens"],
        "credit_samples": window["credit_samples"],
    }


def load_events(data_dir):
    events = []
    paths = sorted(set(glob.glob(str(data_dir / "request-logs" / "requests-*.jsonl")) +
                       glob.glob(str(data_dir / "requests-*.jsonl"))))
    for path in paths:
        try:
            with open(path, encoding="utf-8") as handle:
                for line in handle:
                    try:
                        event = json.loads(line)
                    except json.JSONDecodeError:
                        continue
                    if parse_time(event.get("time")) is not None:
                        events.append(event)
        except OSError:
            continue
    return events, paths


def traffic_summary(events, since, now):
    selected = []
    for event in events:
        stamp = parse_time(event.get("time"))
        if stamp is not None and since <= stamp <= now:
            selected.append((stamp, event))
    statuses = collections.Counter()
    outcomes = collections.Counter()
    models = {}
    durations = []
    work_5m = collections.Counter()
    attempts = 0
    credits = 0.0
    total_duration_ms = 0
    for stamp, event in selected:
        model = event.get("model") or "(unknown)"
        row = models.setdefault(model, {"requests": 0, "successes": 0, "credits": 0.0,
                                        "statuses": collections.Counter()})
        status = int(event.get("status", 0) or 0)
        ok = bool(event.get("ok"))
        duration = max(0, int(event.get("duration_ms", 0) or 0))
        row["requests"] += 1
        row["successes"] += int(ok)
        row["credits"] += float(event.get("credit", 0) or 0)
        row["statuses"][status] += 1
        statuses[status] += 1
        outcomes[event.get("outcome") or "unknown"] += 1
        attempts += max(1, int(event.get("attempts", 1) or 1))
        credits += float(event.get("credit", 0) or 0)
        durations.append(duration)
        total_duration_ms += duration
        bucket = int(stamp.timestamp() // 300 * 300)
        work_5m[bucket] += duration
    count = len(selected)
    window_seconds = max(1, (now - since).total_seconds())
    model_rows = []
    for name, row in models.items():
        requests = row["requests"]
        status_counts = {str(k): v for k, v in sorted(row["statuses"].items())}
        model_rows.append({
            "model": name,
            "requests": requests,
            "success_rate": rounded(100 * row["successes"] / requests),
            "credits": rounded(row["credits"]),
            "statuses": status_counts,
            "http_400": row["statuses"].get(400, 0),
            "http_429": row["statuses"].get(429, 0),
            "http_503": row["statuses"].get(503, 0),
        })
    model_rows.sort(key=lambda row: (-row["requests"], row["model"]))
    peak_estimate = max((work / 300000 for work in work_5m.values()), default=0)
    return {
        "requests": count,
        "success_rate": rounded(100 * sum(bool(e.get("ok")) for _, e in selected) / count) if count else 0,
        "statuses": {str(k): v for k, v in sorted(statuses.items())},
        "outcomes": dict(sorted(outcomes.items())),
        "attempts_per_request": rounded(attempts / count, 4) if count else 0,
        "credits": rounded(credits),
        "p95_duration_ms": rounded(percentile(durations, 0.95), 0),
        "avg_in_flight_estimate": rounded(total_duration_ms / 1000 / window_seconds),
        "peak_5m_in_flight_estimate": rounded(peak_estimate),
        "models": model_rows,
    }


def account_state(accounts, now, max_in_flight):
    enabled = []
    for account in accounts:
        if account.get("disabled") or account.get("paused"):
            continue
        enabled.append(account)
    positive = [a for a in enabled if int(a.get("credits", 0) or 0) > 0]
    cooling = 0
    available = 0
    for account in positive:
        until = parse_time(account.get("until"))
        is_cooling = bool(account.get("cooling")) or bool(until and until > now)
        cooling += int(is_cooling)
        available += int(not is_cooling)
    remaining = sum(max(0, int(a.get("credits", 0) or 0)) for a in enabled)
    totals = [int(a.get("credits_total", 0) or 0) for a in enabled
              if int(a.get("credits_total", 0) or 0) > 0]
    unit = percentile(totals, 0.25)
    expiry = collections.Counter()
    for account in enabled:
        for bucket in account.get("credits_expiry_buckets", []) or []:
            expires = parse_time(bucket.get("expires_at"))
            remain = max(0, int(bucket.get("remain", 0) or 0))
            if expires is None or expires <= now:
                continue
            for days in (1, 3, 7, 30):
                if expires <= now + days * DAY:
                    expiry[str(days)] += remain
    return {
        "accounts_total": len(accounts),
        "accounts_enabled": len(enabled),
        "accounts_credit_positive": len(positive),
        "accounts_cooling_with_credit": cooling,
        "accounts_available_estimate": available,
        "credits_remaining": remaining,
        "credits_total_observed": sum(totals),
        "new_account_credit_unit_p25": rounded(unit, 0),
        "new_account_credit_unit_median": rounded(statistics.median(totals) if totals else 0, 0),
        "credits_expiring": {key: expiry[key] for key in ("1", "3", "7", "30")},
        "max_in_flight_per_account": max_in_flight,
        "in_flight_pool_limit_estimate": available * max_in_flight if max_in_flight > 0 else None,
    }


def credit_replenishment(entries, since, now):
    observed = sum(int(e.get("delta", 0) or 0) for e in entries
                   if int(e.get("delta", 0) or 0) > 0
                   and (parse_time(e.get("time")) is not None)
                   and since <= parse_time(e.get("time")) <= now)
    times = [parse_time(e.get("time")) for e in entries]
    times = [stamp for stamp in times if stamp is not None]
    coverage = min((now - min(times)).total_seconds() / 3600, 24) if times else 0
    return observed, coverage


def scenario(days, daily_burn, remaining, replenishment, unit, expiry, headroom=0.20):
    gross = daily_burn * days * (1 + headroom)
    usable = max(0, remaining - expiry)
    natural = replenishment * days
    shortfall = max(0, gross - usable - natural)
    return {
        "days": days,
        "credits_required_with_20pct_headroom": rounded(gross),
        "credits_usable_after_expiry": rounded(usable),
        "credits_replenishment_assumed": rounded(natural),
        "credits_shortfall": rounded(shortfall),
        "additional_accounts": int(math.ceil(shortfall / unit)) if unit > 0 else None,
    }


def recommendation(priority, category, title, action, evidence):
    return {"priority": priority, "category": category, "title": title,
            "action": action, "evidence": evidence}


def analyze(data_dir, forecast_days=7, recent_hours=24, max_in_flight=None, now=None, config_path=None):
    usage = load_json(data_dir / "usage.json", {})
    state = load_json(data_dir / "state.json", {})
    ledger = load_json(data_dir / "credit-history.json", {})
    runtime_config = load_json(Path(config_path), {}) if config_path else {}
    pool_config = runtime_config.get("pool", {}) or {}
    if max_in_flight is None:
        max_in_flight = int(pool_config.get("max_in_flight", 8) or 8)
    hidden_models = (runtime_config.get("models", {}) or {}).get("hidden", []) or []
    buckets = usage.get("buckets", []) or []
    accounts_raw = state.get("accounts", []) or []
    accounts = list(accounts_raw.values()) if isinstance(accounts_raw, dict) else accounts_raw
    entries = ledger.get("entries", []) or []
    events, request_paths = load_events(data_dir)

    timestamps = [parse_time(usage.get("saved")), parse_time(ledger.get("saved"))]
    timestamps += [parse_time(e.get("time")) for e in events]
    timestamps = [stamp for stamp in timestamps if stamp is not None]
    if now is None:
        now = max(timestamps) if timestamps else dt.datetime.now(dt.timezone.utc)
    now = parse_time(now) if not isinstance(now, dt.datetime) else now
    if now.tzinfo is None:
        now = now.replace(tzinfo=dt.timezone.utc)

    last = sum_usage(buckets, now - DAY, now)
    previous = sum_usage(buckets, now - 2 * DAY, now - DAY)
    week = sum_usage(buckets, now - 7 * DAY, now)
    # A recency-weighted trend is the lower bound; the latest complete rolling day is
    # the capacity-planning baseline. This avoids extrapolating a short-lived spike
    # while refusing to under-provision during rapid growth.
    weighted_burn = 0.7 * last["credits"] + 0.3 * previous["credits"]
    daily_burn = max(last["credits"], weighted_burn)
    daily_requests = max(last["account_attempts"],
                         0.7 * last["account_attempts"] + 0.3 * previous["account_attempts"])
    replenishment, credit_coverage = credit_replenishment(entries, now - DAY, now)
    assumed_replenishment = replenishment * 0.5
    pool = account_state(accounts, now, max_in_flight)
    unit = pool["new_account_credit_unit_p25"]
    traffic = traffic_summary(events, now - recent_hours * HOUR, now)
    traffic_6h = traffic_summary(events, now - 6 * HOUR, now)
    health_6h = {row["model"]: row for row in traffic_6h["models"]}
    for row in traffic["models"]:
        recent = health_6h.get(row["model"])
        row["recent_6h_requests"] = recent["requests"] if recent else 0
        row["recent_6h_success_rate"] = recent["success_rate"] if recent else None

    expiry_by_days = pool["credits_expiring"]
    horizons = {}
    for days in sorted(set((1, 3, forecast_days))):
        expiry = expiry_by_days.get(str(days), expiry_by_days["30"] if days > 7 else 0)
        horizons[str(days)] = {
            "planned_50pct_replenishment": scenario(days, daily_burn, pool["credits_remaining"],
                                                    assumed_replenishment, unit, expiry),
            "stable_replenishment": scenario(days, daily_burn, pool["credits_remaining"],
                                             replenishment, unit, expiry),
            "no_replenishment": scenario(days, daily_burn, pool["credits_remaining"],
                                         0, unit, expiry),
        }
    net_burn = max(0.01, daily_burn - assumed_replenishment)
    runway = pool["credits_remaining"] / net_burn

    event_times = [parse_time(e.get("time")) for e in events]
    event_times = [stamp for stamp in event_times if stamp is not None]
    request_coverage = (now - min(event_times)).total_seconds() / 3600 if event_times else 0
    request_bytes = sum(os.path.getsize(path) for path in request_paths if os.path.exists(path))
    coverage_days = max(request_coverage / 24, 1 / 24)
    archive_mb_30d = max(100, int(math.ceil(request_bytes / coverage_days * 30 * 1.3 / (1 << 20) / 100) * 100))

    recs = []
    main = horizons[str(forecast_days)]["planned_50pct_replenishment"]
    low = horizons[str(forecast_days)]["stable_replenishment"]["additional_accounts"]
    high = horizons[str(forecast_days)]["no_replenishment"]["additional_accounts"]
    if main["additional_accounts"] is None:
        recs.append(recommendation("P1", "data", "缺少单号额度基线",
                                   "先刷新账号积分总额度；无法可靠换算补号数。",
                                   "credits_total 没有有效样本。"))
    elif main["additional_accounts"] > 0:
        recs.append(recommendation(
            "P0" if runway < 3 else "P1", "accounts",
            f"未来 {forecast_days} 天计划补 {main['additional_accounts']} 个号",
            f"分批补充并观察实际额度；补给稳定时约 {low} 个，完全不补给的保守上界约 {high} 个。",
            f"日耗 {rounded(daily_burn)} credits，现有 {pool['credits_remaining']}，"
            f"单号保守额度 {rounded(unit)}，预计裸续航 {rounded(runway, 1)} 天。"))
    else:
        recs.append(recommendation("P2", "accounts", "当前窗口无需为积分补号",
                                   "保持每日复核；需求或模型结构变化后重跑。",
                                   f"{forecast_days} 天计划缺口为 0，裸续航约 {rounded(runway, 1)} 天。"))

    statuses = traffic["statuses"]
    requests = traffic["requests"]
    rate_429 = 100 * statuses.get("429", 0) / requests if requests else 0
    limit = pool["in_flight_pool_limit_estimate"]
    peak = traffic["peak_5m_in_flight_estimate"]
    if requests and rate_429 < 1 and (not limit or peak < 0.7 * limit):
        recs.append(recommendation(
            "P2", "concurrency", "暂不提高单号并发",
            f"max_in_flight 保持 {max_in_flight}；先解决积分和具体模型失败，不用加并发掩盖问题。",
            f"近 {recent_hours}h 429 占比 {rounded(rate_429)}%，5 分钟并发估算峰值 {peak}，"
            f"池上限估算 {limit if limit is not None else '未知'}。"))
    elif rate_429 >= 1:
        recs.append(recommendation(
            "P1", "concurrency", "限流仍明显，优先降并发而非加号",
            f"把 max_in_flight 从 {max_in_flight} 试降到 {max(1, int(max_in_flight * 0.75))}，"
            "观察 429、软冷却和成功率后再回调。",
            f"近 {recent_hours}h 429 占比 {rounded(rate_429)}%。"))

    def is_hidden(model):
        for hidden in hidden_models:
            if model == hidden:
                return True
            if ":" not in hidden and model in ("cn:" + hidden, "global:" + hidden):
                return True
        return False

    for row in traffic["models"]:
        if is_hidden(row["model"]):
            recs.append(recommendation(
                "P2", "model", f"模型 {row['model']} 已按配置隐藏",
                "相关 400 属于预期的网关拒绝，不按模型故障处理，也不需要补号或改超时。",
                f"models.hidden 包含该模型；近 {recent_hours}h 仍有 {row['requests']} 次点名调用，"
                "应从客户端模型列表移除。"))
            continue
        recent_requests = row["recent_6h_requests"]
        recent_success = row["recent_6h_success_rate"]
        if row["requests"] >= 20 and (row["success_rate"] < 90 or
                                      (recent_requests >= 20 and recent_success is not None and recent_success < 90)):
            recovered = recent_requests >= 20 and recent_success is not None and recent_success >= 97
            if recovered:
                recs.append(recommendation(
                    "P2", "model", f"模型 {row['model']} 此前异常，近 6h 已恢复",
                    "暂不改路由或隐藏模型；继续观察 24h 成功率，避免用旧故障做过期调参。",
                    f"近 {recent_hours}h 成功率 {row['success_rate']}%，但近 6h "
                    f"{recent_requests} 次、成功率 {recent_success}%。"))
                continue
            if row["http_400"] > 0 and row["http_400"] >= max(row["http_429"], row["http_503"]):
                action = "优先检查该模型的参数兼容性/目录映射；400 不是补号能解决的问题。可临时将客户端切到同能力健康模型。"
            elif row["http_429"] + row["http_503"] > 0:
                action = "优先检查该模型对应域的账号健康与路由；显式 cn:/global: 请求不会跨域回退，先对照健康域再调整。"
            else:
                action = "失败主要来自中断或流式响应；优先核对客户端超时、SSE 空闲超时与上游稳定性，不要用补号掩盖。"
            recs.append(recommendation(
                "P1", "model", f"模型 {row['model']} 成功率异常",
                action,
                f"近 {recent_hours}h {row['requests']} 次、成功率 {row['success_rate']}%；"
                f"近 6h {recent_requests} 次、成功率 {recent_success}%；"
                f"400={row['http_400']}，429={row['http_429']}，503={row['http_503']}。"))

    recs.append(recommendation(
        "P2", "observability", "延长请求归档以提高下次预测可信度",
        f"建议 request_retention_days=30、request_archive_max_mb={archive_mb_30d}；"
        "usage 长期桶继续保留，请求归档用于成功率和模型健康复核。",
        f"当前请求归档覆盖约 {rounded(request_coverage / 24, 1)} 天、{len(events)} 条，"
        f"积分流水覆盖约 {rounded(credit_coverage, 1)}h。"))

    priority_order = {"P0": 0, "P1": 1, "P2": 2}
    recs.sort(key=lambda row: (priority_order[row["priority"]], row["category"]))
    confidence = "high" if request_coverage >= 24 * 7 and credit_coverage >= 24 else (
        "medium" if request_coverage >= 24 and credit_coverage >= 12 else "low")
    return {
        "generated_at": now.isoformat(),
        "data_dir": str(data_dir),
        "method": {
            "daily_burn": "max(last_24h, 0.7*last_24h + 0.3*previous_24h)",
            "planned_replenishment": "50% of positive credit deltas observed in the last 24h",
            "headroom": 0.20,
            "account_unit": "p25 of positive credits_total across enabled accounts",
            "concurrency": "5-minute completed-request work / 300s; estimate, not exact lease occupancy",
            "usage_semantics": "usage account attempts; request archive logical client requests",
        },
        "confidence": confidence,
        "runtime_policy": {"hidden_models": hidden_models},
        "usage": {
            "last_24h": normalize_usage_window(last),
            "previous_24h": normalize_usage_window(previous),
            "last_7d": normalize_usage_window(week),
            "daily_credits_forecast": rounded(daily_burn),
            "daily_account_attempts_forecast": rounded(daily_requests, 0),
            "credit_growth_vs_previous_24h_pct": rounded((last["credits"] / previous["credits"] - 1) * 100)
            if previous["credits"] > 0 else None,
            "positive_replenishment_observed_24h": replenishment,
            "positive_replenishment_assumed_per_day": rounded(assumed_replenishment),
        },
        "pool": pool,
        "traffic": traffic,
        "forecast": {
            "runway_days_without_headroom": rounded(runway, 2),
            "horizons": horizons,
        },
        "recommendations": recs,
        "coverage": {
            "request_events": len(events),
            "request_hours": rounded(request_coverage),
            "request_bytes": request_bytes,
            "credit_history_hours": rounded(credit_coverage),
            "credit_history_entries": len(entries),
            "usage_buckets": len(buckets),
        },
    }


def fmt(value, digits=0):
    if value is None:
        return "—"
    if isinstance(value, float):
        return f"{value:,.{digits}f}"
    return f"{value:,}" if isinstance(value, int) else str(value)


def render_html(report):
    usage = report["usage"]
    pool = report["pool"]
    traffic = report["traffic"]
    forecast = report["forecast"]
    horizons = forecast["horizons"]
    forecast_days = max(int(key) for key in horizons)
    main = horizons[str(forecast_days)]["planned_50pct_replenishment"]
    stable = horizons[str(forecast_days)]["stable_replenishment"]
    none = horizons[str(forecast_days)]["no_replenishment"]
    title = f"未来 {forecast_days} 天建议补 {main['additional_accounts']} 个号" if main["additional_accounts"] is not None else "容量分析报告"
    cards = [
        ("补号建议", fmt(main["additional_accounts"]), f"稳定补给 {fmt(stable['additional_accounts'])} · 无补给 {fmt(none['additional_accounts'])}"),
        ("预计日耗", fmt(usage["daily_credits_forecast"]), "credits / day"),
        ("当前可用余额", fmt(pool["credits_remaining"]), f"预计续航 {fmt(forecast['runway_days_without_headroom'], 1)} 天"),
        ("近期成功率", fmt(traffic["success_rate"], 2) + "%", f"{fmt(traffic['requests'])} 个逻辑请求"),
        ("账号可服务", fmt(pool["accounts_available_estimate"]), f"总账号 {pool['accounts_total']} · 有余额 {pool['accounts_credit_positive']}"),
        ("并发峰值估算", fmt(traffic["peak_5m_in_flight_estimate"]), f"池上限估算 {fmt(pool['in_flight_pool_limit_estimate'])}"),
    ]
    card_html = "".join(f'<div class="card"><div class="label">{html.escape(label)}</div><div class="value">{html.escape(str(value))}</div><div class="sub">{html.escape(str(sub))}</div></div>' for label, value, sub in cards)
    rec_html = "".join(
        f'<article class="rec {row["priority"].lower()}"><div><span class="tag">{row["priority"]}</span>'
        f'<strong>{html.escape(row["title"])}</strong></div><p>{html.escape(row["action"])}</p>'
        f'<small>{html.escape(row["evidence"])}</small></article>' for row in report["recommendations"])
    horizon_rows = []
    for days in sorted(horizons, key=int):
        for name, label in (("planned_50pct_replenishment", "计划（50% 补给）"),
                            ("stable_replenishment", "补给稳定"),
                            ("no_replenishment", "不计补给")):
            row = horizons[days][name]
            horizon_rows.append(f"<tr><td>{days} 天</td><td>{label}</td><td>{fmt(row['credits_shortfall'])}</td><td>{fmt(row['additional_accounts'])}</td></tr>")
    model_rows = "".join(
        f"<tr><td>{html.escape(row['model'])}</td><td>{fmt(row['requests'])}</td>"
        f"<td>{fmt(row['success_rate'], 1)}%</td><td>{fmt(row['recent_6h_success_rate'], 1) if row['recent_6h_success_rate'] is not None else '—'}</td>"
        f"<td>{fmt(row['credits'])}</td><td>{row['http_400']} / {row['http_429']} / {row['http_503']}</td></tr>"
        for row in traffic["models"][:20])
    return f"""<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>WorkBuddy2API 容量规划</title><style>
:root{{color-scheme:dark;--bg:#0d1117;--panel:#161b22;--line:#30363d;--text:#e6edf3;--muted:#9da7b3;--blue:#58a6ff;--red:#ff7b72;--amber:#d29922;--green:#3fb950}}
*{{box-sizing:border-box}}body{{margin:0;background:var(--bg);color:var(--text);font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC",sans-serif}}main{{max-width:1240px;margin:auto;padding:32px 20px 60px}}h1{{font-size:28px;margin:0 0 4px}}h2{{margin-top:32px;font-size:19px}}.muted,.sub,small{{color:var(--muted)}}.hero{{margin:16px 0 24px;color:var(--blue);font-size:20px;font-weight:650}}.grid{{display:grid;grid-template-columns:repeat(auto-fit,minmax(175px,1fr));gap:12px}}.card,.rec{{background:var(--panel);border:1px solid var(--line);border-radius:12px;padding:16px}}.label{{color:var(--muted)}}.value{{font-size:27px;font-weight:700;margin:4px 0}}.rec{{margin:10px 0}}.rec p{{margin:8px 0 4px}}.rec.p0{{border-left:4px solid var(--red)}}.rec.p1{{border-left:4px solid var(--amber)}}.rec.p2{{border-left:4px solid var(--green)}}.tag{{display:inline-block;font-size:11px;border:1px solid var(--line);border-radius:5px;padding:0 5px;margin-right:8px}}table{{width:100%;border-collapse:collapse;background:var(--panel);border-radius:10px;overflow:hidden}}th,td{{text-align:right;padding:9px 12px;border-bottom:1px solid var(--line)}}th:first-child,td:first-child{{text-align:left}}th{{color:var(--muted);font-weight:500}}.method{{white-space:pre-wrap;color:var(--muted)}}@media(max-width:650px){{main{{padding:20px 12px}}.table{{overflow-x:auto}}table{{min-width:720px}}}}
</style></head><body><main>
<h1>WorkBuddy2API 容量规划</h1><div class="muted">数据截止 {html.escape(report['generated_at'])} · 预测可信度 {html.escape(report['confidence'])} · 只读统计，无 AI/上游调用</div>
<div class="hero">{html.escape(title)}</div><section class="grid">{card_html}</section>
<h2>行动建议</h2>{rec_html}
<h2>补号情景测算</h2><div class="table"><table><thead><tr><th>窗口</th><th>情景</th><th>积分缺口（含 20% 余量）</th><th>需补账号</th></tr></thead><tbody>{''.join(horizon_rows)}</tbody></table></div>
<h2>模型健康</h2><div class="table"><table><thead><tr><th>模型</th><th>请求</th><th>成功率</th><th>近 6h 成功率</th><th>Credits</th><th>400 / 429 / 503</th></tr></thead><tbody>{model_rows}</tbody></table></div>
<h2>口径与限制</h2><div class="method">日耗：{html.escape(report['method']['daily_burn'])}
计划补给：最近 24h 正向积分变动的 50%；单号额度：有效账号 credits_total 的 P25。
用量 q/e 是账号尝试口径；请求归档才是客户端逻辑请求与严格成功率。并发为基于已完成请求时长的 5 分钟估算，不等于实时租约峰值。
历史较短时不要把点估计当承诺，应每天重跑并按实际新号额度修正。</div>
</main></body></html>"""


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data-dir", type=Path, required=True,
                        help="Directory containing usage.json/state.json and request-logs/")
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--forecast-days", type=int, default=7, choices=range(1, 31))
    parser.add_argument("--recent-hours", type=int, default=24, choices=(6, 12, 24, 48, 72, 168))
    parser.add_argument("--max-in-flight", type=int,
                        help="Override pool.max_in_flight; otherwise read --config or use 8")
    parser.add_argument("--config", type=Path,
                        help="Optional config.json; reads only pool limits and models.hidden")
    parser.add_argument("--now", help="Optional RFC3339 data cutoff, mainly for reproducible runs/tests")
    args = parser.parse_args()
    report = analyze(args.data_dir, args.forecast_days, args.recent_hours,
                     args.max_in_flight, args.now, args.config)
    args.output_dir.mkdir(parents=True, exist_ok=True)
    json_path = args.output_dir / "capacity-report.json"
    html_path = args.output_dir / "capacity-report.html"
    with open(json_path, "w", encoding="utf-8") as handle:
        json.dump(report, handle, ensure_ascii=False, indent=2)
        handle.write("\n")
    with open(html_path, "w", encoding="utf-8") as handle:
        handle.write(render_html(report))
    print(json.dumps({
        "json": str(json_path), "html": str(html_path),
        "confidence": report["confidence"],
        "daily_credits_forecast": report["usage"]["daily_credits_forecast"],
        "recommendations": report["recommendations"],
    }, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
