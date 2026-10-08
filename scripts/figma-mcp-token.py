#!/usr/bin/env python3
"""Figma MCP token 管理：从 Claude Code 钥匙串引导，自动续期，同步 .env。

用法:
  figma-mcp-token.py ensure     不足 7 天时自动续期；同步 ~/.deepai/.env（成功时无输出）
  figma-mcp-token.py refresh    强制续期
  figma-mcp-token.py status     查看有效期与来源，不打印密钥

凭证存储: ~/.deepai/figma-mcp-token.json (0600，自创建起)；access token 同步写入
~/.deepai/.env 的 FIGMA_MCP_TOKEN 行——deepai 启动时 env.Load 注入进程环境，
mcp.json 的 ${FIGMA_MCP_TOKEN} 即可解析，无需 shell 导出。
丢失恢复: 删掉该文件后重跑 ensure（钥匙串 refresh_token 实测可复用，无需重新授权）。

原理: Figma 远程 MCP 的白名单只拦 OAuth 注册；token 由 Claude Code（白名单客户端）
铸造。refresh_token 实测可重复使用（续期响应不含轮换），钥匙串仍可作后备引导源。
ensure 成功时静默、失败时向 stderr 报错并以非零码退出——放进 ~/.zshrc 时
不要重定向，让续期失败可见。
"""

import base64
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

TOKEN_FILE = os.path.expanduser("~/.deepai/figma-mcp-token.json")
ENV_FILE = os.path.expanduser("~/.deepai/.env")
KEYCHAIN_SERVICE = "Claude Code-credentials"
TOKEN_ENDPOINT = "https://api.figma.com/v1/oauth/token"
REFRESH_BEFORE = 7 * 86400  # 剩余不足 7 天即续期


def fail(msg):
    print(f"figma-mcp-token: {msg}", file=sys.stderr)
    sys.exit(1)


def write_private(path, data):
    """落盘敏感内容：临时文件自创建起 0600，同目录原子替换，失败不留残件。"""
    tmp = path + ".tmp"
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        with os.fdopen(fd, "w") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def load_file():
    try:
        with open(TOKEN_FILE) as f:
            return json.load(f)
    except (OSError, ValueError):
        return None


def save_file(cred):
    os.makedirs(os.path.dirname(TOKEN_FILE), exist_ok=True)
    write_private(TOKEN_FILE, json.dumps(cred))


def bootstrap_from_keychain():
    """首次使用：从 Claude Code 钥匙串条目提取 figma OAuth 凭证。

    非 macOS 无 security 命令（FileNotFoundError）、条目不存在或无 figma
    授权时返回 None——由调用方给出统一指引。
    """
    try:
        out = subprocess.run(
            ["security", "find-generic-password", "-s", KEYCHAIN_SERVICE, "-w"],
            capture_output=True, text=True, check=True,
        ).stdout
        d = json.loads(out)
    except (OSError, subprocess.CalledProcessError, ValueError):
        return None
    for k, v in d.get("mcpOAuth", {}).items():
        if k.startswith("figma|") and v.get("refreshToken"):
            return {
                "access_token": v.get("accessToken", ""),
                "refresh_token": v["refreshToken"],
                "client_id": v["clientId"],
                "client_secret": v["clientSecret"],
                "expires_at": 0,
            }
    return None


def refresh(cred):
    """用 refresh_token 换新 access token；响应若含新 refresh_token 则一并持久化
    （实测不轮换，保留分支以兼容服务器策略变更）。"""
    basic = base64.b64encode(
        f"{cred['client_id']}:{cred['client_secret']}".encode()
    ).decode()
    data = urllib.parse.urlencode(
        {"grant_type": "refresh_token", "refresh_token": cred["refresh_token"]}
    ).encode()
    req = urllib.request.Request(
        TOKEN_ENDPOINT, data=data,
        headers={"Content-Type": "application/x-www-form-urlencoded",
                 "Authorization": "Basic " + basic},
    )
    try:
        r = json.load(urllib.request.urlopen(req, timeout=20))
    except urllib.error.HTTPError as ex:
        body = ex.read().decode(errors="replace")[:200]
        if ex.code in (400, 401):
            fail(f"refresh 被拒（{ex.code} {body}）——refresh_token 已失效。"
                 f"删除 {TOKEN_FILE} 后在 claude 里 /mcp 重新授权 figma，再跑 ensure。")
        fail(f"token endpoint HTTP {ex.code}: {body}")
    except OSError as ex:
        fail(f"网络错误: {ex}")
    cred["access_token"] = r["access_token"]
    if r.get("refresh_token"):
        cred["refresh_token"] = r["refresh_token"]
    cred["expires_at"] = int(time.time()) + r.get("expires_in", 7776000)
    save_file(cred)
    return cred


def sync_env(token):
    """同步 access token 到 ~/.deepai/.env；幂等（值未变不写），原子替换。"""
    want = f"FIGMA_MCP_TOKEN={token}\n"
    try:
        with open(ENV_FILE) as f:
            lines = f.readlines()
    except OSError:
        lines = []
    hit = False
    for i, l in enumerate(lines):
        s = l.lstrip()
        if s.startswith("export "):
            s = s[7:]
        if s.split("=", 1)[0].strip() == "FIGMA_MCP_TOKEN":
            hit = True
            if s != want:
                lines[i] = want
                break
            return
    if not hit:
        if lines and not lines[-1].endswith("\n"):
            lines[-1] += "\n"
        lines.append(want)
    os.makedirs(os.path.dirname(ENV_FILE), exist_ok=True)
    write_private(ENV_FILE, "".join(lines))


def cmd_ensure(force=False):
    cred = load_file() or bootstrap_from_keychain()
    if cred is None:
        fail("无凭证：本机需先在 claude 里 /mcp 授权 figma（macOS 钥匙串），"
             "或按 docs/figma_mcp.md 的 PAT 方式接入")
    if force or cred.get("expires_at", 0) - time.time() < REFRESH_BEFORE:
        cred = refresh(cred)
    sync_env(cred["access_token"])


def cmd_status():
    cred = load_file()
    if cred is None:
        cred = bootstrap_from_keychain()
        src = "钥匙串（未引导，跑 ensure 落盘）"
    else:
        src = TOKEN_FILE
    if cred is None:
        fail("无任何凭证：先在 claude 里 /mcp 授权 figma")
    exp = cred.get("expires_at", 0)
    days = (exp - time.time()) / 86400 if exp else None
    left = f"{days:.1f} 天" if days is not None else "未知（下次 ensure 时续期）"
    print(f"来源: {src}\n有效期剩余: {left}")


def main():
    cmd = sys.argv[1] if len(sys.argv) > 1 else "ensure"
    if cmd == "ensure":
        cmd_ensure()
    elif cmd == "refresh":
        cmd_ensure(force=True)
    elif cmd == "status":
        cmd_status()
    else:
        fail(f"未知子命令 {cmd}（ensure | refresh | status）")


if __name__ == "__main__":
    main()
