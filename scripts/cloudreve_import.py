#!/usr/bin/env python3
"""scripts/cloudreve_import.py — Cloudreve v4 分享/直链 → 下载（含 zip）→ 解包 → 导入 wb2api 面板

两种入口都吃（都用 --url，可重复给多个）：

    # 1) 分享链接（可带提取码）
    python3 scripts/cloudreve_import.py --url "http://111.170.175.16:5212/s/Je5hW"
    python3 scripts/cloudreve_import.py --url "http://111.170.175.16:5212/s/Je5hW" --pwd 1234

    # 2) 单文件直链（卖家直接发 /f/<id>/<name> 时用这条）
    python3 scripts/cloudreve_import.py \
        --url "http://111.170.175.16:5212/f/vEqTL/workbuddy-accounts-xxx.json" \
        --url "http://111.170.175.16:5212/f/odMsR/workbuddy-accounts-yyy.json"

    # 可选：--panel / --api-key / --ssh-host / --no-ssh / --keep / --dry-run

Cloudreve v4 匿名分享接口（2026-10 实测，从 /assets/index-*.js 反出）：
  1) GET  /api/v4/share/info/<id>?count_views=true        → {id,name,unlocked,expired}
  2) POST /api/v4/share/unlock/<id> {password}            → 仅 unlocked=false 时需要
  3) GET  /api/v4/file?uri=cloudreve://<id>@share[/子目录] → {data:{files:[{type,id,name,size,path}]}}
  4) POST /api/v4/file/url {uris:[...]}                   → {data:{urls:[{url}],expires}}
  5) GET  <url>                                           → 文件本体（带 sign，过期需重取）
注意：`/api/v3/share/info/<id>` 在 v4 上是 **404**（v3 是另一代 API，别按老文档拼）；
`/f/<id>/<name>` 是**文件直链**，匿名可下（实测 200），不需要走分享 API。

落地的账号文件可能是三种形态，这里都吃：
  - cockpit 导出包 `{format:"workbuddy-accounts", version, count, accounts:[...]}`（**camelCase**）
  - 裸数组 `[{...}]`（snake/camel 都行，最终交给 import_accounts.py 归一化）
  - 单条 `{account:{}, auth:{}}` 或扁平 `{uid, accessToken, ...}`
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
import zipfile
from pathlib import Path

import requests

IMPORTER = Path.home() / ".workbuddy/skills/workbuddy2api-account-import/scripts/import_accounts.py"
UA = ("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36")


def die(msg: str, code: int = 1):
    print(f"❌ {msg}", file=sys.stderr)
    sys.exit(code)


def parse_url(url: str) -> tuple[str, str, str]:
    """返回 (base, kind, id_or_rel)，kind ∈ {'share','file'}。"""
    if "/s/" in url:
        base, sid = url.split("/s/", 1)
        return base.rstrip("/"), "share", sid.split("?")[0].strip("/")
    if "/f/" in url:
        base, rest = url.split("/f/", 1)
        return base.rstrip("/"), "file", rest.split("?")[0].strip()
    die(f"链接既不是 /s/<id> 分享也不是 /f/<id>/<name> 直链：{url}")


def new_session(base: str) -> requests.Session:
    s = requests.Session()
    s.trust_env = False
    s.headers.update({"User-Agent": UA, "Accept": "application/json, text/plain, */*",
                      "Origin": base, "Referer": base + "/"})
    return s


def api(s: requests.Session, base: str, method: str, path: str, **kw):
    r = s.request(method, base + "/api/v4" + path, timeout=60, **kw)
    if r.status_code >= 400:
        raise RuntimeError(f"{method} {path} → http {r.status_code}: {r.text[:160]}")
    j = r.json()
    if j.get("code") not in (0, None):
        raise RuntimeError(f"{method} {path} → code={j.get('code')} {j.get('msg')}")
    return j.get("data")


def list_dir(s, base, sid, sub: str = "") -> list[dict]:
    uri = f"cloudreve://{sid}@share" + (("/" + sub.lstrip("/")) if sub else "")
    data = api(s, base, "GET", "/file", params={"uri": uri})
    return (data or {}).get("files") or []


def walk(s, base, sid, sub: str = "", depth: int = 0) -> list[dict]:
    if depth > 4:
        return []
    out = []
    for f in list_dir(s, base, sid, sub):
        if str(f.get("type")) in ("0", "None", "") or f.get("size") is not None:
            out.append(f)
        else:
            out.extend(walk(s, base, sid, str(f.get("path", "")).split("@share/", 1)[-1], depth + 1))
    return out


def save(s: requests.Session, url: str, name: str, outdir: Path):
    r = s.get(url, timeout=180)
    if r.status_code != 200 or not r.content:
        print(f"⚠ 下载失败 {name} http={r.status_code}")
        return None
    p = outdir / (name or "file")
    p.write_bytes(r.content)
    print(f"  ↓ {p.name}  {len(r.content)} B")
    return p


def download_share(s, base, sid, outdir: Path) -> list[Path]:
    files = walk(s, base, sid)
    print(f"分享内文件 {len(files)} 个：")
    for f in files:
        print(f"  - {f.get('name')}  {f.get('size')} B")
    if not files:
        die("分享里没有文件")
    data = api(s, base, "POST", "/file/url", json={"uris": [f["path"] for f in files]})
    urls = [u["url"] for u in (data or {}).get("urls") or []]
    out = []
    for f, u in zip(files, urls):
        p = save(s, u, f.get("name") or "file", outdir)
        if p:
            out.append(p)
    return out


def expand(paths: list[Path], outdir: Path) -> list[Path]:
    """zip 就地解开，返回所有文件路径。"""
    out = []
    for p in paths:
        if p.suffix.lower() == ".zip":
            try:
                with zipfile.ZipFile(p) as z:
                    target = outdir / ("unzip-" + p.stem)
                    target.mkdir(exist_ok=True)
                    z.extractall(target)
                    got = [q for q in target.rglob("*") if q.is_file()]
                    print(f"  解压 {p.name} → {len(got)} 个文件")
                    out.extend(got)
            except Exception as e:
                print(f"⚠ 解压失败 {p.name}: {e}")
        else:
            out.append(p)
    return out


def extract_accounts(paths: list[Path]) -> list[dict]:
    accs = []
    for p in paths:
        if p.suffix.lower() != ".json":
            continue
        try:
            d = json.loads(p.read_text())
        except Exception as e:
            print(f"⚠ {p.name} 不是合法 JSON: {e}")
            continue
        if isinstance(d, dict) and isinstance(d.get("accounts"), list):
            accs.extend(d["accounts"])
        elif isinstance(d, list):
            accs.extend(d)
        elif isinstance(d, dict):
            accs.append(d)
    return accs


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", action="append", default=[], help="分享链接或 /f/ 直链，可重复")
    ap.add_argument("--pwd", help="分享提取码（unlocked=false 时必填）")
    ap.add_argument("--panel", default="https://workbuddy2api.1task.cn")
    ap.add_argument("--api-key", default=os.environ.get("WB2API_API_KEY", ""))
    ap.add_argument("--ssh-host", default="root@154.40.45.18")
    ap.add_argument("--no-ssh", action="store_true")
    ap.add_argument("--keep", action="store_true")
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args()
    if not a.url:
        die("至少要给一个 --url")

    tmp = Path(tempfile.mkdtemp(prefix="creve-acc-"))
    try:
        raw: list[Path] = []
        for u in a.url:
            base, kind, rid = parse_url(u)
            s = new_session(base)

            if kind == "file":
                print(f"Cloudreve {base}  直链 {rid}")
                p = save(s, f"{base}/f/{rid}", rid.split("/", 1)[-1], tmp)
                if p:
                    raw.append(p)
                continue

            print(f"Cloudreve {base}  分享 {rid}")
            info = api(s, base, "GET", f"/share/info/{rid}", params={"count_views": "true"})
            print(f"分享名：{info.get('name')}  浏览 {info.get('visited')} / 下载 {info.get('downloaded')}")
            if info.get("expired"):
                die("分享已过期")
            if not info.get("unlocked", True):
                if not a.pwd:
                    die("该分享需要提取码，请加 --pwd")
                api(s, base, "POST", f"/share/unlock/{rid}", json={"password": a.pwd})
                print("提取码已解锁")
            raw.extend(download_share(s, base, rid, tmp))

        if not raw:
            die("没下载到任何文件")
        expanded = expand(raw, tmp)
        accs = extract_accounts(expanded)
        if not accs:
            die("下载内容里没解析出账号（不是 cockpit 导出包 / 数组 / 单条）")

        print(f"\n解析出 {len(accs)} 个账号")
        for x in accs[:15]:
            acc = x.get("account") if isinstance(x.get("account"), dict) else x
            print(f"  - {str(acc.get('uid'))[:8]}  {acc.get('nickname') or '?'}")

        meta = tmp / "accounts.meta.json"
        meta.write_text(json.dumps(accs, ensure_ascii=False, indent=1))

        if a.dry_run:
            print(f"--dry-run：已写 {meta}，未导入")
            return

        if not IMPORTER.exists():
            die(f"找不到导入脚本 {IMPORTER}")
        cmd = [sys.executable, str(IMPORTER), str(meta), "--url", a.panel, "--concurrency", "10"]
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
            print(f"\n目录已保留：{tmp}")
        else:
            shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    main()
