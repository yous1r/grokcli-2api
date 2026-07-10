"""Email-assisted xAI registration sessions for grokcli-2api.

This module owns the temporary mailbox side and delegates OAuth device
authorization/import to the existing accounts/oidc_auth flow.
"""

from __future__ import annotations

import re
import secrets
import struct
import time
import uuid
from urllib.parse import quote, unquote, urljoin, urlparse, urlunparse
from typing import Any

import httpx
try:
    from curl_cffi import requests as curl_requests
    from curl_cffi.const import CurlHttpVersion
except Exception:  # pragma: no cover - optional runtime dependency fallback
    curl_requests = None
    CurlHttpVersion = None

import accounts
from auth_store import read_auth_map
from config import (
    MOEMAIL_API_KEY,
    MOEMAIL_BASE_URL,
    MOEMAIL_DOMAIN,
    MOEMAIL_EXPIRY_MS,
    XAI_ACCOUNTS_URL,
    XAI_PROXY,
    XAI_PROXY_PASSWORD,
    XAI_PROXY_USERNAME,
)


_sessions: dict[str, dict[str, Any]] = {}
_XAI_USER_AGENT = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
)


def _now() -> float:
    return time.time()


def _headers(api_key: str | None = None) -> dict[str, str]:
    key = api_key or MOEMAIL_API_KEY
    if not key:
        return {}
    return {"X-API-Key": key}


def _compact_session(sess: dict[str, Any]) -> dict[str, Any]:
    out = dict(sess)
    out.pop("_moemail_api_key", None)
    out.pop("_moemail_base_url", None)
    out.pop("_xai_cookies", None)
    out.pop("_xai_proxy", None)
    out.pop("_xai_proxy_curl", None)
    out.pop("_xai_proxy_auth", None)
    if out.get("auth_json"):
        out["auth_json_count"] = len(out["auth_json"])
        out.pop("auth_json", None)
    return out


def _public_session(sess: dict[str, Any], *, include_auth_json: bool = False) -> dict[str, Any]:
    out = dict(sess)
    out.pop("_moemail_api_key", None)
    out.pop("_moemail_base_url", None)
    out.pop("_xai_cookies", None)
    out.pop("_xai_proxy", None)
    out.pop("_xai_proxy_curl", None)
    out.pop("_xai_proxy_auth", None)
    if not include_auth_json:
        out.pop("auth_json", None)
    return out


def _normalize_proxy(
    proxy: str | None = None,
    *,
    username: str | None = None,
    password: str | None = None,
) -> str | None:
    cfg = _normalize_proxy_config(proxy, username=username, password=password)
    return cfg["proxy"] if cfg else None


def _normalize_proxy_config(
    proxy: str | None = None,
    *,
    username: str | None = None,
    password: str | None = None,
) -> dict[str, Any] | None:
    raw = (proxy or XAI_PROXY or "").strip()
    if not raw:
        return None
    env_user = XAI_PROXY_USERNAME
    env_pass = XAI_PROXY_PASSWORD
    lower = raw.lower()
    if lower.startswith("soket5://"):
        raw = "socks5://" + raw.split("://", 1)[1]
    elif lower.startswith("socket5://"):
        raw = "socks5://" + raw.split("://", 1)[1]
    elif "://" not in raw:
        raw = f"http://{raw}"

    parsed = urlparse(raw)
    if parsed.scheme not in {"http", "https", "socks5", "socks5h"}:
        raise ValueError("proxy scheme must be http, https, socks5, or socks5h")
    if not parsed.netloc or not parsed.hostname:
        raise ValueError("proxy must include host and port")
    try:
        port = parsed.port
    except ValueError as e:
        raise ValueError("proxy port is invalid") from e
    proxy_user = (username if username is not None else "").strip()
    proxy_pass = (password if password is not None else "").strip()
    if not proxy_user and username is None:
        proxy_user = env_user
    if not proxy_pass and password is None:
        proxy_pass = env_pass
    if not proxy_user and parsed.username:
        proxy_user = unquote(parsed.username)
    if not proxy_pass and parsed.password:
        proxy_pass = unquote(parsed.password)

    if proxy_pass and not proxy_user:
        raise ValueError("proxy username is required when proxy password is set")

    host = parsed.hostname or ""
    if ":" in host and not host.startswith("["):
        host = f"[{host}]"
    if port is not None:
        host = f"{host}:{port}"
    proxy_no_auth = urlunparse(
        (
            parsed.scheme,
            host,
            parsed.path or "",
            parsed.params or "",
            parsed.query or "",
            parsed.fragment or "",
        )
    )
    proxy_auth = (proxy_user, proxy_pass) if proxy_user else None
    proxy_with_auth = proxy_no_auth
    if proxy_user:
        auth = quote(proxy_user, safe="")
        if proxy_pass:
            auth = f"{auth}:{quote(proxy_pass, safe='')}"
        proxy_with_auth = urlunparse(
            (
                parsed.scheme,
                f"{auth}@{host}",
                parsed.path or "",
                parsed.params or "",
                parsed.query or "",
                parsed.fragment or "",
            )
        )
    return {
        "proxy": proxy_with_auth,
        "curl_proxy": proxy_no_auth,
        "proxy_auth": proxy_auth,
    }


