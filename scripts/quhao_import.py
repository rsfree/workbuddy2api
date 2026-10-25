#!/usr/bin/env python3
"""scripts/quhao_import.py — quhao.lgtapi.shop 取号码 → 批量取号 → 校验 → 导入 wb2api 面板

    python3 scripts/quhao_import.py --codes "QH-XXXX-XXXX-XXXX-XXXX" \
        [--codes-file codes.txt] [--batch 10] [--dry-run] [--keep]

    --codes       取号码，可重复给多次；或用 --codes-file 每行一个
    --batch       每批几个（接口上限 10，默认 10）
    --panel       面板地址，默认 https://workbuddy2api.1task.cn
    --api-key     面板 key（或 env WB2API_API_KEY）
    --ssh-host    指纹比对目标，默认 root@154.40.45.18（--no-ssh 关闭）
    --keep        保留下载目录（默认导入后删）
    --dry-run     只取号+校验+写盘提示，不导入

站点契约（从前端 app.js 反出，实测）：
  POST /api/claim        {code, requestId(uuid)} → {ok, account, firstView, watermark}
  POST /api/claim/batch  {codes:[...]}           → {ok, data:{results:[{ok,code,account,error}], summary}}
  错误码：INVALID_CODE / SOLD_OUT / RATE_LIMITED / BAD_REQUEST

🔴 单条 /api/claim 固定 ~30s 才返回（超时也别换 requestId 重试，用同一个才幂等）；
   **批量接口快得多**：10 个/批实测 7.6s、18.5s，所以永远走 batch，别一条条取。

🔴 `account.expires_at` 单位不固定：10-23 单条返回**毫秒**（13 位），10-24/25 批量返回**秒**（10 位）。
   这里按量级判（>1e12 当毫秒），别写死。
🔴 `account` 顶层**没有 domain**（只有 auth_raw.domain），要补；顶层 `access_token` 才是新的，
   少数批次 auth_raw.accessToken 是上一代（已过期），只认顶层。
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import uuid
from datetime import datetime
from pathlib import Path

import requests

SITE = "https://quhao.lgtapi.shop"
UA = ("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36")
IMPORTER = Path.home() / ".workbuddy/skills/workbuddy2api-account-import/scripts/import_accounts.py"
MAX_BATCH = 10


def die(msg: str, code: int = 1):
    print(f"❌ {msg}", file=sys.stderr)
    sys.exit(code)


def jwt_claims(tok: str) -> dict:
    try:
        b = tok.split(".")[1]
        b += "=" * (-len(b) % 4)
        return json.loads(base64.urlsafe_b64decode(b))
    except Exception:
        return {}


def exp_seconds(v) -> int:
    """expires_at 秒/毫秒自适应（站点不同批次单位不一样）。"""
    try:
        n = int(v or 0)
    except (TypeError, ValueError):
        return 0
    return n // 1000 if n > 10**12 else n


def claim_batch(s: requests.Session, codes: list[str]) -> dict:
    r = s.post(f"{SITE}/api/claim/batch", json={"codes": codes}, timeout=300)
    if r.status_code >= 400:
        die(f"批量取号 http={r.status_code} {r.text[:160]}")
    j = r.json()
    if not j.get("ok") or not j.get("data"):
        die(f"批量取号失败：{json.dumps(j, ensure_ascii=False)[:200]}")
    return j["data"]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--codes", action="append", default=[])
    ap.add_argument("--codes-file")
    ap.add_argument("--batch", type=int, default=MAX_BATCH)
    ap.add_argument("--panel", default="https://workbuddy2api.1task.cn")
    ap.add_argument("--api-key", default=os.environ.get("WB2API_API_KEY", ""))
    ap.add_argument("--ssh-host", default="root@154.40.45.18")
    ap.add_argument("--no-ssh", action="store_true")
    ap.add_argument("--keep", action="store_true")
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args()

    codes = list(a.codes)
    if a.codes_file:
        codes += [l.strip() for l in open(a.codes_file) if l.strip()]
    codes = [c.strip() for c in codes if c.strip()]
    if not codes:
        die("没有取号码（--codes 或 --codes-file）")
    batch = max(1, min(a.batch, MAX_BATCH))
    print(f"取号码 {len(codes)} 个，每批 {batch} 个")

    s = requests.Session()
    s.trust_env = False
    s.headers.update({
        "Content-Type": "application/json",
        "Origin": SITE,
        "Referer": SITE + "/",
        "User-Agent": UA,
    })

    tmp = Path(tempfile.mkdtemp(prefix="quhao-acc-"))
    got, failed = [], []
    try:
        t_all = time.time()
        for i in range(0, len(codes), batch):
            chunk = codes[i:i + batch]
            t0 = time.time()
            data = claim_batch(s, chunk)
            dt = time.time() - t0
            summ = data.get("summary") or {}
            print(f"\n批次 {i // batch + 1}: {len(chunk)} 个 → 成功 {summ.get('success')} / "
                  f"失败 {summ.get('failed')}，{dt:.1f}s")
            for it in data.get("results") or []:
                if not it.get("ok") or not it.get("account"):
                    failed.append((it.get("code", ""), it.get("error", "")))
                    print(f"  ✗ {str(it.get('code',''))[:14]:14s} {it.get('error','') or '无 account'}")
                    continue
                acc = dict(it["account"])
                raw_dom = (acc.get("auth_raw") or {}).get("domain") or ""
                if not acc.get("domain"):
                    acc["domain"] = raw_dom or "www.codebuddy.cn"
                acc.setdefault("variant", "cn")
                tok = acc.get("access_token") or ""
                claims = jwt_claims(tok)
                exp_s = exp_seconds(acc.get("expires_at"))
                ok = claims.get("sub") == acc.get("uid") and exp_s > time.time()
                p = tmp / f"{acc.get('uid') or uuid.uuid4()}.json"
                json.dump(acc, open(p, "w"), ensure_ascii=False, indent=1)
                got.append(p)
                print(f"  {'✅' if ok else '⚠'} {str(acc.get('uid',''))[:8]:8s} "
                      f"{str(acc.get('nickname') or '?'):14s} exp="
                      f"{datetime.fromtimestamp(exp_s).strftime('%m-%d %H:%M') if exp_s else '—'} "
                      f"sub_match={claims.get('sub') == acc.get('uid')}")

        print(f"\n取号汇总：成功 {len(got)} / 失败 {len(failed)}，总耗时 {time.time() - t_all:.1f}s")
        if not got:
            die("一个号都没取到，无可导入")

        if a.dry_run:
            print(f"--dry-run：凭证已写 {tmp}，未导入")
            return

        if not IMPORTER.exists():
            die(f"找不到导入脚本 {IMPORTER}")
        cmd = [sys.executable, str(IMPORTER), *[str(p) for p in got],
               "--url", a.panel, "--concurrency", "10"]
        if not a.no_ssh and a.ssh_host:
            cmd += ["--ssh-host", a.ssh_host]
        env = dict(os.environ)
        for k in ("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"):
            env.pop(k, None)
        env["WB2API_API_KEY"] = a.api_key
        print("\n导入：")
        rc = subprocess.call(cmd, env=env)
        if rc != 0:
            print(f"⚠ 导入脚本退出码 {rc}", file=sys.stderr)
            sys.exit(rc)
    finally:
        if a.keep:
            print(f"\n凭证目录已保留：{tmp}")
        else:
            shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    main()
