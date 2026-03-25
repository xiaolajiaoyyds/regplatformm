"""
Browser-based account provisioning service (Kiro + Gemini).

API:
  POST /kiro/process   { email, proxy?, yydsmail_url, yydsmail_key, ... }
  POST /gemini/process { email, proxy?, yydsmail_url, yydsmail_key, mail_provider?, mail_meta? }

Env:
  KIRO_REG_PORT         (default 5076)
  KIRO_REG_HOST         (default 0.0.0.0)
  KIRO_MAX_CONCURRENT   (default 3)
  GEMINI_MAX_CONCURRENT (default 2)

Usage: python server.py [--port 5076] [--host 0.0.0.0]
"""

import argparse
import asyncio
import json
import logging
import os
import queue
import quopri
import random
import re
import secrets
import ssl
import string
import subprocess
import time
import urllib.parse
import urllib.request
import urllib.error
import warnings
from threading import Event
from typing import Optional

# 代理链支持：环境变量代理（Clash）→ 后端代理 → 目标
try:
    from proxy_chain import chain_proxy, curl_proxy_args
except ImportError:
    # 未部署 proxy_chain 模块时，退化为直连
    def chain_proxy(p): return p or ""
    def curl_proxy_args(p): return []

# 禁止 urllib3 InsecureRequestWarning 刷屏
warnings.filterwarnings("ignore", message="Unverified HTTPS request")

# Python 3.14 SSL 兼容性修复：安装全局自定义 opener（跳过证书验证）
# undetected_chromedriver 已移除，但 OIDC / mail polling 的 urllib 仍需要此 opener
_SSL_CTX = ssl.create_default_context()
_SSL_CTX.check_hostname = False
_SSL_CTX.verify_mode = ssl.CERT_NONE
urllib.request.install_opener(
    urllib.request.build_opener(
        urllib.request.ProxyHandler({}),
        urllib.request.HTTPSHandler(context=_SSL_CTX),
    )
)

from quart import Quart, jsonify, request
from camoufox.async_api import AsyncCamoufox

logger = logging.getLogger("app-service")
logger.setLevel(logging.INFO)
_h = logging.StreamHandler()
_h.setFormatter(logging.Formatter("[%(asctime)s] [%(levelname)s] %(message)s", datefmt="%H:%M:%S"))
logger.addHandler(_h)

app = Quart(__name__)
# 延长响应超时（默认 60s 不够浏览器注册流程）
app.config["RESPONSE_TIMEOUT"] = 400
app.config["BODY_TIMEOUT"] = 30

# 并发控制：允许多个浏览器同时注册（HF Space 可通过环境变量调整）
_MAX_CONCURRENT = int(os.getenv("KIRO_MAX_CONCURRENT", "3"))
_reg_semaphore = asyncio.Semaphore(_MAX_CONCURRENT)
# Gemini 并发控制（独立信号量，不与 Kiro 竞争；与 system_setting.go gemini_max_concurrent 默认值保持一致）
_GEMINI_MAX_CONCURRENT = int(os.getenv("GEMINI_MAX_CONCURRENT", "2"))
_gemini_semaphore = asyncio.Semaphore(_GEMINI_MAX_CONCURRENT)
# 邮件轮询超时（秒）
_MAIL_TIMEOUT = int(os.getenv("KIRO_MAIL_TIMEOUT", "120"))


# ─── 常量 ───────────────────────────────────────────────────────────────────

FIRST_NAMES = [
    "James", "Mary", "Robert", "Patricia", "John", "Jennifer", "Michael", "Linda",
    "David", "Elizabeth", "William", "Barbara", "Richard", "Susan", "Joseph", "Jessica",
    "Thomas", "Sarah", "Christopher", "Karen", "Charles", "Lisa", "Daniel", "Nancy",
    "Emma", "Oliver", "Sophia", "Liam", "Ava", "Noah", "Isabella", "Ethan",
    "Mia", "Mason", "Charlotte", "Logan", "Amelia", "Lucas", "Harper", "Aiden",
]
LAST_NAMES = [
    "Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis",
    "Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez", "Wilson", "Anderson",
    "Taylor", "Thomas", "Jackson", "White", "Harris", "Martin", "Thompson", "Moore",
]


def _detect_ip_location(proxy: str | None) -> dict:
    """
    通过代理出口 IP 自动检测地理位置，返回 timezone / locale / geo 信息。
    失败时返回美国东部默认值（安全回退）。
    """
    default = {
        "timezone": "America/New_York",
        "locale": "en-US",
        "geo": {"latitude": 40.7128, "longitude": -74.0060, "accuracy": 100},
        "country": "US",
    }
    apis = [
        "http://ip-api.com/json/?fields=status,countryCode,lat,lon,timezone",
        "https://ipapi.co/json/",
    ]
    for api_url in apis:
        try:
            # 代理链：通过 Clash 链到后端代理，确保出口 IP 是后端代理
            _vp = _validate_proxy(proxy) if proxy else ""
            chained = chain_proxy(_vp) if _vp else ""
            if chained:
                opener = urllib.request.build_opener(
                    urllib.request.ProxyHandler({"http": chained, "https": chained}),
                    urllib.request.HTTPSHandler(context=_SSL_CTX),
                )
            else:
                opener = urllib.request.build_opener(
                    urllib.request.HTTPSHandler(context=_SSL_CTX),
                )
            req = urllib.request.Request(api_url)
            req.add_header("User-Agent", "Mozilla/5.0 (compatible; RegPlatform/1.0)")
            with opener.open(req, timeout=8) as resp:
                data = json.loads(resp.read())

            # ip-api.com 格式
            if "countryCode" in data and data.get("status") == "success":
                cc = data["countryCode"]
                tz = data.get("timezone", default["timezone"])
                lat = data.get("lat", default["geo"]["latitude"])
                lon = data.get("lon", default["geo"]["longitude"])
            # ipapi.co 格式
            elif "country_code" in data:
                cc = data["country_code"]
                tz = data.get("timezone", default["timezone"])
                lat = data.get("latitude", default["geo"]["latitude"])
                lon = data.get("longitude", default["geo"]["longitude"])
            else:
                continue

            # 国家码 → locale 映射
            locale_map = {
                "US": "en-US", "GB": "en-GB", "CA": "en-CA", "AU": "en-AU",
                "DE": "de-DE", "FR": "fr-FR", "JP": "ja-JP", "KR": "ko-KR",
                "BR": "pt-BR", "MX": "es-MX", "ES": "es-ES", "IT": "it-IT",
                "NL": "nl-NL", "SE": "sv-SE", "NO": "nb-NO", "DK": "da-DK",
                "IN": "en-IN", "SG": "en-SG", "HK": "zh-HK", "TW": "zh-TW",
            }
            locale = locale_map.get(cc, "en-US")

            logger.info("IP 地理位置检测: country=%s tz=%s lat=%.2f lon=%.2f", cc, tz, lat, lon)
            return {
                "timezone": tz,
                "locale": locale,
                "geo": {"latitude": lat, "longitude": lon, "accuracy": 100},
                "country": cc,
            }
        except Exception as e:
            logger.debug("IP 地理位置检测失败 (%s): %s", api_url, e)
            continue

    logger.warning("IP 地理位置检测全部失败，使用美国东部默认值")
    return default


# ─── AWS OIDC Device Flow 常量 ───────────────────────────────────────────────

# AWS OIDC 服务端点（us-east-1，Builder ID 唯一可用区域）
OIDC_BASE_URL = "https://oidc.us-east-1.amazonaws.com"
# Kiro / Amazon Q Builder ID 起始 URL
OIDC_START_URL = "https://view.awsapps.com/start"
# 模拟 AWS SDK Rust 客户端头（绕过简单的 UA 过滤）
_OIDC_HEADERS = {
    "Content-Type": "application/json",
    "User-Agent": "aws-sdk-rust/1.3.9 os/windows lang/rust/1.87.0",
    "x-amz-user-agent": (
        "aws-sdk-rust/1.3.9 ua/2.1 api/ssooidc/1.88.0 "
        "os/windows lang/rust/1.87.0 m/E app/AmazonQ-For-CLI"
    ),
    "amz-sdk-request": "attempt=1; max=3",
}

# AWS Builder ID 注册发件人域名白名单
AWS_SENDER_DOMAINS = (
    "signin.aws", "verify.signin.aws", "amazon.com",
    "aws.amazon.com", "noreply@", "no-reply@",
)

# OTP 精准提取模式（优先高精度，回退宽泛）
# 支持纯数字和字母数字混合验证码（Google Gemini 可能发送 A-Z0-9 混合 6 位码）
RE_OTP_PRECISE = re.compile(r'(?:verification code|verify|code)[^0-9]{0,30}(\d{6})', re.IGNORECASE)
RE_OTP_ALNUM = re.compile(r'(?:verification code|verify|code)\s*[:：\s]*\b([A-Z0-9]{6})\b', re.IGNORECASE)
RE_OTP_FALLBACK = re.compile(r'\b(\d{6})\b')
RE_OTP_ALNUM_ANYWHERE = re.compile(r'\b([A-Z0-9]{6,8})\b')
# 排除常见英文单词误匹配（全大写 6 字母）
_OTP_WORD_BLACKLIST = frozenset({"GOOGLE", "GEMINI", "VERIFY", "SIGNIN", "PLEASE", "CHANGE", "ACCEPT", "CANCEL", "SUBMIT"})

# Google 验证邮件 HTML class 精确提取（<span class="verification-code">AB12CD</span>）
RE_HTML_SPAN_CODE = re.compile(
    r'<span[^>]*class="[^"]*verification-code[^"]*"[^>]*>\s*([A-Z0-9]{6})\s*</span>',
    re.IGNORECASE
)

DEFAULT_YYDSMAIL_BASE_URL = "https://maliapi.215.im"

# 已移除域名黑名单机制：后台配什么邮箱 provider 就用什么


# ─── Google Gemini 常量 ──────────────────────────────────────────────────────

GEMINI_AUTH_URL = "https://auth.business.gemini.google/login"

# Google 验证邮件发件人域名白名单
GOOGLE_SENDER_DOMAINS = (
    "google.com", "googlemail.com", "accounts.google.com",
    "noreply@google", "no-reply@google", "workspace-noreply@google",
)


def _is_google_email(sender: str, subject: str) -> bool:
    """判断是否为 Google 验证邮件（Gemini Business 注册用）。"""
    sender_lower = _normalize_sender(sender).lower()
    subject_lower = subject.lower()
    domain_match = any(d in sender_lower for d in GOOGLE_SENDER_DOMAINS)
    if not domain_match and sender_lower:
        domain_match = ("google" in sender_lower) or ("gemini" in sender_lower)
    subject_match = any(kw in subject_lower for kw in
                        ("verification code", "verify", "验证码", "code",
                         "sign in", "登录", "google", "gemini", "workspace", "business"))
    return subject_match and (domain_match or not sender_lower.strip())


def _get_email_domain(email: str) -> str:
    """提取邮箱 @ 后的域名后缀"""
    return email.rsplit("@", 1)[-1].lower() if "@" in email else ""


def _normalize_sender(sender_obj) -> str:
    """把不同 provider 返回的发件人结构归一成邮箱字符串。"""
    if isinstance(sender_obj, dict):
        return str(sender_obj.get("address", "") or sender_obj.get("name", "") or "")
    if isinstance(sender_obj, list):
        for item in sender_obj:
            sender = _normalize_sender(item)
            if sender:
                return sender
        return ""
    return str(sender_obj) if sender_obj else ""


def _iter_text_chunks(value) -> list[str]:
    """统一展开 text/html 字段，兼容字符串和字符串数组。"""
    if isinstance(value, str):
        return [value] if value else []
    if isinstance(value, list):
        return [item for item in value if isinstance(item, str) and item]
    return []


def _is_plausible_otp_candidate(code: str, *, allow_plain_digits: bool = True) -> bool:
    """过滤明显不是验证码的 6-8 位候选值。"""
    if not code:
        return False
    code = code.upper()
    if code in _OTP_WORD_BLACKLIST:
        return False
    if code.isdigit():
        if not allow_plain_digits:
            return False
        if re.match(r'^(19|20)\d{4}$|^0{6,8}$', code):
            return False
        return True
    return any(ch.isdigit() for ch in code)


# ─── 工具函数 ────────────────────────────────────────────────────────────────

def _generate_password(length: int = 16) -> str:
    """生成高强度密码：大小写 + 数字 + 特殊字符（使用 CSPRNG）"""
    chars = string.ascii_letters + string.digits + "!@#$%^&*"
    pwd = (
        secrets.choice(string.ascii_uppercase)
        + secrets.choice(string.ascii_lowercase)
        + secrets.choice(string.digits)
        + secrets.choice("!@#$%^&*")
        + "".join(secrets.choice(chars) for _ in range(length - 4))
    )
    lst = list(pwd)
    # Fisher-Yates shuffle with CSPRNG
    for i in range(len(lst) - 1, 0, -1):
        j = secrets.randbelow(i + 1)
        lst[i], lst[j] = lst[j], lst[i]
    return "".join(lst)


def _random_name() -> tuple:
    return random.choice(FIRST_NAMES), random.choice(LAST_NAMES)


# ─── GPTMail 轮询 ────────────────────────────────────────────────────────────

def _is_aws_email(sender: str, subject: str) -> bool:
    """
    精准判断是否为 AWS Builder ID 验证邮件。
    发件人域名白名单 + 主题关键词双重过滤，防止误匹配广告邮件。
    """
    combined = (sender + " " + subject).lower()
    # 发件人域名白名单匹配
    domain_match = any(d in sender.lower() for d in AWS_SENDER_DOMAINS)
    # 主题关键词：AWS 特定词
    subject_match = any(kw in subject.lower() for kw in
                        ("verification code", "verify your", "builder id", "builder-id", "sign in",
                         "构建者 id", "构建者id", "验证"))
    # 发件人为空时（GPTMail 部分邮件不填 from）只用主题判断
    return subject_match and (domain_match or not sender.strip())


