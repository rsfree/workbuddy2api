#!/usr/bin/env python3
"""scripts/quark_import.py — 夸克网盘分享 → 下载账号 JSON → 校验 → 导入 wb2api 面板

一条命令（匿名，无需夸克账号）：

    python3 scripts/quark_import.py --url "https://pan.quark.cn/s/86bb4702ee09?pwd=4l4t"

    # 常用开关
    --url        分享链接（提取码从 ?pwd= 自动取，也可 --pwd 单独给）
    --panel      面板地址，默认 https://workbuddy2api.1task.cn
    --api-key    面板 key（或 env WB2API_API_KEY）
    --ssh-host   指纹比对用的 ssh 目标，默认 root@154.40.45.18（--no-ssh 关闭）
    --only-cockpit 只导入 cockpit-*.json（默认行为；没有则回退 workbuddy-*.json）
    --keep       保留下载目录（默认导入后删掉含 token 明文的临时文件）
    --dry-run    只下载+校验+打印计划，不导入

为什么不用 curl：本机 curl 解析 pan.quark.cn 会 DNS 超时（`Resolving timed out`），
而 dig / python getaddrinfo 0.04s 正常 —— 这是 curl 自己的解析路径问题。
requests 还要 trust_env=False，否则会被会话里的 HTTP_PROXY 代理走。

匿名分享链路（实测）：
  分享页拿 cookie(ctoken/web-grey-id) → POST /share/sharepage/token {pwd_id,passcode} 取 stoken
  → GET /share/sharepage/detail?pwd_id&stoken&pdir_fid=0&_size=200 列表
  → POST /file/download {fids,pwd_id,stoken} 取直链 → GET
注意 `share/sharepage/download` 是 404，`share/sharepage/save` 匿名 401。
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import re
import subprocess
import sys
import tempfile
import time
from datetime import datetime
from pathlib import Path

import requests

UA = ("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36")
DRIVE = "https://drive-pc.quark.cn/1/clouddrive"
IMPORTER = Path.home() / ".workbuddy/skills/workbuddy2api-account-import/scripts/import_accounts.py"


def die(msg: str, code: int = 1):
    print(f"❌ {msg}", file=sys.stderr)
    sys.exit(code)


def parse_share(url: str, pwd: str | None) -> tuple[str, str]:
    m = re.search(r"/s/([0-9a-zA-Z]+)", url or "")
    if not m:
        die(f"链接里找不到 /s/<pwd_id>：{url}")
    pwd_id = m.group(1)
    if not pwd:
        pm = re.search(r"[?&]pwd=([0-9a-zA-Z]+)", url or "")
        pwd = pm.group(1) if pm else ""
    if not pwd:
        die("缺少提取码（--pwd 或链接里的 ?pwd=）")
    return pwd_id, pwd


def pick_credentials(files: list[dict]) -> list[dict]:
    """cockpit 优先；一个 cockpit 都没有才回退 workbuddy。"""
    cockpit = [f for f in files if f["file_name"].startswith("cockpit-") and f["file_name"].endswith(".json")]
    if cockpit:
        return cockpit
    return [f for f in files if f["file_name"].startswith("workbuddy-") and f["file_name"].endswith(".json")]


def session_with_cookie(pwd_id: str) -> requests.Session:
    s = requests.Session()
    s.trust_env = False  # 会话代理会把国内域名带偏
    s.headers.update({
        "User-Agent": UA,
        "Accept": "application/json, text/plain, */*",
        "Referer": "https://pan.quark.cn/",
        "Origin": "https://pan.quark.cn",
    })
    r = s.get(f"https://pan.quark.cn/s/{pwd_id}", timeout=25)
    if r.status_code >= 400:
        die(f"分享页打不开 http={r.status_code}")
    return s


def list_files(s: requests.Session, pwd_id: str, pwd: str) -> tuple[str, list[dict]]:
    r = s.post(f"{DRIVE}/share/sharepage/token?pr=ucpro&fr=pc",
               json={"pwd_id": pwd_id, "passcode": pwd}, timeout=25)
    j = r.json()
    if j.get("status") != 200 or not j.get("data", {}).get("stoken"):
        die(f"取 stoken 失败：{json.dumps(j, ensure_ascii=False)[:200]}")
    stoken = j["data"]["stoken"]

    r = s.get(f"{DRIVE}/share/sharepage/detail", params={
        "pr": "ucpro", "fr": "pc", "pwd_id": pwd_id, "stoken": stoken,
        "pdir_fid": "0", "force": "0", "_page": "1", "_size": "200",
        "_fetch_banner": "1", "_fetch_share": "1", "_fetch_total": "1",
        "_sort": "file_type:asc,updated_at:desc",
    }, timeout=25)
    j = r.json()
    if j.get("status") != 200:
        die(f"列目录失败：{json.dumps(j, ensure_ascii=False)[:200]}")
    return stoken, j["data"]["list"]


def download(s: requests.Session, pwd_id: str, stoken: str, files: list[dict], outdir: Path) -> list[Path]:
    fids = [f["fid"] for f in files]
    r = s.post(f"{DRIVE}/file/download?pr=ucpro&fr=pc",
               json={"fids": fids, "pwd_id": pwd_id, "stoken": stoken}, timeout=30)
    j = r.json()
    if j.get("status") != 200:
        die(f"取直链失败：{json.dumps(j, ensure_ascii=False)[:200]}")
    urls = {d["fid"]: d["download_url"] for d in j["data"]}

    out: list[Path] = []
    for f in files:
        u = urls.get(f["fid"])
        if not u:
            print(f"⚠ 无直链，跳过 {f['file_name']}")
            continue
        rr = s.get(u, timeout=120)
        if rr.status_code != 200 or not rr.content:
            print(f"⚠ 下载失败 {f['file_name']} http={rr.status_code}")
            continue
        p = outdir / f["file_name"]
        p.write_bytes(rr.content)
        out.append(p)
        print(f"  ↓ {f['file_name']}  {len(rr.content)} B")
    return out


def jwt_claims(tok: str) -> dict:
    try:
        b = tok.split(".")[1]
        b += "=" * (-len(b) % 4)
        return json.loads(base64.urlsafe_b64decode(b))
    except Exception:
        return {}


def inspect(paths: list[Path]) -> list[dict]:
    """解出每个文件的账号形态，供导入与人工核对。"""
    rows = []
    for p in paths:
        try:
            d = json.load(open(p))
        except Exception as e:
            print(f"⚠ {p.name} 不是合法 JSON：{e}")
            continue
        items = d if isinstance(d, list) else [d]
        for a in items:
            if not isinstance(a, dict):
                continue
            nested = isinstance(a.get("auth"), dict)
            acc = a.get("account") if nested else a
            auth = a["auth"] if nested else a
            tok = auth.get("accessToken") or auth.get("access_token") or ""
            exp_raw = auth.get("expiresAt") or auth.get("expires_at") or 0
            claims = jwt_claims(tok)
            # 展示用：嵌套形 expiresAt 是秒，cockpit 是毫秒
            exp_sec = exp_raw / 1000 if exp_raw > 10**12 else exp_raw
            rows.append({
                "file": p.name,
                "shape": "nested" if nested else ("array" if isinstance(d, list) else "flat"),
                "uid": (acc or {}).get("uid", ""),
                "nickname": (acc or {}).get("nickname", ""),
                "sub_ok": bool(claims.get("sub")) and claims.get("sub") == (acc or {}).get("uid"),
                "exp_text": datetime.fromtimestamp(exp_sec).strftime("%Y-%m-%d %H:%M") if exp_sec else "—",
                "expired": bool(exp_sec) and exp_sec < time.time(),
            })
    return rows


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--pwd")
    ap.add_argument("--panel", default="https://workbuddy2api.1task.cn")
    ap.add_argument("--api-key", default=os.environ.get("WB2API_API_KEY", ""))
    ap.add_argument("--ssh-host", default="root@154.40.45.18")
    ap.add_argument("--no-ssh", action="store_true")
    ap.add_argument("--keep", action="store_true")
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args()

    pwd_id, pwd = parse_share(a.url, a.pwd)
    print(f"分享 {pwd_id}  提取码 {pwd}")

    s = session_with_cookie(pwd_id)
    stoken, files = list_files(s, pwd_id, pwd)
    print(f"目录 {len(files)} 项：")
    for f in files:
        print(f"  - {f['file_name']}  {f.get('size', 0)} B")

    targets = pick_credentials(files)
    if not targets:
        die("目录里没有 cockpit-*.json 或 workbuddy-*.json")
    print(f"取 {len(targets)} 份凭证（{'cockpit' if targets[0]['file_name'].startswith('cockpit-') else 'workbuddy'} 优先）")

    tmp = Path(tempfile.mkdtemp(prefix="quark-acc-"))
    try:
        got = download(s, pwd_id, stoken, targets, tmp)
        if not got:
            die("没下载到任何文件")
        rows = inspect(got)
        print("\n校验：")
        bad = 0
        for r in rows:
            flag = "✅" if (r["sub_ok"] and not r["expired"]) else "⚠"
            if not (r["sub_ok"] and not r["expired"]):
                bad += 1
            print(f"  {flag} {r['file']}  {r['shape']:6s} uid={r['uid'][:8] or '?':8s} "
                  f"nick={r['nickname'] or '?':12s} exp={r['exp_text']} sub_match={r['sub_ok']}")
        if bad:
            print(f"⚠ {bad} 份凭证 uid/token 不一致或已过期，仍交给导入脚本判定")

        if a.dry_run:
            print("\n--dry-run：只下载校验，未导入")
            return

        if not IMPORTER.exists():
            die(f"找不到导入脚本 {IMPORTER}")
        cmd = [sys.executable, str(IMPORTER), *[str(p) for p in got],
               "--url", a.panel, "--concurrency", "10"]
        if not a.no_ssh and a.ssh_host:
            cmd += ["--ssh-host", a.ssh_host]
        print("\n导入：")
        env = dict(os.environ)
        # 会话代理会吞掉带 Authorization 的 POST
        for k in ("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"):
            env.pop(k, None)
        env["WB2API_API_KEY"] = a.api_key
        rc = subprocess.call(cmd, env=env)
        if rc != 0:
            print(f"⚠ 导入脚本退出码 {rc}", file=sys.stderr)
            sys.exit(rc)
    finally:
        if a.keep:
            print(f"\n已保留下载目录：{tmp}")
        else:
            import shutil
            shutil.rmtree(tmp, ignore_errors=True)
            print("\n临时凭证已删除")


if __name__ == "__main__":
    main()