def _clean_old_sessions() -> None:
    cutoff = _now() - 6 * 3600
    for sid in list(_sessions.keys()):
        sess = _sessions.get(sid) or {}
        if float(sess.get("updated_at") or 0) < cutoff:
            _sessions.pop(sid, None)


def _extract_codes_and_links(text: str) -> dict[str, list[str]]:
    codes = sorted(set(re.findall(r"(?<!\d)\d{6,8}(?!\d)", text or "")))
    links = sorted(set(re.findall(r"https?://[^\s\"'<>)]+", text or "")))
    return {"codes": codes, "links": links}


def _extract_xai_otp(messages: list[dict[str, Any]]) -> tuple[str | None, str | None]:
    """Return (wire_code, display_code) from xAI's AAA-BBB email code format."""
    for item in messages:
        text = "\n".join(
            str(item.get(k) or "") for k in ("subject", "content", "html")
        )
        match = re.search(r"\b([A-Z0-9]{3})-([A-Z0-9]{3})\b", text)
        if match:
            return "".join(match.groups()), match.group(0)
    return None, None


def _xai_base_url() -> str:
    return XAI_ACCOUNTS_URL.rstrip("/") or "https://accounts.x.ai"


def _proto_varint(value: int) -> bytes:
    out = bytearray()
    while True:
        b = value & 0x7F
        value >>= 7
        if value:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def _proto_string_field(field_id: int, value: str) -> bytes:
    raw = value.encode("utf-8")
    return _proto_varint((field_id << 3) | 2) + _proto_varint(len(raw)) + raw


def _grpc_web_frame(payload: bytes) -> bytes:
    return b"\x00" + struct.pack(">I", len(payload)) + payload


def _xai_grpc_headers() -> dict[str, str]:
    base = _xai_base_url()
    return {
        "user-agent": _XAI_USER_AGENT,
        "accept": "application/grpc-web+proto",
        "content-type": "application/grpc-web+proto",
        "x-grpc-web": "1",
        "x-user-agent": "connect-es/2.1.1",
        "origin": base,
        "referer": f"{base}/sign-up?redirect=grok-com",
    }


def _xai_cookie_dict(sess: dict[str, Any]) -> dict[str, str]:
    cookies = sess.get("_xai_cookies")
    return cookies if isinstance(cookies, dict) else {}


def _xai_proxy(sess: dict[str, Any]) -> str | None:
    proxy = sess.get("_xai_proxy")
    return str(proxy) if proxy else None


def _xai_curl_proxy(sess: dict[str, Any]) -> str | None:
    proxy = sess.get("_xai_proxy_curl") or sess.get("_xai_proxy")
    return str(proxy) if proxy else None


def _xai_proxy_auth(sess: dict[str, Any]) -> tuple[str, str] | None:
    auth = sess.get("_xai_proxy_auth")
    if isinstance(auth, (list, tuple)) and auth:
        user = str(auth[0] or "")
        password = str(auth[1] or "") if len(auth) > 1 else ""
        if user:
            return (user, password)
    return None