def _extract_otp(text: str) -> Optional[str]:
    """
    从邮件文本中提取 6 位 OTP 验证码。
    支持纯数字和字母数字混合格式（Google Gemini 验证码可能包含字母）。
    优先级：HTML span.verification-code > 关键词精准 > 字母数字混合 > quopri解码重试 > 6位数字回退。
    """
    if not text:
        return None

    # 最高优先级：HTML span.verification-code 精确提取
    m = RE_HTML_SPAN_CODE.search(text)
    if m:
        code = m.group(1).upper()
        if code not in _OTP_WORD_BLACKLIST:
            return code

    # 高精度模式：纯数字验证码（宽松间距）
    m = RE_OTP_PRECISE.search(text)
    if m:
        return m.group(1)
    # 字母数字混合模式（Google Gemini 验证码）
    m = RE_OTP_ALNUM.search(text)
    if m:
        code = m.group(1).upper()
        if code not in _OTP_WORD_BLACKLIST:
            return code

    # quopri 解码重试（邮件正文可能使用 quoted-printable 编码，导致验证码被截断）
    try:
        decoded = quopri.decodestring(text.encode("utf-8", errors="ignore")).decode("utf-8", errors="ignore")
        if decoded and decoded != text:
            for pat in (RE_OTP_PRECISE, RE_OTP_ALNUM):
                m = pat.search(decoded)
                if m:
                    code = m.group(1).upper() if pat is RE_OTP_ALNUM else m.group(1)
                    if pat is not RE_OTP_ALNUM or code not in _OTP_WORD_BLACKLIST:
                        return code
    except Exception:
        pass

    # HTML 邮件里验证码常被单独放进块级元素中，兜底扫描任意 6-8 位字母数字串。
    # 只接受包含数字的候选，避免把 GOOGLE/GEMINI 这类单词误判成验证码。
    for candidate in RE_OTP_ALNUM_ANYWHERE.findall(text.upper()):
        if _is_plausible_otp_candidate(candidate, allow_plain_digits=False):
            return candidate

    # 回退：任意 6 位数字（排除明显的年份/ID）
    candidates = RE_OTP_FALLBACK.findall(text)
    for c in candidates:
        if _is_plausible_otp_candidate(c):
            return c
    return None


def _poll_yydsmail(yydsmail_url: str, yydsmail_key: str, email: str, meta: dict,
                   timeout: int = 120,
                   log_q: Optional[queue.Queue] = None,
                   cancel: Optional[Event] = None,
                   email_filter=None) -> Optional[str]:
    """轮询 YYDS Mail API 获取验证码。
    1. GET /v1/messages?address=xxx (Bearer token) 获取邮件列表
    2. GET /v1/messages/{id} (Bearer token) 获取邮件详情
    3. 从 subject/text 中提取验证码
    email_filter: 可选的邮件过滤函数 (sender, subject) -> bool
    """
    token = meta.get("token", "")
    if not token:
        logger.warning("[%s] yydsmail: 缺少 token", email)
        return None

    base_url = _normalize_yydsmail_base_url(yydsmail_url)
    list_url = f"{base_url}/v1/messages?address={urllib.parse.quote(email)}"
    start = time.time()
    attempt = 0
    _consecutive_errors = 0
    _last_checked_at: dict[str, float] = {}
    _recheck_interval = 5.0
    while time.time() - start < timeout:
        if cancel is not None and cancel.is_set():
            logger.info("[%s] yydsmail 轮询被取消", email)
            return None
        attempt += 1
        if log_q is not None and attempt % 5 == 1 and attempt > 1:
            elapsed = int(time.time() - start)
            log_q.put(f"等待验证码 (yydsmail)... ({elapsed}s / {timeout}s)")
        try:
            req = urllib.request.Request(list_url)
            req.add_header("User-Agent", "Mozilla/5.0 (compatible; RegPlatform/1.0)")
            req.add_header("Authorization", f"Bearer {token}")
            with urllib.request.urlopen(req, timeout=10, context=_SSL_CTX) as resp:
                data = json.loads(resp.read())

            if not data.get("success"):
                _consecutive_errors += 1
                if _consecutive_errors >= 3:
                    logger.warning("[%s] yydsmail 连续错误，放弃", email)
                    return None
                time.sleep(2)
                continue

            _consecutive_errors = 0
            messages = data.get("data", {}).get("messages", [])
            for msg in messages:
                msg_id = msg.get("id", "")
                if msg_id:
                    last_checked = _last_checked_at.get(msg_id, 0.0)
                    if last_checked and time.time() - last_checked < _recheck_interval:
                        continue
                    _last_checked_at[msg_id] = time.time()

                subject = msg.get("subject", "") or ""
                sender = _normalize_sender(msg.get("from", {}))

                # 可选邮件过滤
                if email_filter and not email_filter(sender, subject):
                    continue

                # 先从 subject 提取
                code = _extract_otp(subject)
                if code:
                    logger.info("[%s] yydsmail subject 验证码: %s (第 %d 次)", email, code, attempt)
                    return code

                # 获取邮件详情
                if msg_id:
                    try:
                        detail_url = f"{base_url}/v1/messages/{urllib.parse.quote(msg_id, safe='')}"
                        dreq = urllib.request.Request(detail_url)
                        dreq.add_header("Authorization", f"Bearer {token}")
                        dreq.add_header("User-Agent", "Mozilla/5.0 (compatible; RegPlatform/1.0)")
                        with urllib.request.urlopen(dreq, timeout=10, context=_SSL_CTX) as dresp:
                            detail = json.loads(dresp.read())
                        if detail.get("success"):
                            d = detail.get("data", {})
                            for field in ("text", "html"):
                                for text in _iter_text_chunks(d.get(field, "")):
                                    code = _extract_otp(text)
                                    if code:
                                        logger.info("[%s] yydsmail %s 验证码: %s (第 %d 次)", email, field, code, attempt)
                                        return code
                    except Exception as e:
                        logger.debug("[%s] yydsmail 详情获取失败(id=%s): %s", email, msg_id, e)

        except urllib.error.HTTPError as exc:
            logger.warning("[%s] yydsmail HTTP %s (第%d次)", email, exc.code, attempt)
            _consecutive_errors += 1
            if _consecutive_errors >= 3:
                return None
        except Exception as exc:
            logger.warning("[%s] yydsmail 轮询出错(第%d次): %s", email, attempt, exc)
            _consecutive_errors += 1
            if _consecutive_errors >= 5:
                return None
        if cancel is not None:
            if cancel.wait(2):
                return None
        else:
            time.sleep(2)
    return None


def _normalize_yydsmail_base_url(raw: str) -> str:
    base_url = (raw or "").strip().rstrip("/")
    if base_url.endswith("/v1"):
        base_url = base_url[:-3].rstrip("/")
    return base_url or DEFAULT_YYDSMAIL_BASE_URL


# ─── Mail.tm 轮询 ─────────────────────────────────────────────────────────────

def _poll_mailtm(email: str, meta: dict, timeout: int = 120,
                 log_q: Optional[queue.Queue] = None,
                 cancel: Optional[Event] = None,
                 email_filter=None) -> Optional[str]:
    """轮询 Mail.tm 收件箱，提取验证码。email_filter 可切换 AWS/Google 过滤。"""
    _filter = email_filter or _is_aws_email
    token = meta.get("token", "")
    if not token:
        logger.warning("[%s] Mail.tm: 缺少 token", email)
        return None
    base_url = "https://api.mail.tm"
    start = time.time()
    attempt = 0
    _consecutive_errors = 0
    while time.time() - start < timeout:
        if cancel is not None and cancel.is_set():
            return None
        attempt += 1
        if log_q is not None and attempt % 5 == 1 and attempt > 1:
            log_q.put(f"等待验证码 (Mail.tm)... ({int(time.time() - start)}s / {timeout}s)")
        try:
            req = urllib.request.Request(f"{base_url}/messages")
            req.add_header("Authorization", f"Bearer {token}")
            req.add_header("Accept", "application/json")
            with urllib.request.urlopen(req, timeout=10, context=_SSL_CTX) as resp:
                data = json.loads(resp.read())
            _consecutive_errors = 0
            members = data.get("hydra:member", data) if isinstance(data, dict) else data
            if not isinstance(members, list):
                members = []
            for msg in members:
                if not isinstance(msg, dict):
                    continue
                subject = msg.get("subject", "") or ""
                sender = msg.get("from", {})
                if isinstance(sender, dict):
                    sender = sender.get("address", "")
                elif isinstance(sender, list) and sender:
                    sender = sender[0].get("address", "") if isinstance(sender[0], dict) else str(sender[0])
                else:
                    sender = str(sender) if sender else ""
                if not _filter(sender, subject):
                    continue
                code = _extract_otp(subject)
                if code:
                    logger.info("[%s] Mail.tm 从 subject 提取验证码: %s", email, code)
                    return code
                msg_id = msg.get("id", "")
                if not msg_id:
                    continue
                try:
                    detail_req = urllib.request.Request(f"{base_url}/messages/{msg_id}")
                    detail_req.add_header("Authorization", f"Bearer {token}")
                    detail_req.add_header("Accept", "application/json")
                    with urllib.request.urlopen(detail_req, timeout=10, context=_SSL_CTX) as dresp:
                        detail = json.loads(dresp.read())
                    for field in ("text", "html", "intro"):
                        text = detail.get(field, "") or ""
                        if text:
                            code = _extract_otp(text)
                            if code:
                                logger.info("[%s] Mail.tm 从 %s 提取验证码: %s", email, field, code)
                                return code
                except Exception as e:
                    logger.warning("[%s] Mail.tm 读取消息详情失败: %s", email, e)
        except Exception as exc:
            logger.warning("[%s] Mail.tm 轮询出错(第%d次): %s", email, attempt, exc)
            _consecutive_errors += 1
            if _consecutive_errors >= 5:
                logger.warning("[%s] Mail.tm 连续 %d 次错误，放弃轮询", email, _consecutive_errors)
                return None
        if cancel is not None:
            if cancel.wait(3):
                return None
        else:
            time.sleep(3)
    return None


# ─── Guerrilla Mail 轮询 ──────────────────────────────────────────────────────

def _poll_guerrillamail(email: str, meta: dict, timeout: int = 120,
                        log_q: Optional[queue.Queue] = None,
                        cancel: Optional[Event] = None,
                        email_filter=None) -> Optional[str]:
    """轮询 Guerrilla Mail 收件箱，提取验证码。email_filter 可切换 AWS/Google 过滤。"""
    _filter = email_filter or _is_aws_email
    sid_token = meta.get("sid_token", "")
    if not sid_token:
        logger.warning("[%s] GuerrillaMail: 缺少 sid_token", email)
        return None
    base_url = "https://api.guerrillamail.com/ajax.php"
    start = time.time()
    attempt = 0
    _consecutive_errors = 0
    while time.time() - start < timeout:
        if cancel is not None and cancel.is_set():
            return None
        attempt += 1
        if log_q is not None and attempt % 5 == 1 and attempt > 1:
            log_q.put(f"等待验证码 (GuerrillaMail)... ({int(time.time() - start)}s / {timeout}s)")
        try:
            url = f"{base_url}?f=check_email&seq=0&sid_token={sid_token}"
            req = urllib.request.Request(url)
            with urllib.request.urlopen(req, timeout=10, context=_SSL_CTX) as resp:
                data = json.loads(resp.read())
            _consecutive_errors = 0
            mail_list = data.get("list", [])
            for msg in mail_list:
                if not isinstance(msg, dict):
                    continue
                subject = msg.get("mail_subject", "") or ""
                sender = msg.get("mail_from", "") or ""
                body = msg.get("mail_body", "") or ""
                if "Welcome to Guerrilla Mail" in subject:
                    continue
                if not _filter(sender, subject):
                    continue
                for text in (subject, body):
                    if text:
                        code = _extract_otp(text)
                        if code:
                            logger.info("[%s] GuerrillaMail 提取验证码: %s", email, code)
                            return code
        except Exception as exc:
            logger.warning("[%s] GuerrillaMail 轮询出错(第%d次): %s", email, attempt, exc)
            _consecutive_errors += 1
            if _consecutive_errors >= 5:
                logger.warning("[%s] GuerrillaMail 连续 %d 次错误，放弃轮询", email, _consecutive_errors)
                return None
        if cancel is not None:
            if cancel.wait(3):
                return None
        else:
            time.sleep(3)
    return None


# ─── tempmail.lol 轮询 ────────────────────────────────────────────────────────

def _poll_templol(email: str, meta: dict, timeout: int = 120,
                  log_q: Optional[queue.Queue] = None,
                  cancel: Optional[Event] = None,
                  email_filter=None) -> Optional[str]:
    """轮询 tempmail.lol 收件箱，提取验证码。email_filter 可切换 AWS/Google 过滤。"""
    _filter = email_filter or _is_aws_email
    token = meta.get("token", "")
    if not token:
        logger.warning("[%s] tempmail.lol: 缺少 token", email)
        return None
    base_url = "https://api.tempmail.lol"
    start = time.time()
    attempt = 0
    _consecutive_errors = 0
    while time.time() - start < timeout:
        if cancel is not None and cancel.is_set():
            return None
        attempt += 1
        if log_q is not None and attempt % 5 == 1 and attempt > 1:
            log_q.put(f"等待验证码 (tempmail.lol)... ({int(time.time() - start)}s / {timeout}s)")
        try:
            url = f"{base_url}/auth/{token}"
            req = urllib.request.Request(url)
            with urllib.request.urlopen(req, timeout=10, context=_SSL_CTX) as resp:
                data = json.loads(resp.read())
            _consecutive_errors = 0
            emails_list = data.get("email", [])
            for msg in emails_list:
                if not isinstance(msg, dict):
                    continue
                subject = msg.get("subject", "") or ""
                sender = msg.get("from", "") or ""
                body = msg.get("body", "") or msg.get("html", "") or ""
                if not _filter(sender, subject):
                    continue
                for text in (subject, body):
                    if text:
                        code = _extract_otp(text)
                        if code:
                            logger.info("[%s] tempmail.lol 提取验证码: %s (第 %d 次轮询)", email, code, attempt)
                            return code
        except Exception as exc:
            logger.warning("[%s] tempmail.lol 轮询出错(第%d次): %s", email, attempt, exc)
            _consecutive_errors += 1
            if _consecutive_errors >= 5:
                logger.warning("[%s] tempmail.lol 连续 %d 次错误，放弃轮询", email, _consecutive_errors)
                return None
        if cancel is not None:
            if cancel.wait(3):
                return None
        else:
            time.sleep(3)
    return None