def _xai_curl_attempts(sess: dict[str, Any]) -> list[dict[str, Any]]:
    proxy_no_auth = _xai_curl_proxy(sess)
    proxy_with_auth = _xai_proxy(sess)
    proxy_auth = _xai_proxy_auth(sess)
    attempts: list[dict[str, Any]] = []
    attempts.append(
        {
            "name": "proxy_auth",
            "proxy": proxy_no_auth,
            "proxy_auth": proxy_auth,
            "http_version": None,
        }
    )
    if proxy_with_auth and proxy_with_auth != proxy_no_auth:
        attempts.append(
            {
                "name": "url_auth",
                "proxy": proxy_with_auth,
                "proxy_auth": None,
                "http_version": None,
            }
        )
    if CurlHttpVersion is not None:
        attempts.append(
            {
                "name": "proxy_auth_http11",
                "proxy": proxy_no_auth,
                "proxy_auth": proxy_auth,
                "http_version": CurlHttpVersion.V1_1,
            }
        )
        if proxy_with_auth and proxy_with_auth != proxy_no_auth:
            attempts.append(
                {
                    "name": "url_auth_http11",
                    "proxy": proxy_with_auth,
                    "proxy_auth": None,
                    "http_version": CurlHttpVersion.V1_1,
                }
            )
    return attempts


def _xai_curl_request(
    sess: dict[str, Any],
    method: str,
    url: str,
    *,
    headers: dict[str, str],
    timeout: float,
    data: bytes | None = None,
    allow_redirects: bool = True,
) -> tuple[Any | None, list[dict[str, Any]]]:
    errors: list[dict[str, Any]] = []
    for attempt in _xai_curl_attempts(sess):
        try:
            with curl_requests.Session(impersonate="chrome") as client:
                if _xai_cookie_dict(sess):
                    client.cookies.update(_xai_cookie_dict(sess))
                resp = client.request(
                    method,
                    url,
                    data=data,
                    headers=headers,
                    timeout=timeout,
                    proxy=attempt["proxy"],
                    proxy_auth=attempt["proxy_auth"],
                    http_version=attempt["http_version"],
                    allow_redirects=allow_redirects,
                )
                _xai_save_curl_cookies(sess, client)
                return resp, errors + [
                    {
                        "name": attempt["name"],
                        "ok": True,
                        "status_code": int(resp.status_code),
                    }
                ]
        except Exception as e:  # noqa: BLE001
            errors.append(
                {
                    "name": attempt["name"],
                    "ok": False,
                    "error": str(e)[:300],
                }
            )
    return None, errors


def _xai_save_cookies(sess: dict[str, Any], client: httpx.Client) -> None:
    sess["_xai_cookies"] = {k: v for k, v in client.cookies.items()}


def _xai_save_curl_cookies(sess: dict[str, Any], client: Any) -> None:
    cookies = getattr(client, "cookies", None)
    if cookies is None:
        return
    if hasattr(cookies, "get_dict"):
        sess["_xai_cookies"] = dict(cookies.get_dict())
        return
    try:
        sess["_xai_cookies"] = dict(cookies)
    except Exception:
        pass


def _xai_grpc_post(
    sess: dict[str, Any],
    path: str,
    payload: bytes,
) -> dict[str, Any]:
    proxy = _xai_proxy(sess)
    if curl_requests is not None:
        result = _xai_grpc_post_curl(sess, path, payload)
        if result.get("ok") or result.get("status_code") != 403:
            return result
    base = _xai_base_url()
    with httpx.Client(
        timeout=30.0,
        headers={"user-agent": _XAI_USER_AGENT},
        cookies=_xai_cookie_dict(sess),
        follow_redirects=True,
        proxy=proxy,
    ) as client:
        # Warm up the sign-up route so xAI can attach normal session cookies.
        try:
            client.get(f"{base}/sign-up?redirect=grok-com")
        except httpx.HTTPError:
            pass
        resp = client.post(
            urljoin(base + "/", path.lstrip("/")),
            content=payload,
            headers=_xai_grpc_headers(),
        )
        _xai_save_cookies(sess, client)
        text = resp.text[:500] if resp.text else ""
        return {
            "ok": resp.status_code == 200,
            "status_code": resp.status_code,
            "body_preview": text,
            "transport": "httpx",
            "proxy_enabled": bool(proxy),
        }