# ─── mail.gw 轮询 ────────────────────────────────────────────────────────────

def _poll_mailgw(email: str, meta: dict, timeout: int = 120,
                 log_q: Optional[queue.Queue] = None,
                 cancel: Optional[Event] = None,
                 email_filter=None) -> Optional[str]:
    """轮询 mail.gw 收件箱，提取验证码。API 与 Mail.tm 完全一致。email_filter 可切换 AWS/Google 过滤。"""
    _filter = email_filter or _is_aws_email
    token = meta.get("token", "")
    if not token:
        logger.warning("[%s] mail.gw: 缺少 token", email)
        return None
    base_url = "https://api.mail.gw"
    start = time.time()
    attempt = 0
    _consecutive_errors = 0
    while time.time() - start < timeout:
        if cancel is not None and cancel.is_set():
            return None
        attempt += 1
        if log_q is not None and attempt % 5 == 1 and attempt > 1:
            log_q.put(f"等待验证码 (mail.gw)... ({int(time.time() - start)}s / {timeout}s)")
        try:
            req = urllib.request.Request(f"{base_url}/messages")
            req.add_header("Authorization", f"Bearer {token}")
            req.add_header("Accept", "application/json")
            with urllib.request.urlopen(req, timeout=10, context=_SSL_CTX) as resp:
                data = json.loads(resp.read())
            _consecutive_errors = 0
            members = data.get("hydra:member", data) if isinstance(data, dict) else data
            if not isinstance(members, list):
                members = []
            for msg in members:
                if not isinstance(msg, dict):
                    continue
                subject = msg.get("subject", "") or ""
                sender = msg.get("from", {})
                if isinstance(sender, dict):
                    sender = sender.get("address", "")
                elif isinstance(sender, list) and sender:
                    sender = sender[0].get("address", "") if isinstance(sender[0], dict) else str(sender[0])
                else:
                    sender = str(sender) if sender else ""
                if not _filter(sender, subject):
                    continue
                code = _extract_otp(subject)
                if code:
                    logger.info("[%s] mail.gw 从 subject 提取验证码: %s (第 %d 次轮询)", email, code, attempt)
                    return code
                msg_id = msg.get("id", "")
                if not msg_id:
                    continue
                try:
                    detail_req = urllib.request.Request(f"{base_url}/messages/{msg_id}")
                    detail_req.add_header("Authorization", f"Bearer {token}")
                    detail_req.add_header("Accept", "application/json")
                    with urllib.request.urlopen(detail_req, timeout=10, context=_SSL_CTX) as dresp:
                        detail = json.loads(dresp.read())
                    for field in ("text", "html", "intro"):
                        text = detail.get(field, "") or ""
                        if text:
                            code = _extract_otp(text)
                            if code:
                                logger.info("[%s] mail.gw 从 %s 提取验证码: %s", email, field, code)
                                return code
                except Exception as e:
                    logger.warning("[%s] mail.gw 读取消息详情失败: %s", email, e)
        except Exception as exc:
            logger.warning("[%s] mail.gw 轮询出错(第%d次): %s", email, attempt, exc)
            _consecutive_errors += 1
            if _consecutive_errors >= 5:
                logger.warning("[%s] mail.gw 连续 %d 次错误，放弃轮询", email, _consecutive_errors)
                return None
        if cancel is not None:
            if cancel.wait(3):
                return None
        else:
            time.sleep(3)
    return None


# ─── temp-mail.io 轮询 ───────────────────────────────────────────────────────

def _poll_tempmailio(email: str, meta: dict, timeout: int = 120,
                     log_q: Optional[queue.Queue] = None,
                     cancel: Optional[Event] = None,
                     email_filter=None) -> Optional[str]:
    """轮询 temp-mail.io 收件箱，提取验证码。email_filter 可切换 AWS/Google 过滤。"""
    _filter = email_filter or _is_aws_email
    token = meta.get("token", "")
    if not token:
        logger.warning("[%s] temp-mail.io: 缺少 token", email)
        return None
    base_url = "https://api.internal.temp-mail.io/api/v3"
    start = time.time()
    attempt = 0
    _consecutive_errors = 0
    while time.time() - start < timeout:
        if cancel is not None and cancel.is_set():
            return None
        attempt += 1
        if log_q is not None and attempt % 5 == 1 and attempt > 1:
            log_q.put(f"等待验证码 (temp-mail.io)... ({int(time.time() - start)}s / {timeout}s)")
        try:
            url = f"{base_url}/email/{email}/messages"
            req = urllib.request.Request(url)
            req.add_header("Authorization", f"Bearer {token}")
            with urllib.request.urlopen(req, timeout=10, context=_SSL_CTX) as resp:
                data = json.loads(resp.read())
            _consecutive_errors = 0
            if not isinstance(data, list):
                data = []
            for msg in data:
                if not isinstance(msg, dict):
                    continue
                subject = msg.get("subject", "") or ""
                sender = msg.get("from", "") or ""
                body = msg.get("body_text", "") or msg.get("body_html", "") or ""
                if not _filter(sender, subject):
                    continue
                for text in (subject, body):
                    if text:
                        code = _extract_otp(text)
                        if code:
                            logger.info("[%s] temp-mail.io 提取验证码: %s (第 %d 次轮询)", email, code, attempt)
                            return code
        except Exception as exc:
            logger.warning("[%s] temp-mail.io 轮询出错(第%d次): %s", email, attempt, exc)
            _consecutive_errors += 1
            if _consecutive_errors >= 5:
                logger.warning("[%s] temp-mail.io 连续 %d 次错误，放弃轮询", email, _consecutive_errors)
                return None
        if cancel is not None:
            if cancel.wait(3):
                return None
        else:
            time.sleep(3)
    return None


# ─── Mailfree 自建邮箱轮询 ────────────────────────────────────────────────────

def _poll_mailfree(email: str, meta: dict, timeout: int = 120,
                   log_q: Optional[queue.Queue] = None,
                   cancel: Optional[Event] = None,
                   email_filter=None) -> Optional[str]:
    """轮询 mailfree 自建 Cloudflare Workers 邮箱服务，提取验证码。
    mailfree API 自带 verification_code 字段，优先使用服务端提取结果。
    email_filter 可切换 AWS/Google 过滤（用于 fallback 匹配）。"""
    _filter = email_filter or _is_aws_email
    base_url = meta.get("base_url", "").rstrip("/")
    admin_token = meta.get("admin_token", "")
    if not base_url or not admin_token:
        logger.warning("[%s] mailfree: 缺少 base_url 或 admin_token", email)
        return None

    api_url = f"{base_url}/api/emails?mailbox={urllib.parse.quote(email)}"
    _opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({}),
        urllib.request.HTTPSHandler(context=_SSL_CTX),
    )
    start = time.time()
    attempt = 0
    _consecutive_errors = 0

    while time.time() - start < timeout:
        if cancel is not None and cancel.is_set():
            return None
        attempt += 1
        if log_q is not None and attempt % 5 == 1 and attempt > 1:
            log_q.put(f"等待验证码 (mailfree)... ({int(time.time() - start)}s / {timeout}s)")
        try:
            req = urllib.request.Request(api_url)
            req.add_header("Authorization", f"Bearer {admin_token}")
            req.add_header("User-Agent", "Mozilla/5.0 (compatible; RegPlatform/1.0)")
            with _opener.open(req, timeout=10) as resp:
                data = json.loads(resp.read())

            _consecutive_errors = 0

            if not isinstance(data, list):
                data = []

            for msg in data:
                if not isinstance(msg, dict):
                    continue

                # 优先使用服务端提取的验证码
                vc = msg.get("verification_code", "")
                if vc:
                    logger.info("[%s] mailfree 服务端验证码: %s (第 %d 次轮询)", email, vc, attempt)
                    return str(vc)

                subject = msg.get("subject", "") or ""
                sender = msg.get("from", "") or ""
                if _filter and not _filter(sender, subject):
                    continue

                # 列表接口只返回截断文本，需要拉取完整邮件内容
                email_id = msg.get("id", "")
                if email_id:
                    try:
                        detail_url = f"{base_url}/api/email/{urllib.parse.quote(str(email_id), safe='')}"
                        dreq = urllib.request.Request(detail_url)
                        dreq.add_header("Authorization", f"Bearer {admin_token}")
                        dreq.add_header("User-Agent", "Mozilla/5.0 (compatible; RegPlatform/1.0)")
                        with _opener.open(dreq, timeout=10) as dresp:
                            full_msg = json.loads(dresp.read())
                        if isinstance(full_msg, dict):
                            # 合并完整内容到 msg
                            for k, v in full_msg.items():
                                if v and not msg.get(k):
                                    msg[k] = v
                    except Exception as e:
                        logger.debug("[%s] mailfree 邮件详情获取失败(id=%s): %s", email, email_id, e)

                for field in ("subject", "content", "html_content", "text_content", "body", "text"):
                    text = msg.get(field, "") or ""
                    if text:
                        code = _extract_otp(text)
                        if code:
                            logger.info("[%s] mailfree 客户端提取验证码: %s (字段: %s, 第 %d 次轮询)", email, code, field, attempt)
                            return code

        except Exception as exc:
            logger.warning("[%s] mailfree 轮询出错(第%d次): %s", email, attempt, exc)
            _consecutive_errors += 1
            if _consecutive_errors >= 5:
                logger.warning("[%s] mailfree 连续 %d 次错误，放弃轮询", email, _consecutive_errors)
                return None

        if cancel is not None:
            if cancel.wait(2):
                return None
        else:
            time.sleep(2)
    return None


# ─── 验证码轮询 provider 分发 ────────────────────────────────────────────────

def _poll_by_provider(email: str, provider: str, meta: dict,
                      yydsmail_url: str = "", yydsmail_key: str = "",
                      timeout: int = 120,
                      log_q: Optional[queue.Queue] = None,
                      cancel: Optional[Event] = None,
                      email_filter=None) -> Optional[str]:
    provider_key = (provider or meta.get("provider") or "yydsmail").strip().lower()

    if provider_key == "yydsmail":
        return _poll_yydsmail(yydsmail_url, yydsmail_key, email, meta,
                              timeout=timeout, log_q=log_q, cancel=cancel,
                              email_filter=email_filter)
    if provider_key in {"mailtm", "mail.tm"}:
        return _poll_mailtm(email, meta, timeout=timeout, log_q=log_q, cancel=cancel,
                            email_filter=email_filter)
    if provider_key in {"guerrillamail", "guerrilla", "guerrilla_mail"}:
        return _poll_guerrillamail(email, meta, timeout=timeout, log_q=log_q, cancel=cancel,
                                   email_filter=email_filter)
    if provider_key in {"templol", "tempmail.lol", "tempmail_lol"}:
        return _poll_templol(email, meta, timeout=timeout, log_q=log_q, cancel=cancel,
                             email_filter=email_filter)
    if provider_key in {"mailgw", "mail.gw"}:
        return _poll_mailgw(email, meta, timeout=timeout, log_q=log_q, cancel=cancel,
                            email_filter=email_filter)
    if provider_key in {"tempmailio", "temp-mail.io", "temp_mail_io"}:
        return _poll_tempmailio(email, meta, timeout=timeout, log_q=log_q, cancel=cancel,
                                email_filter=email_filter)
    if provider_key == "mailfree":
        return _poll_mailfree(email, meta, timeout=timeout, log_q=log_q, cancel=cancel,
                              email_filter=email_filter)

    logger.warning("[%s] 未知邮箱 provider=%s，回退 yydsmail 轮询", email, provider_key)
    return _poll_yydsmail(yydsmail_url, yydsmail_key, email, meta,
                          timeout=timeout, log_q=log_q, cancel=cancel,
                          email_filter=email_filter)


# ─── 统一验证码轮询分发 ───────────────────────────────────────────────────────

def _poll_verification_code(email: str, provider: str, meta: dict,
                            yydsmail_url: str = "", yydsmail_key: str = "",
                            timeout: int = 120,
                            log_q: Optional[queue.Queue] = None,
                            cancel: Optional[Event] = None) -> Optional[str]:
    """验证码轮询（按 provider 分发，AWS 使用默认过滤规则）。"""
    logger.info("[%s] 验证码轮询: provider=%s", email, provider)
    return _poll_by_provider(email, provider, meta,
                             yydsmail_url=yydsmail_url, yydsmail_key=yydsmail_key,
                             timeout=timeout, log_q=log_q, cancel=cancel)


# ─── Gemini 验证码轮询 ──────────────────────────────────────────────────────

def _poll_gemini_verification_code(email: str, provider: str, meta: dict,
                                    yydsmail_url: str = "", yydsmail_key: str = "",
                                    timeout: int = 120,
                                    log_q: Optional[queue.Queue] = None,
                                    cancel: Optional[Event] = None) -> Optional[str]:
    """Gemini 验证码轮询（按 provider 分发，并套用 Google 邮件过滤）。"""
    logger.info("[%s] Gemini 验证码轮询: provider=%s", email, provider)
    return _poll_by_provider(email, provider, meta,
                             yydsmail_url=yydsmail_url, yydsmail_key=yydsmail_key,
                             timeout=timeout, log_q=log_q, cancel=cancel,
                             email_filter=_is_google_email)


# ─── Playwright 页面交互辅助 ─────────────────────────────────────────────────

async def _pw_click_next(page, lq=None):
    """
    点击注册流程的「下一步」按钮，多选择器兜底。
    Playwright locator 模式，不再依赖 WebDriverWait。
    """
    # 精确 data-testid 选择器（优先）
    selectors = [
        '[data-testid="email-verification-verify-button"]',
        '[data-testid="signup-next-button"]',
        '[data-testid="test-primary-button"]',
        'button[type="submit"]',
        'input[type="submit"]',
    ]
    for sel in selectors:
        try:
            btn = page.locator(sel).first
            if await btn.count() > 0 and await btn.is_visible():
                if lq:
                    lq(f"点击按钮: {sel[:60]}")
                await btn.click()
                return True
        except Exception:
            pass

    # 文本匹配兜底
    for text in ["Next", "Continue", "继续", "Create", "Verify", "Submit", "Confirm"]:
        try:
            btn = page.locator(f"button:has-text('{text}')").first
            if await btn.count() > 0 and await btn.is_visible():
                if lq:
                    lq(f"点击按钮: text={text}")
                await btn.click()
                return True
        except Exception:
            pass

    # 按钮 class 兜底
    try:
        btn = page.locator("button.primary, button[class*='primary']").first
        if await btn.count() > 0 and await btn.is_visible():
            if lq:
                lq("点击按钮: class=primary")
            await btn.click()
            return True
    except Exception:
        pass

    # 最后手段：Enter 键
    if lq:
        lq("[!] 找不到下一步按钮，尝试 Enter 键提交")
    try:
        await page.keyboard.press("Enter")
        return True
    except Exception:
        pass
    return False


async def _pw_wait_navigation(page, old_url: str, timeout: float = 15,
                              expect_selector: str = "", lq=None):
    """
    等待页面 URL 变化或目标元素出现（SPA 场景）。
    表单提交后调用，确认页面已跳转再进入下一步。
    expect_selector: 期望出现的目标元素选择器，避免误判。
    """
    interval = 0.5
    elapsed = 0.0
    while elapsed < timeout:
        if page.url != old_url:
            if lq:
                lq(f"页面已跳转: {page.url[:80]}")
            return True
        # SPA 路由：URL 不变但目标元素已出现
        if expect_selector and elapsed > 2:
            try:
                cnt = await page.locator(expect_selector).count()
                if cnt > 0:
                    if lq:
                        lq("页面内容已更新（检测到目标元素）")
                    return True
            except Exception:
                pass
        await asyncio.sleep(interval)
        elapsed += interval
    if lq:
        lq(f"[!] 等待页面跳转超时（{timeout}s），URL: {page.url[:80]}")
    return False


async def _pw_dismiss_cookie(page):
    """快速处理 AWS Cookie 弹窗（接受/拒绝都行，只要关掉）"""
    for sel in [
        "button.awsccc-u-btn-primary",
        "button[id*='truste-consent']",
    ]:
        try:
            btn = page.locator(sel).first
            if await btn.count() > 0 and await btn.is_visible():
                await btn.click()
                await asyncio.sleep(0.8)
                return
        except Exception:
            pass
    # 文本匹配：AWS cookie 弹窗按钮通常包含 awsccc class
    for text in ["Accept", "接受", "Decline", "拒绝"]:
        try:
            # 限定 awsccc 或 cookie 相关容器内的按钮
            btn = page.locator(f".awsccc-cs-container button:has-text('{text}')").first
            if await btn.count() > 0 and await btn.is_visible():
                await btn.click()
                await asyncio.sleep(0.8)
                return
        except Exception:
            pass


async def _pw_check_success(page) -> bool:
    """通过页面内容判断注册是否真正成功"""
    try:
        page_text = (await page.locator("body").inner_text()).lower()
        success_keywords = [
            "successfully created", "account created", "welcome to",
            "you're all set", "get started", "verification complete",
            "your account is ready", "sign in", "builder id created",
            "请求已批准", "request approved", "您可以关闭此窗口",
        ]
        return any(kw in page_text for kw in success_keywords)
    except Exception:
        return False


# ─── 代理处理 ────────────────────────────────────────────────────────────────

_PROXY_RE = re.compile(
    r'^(https?|socks[45])://[a-zA-Z0-9._\-:@]+(:\d{1,5})?(/[^\s]*)?$'
)

# 允许的 region 值
_ALLOWED_REGIONS = {"usa", "germany", "japan"}


def _mask_proxy(proxy: str) -> str:
    """脱敏代理 URL，隐藏用户名和密码。http://user:pass@host:port → http://***@host:port"""
    if not proxy or "@" not in proxy:
        return proxy or ""
    scheme_rest = proxy.split("://", 1)
    if len(scheme_rest) != 2:
        return "***"
    scheme, rest = scheme_rest
    host_part = rest.split("@", 1)[-1]
    return f"{scheme}://***@{host_part}"


def _validate_proxy(proxy: str) -> Optional[str]:
    """校验代理 URL 格式，防止恶意字符串注入。"""
    if not proxy:
        return None
    proxy = proxy.strip()
    if not _PROXY_RE.match(proxy):
        logger.warning("代理格式无效，已拒绝: %r", proxy[:100])
        return None
    # Docker 容器内 127.0.0.1 指向容器自身，需替换为宿主机地址
    if os.path.exists("/.dockerenv"):
        proxy = proxy.replace("127.0.0.1", "host.docker.internal")
        proxy = proxy.replace("localhost", "host.docker.internal")
    return proxy


def _parse_proxy_for_playwright(proxy_str: str) -> Optional[dict]:
    """将代理 URL 解析为 Playwright proxy 配置格式。"""
    if not proxy_str:
        return None
    # 格式: scheme://user:pass@host:port 或 scheme://host:port
    if "://" in proxy_str and "@" in proxy_str:
        scheme, rest = proxy_str.split("://", 1)
        auth, addr = rest.rsplit("@", 1)
        user, pwd = auth.split(":", 1)
        return {
            "server": f"{scheme}://{addr}",
            "username": user,
            "password": pwd,
        }
    return {"server": proxy_str}


# ─── AWS OIDC Device Flow ────────────────────────────────────────────────────

def _curl_post(url: str, payload: dict, headers: dict,
               proxy: Optional[str] = None, timeout: int = 30,
               retries: int = 3) -> dict:
    """HTTPS POST via curl — 代理 SSL 隧道不稳定时自动重试。"""
    cmd = ["curl", "-s", "-X", "POST", url,
           "--max-time", str(timeout), "--connect-timeout", "10",
           "--tlsv1.2", "-H", "Content-Type: application/json",
           "--retry", "2", "--retry-delay", "1"]
    for k, v in headers.items():
        if k.lower() != "content-type":
            cmd.extend(["-H", f"{k}: {v}"])
    cmd.extend(["-d", json.dumps(payload)])
    if proxy:
        cmd.extend(curl_proxy_args(proxy))  # 代理链：--preproxy Clash
        cmd.extend(["--proxy", proxy, "--insecure", "--proxy-insecure"])

    last_err = None
    for attempt in range(1, retries + 1):
        try:
            result = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout + 10)
            if result.returncode == 0 and result.stdout.strip():
                return json.loads(result.stdout)
            stderr_info = result.stderr.strip()[:300] if result.stderr else "(no stderr)"
            last_err = f"curl exit {result.returncode}: {stderr_info}"
        except subprocess.TimeoutExpired:
            last_err = f"curl subprocess timeout ({timeout + 10}s)"
        except Exception as e:
            last_err = str(e)
        if attempt < retries:
            delay = attempt * 3  # 递增重试间隔: 3s, 6s
            logger.warning("_curl_post 第 %d 次失败: %s，%ds 后重试...", attempt, last_err, delay)
            time.sleep(delay)
    raise RuntimeError(last_err or "curl failed")

def _oidc_get_verification_url(proxy: Optional[str] = None, log_q=None):
    """
    通过 AWS OIDC Device Flow 获取新鲜的 verificationUriComplete。
    每次调用都向 AWS API 请求全新 session，不会像硬编码 workflowID 那样过期。

    流程：
      1. POST /client/register  → clientId + clientSecret
      2. POST /device_authorization → verificationUriComplete + deviceCode

    返回: (verificationUriComplete, deviceCode, clientId, clientSecret, interval)
    """
    def lq(msg):
        logger.info("[oidc] %s", msg)
        if log_q:
            log_q.put(msg)

    # OIDC API 通过 curl 走代理（urllib HTTPS 隧道 SSL 不稳定）
    def _oidc_post(path, payload):
        return _curl_post(f"{OIDC_BASE_URL}{path}", payload, _OIDC_HEADERS, proxy=proxy)

    # Step 1: 注册 OIDC 公共客户端
    lq("OIDC Step 1: 注册客户端...")
    reg_data = _oidc_post("/client/register", {
        "clientName": "Amazon Q Developer for command line",
        "clientType": "public",
        "scopes": [
            "codewhisperer:completions",
            "codewhisperer:analysis",
            "codewhisperer:conversations",
        ],
        "grantTypes": [
            "urn:ietf:params:oauth:grant-type:device_code",
            "refresh_token",
        ],
        "issuerUrl": OIDC_START_URL,
    })

    client_id = reg_data["clientId"]
    client_secret = reg_data["clientSecret"]
    lq(f"OIDC: 客户端已注册 ({client_id[:24]}...)")

    # Step 2: 发起设备授权，拿到 verificationUriComplete
    lq("OIDC Step 2: 获取设备授权 URL...")
    auth_data = _oidc_post("/device_authorization", {
        "clientId": client_id,
        "clientSecret": client_secret,
        "startUrl": OIDC_START_URL,
    })

    verification_url = auth_data["verificationUriComplete"]
    device_code = auth_data["deviceCode"]
    user_code = auth_data.get("userCode", "")
    interval = max(int(auth_data.get("interval", 5)), 5)

    lq(f"OIDC: 授权 URL 已获取，user_code={user_code}")
    return verification_url, device_code, client_id, client_secret, interval


def _oidc_poll_token(client_id, client_secret, device_code, interval=5,
                     proxy=None, timeout=60, log_q=None):
    """
    OIDC Device Flow 最后一步：轮询 /token 获取 access_token + refresh_token。
    在浏览器完成设备授权确认（Confirm + Allow）后调用。

    返回: dict（含 accessToken, refreshToken 等）或 None（超时/错误）
    """
    def lq(msg):
        logger.info("[oidc-token] %s", msg)
        if log_q:
            log_q.put(msg)

    # OIDC token 轮询通过 curl 走代理
    lq("开始轮询 OIDC token...")
    start = time.time()
    while time.time() - start < timeout:
        try:
            data = _curl_post(
                f"{OIDC_BASE_URL}/token",
                {
                    "clientId": client_id,
                    "clientSecret": client_secret,
                    "deviceCode": device_code,
                    "grantType": "urn:ietf:params:oauth:grant-type:device_code",
                },
                _OIDC_HEADERS, proxy=proxy, timeout=15,
            )

            if "accessToken" in data:
                lq("OIDC token 获取成功")
                return data
            # authorization_pending / slow_down 在正常 JSON 响应中
            if "slow_down" in str(data).lower():
                interval = min(interval + 2, 15)
        except RuntimeError as exc:
            err_str = str(exc).lower()
            if "authorization_pending" in err_str:
                pass
            elif "slow_down" in err_str:
                interval = min(interval + 2, 15)
            else:
                lq(f"OIDC token 轮询错误: {exc}")
                return None
        except Exception as e:
            lq(f"OIDC token 轮询异常: {e}")
            return None

        time.sleep(interval)

    lq("OIDC token 轮询超时")
    return None


# ─── 核心注册逻辑（异步，Camoufox + Playwright）──────────────────────────────