def _xai_grpc_post_curl(
    sess: dict[str, Any],
    path: str,
    payload: bytes,
) -> dict[str, Any]:
    base = _xai_base_url()
    try:
        warm_headers = {
            "user-agent": _XAI_USER_AGENT,
            "accept": "*/*",
            "accept-language": "zh-CN,zh;q=0.9,en;q=0.8",
        }
        _xai_curl_request(
            sess,
            "GET",
            f"{base}/sign-up?redirect=grok-com",
            headers=warm_headers,
            timeout=30,
            allow_redirects=True,
        )
        resp, attempts = _xai_curl_request(
            sess,
            "POST",
            urljoin(base + "/", path.lstrip("/")),
            data=payload,
            headers=_xai_grpc_headers(),
            timeout=45,
            allow_redirects=True,
        )
        if resp is not None:
            text = resp.text[:500] if getattr(resp, "text", None) else ""
            return {
                "ok": resp.status_code == 200,
                "status_code": resp.status_code,
                "body_preview": text,
                "transport": "curl_cffi",
                "proxy_enabled": bool(_xai_curl_proxy(sess)),
                "proxy_attempts": attempts,
            }
        return {
            "ok": False,
            "status_code": 0,
            "body_preview": attempts[-1]["error"] if attempts else "curl request failed",
            "transport": "curl_cffi",
            "proxy_enabled": bool(_xai_curl_proxy(sess)),
            "proxy_attempts": attempts,
        }
    except Exception as e:  # noqa: BLE001
        return {
            "ok": False,
            "status_code": 0,
            "body_preview": str(e)[:500],
            "transport": "curl_cffi",
            "proxy_enabled": bool(_xai_curl_proxy(sess)),
        }


def test_xai_proxy(
    *,
    proxy: str | None = None,
    proxy_username: str | None = None,
    proxy_password: str | None = None,
) -> dict[str, Any]:
    """Check whether the configured proxy can reach accounts.x.ai."""
    try:
        proxy_cfg = _normalize_proxy_config(
            proxy,
            username=proxy_username,
            password=proxy_password,
        )
    except ValueError as e:
        return {"ok": False, "error": str(e), "proxy_enabled": False}
    sess = {
        "_xai_proxy": proxy_cfg["proxy"] if proxy_cfg else None,
        "_xai_proxy_curl": proxy_cfg["curl_proxy"] if proxy_cfg else None,
        "_xai_proxy_auth": proxy_cfg["proxy_auth"] if proxy_cfg else None,
    }
    base = _xai_base_url()
    if curl_requests is None:
        return {
            "ok": False,
            "error": "curl_cffi is not installed",
            "proxy_enabled": bool(proxy_cfg),
        }
    try:
        resp, attempts = _xai_curl_request(
            sess,
            "GET",
            f"{base}/sign-up?redirect=grok-com",
            headers={
                "user-agent": _XAI_USER_AGENT,
                "accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
                "accept-language": "zh-CN,zh;q=0.9,en;q=0.8",
            },
            timeout=45,
            allow_redirects=True,
        )
        if resp is not None:
            text = resp.text[:500] if getattr(resp, "text", None) else ""
            return {
                "ok": 200 <= int(resp.status_code) < 400,
                "status_code": int(resp.status_code),
                "body_preview": text,
                "transport": "curl_cffi",
                "proxy_enabled": bool(proxy_cfg),
                "proxy_attempts": attempts,
            }
        return {
            "ok": False,
            "status_code": 0,
            "body_preview": attempts[-1]["error"] if attempts else "curl request failed",
            "transport": "curl_cffi",
            "proxy_enabled": bool(proxy_cfg),
            "proxy_attempts": attempts,
        }
    except Exception as e:  # noqa: BLE001
        return {
            "ok": False,
            "status_code": 0,
            "body_preview": str(e)[:500],
            "transport": "curl_cffi",
            "proxy_enabled": bool(proxy_cfg),
        }


def _xai_send_email_code(sess: dict[str, Any], email: str) -> dict[str, Any]:
    payload = _grpc_web_frame(_proto_string_field(1, email))
    return _xai_grpc_post(
        sess,
        "/auth_mgmt.AuthManagement/CreateEmailValidationCode",
        payload,
    )


def _xai_verify_email_code(
    sess: dict[str, Any],
    email: str,
    code: str,
) -> dict[str, Any]:
    payload = _grpc_web_frame(
        _proto_string_field(1, email) + _proto_string_field(2, code)
    )
    return _xai_grpc_post(
        sess,
        "/auth_mgmt.AuthManagement/VerifyEmailValidationCode",
        payload,
    )


def _moemail_create_mailbox(
    *,
    name: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,
    api_key: str | None = None,
    base_url: str | None = None,
    proxy: str | None = None,
    proxy_username: str | None = None,
    proxy_password: str | None = None,
) -> dict[str, Any]:
    if not (api_key or MOEMAIL_API_KEY):
        raise ValueError(
            "MoeMail API key missing. Set GROK2API_MOEMAIL_API_KEY or pass api_key."
        )

    base = (base_url or MOEMAIL_BASE_URL).rstrip("/")
    payload: dict[str, Any] = {
        "expiryTime": int(expiry_ms or MOEMAIL_EXPIRY_MS),
        "domain": domain or MOEMAIL_DOMAIN,
    }
    if name:
        payload["name"] = name

    with httpx.Client(timeout=30.0) as client:
        headers = {**_headers(api_key), "Content-Type": "application/json"}
        resp = client.post(f"{base}/api/emails/generate", json=payload, headers=headers)
        if resp.status_code == 400 and "域名" in resp.text and not domain:
            inferred = _moemail_infer_domain(client, base, api_key=api_key)
            if inferred and inferred != payload.get("domain"):
                payload["domain"] = inferred
                resp = client.post(
                    f"{base}/api/emails/generate",
                    json=payload,
                    headers=headers,
                )
        if resp.status_code >= 400:
            raise RuntimeError(
                f"MoeMail create failed {resp.status_code}: {resp.text[:500]}"
            )
        data = resp.json()

    email_id = data.get("id") or data.get("emailId")
    address = data.get("email") or data.get("address")
    if not email_id or not address:
        raise RuntimeError(f"Unexpected MoeMail create response: {data}")
    return {"id": str(email_id), "email": str(address), "raw": data}


def _moemail_infer_domain(
    client: httpx.Client,
    base: str,
    *,
    api_key: str | None = None,
) -> str | None:
    try:
        resp = client.get(f"{base}/api/emails", headers=_headers(api_key))
        if resp.status_code >= 400:
            return None
        data = resp.json()
    except Exception:
        return None
    emails = data.get("emails") if isinstance(data, dict) else None
    if not isinstance(emails, list):
        return None
    for item in emails:
        if not isinstance(item, dict):
            continue
        address = item.get("email") or item.get("address")
        if isinstance(address, str) and "@" in address:
            return address.rsplit("@", 1)[1].strip() or None
    return None


def _moemail_fetch_messages(
    email_id: str,
    *,
    api_key: str | None = None,
    base_url: str | None = None,
    include_details: bool = True,
) -> list[dict[str, Any]]:
    if not email_id:
        return []
    if not (api_key or MOEMAIL_API_KEY):
        return []

    base = (base_url or MOEMAIL_BASE_URL).rstrip("/")
    with httpx.Client(timeout=30.0) as client:
        resp = client.get(f"{base}/api/emails/{email_id}", headers=_headers(api_key))
        if resp.status_code >= 400:
            raise RuntimeError(
                f"MoeMail list failed {resp.status_code}: {resp.text[:500]}"
            )
        data = resp.json()
        messages = data.get("messages") if isinstance(data, dict) else None
        if not isinstance(messages, list):
            return []

        out: list[dict[str, Any]] = []
        for raw in messages[:20]:
            if not isinstance(raw, dict):
                continue
            item = dict(raw)
            msg_id = item.get("id") or item.get("messageId")
            if include_details and msg_id:
                detail = client.get(
                    f"{base}/api/emails/{email_id}/{msg_id}",
                    headers=_headers(api_key),
                )
                if detail.status_code == 200:
                    d = detail.json()
                    msg = d.get("message") if isinstance(d, dict) else None
                    if isinstance(msg, dict):
                        item.update(msg)
            text = "\n".join(
                str(item.get(k) or "")
                for k in ("subject", "content", "html", "from_address", "from")
            )
            item["extracted"] = _extract_codes_and_links(text)
            out.append(item)
        return out