async def _do_register(
    email: str,
    proxy: Optional[str],
    yydsmail_url: str,
    yydsmail_key: str,
    region: str = "usa",
    log_q: Optional[queue.Queue] = None,
    cancel: Optional[Event] = None,
    mail_provider: str = "",
    mail_meta: Optional[dict] = None,
) -> dict:
    def lq(msg: str):
        """同时写入 logger 和流式日志队列"""
        logger.info("[%s] %s", email, msg)
        if log_q is not None:
            log_q.put(msg)

    def _cancelled() -> bool:
        return cancel is not None and cancel.is_set()

    domain = _get_email_domain(email)

    first_name, last_name = _random_name()
    full_name = f"{first_name} {last_name}"
    password = _generate_password()

    # IP 地理位置自动检测（同步函数，放线程执行）
    ip_loc = await asyncio.to_thread(_detect_ip_location, proxy)
    tz = ip_loc["timezone"]
    geo = ip_loc["geo"]
    detected_locale = ip_loc["locale"]
    lq(f"IP 地理位置: {ip_loc['country']} tz={tz} locale={detected_locale}")

    browser = None
    context = None
    page = None

    try:
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}

        # ── Camoufox + OIDC 并行启动 ──────────────────────────────────────
        lq("并行启动: Camoufox + OIDC Device Flow...")

        # OIDC 在后台线程跑（纯 HTTP，不依赖浏览器）
        oidc_task = asyncio.create_task(asyncio.to_thread(
            _oidc_get_verification_url, proxy=proxy, log_q=log_q,
        ))

        # Headless 模式控制（与旧版逻辑一致）
        import sys as _sys
        _headless_env = os.getenv("KIRO_HEADLESS", "").lower()
        if _headless_env in ("false", "0", "no"):
            _use_headless = False
        elif _headless_env in ("true", "1", "yes"):
            _use_headless = True
        else:
            _use_headless = _sys.platform not in ("darwin", "win32") and not os.getenv("DISPLAY")

        # 启动 Camoufox 浏览器实例（每次注册独立实例，指纹完全隔离）
        cf = AsyncCamoufox(headless=_use_headless)
        browser = await cf.start()

        # 构建 Playwright context 选项（代理 + 地理 + locale + 时区）
        context_opts = {
            "locale": detected_locale,
            "timezone_id": tz,
            "geolocation": {"latitude": geo["latitude"], "longitude": geo["longitude"], "accuracy": geo["accuracy"]},
            "permissions": ["geolocation"],
        }

        if proxy:
            proxy = _validate_proxy(proxy)
            if proxy:
                # 代理链：浏览器连本地链式代理，经 Clash 转发到后端代理
                chained = chain_proxy(proxy)
                pw_proxy = _parse_proxy_for_playwright(chained)
                if pw_proxy:
                    context_opts["proxy"] = pw_proxy
                    logger.info("[%s] 使用代理: %s (chain→%s)", email, _mask_proxy(proxy), chained.split(":")[-1] if "127.0.0.1" in chained else "direct")
            else:
                lq("[!] 代理格式无效，已忽略")

        context = await browser.new_context(**context_opts)
        page = await context.new_page()
        lq("浏览器已启动")

        # 等待 OIDC 结果（Chrome 启动期间已在并行执行，通常此时已完成）
        try:
            oidc_result = await asyncio.wait_for(oidc_task, timeout=30)
        except asyncio.TimeoutError:
            lq("[!] OIDC API 超时（30s 内未返回）")
            return {"ok": False, "error": "AWS OIDC API 超时"}
        except Exception as exc:
            lq(f"[!] OIDC API 调用失败: {exc}")
            return {"ok": False, "error": f"AWS OIDC API 失败: {exc}"}

        signup_url, _device_code, _oidc_cid, _oidc_csec, _oidc_itv = oidc_result

        # ── Step 1: 打开注册链接 ──────────────────────────────────────────
        lq(f"注册链接: {signup_url[:80]}...")
        await page.goto(signup_url, wait_until="domcontentloaded", timeout=60000)
        await asyncio.sleep(1)
        lq(f"页面: {(await page.title())[:60]} | {page.url[:80]}")

        # 随机初始延迟（模拟人类初次浏览）
        await asyncio.sleep(random.uniform(0.5, 1.5))

        # Cookie 弹窗
        await _pw_dismiss_cookie(page)

        # ── Step 2: 设备确认页（SPA 渲染需要时间，轮询等待按钮出现）────
        # 扩展等待时间到 60s，并增加更多选择器以覆盖 AWS UI 变化
        _confirm_clicked = False
        _initial_url = page.url
        lq(f"初始 URL: {_initial_url[:80]}")

        # 尝试更多选择器：按 text 内容、CSS class、aria-label 等
        _button_selectors = [
            "button#cli_verification_btn",
            "button[data-testid='confirm-device-button']",
            "button:has-text('Confirm')",
            "button:has-text('Continue')",
            "button:has-text('confirm')",
            "button.awsui-button:has-text('Confirm')",
            "[data-testid='confirm-button']",
            "button[type='submit']",
        ]

        for _wait in range(120):  # 最多等 60s（120 × 0.5s）
            if _cancelled():
                return {"ok": False, "error": "任务已取消"}

            # 检查 URL 是否已变化（可能页面已自动跳转）
            if page.url != _initial_url and "device" not in page.url:
                lq(f"[+] 页面已自动跳转: {page.url[:80]}")
                _confirm_clicked = True  # 标记为已确认（自动跳转）
                break

            for sel in _button_selectors:
                try:
                    btn = page.locator(sel).first
                    if await btn.count() > 0:
                        # 检查按钮是否可见（避免点击隐藏元素）
                        try:
                            if await btn.is_visible(timeout=1000):
                                lq(f"设备确认页：点击按钮 ({sel})...")
                                await btn.click()
                                _confirm_clicked = True
                                await asyncio.sleep(2)
                                lq(f"确认后 URL: {page.url[:80]}")
                                break
                        except Exception:
                            # is_visible() 可能失败，尝试直接点击
                            lq(f"设备确认页：尝试点击按钮 ({sel})...")
                            await btn.click(force=True)
                            _confirm_clicked = True
                            await asyncio.sleep(2)
                            lq(f"确认后 URL: {page.url[:80]}")
                            break
                except Exception:
                    continue

            if _confirm_clicked:
                break

            # 每 10 秒输出一次进度
            if _wait > 0 and _wait % 20 == 0:
                lq(f"[*] 仍在等待设备确认... ({_wait * 0.5:.0f}s/60s)")

            await asyncio.sleep(0.5)

        if not _confirm_clicked:
            lq("[!] 设备确认按钮未找到（60s 超时）")
            # 如果仍在设备授权页面，说明需要手动确认，返回明确错误
            if "device" in page.url and "user_code" in page.url:
                lq(f"[!] 仍在设备授权页面，需要手动确认: {page.url[:80]}")
                return {
                    "ok": False,
                    "error": "设备授权超时：页面停留在设备确认页（需在另一设备上确认），无法自动继续",
                    "retriable": False,
                }
            lq("[!] 设备确认按钮未找到，尝试继续...")

        # ── Step 3: 填写邮箱 ─────────────────────────────────────────────
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}
        lq(f"填写邮箱: {email} | 当前页: {page.url[:80]}")

        # 等待 SPA 渲染完成（AWS signin 是 React SPA，domcontentloaded 时 UI 还没挂载）
        try:
            await page.wait_for_load_state("networkidle", timeout=20000)
        except Exception:
            lq("[!] networkidle 等待超时（20s），继续尝试...")
        await asyncio.sleep(1)

        # 等待邮箱输入框出现（多选择器并行等待，覆盖 AWS CloudScape UI）
        email_input = page.locator(
            'input[placeholder="username@example.com"], '
            'input[type="email"], '
            'input[name="email"], '
            'input[autocomplete="username"], '
            'input[autocomplete="email"], '
            'input[data-testid="username-input"], '
            'input[data-testid="email-input"], '
            'input.awsui-input-type-text, '
            '#awsui-input-0'
        ).first
        try:
            await email_input.wait_for(state="visible", timeout=30000)
        except Exception:
            # SPA 可能还没渲染完，再等一轮重试
            await asyncio.sleep(5)
            try:
                email_input = page.locator('input[type="text"], input[type="email"]').first
                await email_input.wait_for(state="visible", timeout=10000)
            except Exception:
                lq(f"[!] 找不到邮箱输入框，URL={page.url[:100]}")
                return {"ok": False, "error": "找不到邮箱输入框，页面可能已变化或被重定向"}

        lq(f"邮箱输入框已找到")
        await asyncio.sleep(random.uniform(0.3, 0.8))

        # Playwright fill() 生成 isTrusted:true 事件，无需 React setter hack
        await email_input.fill(email)
        filled = await email_input.input_value()
        lq(f"邮箱 read-back: '{filled}' (期望: '{email}')")
        await asyncio.sleep(0.3)
        await _pw_click_next(page, lq)
        await asyncio.sleep(0.5)
        await _pw_dismiss_cookie(page)

        # ── Step 4: 填写姓名 ─────────────────────────────────────────────
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}
        lq(f"填写姓名: {full_name} | 当前页: {page.url[:80]}")

        # 等 URL 跳转到 profile.aws（最多 15s）
        for _i in range(15):
            if "profile.aws" in page.url:
                break
            await asyncio.sleep(1)
        else:
            lq(f"[!] 邮件提交后未跳转 profile.aws")
            return {"ok": False, "error": "email_domain_rejected", "retriable": True}

        # 再等 1s 让 SPA 路由稳定
        await asyncio.sleep(1)
        if "profile.aws" not in page.url:
            lq(f"邮箱域名被 AWS 拒绝（中转后跳回 signin.aws）: {page.url[:80]}")
            return {"ok": False, "error": "email_domain_rejected", "retriable": True}

        name_input = page.locator(
            'input[placeholder*="Silva"], '
            'input[placeholder*="name" i], '
            'input[name="full_name"], '
            'input[name="name"]'
        ).first
        try:
            await name_input.wait_for(state="visible", timeout=15000)
        except Exception:
            logger.warning("[%s] 未找到姓名输入框，跳过", email)
            name_input = None

        if name_input:
            lq(f"姓名输入框已找到")
            await asyncio.sleep(random.uniform(0.3, 0.8))
            await name_input.fill(full_name)
            filled = await name_input.input_value()
            lq(f"姓名字段 read-back: '{filled}' (期望: '{full_name}')")
            await asyncio.sleep(0.3)

            await _pw_dismiss_cookie(page)
            await _pw_click_next(page, lq)

            # 轮询等待跳转，同时检查错误
            _name_err_texts = ("抱歉，处理您的请求时出错", "Sorry, there was an error", "Please try again", "错误")
            _jumped = False
            for _i in range(10):
                await asyncio.sleep(1)
                await _pw_dismiss_cookie(page)
                if "verify-otp" in page.url or "create-password" in page.url:
                    _jumped = True
                    break
                try:
                    _body = await page.locator("body").inner_text()
                except Exception:
                    _body = ""
                if any(k in _body for k in _name_err_texts):
                    lq(f"[!] 名字页报错: {_body[:120]}")
                    return {"ok": False, "error": "email_domain_rejected", "retriable": True}
            if not _jumped:
                try:
                    _body = await page.locator("body").inner_text()
                except Exception:
                    _body = ""
                lq(f"名字页 10s 未跳转: {_body[:80]}")
                return {"ok": False, "error": "name_page_blocked", "retriable": True}
            await asyncio.sleep(0.3)

        # ── Step 5: 等待验证码并填写（仅在确实处于 OTP 页时执行）────────────
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}

        otp_selectors = [
            'input[placeholder="6 位数"]',
            'input[placeholder*="digit" i]',
            'input[placeholder*="code" i]',
            'input[autocomplete="one-time-code"]',
            'input[name="code"]',
            'input[name="otp"]',
            'input[type="text"][maxlength="6"]',
            'input[type="number"]',
        ]
        pwd_selector = 'input[type="password"]'

        otp_input = None
        direct_password_ready = False
        for _ in range(10):
            if _cancelled():
                return {"ok": False, "error": "任务已取消"}

            if "create-password" in page.url:
                direct_password_ready = True
                break

            try:
                if await page.locator(pwd_selector).count() > 0:
                    direct_password_ready = True
                    break
            except Exception:
                pass

            for sel in otp_selectors:
                try:
                    loc = page.locator(sel).first
                    if await loc.count() > 0 and await loc.is_visible():
                        otp_input = loc
                        break
                except Exception:
                    pass
            if otp_input:
                break
            await asyncio.sleep(0.5)

        if direct_password_ready:
            lq("检测到流程已直接进入密码设置页，跳过邮箱验证码轮询")
        else:
            lq(f"等待 AWS 验证码邮件（最长 120s）... | provider={mail_provider or 'yydsmail'} | 当前页: {page.url[:80]}")

            # 邮件轮询在线程中执行（同步阻塞函数）
            code = await asyncio.to_thread(
                _poll_verification_code,
                email, provider=mail_provider or "yydsmail", meta=mail_meta or {},
                yydsmail_url=yydsmail_url, yydsmail_key=yydsmail_key,
                timeout=_MAIL_TIMEOUT, log_q=log_q, cancel=cancel,
            )
            if not code:
                return {"ok": False, "error": f"验证码超时（{_MAIL_TIMEOUT}s），provider={mail_provider or 'yydsmail'}"}

            lq("验证码已收到，正在填写...")

            if not otp_input:
                for _ in range(30):
                    if _cancelled():
                        return {"ok": False, "error": "任务已取消"}
                    for sel in otp_selectors:
                        try:
                            loc = page.locator(sel).first
                            if await loc.count() > 0 and await loc.is_visible():
                                otp_input = loc
                                break
                        except Exception:
                            pass
                    if otp_input:
                        break
                    await asyncio.sleep(0.5)
            if not otp_input:
                return {"ok": False, "error": "找不到验证码输入框"}
            lq("验证码输入框已找到")

            await asyncio.sleep(random.uniform(0.3, 0.6))
            await otp_input.fill(code)
            lq(f"验证码 read-back: '{await otp_input.input_value()}'")
            await asyncio.sleep(0.3)
            _url_before_otp = page.url
            await _pw_click_next(page, lq)
            # 等待页面跳转到密码设置页，而不是盲等 1s
            await _pw_wait_navigation(page, _url_before_otp, timeout=20,
                                      expect_selector=pwd_selector, lq=lq)
            await asyncio.sleep(0.5)

        # ── Step 6: 设置密码 ─────────────────────────────────────────────
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}
        lq("设置账号密码...")

        # 快速轮询等密码框出现（从 8s 提升到 20s，给页面跳转留足时间）
        pwd_inputs = []
        for _ in range(40):
            pwd_inputs = await page.locator('input[type="password"]').all()
            if len(pwd_inputs) >= 2:
                break
            # 也检查单个密码框的情况
            if len(pwd_inputs) == 1:
                await asyncio.sleep(1)
                pwd_inputs = await page.locator('input[type="password"]').all()
                break
            await asyncio.sleep(0.5)

        if len(pwd_inputs) >= 2:
            lq(f"密码框已找到（{len(pwd_inputs)} 个），快速填写...")
            await pwd_inputs[0].fill(password)
            await pwd_inputs[1].fill(password)
            _url_before_pwd = page.url
            await _pw_click_next(page, lq)
            await _pw_wait_navigation(page, _url_before_pwd, timeout=15,
                                      expect_selector='button#cli_verification_btn, [data-testid="allow-access-button"]', lq=lq)
            await asyncio.sleep(0.5)
        elif len(pwd_inputs) == 1:
            lq("只找到 1 个密码框，填写...")
            await pwd_inputs[0].fill(password)
            _url_before_pwd = page.url
            await _pw_click_next(page, lq)
            await _pw_wait_navigation(page, _url_before_pwd, timeout=15,
                                      expect_selector='button#cli_verification_btn, [data-testid="allow-access-button"]', lq=lq)
            await asyncio.sleep(0.5)
        else:
            lq("[!] 未找到密码输入框，尝试等待更长时间...")
            # 最后兜底：再等 10s 看看密码框会不会出现
            _last_chance = []
            for _ in range(20):
                _last_chance = await page.locator('input[type="password"]').all()
                if _last_chance:
                    break
                await asyncio.sleep(0.5)
            if _last_chance:
                lq(f"兜底等待成功，找到 {len(_last_chance)} 个密码框")
                await _last_chance[0].fill(password)
                if len(_last_chance) >= 2:
                    await _last_chance[1].fill(password)
                _url_before_pwd = page.url
                await _pw_click_next(page, lq)
                await _pw_wait_navigation(page, _url_before_pwd, timeout=15,
                                          expect_selector='button#cli_verification_btn, [data-testid="allow-access-button"]', lq=lq)
                await asyncio.sleep(0.5)
            else:
                lq("[!] 密码输入框始终未出现，流程可能已跳过")

        # ── Step 7: 二次设备确认 ──────────────────────────────────────────
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}
        lq("等待设备确认页面...")
        _dev_clicked = False
        for _ in range(30):
            if _cancelled():
                return {"ok": False, "error": "任务已取消"}
            for sel in ["button#cli_verification_btn", "button[data-testid='confirm-device-button']"]:
                try:
                    btn = page.locator(sel).first
                    if await btn.count() > 0 and await btn.is_visible():
                        lq("设备确认：点击 Confirm...")
                        await btn.click()
                        _dev_clicked = True
                        break
                except Exception:
                    pass
            if _dev_clicked:
                break
            await asyncio.sleep(0.5)

        if _dev_clicked:
            await asyncio.sleep(3)
        else:
            lq("未找到设备确认按钮（可能已自动跳过），继续...")

        # ── Step 7.5: 允许访问授权页 ──────────────────────────────────────
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}
        lq("等待允许访问页面...")
        _allow_clicked = False
        for _ in range(30):
            if _cancelled():
                return {"ok": False, "error": "任务已取消"}
            try:
                btn = page.locator('[data-testid="allow-access-button"]').first
                if await btn.count() > 0 and await btn.is_visible():
                    lq("点击「允许访问」完成授权...")
                    await btn.click()
                    _allow_clicked = True
                    break
            except Exception:
                pass
            await asyncio.sleep(0.5)

        if _allow_clicked:
            await asyncio.sleep(3)
        else:
            lq("未找到允许访问按钮（可能已自动跳过），继续...")

        # ── Step 7.7: OIDC token 轮询 ────────────────────────────────────
        _token_data = None
        if _dev_clicked or _allow_clicked:
            if not _cancelled():
                _token_data = await asyncio.to_thread(
                    _oidc_poll_token,
                    _oidc_cid, _oidc_csec, _device_code,
                    _oidc_itv, proxy, 30, log_q,
                )

        # ── Step 8: 验证注册结果 ──────────────────────────────────────────
        await asyncio.sleep(0.5)
        final_url = page.url
        lq(f"最终 URL: {final_url}")

        page_success = await _pw_check_success(page)
        url_success = (
            any(d in final_url for d in (
                "builder.aws.com", "aws.amazon.com", "profile.aws",
                "view.awsapps.com",
            ))
            and "sign-in" not in final_url
            and "error" not in final_url.lower()
        )
        token_success = _token_data is not None and "accessToken" in (_token_data or {})
        lq(f"页面标题: {(await page.title())[:60]}")

        if page_success or url_success or token_success:
            lq(f"注册成功（page={page_success} url={url_success} token={token_success}）")
            result = {"ok": True, "email": email, "password": password, "name": full_name}
            if _token_data and "accessToken" in _token_data:
                from datetime import datetime, timezone, timedelta
                expires_in = _token_data.get("expiresIn", 3600)
                now = datetime.now(timezone(timedelta(hours=8)))
                result["access_token"] = _token_data["accessToken"]
                result["refresh_token"] = _token_data.get("refreshToken", "")
                result["client_id"] = _oidc_cid
                result["client_secret"] = _oidc_csec
                result["expires_at"] = (now + timedelta(seconds=expires_in)).strftime("%Y-%m-%dT%H:%M:%S+08:00")
                result["last_refresh"] = now.strftime("%Y-%m-%dT%H:%M:%S+08:00")
                result["region"] = "us-east-1"
            return result
        else:
            logger.warning("[%s] 注册流程完成但无法确认成功，final_url=%s", email, final_url)
            return {"ok": False, "error": "注册流程已执行但无法确认账户创建成功，请检查邮箱是否已激活"}

    except asyncio.CancelledError:
        logger.info("[%s] 注册任务被取消", email)
        return {"ok": False, "error": "任务已取消"}

    except Exception as exc:
        logger.error("[%s] 注册异常: %s", email, exc, exc_info=True)
        return {"ok": False, "error": str(exc)}

    finally:
        # Playwright 资源清理（page → context → browser）
        for resource in [page, context, browser]:
            if resource:
                try:
                    await resource.close()
                except Exception:
                    pass