def _auth_json_for_account(account_id: str | None) -> dict[str, Any]:
    if not account_id:
        return {}
    data = read_auth_map()
    entry = data.get(account_id)
    if isinstance(entry, dict):
        return {account_id: entry}
    return {}


def start_email_registration(
    *,
    provider: str = "moemail",
    protocol: str = "grpc",
    email: str | None = None,
    mailbox_id: str | None = None,
    prefix: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,
    api_key: str | None = None,
    base_url: str | None = None,
    proxy: str | None = None,
    proxy_username: str | None = None,
    proxy_password: str | None = None,
) -> dict[str, Any]:
    """Create a mailbox, start xAI device auth, and return a registration session."""
    _clean_old_sessions()
    provider = (provider or "moemail").lower().strip()
    if provider != "moemail":
        return {"ok": False, "error": f"unsupported provider: {provider}"}
    protocol = (protocol or "grpc").lower().strip()
    if protocol != "grpc":
        return {"ok": False, "error": f"unsupported protocol: {protocol}"}
    try:
        proxy_cfg = _normalize_proxy_config(
            proxy,
            username=proxy_username,
            password=proxy_password,
        )
    except ValueError as e:
        return {"ok": False, "error": str(e)}

    mailbox: dict[str, Any]
    if email:
        mailbox = {"id": mailbox_id or "", "email": email, "raw": {"manual": True}}
    else:
        try:
            mailbox = _moemail_create_mailbox(
                name=prefix or f"grok-{secrets.token_hex(4)}",
                domain=domain,
                expiry_ms=expiry_ms,
                api_key=api_key,
                base_url=base_url,
            )
        except Exception as e:  # noqa: BLE001
            return {"ok": False, "error": str(e)}

    login = accounts.start_login(mode="device", capture=True)
    if not login.get("ok"):
        return {"ok": False, "error": login.get("error") or "device auth failed"}

    sid = uuid.uuid4().hex[:12]
    sess = {
        "id": sid,
        "provider": provider,
        "protocol": protocol,
        "status": "waiting_registration",
        "started_at": _now(),
        "updated_at": _now(),
        "mailbox": mailbox,
        "email": mailbox.get("email"),
        "mailbox_id": mailbox.get("id"),
        "_moemail_api_key": api_key or None,
        "_moemail_base_url": base_url or None,
        "_xai_proxy": proxy_cfg["proxy"] if proxy_cfg else None,
        "_xai_proxy_curl": proxy_cfg["curl_proxy"] if proxy_cfg else None,
        "_xai_proxy_auth": proxy_cfg["proxy_auth"] if proxy_cfg else None,
        "xai_proxy_enabled": bool(proxy_cfg),
        "accounts_url": XAI_ACCOUNTS_URL,
        "device_session_id": login.get("session_id"),
        "user_code": login.get("user_code"),
        "verification_url": login.get("verification_url"),
        "message": (
            "Use the generated email on accounts.x.ai, then approve the device "
            "authorization. Tokens are imported automatically after approval."
        ),
        "latest_messages": [],
        "xai_email_code_sent": False,
        "xai_email_code_verified": False,
        "xai_email_code": None,
        "xai_protocol_result": None,
        "auth_json": {},
        "account_id": None,
        "error": None,
    }
    if protocol == "grpc" and sess.get("email"):
        try:
            result = _xai_send_email_code(sess, str(sess["email"]))
            sess["xai_protocol_result"] = result
            if result.get("ok"):
                sess["xai_email_code_sent"] = True
                sess["status"] = "waiting_email_code"
                sess["message"] = (
                    "xAI email code requested by protocol. Waiting for MoeMail, "
                    "then the code will be verified automatically. Turnstile still "
                    "requires manual completion before device authorization."
                )
            else:
                sess["status"] = "protocol_blocked"
                sess["error"] = (
                    f"xAI protocol send-code failed HTTP {result.get('status_code')}: "
                    f"{result.get('body_preview') or 'empty response'}"
                )
                sess["message"] = (
                    "accounts.x.ai blocked or timed out the protocol send-code "
                    "request. Check network/proxy access to accounts.x.ai."
                )
        except Exception as e:  # noqa: BLE001
            sess["status"] = "protocol_error"
            sess["error"] = str(e)
            sess["message"] = (
                "xAI protocol send-code failed. Check network/proxy access to "
                "accounts.x.ai."
            )
    _sessions[sid] = sess
    return {"ok": True, **_public_session(sess, include_auth_json=True)}