# ─── Gemini 辅助函数 ──────────────────────────────────────────────────────────

def _extract_gemini_xsrf(html: str) -> str:
    """从 Gemini 登录页面提取 XSRF Token（4 种策略：meta tag / hidden input / JS var / URL param）。"""
    for pattern in [
        r'name=["\']xsrf-token["\']\s+content=["\']([^"\']+)["\']',
        r'name=["\']xsrfToken["\'][^>]*value=["\']([A-Za-z0-9_-]{20,})["\']',
        r'xsrfToken["\']?\s*[=:]\s*["\']([A-Za-z0-9_-]{20,})["\']',
        r'xsrfToken=([A-Za-z0-9_-]{20,})',
    ]:
        m = re.search(pattern, html, re.IGNORECASE)
        if m:
            return m.group(1)
    # 不再使用硬编码 fallback（会过期），返回空串让调用方处理
    logger.warning("XSRF Token 提取失败，所有策略均未匹配")
    return ""


async def _get_page_text(page) -> str:
    """安全获取页面 body 文本"""
    try:
        return await page.locator("body").inner_text()
    except Exception:
        return ""


async def _human_type(page, locator, text: str):
    """模拟人类打字：逐字符输入，随机 80-180ms 间隔，20% 概率额外停顿 200-500ms。"""
    await locator.click()
    await asyncio.sleep(random.uniform(0.1, 0.3))
    for ch in text:
        await page.keyboard.type(ch)
        delay = random.uniform(0.08, 0.18)
        if random.random() < 0.2:
            delay += random.uniform(0.2, 0.5)
        await asyncio.sleep(delay)


_GEMINI_CODE_INPUT_SELECTORS = [
    "input[jsname='ovqh0b']",
    "input[type='tel']",
    "input[name='pinInput']",
    "input[autocomplete='one-time-code']",
    "input[aria-label*='验证码']",
    "input[aria-label*='verification code' i]",
]

_GEMINI_EMAIL_INPUT_SELECTORS = [
    "#email-input",
    "input[name='loginHint']",
    "input[aria-label='邮箱']",
    "input[aria-label*='email' i]",
]

_GEMINI_EMAIL_CONTINUE_SELECTORS = [
    "#log-in-button",
    "button#log-in-button",
    "button[jsname='jXw9Fb']",
    "button[aria-label='使用邮箱继续']",
    "button[aria-label='Continue with email']",
    "button[aria-label='Use email to continue']",
    "button[type='submit']",
]


def _classify_gemini_page_state(
    url: str,
    body_text: str = "",
    *,
    has_code_input: bool = False,
    has_email_input: bool = False,
    has_login_button: bool = False,
) -> str:
    """把 Gemini 页面粗分为邮箱输入页 / 验证码页 / 已登录 / signin-error。"""
    url_lower = (url or "").lower()
    body_lower = (body_text or "").lower()

    if "signin-error" in url_lower:
        return "signin_error"
    if "business.gemini.google" in url_lower and "csesidx=" in url_lower and "/cid/" in url_lower:
        return "signed_in"

    code_hints = (
        "请输入验证码",
        "重新发送验证码",
        "we sent a 6 character code",
        "enter the verification code",
        "verification code sent",
    )
    if "verify-oob-code" in url_lower or has_code_input or any(h in body_lower for h in code_hints):
        return "verify_code"

    email_hints = (
        "使用邮箱继续",
        "继续使用邮箱",
        "work email",
        "login or create a free trial account",
        "create account or sign in",
    )
    if has_email_input or has_login_button or any(h in body_lower for h in email_hints):
        return "email_entry"

    return "unknown"


async def _get_first_visible_locator(page, selectors):
    """返回首个可见 locator；找不到时返回 None。"""
    for selector in selectors:
        try:
            loc = page.locator(selector).first
            if await loc.count() > 0 and await loc.is_visible():
                return loc
        except Exception:
            continue
    return None


async def _get_gemini_page_state(page):
    """读取当前 Gemini 页面状态和正文文本。"""
    body_text = await _get_page_text(page)
    code_input = await _get_first_visible_locator(page, _GEMINI_CODE_INPUT_SELECTORS)
    email_input = await _get_first_visible_locator(page, _GEMINI_EMAIL_INPUT_SELECTORS)
    login_button = await _get_first_visible_locator(page, _GEMINI_EMAIL_CONTINUE_SELECTORS[:-1])

    state = _classify_gemini_page_state(
        page.url,
        body_text,
        has_code_input=code_input is not None,
        has_email_input=email_input is not None,
        has_login_button=login_button is not None,
    )
    return {
        "state": state,
        "url": page.url,
        "body_text": body_text,
        "code_input": code_input,
        "email_input": email_input,
        "login_button": login_button,
    }


async def _wait_for_gemini_page_state(page, timeout: float = 15.0, interval: float = 0.5, lq=None):
    """
    等待 Gemini 页面稳定到可识别状态。
    解决 Google 前端渲染较慢时，3-5s 固定 sleep 还没落到真实页面的问题。
    """
    deadline = asyncio.get_running_loop().time() + timeout
    last_state = None
    last_info = None

    while True:
        info = await _get_gemini_page_state(page)
        last_info = info
        if info["state"] != last_state:
            last_state = info["state"]
            if lq:
                lq(f"Gemini 页面状态: {info['state']} | {info['url'][:80]}")
        if info["state"] != "unknown":
            return info
        if asyncio.get_running_loop().time() >= deadline:
            return info
        await asyncio.sleep(interval)


async def _click_gemini_email_continue(page, email: str, lq) -> bool:
    """在邮箱输入页点击当前真实的“使用邮箱继续”按钮。"""
    email_input = await _get_first_visible_locator(page, _GEMINI_EMAIL_INPUT_SELECTORS)
    if email_input is not None:
        try:
            current_value = (await email_input.input_value()).strip()
        except Exception:
            current_value = ""
        if current_value.lower() != email.lower():
            try:
                await email_input.fill("")
            except Exception:
                pass
            await asyncio.sleep(random.uniform(0.2, 0.5))
            await _human_type(page, email_input, email)
            await asyncio.sleep(random.uniform(0.3, 0.8))
            lq("邮箱输入框内容已补齐")

    continue_button = await _get_first_visible_locator(page, _GEMINI_EMAIL_CONTINUE_SELECTORS)
    if continue_button is not None:
        label = ""
        try:
            label = (await continue_button.inner_text()).strip()
        except Exception:
            try:
                label = (await continue_button.get_attribute("aria-label")) or ""
            except Exception:
                label = ""
        await continue_button.click()
        await asyncio.sleep(random.uniform(1.0, 2.0))
        lq(f"点击邮箱继续按钮: {label or 'submit'}")
        return True

    for text_kw in ("使用邮箱继续", "Continue with email", "Use email to continue", "继续", "Next"):
        try:
            btn = page.locator(f"button:has-text('{text_kw}'), div[role='button']:has-text('{text_kw}')").first
            if await btn.count() > 0 and await btn.is_visible():
                await btn.click()
                await asyncio.sleep(random.uniform(1.0, 2.0))
                lq(f"点击文本按钮成功: {text_kw}")
                return True
        except Exception:
            continue

    return False


async def _extract_gemini_config(page, context, email: str, lq) -> dict:
    """从 Gemini Business 页面提取 config_id、csesidx 和 cookies。"""
    url = page.url
    if "cid/" not in url:
        try:
            await page.goto("https://business.gemini.google/",
                            wait_until="domcontentloaded", timeout=30000)
            await asyncio.sleep(random.uniform(2, 4))
            url = page.url
        except Exception:
            pass

    if "cid/" not in url:
        return {"ok": False, "error": "cid not found in URL"}

    try:
        config_id = url.split("cid/")[1].split("?")[0].split("/")[0]
    except (IndexError, ValueError):
        return {"ok": False, "error": "无法解析 config_id"}

    csesidx = ""
    if "csesidx=" in url:
        try:
            csesidx = url.split("csesidx=")[1].split("&")[0]
        except (IndexError, ValueError):
            pass

    # 轮询等待 cookies 就位
    c_ses = ""
    c_oses = ""
    ses_obj = None
    for _ in range(20):
        cookies = await context.cookies()
        c_ses = next((c["value"] for c in cookies if c["name"] == "__Secure-C_SES"), "")
        c_oses = next((c["value"] for c in cookies if c["name"] == "__Host-C_OSES"), "")
        ses_obj = next((c for c in cookies if c["name"] == "__Secure-C_SES"), None)
        if c_ses and c_oses:
            break
        await asyncio.sleep(1)

    if not c_ses or not c_oses:
        missing = []
        if not c_ses:
            missing.append("C_SES")
        if not c_oses:
            missing.append("C_OSES")
        lq(f"[!] Cookie 不完整 ({', '.join(missing)} 缺失)，账号不可用")
        return {"ok": False, "error": f"Cookie 不完整 ({', '.join(missing)} 缺失)", "retriable": False}

    from datetime import datetime, timezone as dt_timezone, timedelta
    beijing_tz = dt_timezone(timedelta(hours=8))
    if ses_obj and "expires" in ses_obj:
        cookie_expire = datetime.fromtimestamp(ses_obj["expires"], tz=beijing_tz)
        expires_at = (cookie_expire - timedelta(hours=12)).strftime("%Y-%m-%dT%H:%M:%S+08:00")
    else:
        expires_at = (datetime.now(beijing_tz) + timedelta(hours=12)).strftime("%Y-%m-%dT%H:%M:%S+08:00")

    # 提取试用期
    trial_end_date = await _extract_gemini_trial_end(page, csesidx, config_id, lq)

    result = {
        "ok": True,
        "email": email,
        "config_id": config_id,
        "csesidx": csesidx,
        "c_ses": c_ses,
        "c_oses": c_oses,
        "expires_at": expires_at,
    }
    if trial_end_date:
        result["trial_end_date"] = trial_end_date

    lq(f"配置提取完成: config_id={config_id}, csesidx={csesidx[:20]}...")
    return result