def get_registration_session(
    session_id: str,
    *,
    include_auth_json: bool = False,
    poll_mailbox: bool = True,
) -> dict[str, Any] | None:
    sess = _sessions.get(session_id)
    if not sess:
        return None

    if poll_mailbox and sess.get("mailbox_id"):
        try:
            sess["latest_messages"] = _moemail_fetch_messages(
                str(sess["mailbox_id"]),
                api_key=sess.get("_moemail_api_key"),
                base_url=sess.get("_moemail_base_url"),
            )
        except Exception as e:  # noqa: BLE001
            sess["mailbox_error"] = str(e)

    if (
        sess.get("protocol") == "grpc"
        and sess.get("xai_email_code_sent")
        and not sess.get("xai_email_code_verified")
        and sess.get("email")
    ):
        wire_code, display_code = _extract_xai_otp(sess.get("latest_messages") or [])
        if wire_code:
            sess["xai_email_code"] = display_code or wire_code
            try:
                result = _xai_verify_email_code(sess, str(sess["email"]), wire_code)
                sess["xai_protocol_result"] = result
                if result.get("ok"):
                    sess["xai_email_code_verified"] = True
                    sess["status"] = "waiting_turnstile"
                    sess["message"] = (
                        "Email code verified by xAI protocol. Complete the "
                        "accounts.x.ai Turnstile/account form manually, then approve "
                        "the device code. Tokens import automatically after approval."
                    )
                else:
                    sess["status"] = "protocol_error"
                    sess["error"] = (
                        f"xAI protocol verify-code failed HTTP "
                        f"{result.get('status_code')}: "
                        f"{result.get('body_preview') or 'empty response'}"
                    )
            except Exception as e:  # noqa: BLE001
                sess["status"] = "protocol_error"
                sess["error"] = str(e)

    device_id = sess.get("device_session_id")
    if device_id:
        device = accounts.get_login_session(str(device_id))
        if device:
            sess["device_status"] = device.get("status")
            sess["device_message"] = device.get("message")
            if device.get("status") == "success":
                sess["status"] = "imported"
                sess["account_id"] = device.get("account_id")
                sess["email"] = device.get("email") or sess.get("email")
                sess["auth_json"] = _auth_json_for_account(device.get("account_id"))
                sess["message"] = "Authorization completed and auth.json was imported."
            elif device.get("status") in ("error", "expired"):
                sess["status"] = device.get("status")
                sess["error"] = device.get("error")
                sess["message"] = device.get("message") or sess.get("message")
            elif sess.get("status") not in (
                "waiting_email_code",
                "waiting_turnstile",
                "protocol_blocked",
                "protocol_error",
            ):
                sess["status"] = "waiting_authorization"

    sess["updated_at"] = _now()
    if include_auth_json:
        return {"ok": True, **_public_session(sess, include_auth_json=True)}
    return {"ok": True, **_compact_session(sess)}


def list_registration_sessions() -> dict[str, Any]:
    _clean_old_sessions()
    return {
        "ok": True,
        "sessions": [_compact_session(s) for s in _sessions.values()],
        "total": len(_sessions),
    }