async def _extract_gemini_trial_end(page, csesidx: str, config_id: str, lq) -> Optional[str]:
    """从页面源码中提取 Gemini 试用期到期日期。"""
    from datetime import datetime, timezone as dt_timezone, timedelta

    def _days_to_end(days: int) -> str:
        return (datetime.now(dt_timezone(timedelta(hours=8))) + timedelta(days=days)).strftime("%Y-%m-%d")

    def _search_source(source: str) -> Optional[str]:
        m = re.search(r'"daysLeft"\s*:\s*(\d+)', source)
        if m:
            return _days_to_end(int(m.group(1)))
        m = re.search(r'"trialDaysRemaining"\s*:\s*(\d+)', source)
        if m:
            return _days_to_end(int(m.group(1)))
        m = re.search(r'\[(\d{4}),(\d{1,2}),(\d{1,2})\].*?\[(\d{4}),(\d{1,2}),(\d{1,2})\]', source)
        if m:
            try:
                end_date = f"{m.group(4):0>4}-{int(m.group(5)):02d}-{int(m.group(6)):02d}"
                if 2025 <= int(m.group(4)) <= 2030:
                    return end_date
            except Exception:
                pass
        m = re.search(r'(\d+)\s*days?\s*left', source, re.IGNORECASE)
        if m:
            return _days_to_end(int(m.group(1)))
        # ISO 日期格式（freeTrialEndDate / trialExpiry / expirationDate）
        m = re.search(r'(?:freeTrialEndDate|trialExpiry|expirationDate|endDate)["\s:]+(\d{4}-\d{2}-\d{2})', source)
        if m:
            try:
                year = int(m.group(1)[:4])
                if 2025 <= year <= 2030:
                    return m.group(1)
            except Exception:
                pass
        return None

    try:
        source = await page.content()
        result = _search_source(source or "")
        if result:
            lq(f"试用期到期: {result}")
            return result
        # 尝试 settings 页
        settings_url = f"https://business.gemini.google/cid/{config_id}/settings?csesidx={csesidx}"
        await page.goto(settings_url, wait_until="domcontentloaded", timeout=30000)
        await asyncio.sleep(random.uniform(1.5, 3))
        source = await page.content()
        result = _search_source(source or "")
        if result:
            lq(f"试用期到期: {result}")
            return result
    except Exception as e:
        lq(f"[!] 获取试用期失败: {e}")
    return None


# ─── 403/风控页面检测（Gemini 专用）────────────────────────────────────────

_GEMINI_BLOCK_PATTERNS = [
    "Access Restricted",
    "This service is restricted",
    "account suspended",
    "not available for your account",
    "account has been disabled",
    "unusual activity",
    "can't sign you in",
    "couldn't verify",
]


def _detect_gemini_block(body_text: str, email: str, lq) -> Optional[dict]:
    """检测 403 / 风控页面，返回错误 dict 或 None。"""
    for pattern in _GEMINI_BLOCK_PATTERNS:
        if pattern.lower() in body_text.lower():
            domain = email.rsplit("@", 1)[-1] if "@" in email else "unknown"
            lq(f"[!] 页面被拦截: {pattern} ({domain})")
            return {"ok": False, "error": f"blocked: {pattern} ({domain})", "retriable": False}
    return None


# ── CAPTCHA / 人机验证检测 ──────────────────────────────────────────────────

_CAPTCHA_PATTERNS = [
    "recaptcha",
    "g-recaptcha",
    "verify you're a human",
    "prove you're not a robot",
    "unusual traffic from your computer",
    "automated queries",
    "complete the security check",
    "are you a robot",
]


def _detect_captcha(body_text: str, email: str, lq) -> Optional[dict]:
    """检测 CAPTCHA / 人机验证拦截，返回错误 dict 或 None。"""
    lower = body_text.lower()
    for pattern in _CAPTCHA_PATTERNS:
        if pattern.lower() in lower:
            lq(f"[!] CAPTCHA 拦截: {pattern}")
            return {"ok": False, "error": f"captcha: {pattern}", "retriable": True}
    return None


# ─── Gemini 注册核心逻辑（异步，Camoufox + Playwright）──────────────────────

async def _do_gemini_register(
    email: str,
    proxy: Optional[str],
    yydsmail_url: str,
    yydsmail_key: str,
    log_q: Optional[queue.Queue] = None,
    cancel: Optional[Event] = None,
    mail_provider: str = "",
    mail_meta: Optional[dict] = None,
) -> dict:
    """Gemini Business 注册流程（Camoufox + Playwright）。"""
    def lq(msg: str):
        logger.info("[%s] %s", email, msg)
        if log_q is not None:
            log_q.put(msg)

    def _cancelled() -> bool:
        return cancel is not None and cancel.is_set()

    first_name, last_name = _random_name()
    full_name = f"{first_name} {last_name}"

    # IP 地理位置自动检测
    ip_loc = await asyncio.to_thread(_detect_ip_location, proxy)
    tz = ip_loc["timezone"]
    geo = ip_loc["geo"]
    detected_locale = ip_loc["locale"]
    lq(f"IP 地理位置: {ip_loc['country']} tz={tz}")

    browser = None
    context = None
    page = None

    try:
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}

        # ── 启动 Camoufox ──────────────────────────────────────────
        lq("启动 Camoufox 浏览器...")
        import sys as _sys
        _headless_env = os.getenv("GEMINI_HEADLESS", os.getenv("KIRO_HEADLESS", "")).lower()
        if _headless_env in ("false", "0", "no"):
            _use_headless = False
        elif _headless_env in ("true", "1", "yes"):
            _use_headless = True
        else:
            _use_headless = _sys.platform not in ("darwin", "win32") and not os.getenv("DISPLAY")

        cf = AsyncCamoufox(headless=_use_headless)
        browser = await cf.start()

        context_opts = {
            "locale": detected_locale,
            "timezone_id": tz,
            "geolocation": {"latitude": geo["latitude"], "longitude": geo["longitude"], "accuracy": geo["accuracy"]},
            "permissions": ["geolocation"],
        }

        if proxy:
            proxy = _validate_proxy(proxy)
            if proxy:
                chained = chain_proxy(proxy)
                pw_proxy = _parse_proxy_for_playwright(chained)
                if pw_proxy:
                    context_opts["proxy"] = pw_proxy
                    logger.info("[%s] Gemini 使用代理: %s", email, _mask_proxy(proxy))
            else:
                lq("[!] 代理格式无效，已忽略")

        context = await browser.new_context(**context_opts)
        # WebRTC 泄漏保护：阻止通过 RTCPeerConnection 暴露真实 IP
        await context.add_init_script("""
            Object.defineProperty(navigator, 'mediaDevices', {
                get: () => ({
                    enumerateDevices: () => Promise.resolve([]),
                    getUserMedia: () => Promise.reject(new DOMException('NotAllowedError')),
                })
            });
            window.RTCPeerConnection = undefined;
            window.mozRTCPeerConnection = undefined;
            window.webkitRTCPeerConnection = undefined;
        """)
        page = await context.new_page()
        lq("浏览器已启动")

        # ── Step 1: 打开登录页面 ──────────────────────────────────
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}
        lq("打开 Gemini 登录页面...")
        await page.goto(GEMINI_AUTH_URL, wait_until="domcontentloaded", timeout=60000)
        await asyncio.sleep(random.uniform(1, 2))
        lq(f"页面: {(await page.title())[:60]} | {page.url[:80]}")

        # ── Step 2: 提取 XSRF Token + 设置 Cookie ────────────────
        html = await page.content()
        xsrf_token = _extract_gemini_xsrf(html)
        if not xsrf_token:
            lq("[!] XSRF Token 提取失败，尝试刷新页面重新获取...")
            await page.reload(wait_until="domcontentloaded", timeout=30000)
            await asyncio.sleep(random.uniform(1, 2))
            html = await page.content()
            xsrf_token = _extract_gemini_xsrf(html)
            if not xsrf_token:
                lq("[!] XSRF Token 二次提取仍失败，Google 页面结构可能已变化")
                return {"ok": False, "error": "XSRF token extraction failed", "retriable": True}
        lq(f"XSRF Token: {xsrf_token[:20]}...")

        # __Host- 前缀 cookie 不允许 domain 属性（RFC 6265bis），
        # Camoufox (Playwright/Firefox) 会剥离 domain 导致验证失败；
        # 改用 url 让 Playwright 自动推导 domain + path
        await context.add_cookies([{
            "name": "__Host-AP_SignInXsrf",
            "value": xsrf_token,
            "url": "https://auth.business.gemini.google/",
            "secure": True,
        }])

        # ── Step 3: URL 方式提交邮箱 ─────────────────────────────
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}
        login_hint = urllib.parse.quote(email, safe="")
        login_url = (
            f"https://auth.business.gemini.google/login/email"
            f"?continueUrl=https%3A%2F%2Fbusiness.gemini.google%2F"
            f"&loginHint={login_hint}&xsrfToken={xsrf_token}"
        )
        lq("URL 方式提交邮箱...")
        await page.goto(login_url, wait_until="domcontentloaded", timeout=60000)
        page_state = await _wait_for_gemini_page_state(page, timeout=15.0, lq=lq)
        current_url = page_state["url"]
        lq(f"当前 URL: {current_url[:80]}")

        # 检查 signin-error
        if page_state["state"] == "signin_error":
            lq("[!] signin-error 页面")
            return {"ok": False, "error": "signin-error: token rejected by Google", "retriable": False}

        # 检查是否已登录
        if page_state["state"] == "signed_in":
            lq("已登录，直接提取配置")
            return await _extract_gemini_config(page, context, email, lq)

        # 检查 403 / 风控
        body_text = page_state["body_text"]
        block_result = _detect_gemini_block(body_text, email, lq)
        if block_result:
            return block_result
        captcha_result = _detect_captcha(body_text, email, lq)
        if captcha_result:
            return captcha_result

        # ── Step 4: 点击发送验证码 ───────────────────────────────
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}
        send_ok = False
        if page_state["state"] == "verify_code":
            lq("URL 提交后已直接进入验证码页，跳过发送按钮点击")
            send_ok = True
        else:
            lq("当前仍在邮箱确认页，尝试点击“使用邮箱继续”...")
            for send_attempt in range(3):
                if _cancelled():
                    return {"ok": False, "error": "任务已取消"}

                page_state = await _get_gemini_page_state(page)
                if page_state["state"] == "verify_code":
                    lq("检测到验证码页已出现，验证码应已自动发送")
                    send_ok = True
                    break
                if page_state["state"] == "signin_error":
                    lq("[!] signin-error 页面")
                    return {"ok": False, "error": "signin-error: token rejected by Google", "retriable": False}
                if page_state["state"] == "signed_in":
                    lq("已登录，直接提取配置")
                    return await _extract_gemini_config(page, context, email, lq)

                body_text = page_state["body_text"]
                block_result = _detect_gemini_block(body_text, email, lq)
                if block_result:
                    return block_result
                captcha_result = _detect_captcha(body_text, email, lq)
                if captcha_result:
                    return captcha_result

                clicked = await _click_gemini_email_continue(page, email, lq)
                if not clicked:
                    delay = [4, 6, 8][min(send_attempt, 2)]
                    lq(f"未找到当前页面的邮箱继续按钮，{delay}s 后重试 ({send_attempt + 1}/3)")
                    await asyncio.sleep(delay)
                    continue

                page_state = await _wait_for_gemini_page_state(page, timeout=20.0, lq=lq)
                if page_state["state"] == "verify_code":
                    lq("点击后进入验证码页，验证码发送流程已触发")
                    send_ok = True
                    break
                if page_state["state"] == "signed_in":
                    lq("点击后直接登录成功，跳过验证码轮询")
                    return await _extract_gemini_config(page, context, email, lq)
                if page_state["state"] == "signin_error":
                    lq("[!] signin-error 页面")
                    return {"ok": False, "error": "signin-error: token rejected by Google", "retriable": False}

                body_text = page_state["body_text"]
                block_result = _detect_gemini_block(body_text, email, lq)
                if block_result:
                    return block_result
                captcha_result = _detect_captcha(body_text, email, lq)
                if captcha_result:
                    return captcha_result

                delay = [4, 6, 8][min(send_attempt, 2)]
                lq(f"点击后仍未进入验证码页，{delay}s 后重试 ({send_attempt + 1}/3)")
                await asyncio.sleep(delay)

        if not send_ok:
            lq("[!] 验证码发送失败（可能触发风控）")
            return {"ok": False, "error": "send code failed", "retriable": True}

        # ── Step 5: 等待验证码输入框 ─────────────────────────────
        lq("等待验证码输入框...")
        code_input = page_state.get("code_input")
        _code_selectors = _GEMINI_CODE_INPUT_SELECTORS
        for _ in range(30):
            if _cancelled():
                return {"ok": False, "error": "任务已取消"}
            if code_input is not None:
                break
            for sel in _code_selectors:
                try:
                    loc = page.locator(sel).first
                    if await loc.count() > 0 and await loc.is_visible():
                        code_input = loc
                        break
                except Exception:
                    pass
            if code_input:
                break
            await asyncio.sleep(1)

        if not code_input:
            # 检查是否被 CAPTCHA 拦截导致输入框未出现
            body_text = await _get_page_text(page)
            captcha_result = _detect_captcha(body_text, email, lq)
            if captcha_result:
                return captcha_result
            lq("[!] 验证码输入框未出现")
            return {"ok": False, "error": "code input not found"}
        lq("验证码输入框已找到")

        # ── Step 6: 轮询邮件获取验证码 ───────────────────────────
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}
        lq(f"等待 Google 验证码邮件... (provider={mail_provider or 'yydsmail'})")

        code = await asyncio.to_thread(
            _poll_gemini_verification_code,
            email, provider=mail_provider or "yydsmail", meta=mail_meta or {},
            yydsmail_url=yydsmail_url, yydsmail_key=yydsmail_key,
            timeout=_MAIL_TIMEOUT, log_q=log_q, cancel=cancel,
        )
        if not code:
            # 重试：点重发按钮后再轮询
            lq("验证码超时，尝试重新发送...")
            try:
                resend_btn = page.locator(
                    "button:has-text('重新'), button:has-text('Resend'), button:has-text('resend'), "
                    "button:has-text('Send again'), button:has-text('Try again'), button:has-text('重新发送')"
                ).first
                if await resend_btn.count() > 0:
                    await resend_btn.click()
                    await asyncio.sleep(2)
                    code = await asyncio.to_thread(
                        _poll_gemini_verification_code,
                        email, provider=mail_provider or "yydsmail", meta=mail_meta or {},
                        yydsmail_url=yydsmail_url, yydsmail_key=yydsmail_key,
                        timeout=90, log_q=log_q, cancel=cancel,
                    )
            except Exception:
                pass
            if not code:
                return {"ok": False, "error": f"验证码超时 (provider={mail_provider or 'yydsmail'})", "retriable": True}

        lq(f"验证码: {code[:2]}{'*' * max(len(code) - 2, 0)}")

        # ── Step 7: 输入验证码并提交 ─────────────────────────────
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}

        # 重新获取输入框（可能已刷新 DOM）
        for sel in _code_selectors:
            try:
                loc = page.locator(sel).first
                if await loc.count() > 0 and await loc.is_visible():
                    code_input = loc
                    break
            except Exception:
                pass

        await asyncio.sleep(random.uniform(0.3, 0.8))
        # 人类模拟打字（逐字符输入，降低风控检测概率）
        await _human_type(page, code_input, code)
        lq(f"验证码 read-back: {await code_input.input_value()}")
        await asyncio.sleep(random.uniform(0.3, 0.6))

        # 提交：先 Enter，再找验证按钮兜底
        await code_input.press("Enter")
        await asyncio.sleep(random.uniform(0.8, 1.5))

        if "verify-oob-code" in page.url:
            for btn_text in ["验证", "Verify", "Next", "确认"]:
                try:
                    vbtn = page.locator(f"button:has-text('{btn_text}')").first
                    if await vbtn.count() > 0 and await vbtn.is_visible():
                        await vbtn.click()
                        await asyncio.sleep(1)
                        break
                except Exception:
                    continue

        # ── Step 8: 检查提交结果 + 403 检测 ──────────────────────
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}
        await asyncio.sleep(1.5)

        body_text = await _get_page_text(page)
        block_result = _detect_gemini_block(body_text, email, lq)
        if block_result:
            return block_result
        captcha_result = _detect_captcha(body_text, email, lq)
        if captcha_result:
            return captcha_result

        if "verify-oob-code" in page.url:
            lq("[!] 验证码提交失败")
            return {"ok": False, "error": "verification code submission failed", "retriable": True}

        # ── Step 9: 处理用户名设置（新账号）─────────────────────
        lq("等待用户名设置页面...")
        _name_selectors = [
            "input[formcontrolname='fullName']",
            "input#mat-input-0",
            "input[placeholder='全名']",
            "input[placeholder='Full name']",
            "input[name='displayName']",
            "input[type='text']",
        ]
        name_input = None
        for _ in range(30):
            if _cancelled():
                return {"ok": False, "error": "任务已取消"}
            # 检查是否已到达业务页面
            if "business.gemini.google" in page.url and "csesidx=" in page.url and "/cid/" in page.url:
                lq("已到达业务页面，跳过用户名设置")
                break
            for sel in _name_selectors:
                try:
                    loc = page.locator(sel).first
                    if await loc.count() > 0 and await loc.is_visible():
                        name_input = loc
                        break
                except Exception:
                    pass
            if name_input:
                break
            await asyncio.sleep(1)

        if name_input:
            lq(f"输入姓名: {full_name}")
            await asyncio.sleep(random.uniform(0.3, 0.8))
            await _human_type(page, name_input, full_name)
            await asyncio.sleep(random.uniform(0.3, 0.6))
            await name_input.press("Enter")
            await asyncio.sleep(random.uniform(0.8, 1.5))

            # Enter 没跳转时找提交按钮
            if "cid" not in page.url:
                for btn_text in ["继续", "Continue", "提交", "Submit", "Next"]:
                    try:
                        sbtn = page.locator(f"button:has-text('{btn_text}')").first
                        if await sbtn.count() > 0 and await sbtn.is_visible():
                            await sbtn.click()
                            await asyncio.sleep(1)
                            break
                    except Exception:
                        continue

        # ── Step 10: 处理协议页面 ─────────────────────────────────
        if "/admin/create" in page.url:
            try:
                agree_btn = page.locator("button.agree-button").first
                if await agree_btn.count() > 0:
                    await agree_btn.click()
                    await asyncio.sleep(random.uniform(1, 2))
            except Exception:
                pass

        # ── Step 11: 等待业务页面参数（csesidx + cid）──────────
        lq("等待 Gemini 工作台 URL...")
        _got_params = False
        for _ in range(45):
            if _cancelled():
                return {"ok": False, "error": "任务已取消"}
            url = page.url
            if "csesidx=" in url and "/cid/" in url:
                _got_params = True
                break
            await asyncio.sleep(1)

        if not _got_params:
            lq("URL 参数未生成，尝试导航到主页...")
            try:
                await page.goto("https://business.gemini.google/",
                                wait_until="domcontentloaded", timeout=30000)
                await asyncio.sleep(random.uniform(2, 3))
            except Exception:
                pass
            for _ in range(15):
                url = page.url
                if "csesidx=" in url and "/cid/" in url:
                    _got_params = True
                    break
                await asyncio.sleep(1)

        if not _got_params:
            body_text = await _get_page_text(page)
            block_result = _detect_gemini_block(body_text, email, lq)
            if block_result:
                return block_result
            lq(f"[!] URL 参数生成失败: {page.url[:80]}")
            return {"ok": False, "error": "URL parameters not found (csesidx/cid)"}

        # ── Step 12: 提取配置 ─────────────────────────────────────
        lq("注册成功，提取配置...")
        return await _extract_gemini_config(page, context, email, lq)

    except asyncio.CancelledError:
        return {"ok": False, "error": "任务已取消"}

    except Exception as exc:
        logger.error("[%s] Gemini 注册异常: %s", email, exc, exc_info=True)
        return {"ok": False, "error": str(exc)}

    finally:
        # asyncio.shield 防止 CancelledError 中断浏览器资源释放
        async def _cleanup_resources():
            for resource in [page, context, browser]:
                if resource:
                    try:
                        await resource.close()
                    except Exception:
                        pass
        try:
            await asyncio.shield(_cleanup_resources())
        except asyncio.CancelledError:
            pass


# ─── HTTP 路由 ───────────────────────────────────────────────────────────────

@app.route("/kiro/process", methods=["POST"])
async def register():
    data = await request.get_json()
    if not data or not data.get("email"):
        return jsonify({"ok": False, "error": "invalid request"}), 400

    email       = data["email"]
    proxy       = data.get("proxy")
    yydsmail_url = data.get("yydsmail_url", "")
    yydsmail_key = data.get("yydsmail_key", "")
    region      = data.get("region", "usa")
    mail_provider = data.get("mail_provider", "")
    mail_meta     = data.get("mail_meta", {})

    if region not in _ALLOWED_REGIONS:
        return jsonify({"ok": False, "error": f"不支持的 region: {region}"}), 400

    logger.info("收到注册请求: %s proxy=%s region=%s", email, "***" if proxy else "none", region)

    async def generate():
        async with _reg_semaphore:
            log_q: queue.Queue = queue.Queue()
            cancel_event = Event()

            reg_task = asyncio.create_task(
                _do_register(
                    email, proxy, yydsmail_url, yydsmail_key, region,
                    log_q, cancel_event, mail_provider, mail_meta,
                )
            )

            last_keepalive = asyncio.get_event_loop().time()
            try:
                while not reg_task.done():
                    drained = False
                    while True:
                        try:
                            msg = log_q.get_nowait()
                            yield f"LOG:{msg}\n"
                            drained = True
                            last_keepalive = asyncio.get_event_loop().time()
                        except queue.Empty:
                            break
                    # 超过 10s 没有日志则推心跳（Go 侧会忽略）
                    now = asyncio.get_event_loop().time()
                    if not drained and now - last_keepalive > 10:
                        yield "LOG: .\n"
                        last_keepalive = now
                    if not drained:
                        await asyncio.sleep(0.2)

                # 排尽剩余日志
                while True:
                    try:
                        msg = log_q.get_nowait()
                        yield f"LOG:{msg}\n"
                    except queue.Empty:
                        break

                # 最后一行：JSON 结果
                result = await reg_task
                yield json.dumps(result, ensure_ascii=False) + "\n"

            except (asyncio.CancelledError, GeneratorExit):
                # 客户端断开连接，通知注册协程取消
                cancel_event.set()
                reg_task.cancel()
                try:
                    await reg_task
                except (asyncio.CancelledError, Exception):
                    pass
                logger.info("[%s] 客户端断开，注册任务已取消", email)
                raise

    return app.response_class(generate(), content_type="text/plain; charset=utf-8")


@app.route("/gemini/process", methods=["POST"])
async def gemini_register():
    data = await request.get_json()
    if not data or not data.get("email"):
        return jsonify({"ok": False, "error": "invalid request"}), 400

    email       = data["email"]
    proxy       = data.get("proxy")
    yydsmail_url = data.get("yydsmail_url", "")
    yydsmail_key = data.get("yydsmail_key", "")
    mail_provider = data.get("mail_provider", "")
    mail_meta     = data.get("mail_meta", {})

    logger.info("收到 Gemini 注册请求: %s proxy=%s", email, "***" if proxy else "none")

    async def generate():
        async with _gemini_semaphore:
            log_q: queue.Queue = queue.Queue()
            cancel_event = Event()

            reg_task = asyncio.create_task(
                _do_gemini_register(
                    email, proxy, yydsmail_url, yydsmail_key,
                    log_q, cancel_event, mail_provider, mail_meta,
                )
            )

            last_keepalive = asyncio.get_event_loop().time()
            try:
                while not reg_task.done():
                    drained = False
                    while True:
                        try:
                            msg = log_q.get_nowait()
                            yield f"LOG:{msg}\n"
                            drained = True
                            last_keepalive = asyncio.get_event_loop().time()
                        except queue.Empty:
                            break
                    now = asyncio.get_event_loop().time()
                    if not drained and now - last_keepalive > 10:
                        yield "LOG: .\n"
                        last_keepalive = now
                    if not drained:
                        await asyncio.sleep(0.2)

                while True:
                    try:
                        msg = log_q.get_nowait()
                        yield f"LOG:{msg}\n"
                    except queue.Empty:
                        break

                result = await reg_task
                yield json.dumps(result, ensure_ascii=False) + "\n"

            except (asyncio.CancelledError, GeneratorExit):
                cancel_event.set()
                reg_task.cancel()
                try:
                    await reg_task
                except (asyncio.CancelledError, Exception):
                    pass
                logger.info("[%s] Gemini 客户端断开，注册任务已取消", email)
                raise

    return app.response_class(generate(), content_type="text/plain; charset=utf-8")


@app.route("/health")
async def health():
    return jsonify({
        "status": "ok",
        "service": "aws-builder-id-reg",
        "engine": "camoufox",
        "platforms": ["kiro", "gemini"],
        "concurrency": {
            "kiro": _MAX_CONCURRENT,
            "gemini": _GEMINI_MAX_CONCURRENT,
        },
    })


@app.route("/")
async def index():
    return jsonify({
        "name": "edge-service",
        "version": "1.0.0",
        "status": "healthy",
    })


def main():
    parser = argparse.ArgumentParser(description="账号注册服务 — Kiro + Gemini (Camoufox)")
    parser.add_argument("--host", default=os.getenv("KIRO_REG_HOST", "0.0.0.0"))
    parser.add_argument("--port", type=int, default=int(os.getenv("KIRO_REG_PORT", "5076")))
    args = parser.parse_args()
    logger.info("AWS Builder ID 注册服务启动 (Camoufox): %s:%d", args.host, args.port)

    import hypercorn.asyncio as _ha
    import hypercorn.config as _hc
    _cfg = _hc.Config()
    _cfg.bind = [f"{args.host}:{args.port}"]
    _cfg.keep_alive_timeout = int(os.getenv("KIRO_KEEPALIVE_TIMEOUT", "310"))
    _cfg.response_timeout = int(os.getenv("KIRO_RESPONSE_TIMEOUT", "400"))
    asyncio.run(_ha.serve(app, _cfg))


if __name__ == "__main__":
    main()
