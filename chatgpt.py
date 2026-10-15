import os
import re
import sys
import time
import json
import uuid
import base64
import random
import argparse
from pathlib import Path
from datetime import datetime
from concurrent.futures import ThreadPoolExecutor, as_completed

# Enable native Windows Virtual Terminal Sequences (ANSI escape codes)
if sys.platform == "win32":
    try:
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
        sys.stderr.reconfigure(encoding="utf-8", errors="replace")
        os.system("")
    except Exception:
        pass

# Nerd Fonts glyphs & symbols
NF_CHECK   = "󰄬"   # \U000f012c (Checkmark)
NF_CROSS   = "󰅖"   # \U000f0156 (Cross mark)
NF_WARN    = "󰀦"   # \U000f0026 (Warning triangle)
NF_USER    = ""   # \U0000f007 (User profile)
NF_PLAN    = "󰈙"   # \U000f0219 (Document / Plan)
NF_CLOCK   = "󰥔"   # \U000f0954 (Clock / Expiry)
NF_ARROW   = "󰁕"   # \U000f0055 (Arrow right)
NF_DIR     = ""   # \U0000f07c (Folder)
NF_CPU     = "󰍛"   # \U000f035b (Worker / CPU)
NF_FILE    = ""   # \U0000f15c (File)
NF_NET     = "󰛳"   # \U000f06f3 (Network)
NF_LOCK    = "󰌾"   # \U000f033e (Lock / Security)
NF_PROXY   = "󰒄"   # \U000f0484 (Proxy / Cloud globe)
NF_DIRECT  = "󰌘"   # \U000f0318 (Direct link / Local network)
NF_DIV     = "│"

# High-visibility bold ANSI styling
C_GREEN    = "\033[1;38;5;82m"
C_RED      = "\033[1;38;5;196m"
C_YELLOW   = "\033[1;38;5;220m"
C_CYAN     = "\033[1;38;5;51m"
C_MAGENTA  = "\033[1;38;5;201m"
C_GRAY     = "\033[0;38;5;244m"
C_WHITE    = "\033[1;38;5;255m"
C_RESET    = "\033[0m"

try:
    from curl_cffi import requests as curl_requests
    HAS_CURL_CFFI = True
except ImportError:
    import requests as curl_requests
    HAS_CURL_CFFI = False

BASE = "https://chatgpt.com"
SESSION_URL = f"{BASE}/api/auth/session"
ME_URL = f"{BASE}/backend-api/me"
MODELS_URL = f"{BASE}/backend-api/models"
HEALTH_CHECK_URL = "https://httpbin.org/ip"


def parse_proxy_string(raw_p: str) -> dict:
    raw_p = raw_p.strip()
    if not raw_p:
        return None

    if "://" not in raw_p and raw_p.count(":") == 3:
        host, port, user, pwd = raw_p.split(":")
        proxy_url = f"http://{user}:{pwd}@{host}:{port}"
    elif "://" not in raw_p and raw_p.count(":") == 1:
        proxy_url = f"http://{raw_p}"
    else:
        proxy_url = raw_p

    return {"http": proxy_url, "https": proxy_url}


def check_single_proxy(proxy_dict: dict, timeout: int = 5) -> bool:
    try:
        if HAS_CURL_CFFI:
            resp = curl_requests.get(
                HEALTH_CHECK_URL,
                proxies=proxy_dict,
                impersonate="chrome124",
                timeout=timeout,
            )
        else:
            resp = curl_requests.get(
                HEALTH_CHECK_URL,
                proxies=proxy_dict,
                timeout=timeout,
            )
        return resp.status_code == 200
    except Exception:
        return False


def load_and_verify_proxies(proxy_input: str, max_check_workers: int = 16) -> list:
    if not proxy_input:
        return []

    p_path = Path(proxy_input)
    raw_candidates = []
    if p_path.is_file():
        try:
            lines = p_path.read_text(encoding="utf-8", errors="ignore").splitlines()
            for line in lines:
                parsed = parse_proxy_string(line)
                if parsed:
                    raw_candidates.append(parsed)
        except Exception as e:
            print(f" {C_RED}{NF_WARN} Error reading proxy file:{C_RESET} {e}")
    else:
        parsed = parse_proxy_string(proxy_input)
        if parsed:
            raw_candidates.append(parsed)

    if not raw_candidates:
        return []

    print(f" {C_CYAN}{NF_PROXY} Pre-checking {len(raw_candidates)} candidate proxies...{C_RESET}", end="", flush=True)

    alive_proxies = []
    with ThreadPoolExecutor(max_workers=min(max_check_workers, len(raw_candidates))) as executor:
        future_to_proxy = {executor.submit(check_single_proxy, p): p for p in raw_candidates}
        for future in as_completed(future_to_proxy):
            try:
                if future.result():
                    alive_proxies.append(future_to_proxy[future])
            except Exception:
                pass

    alive_count = len(alive_proxies)
    dead_count = len(raw_candidates) - alive_count
    print(f"\r {C_CYAN}{NF_PROXY} Proxies Verified:{C_RESET} {C_GREEN}{alive_count} alive{C_RESET}, {C_RED}{dead_count} dead{C_RESET}    \n")
    return alive_proxies


def load_cookies(file_path: Path) -> dict:
    """
    Parses Netscape cookie file, preserving #HttpOnly_ cookies and JSON exports.
    """
    cookies = {}
    try:
        content = file_path.read_text(encoding="utf-8", errors="replace")
    except Exception as e:
        print(f" {C_RED}{NF_WARN} Error reading {file_path.name}:{C_RESET} {e}")
        return {}

    # JSON export fallback
    if content.strip().startswith(("[", "{")):
        try:
            data = json.loads(content)
            if isinstance(data, list):
                for item in data:
                    name = str(item.get("name", "") or item.get("Name", ""))
                    val = item.get("value") or item.get("Value")
                    if name and val:
                        cookies[name] = str(val)
                return cookies
            elif isinstance(data, dict):
                for k, v in data.items():
                    if isinstance(v, str):
                        cookies[k] = v
                return cookies
        except Exception:
            pass

    # Netscape format line parser
    for line in content.splitlines():
        line = line.rstrip("\r\n").strip()
        if not line:
            continue
        if line.startswith("#HttpOnly_"):
            line = line[len("#HttpOnly_"):]
        elif line.startswith("#"):
            continue

        parts = line.split("\t")
        if len(parts) != 7:
            parts = line.split()
            if len(parts) != 7:
                continue
        cookies[parts[5]] = parts[6]

    return cookies


def build_session(cookies: dict, profile: str, proxy: dict = None):
    if HAS_CURL_CFFI:
        sess_kwargs = {"impersonate": profile}
        if proxy:
            sess_kwargs["proxies"] = proxy
        s = curl_requests.Session(**sess_kwargs)
    else:
        s = curl_requests.Session()
        if proxy:
            s.proxies.update(proxy)

    s.cookies.update(cookies)
    s.headers.update({
        "Accept-Language": "en-US,en;q=0.9",
        "Origin": BASE,
        "Referer": f"{BASE}/",
        "oai-device-id": cookies.get("oai-did", str(uuid.uuid4())),
        "oai-language": "en-US",
        "oai-client-version": "prod-f9b775c0e3ff13d0d7f21c9c6a1c1d3c",
    })
    return s


def decode_jwt(tok: str) -> dict:
    try:
        parts = tok.split(".")
        if len(parts) < 2:
            return {}
        p = parts[1] + "=" * (-len(parts[1]) % 4)
        return json.loads(base64.urlsafe_b64decode(p))
    except Exception:
        return {}


def format_expiry(exp_timestamp):
    if not exp_timestamp:
        return "Unknown"
    try:
        return datetime.fromtimestamp(exp_timestamp).strftime("%Y-%m-%d %H:%M")
    except Exception:
        return str(exp_timestamp)


def is_network_or_dns_error(err_str: str) -> bool:
    err = err_str.lower()
    indicators = [
        "could not resolv",
        "could not resolve",
        "resolve host",
        "curl: (6)",
        "curl: (7)",
        "curl: (28)",
        "curl: (35)",
        "connection refused",
        "proxy error",
        "proxy",
        "timed out",
        "timeout",
        "network unreachable",
        "ssl_connect_error",
    ]
    return any(ind in err for ind in indicators)


def validate_session(cookie_jar: dict, proxy_list: list = None, max_proxy_attempts: int = 3):
    """
    Validates cookies against OpenAI backend.
    Handles DNS / network / proxy glitches by retrying and falling back to direct connection.
    Only returns is_transient_network_error=True if both proxy and direct fail due to network/DNS,
    preventing good cookies from being wrongly moved to invalid/.
    Returns: (is_live, details, error_message, is_network_error)
    """
    use_direct_fallback = False
    attempts = 0
    max_total_attempts = (max_proxy_attempts + 2) if proxy_list else 3
    last_network_error = None

    while attempts < max_total_attempts:
        attempts += 1
        active_proxy = None
        if proxy_list and not use_direct_fallback and attempts <= max_proxy_attempts:
            active_proxy = random.choice(proxy_list)
        else:
            use_direct_fallback = True
            active_proxy = None

        profile = "chrome124"
        try:
            session = build_session(cookie_jar, profile, proxy=active_proxy)

            # Step 1: Query NextAuth session endpoint
            resp = session.get(
                SESSION_URL,
                headers={"Accept": "application/json"},
                timeout=15,
            )

            if resp.status_code != 200:
                if resp.status_code in (401, 403):
                    return False, None, f"HTTP {resp.status_code} Unauthorized", False
                if attempts < max_total_attempts:
                    if attempts >= max_proxy_attempts:
                        use_direct_fallback = True
                    time.sleep(0.5)
                    continue
                return False, None, f"HTTP {resp.status_code}", False

            session_json = resp.json() or {}
            access_token = session_json.get("accessToken")
            user = session_json.get("user")

            if not access_token or not user or not user.get("email"):
                return False, None, "No active user session (Anonymous / Expired)", False

            email = user.get("email", "Active User")
            account = session_json.get("account") or {}
            plan_type = account.get("plan_type", "Free / Standard").capitalize()

            # Step 2: Backend-API verification probe
            claims = decode_jwt(access_token)
            account_id = (claims.get("https://api.openai.com/auth") or {}).get("chatgpt_account_id")
            exp_ts = claims.get("exp")
            expiry_str = format_expiry(exp_ts)

            auth_headers = {
                "Authorization": f"Bearer {access_token}",
                "Accept": "application/json",
            }
            if account_id:
                auth_headers["chatgpt-account-id"] = account_id

            backend_resp = session.get(ME_URL, headers=auth_headers, timeout=12)
            if backend_resp.status_code != 200:
                backend_resp = session.get(MODELS_URL, headers=auth_headers, timeout=12)

            if backend_resp.status_code != 200:
                return False, None, f"Backend Token Rejected (HTTP {backend_resp.status_code})", False

            mode_tag = f"{C_GRAY}[Direct]{C_RESET} " if use_direct_fallback and proxy_list else ""
            details = (
                f"{mode_tag}{C_WHITE}{NF_USER} {email}{C_RESET} {C_GRAY}{NF_DIV}{C_RESET} "
                f"{C_MAGENTA}{NF_PLAN} {plan_type}{C_RESET} {C_GRAY}{NF_DIV}{C_RESET} "
                f"{C_YELLOW}{NF_CLOCK} {expiry_str}{C_RESET}"
            )
            return True, details, None, False

        except Exception as e:
            err_msg = str(e)
            if is_network_or_dns_error(err_msg):
                last_network_error = err_msg
                # If proxy failed with DNS / socket drop, immediately switch to direct
                if not use_direct_fallback and attempts >= max_proxy_attempts:
                    use_direct_fallback = True
                time.sleep(1.0)
                continue

            if attempts < max_total_attempts:
                time.sleep(0.5)
                continue

            return False, None, err_msg[:50], False

    # If all attempts exhausted due to network/DNS resolution, report as transient network error
    display_err = f"Network/DNS Error: {last_network_error[:45]}" if last_network_error else "Connection Failed"
    return False, None, display_err, True


def process_file(file_path: Path, valid_dir: Path, invalid_dir: Path, idx: int, total: int, proxy_list: list = None):
    counter = f"{C_GRAY}[{idx:03d}/{total:03d}]{C_RESET}"
    cookies = load_cookies(file_path)

    if not cookies:
        print(
            f" {counter} {C_YELLOW}{NF_WARN}  EMPTY{C_RESET} {C_GRAY}─{C_RESET} "
            f"{C_WHITE}{NF_FILE} {file_path.name:<24}{C_RESET} "
            f"{C_GRAY}{NF_ARROW} No cookies found{C_RESET}"
        )
        return False

    is_live, details, err, is_network_error = validate_session(cookies, proxy_list=proxy_list)

    if is_live:
        print(
            f" {counter} {C_GREEN}{NF_CHECK}  LIVE{C_RESET}  {C_GRAY}─{C_RESET} "
            f"{C_WHITE}{NF_FILE} {file_path.name:<24}{C_RESET} "
            f"{C_CYAN}{NF_ARROW}{C_RESET} {details}"
        )
        dest = valid_dir / file_path.name
        try:
            file_path.replace(dest)
        except Exception as e:
            print(f" {C_RED}{NF_WARN} Move Error:{C_RESET} {e}")
        return True

    elif is_network_error:
        # Transient network / DNS failure: DO NOT move to invalid! Leave in place for re-scan.
        print(
            f" {counter} {C_YELLOW}{NF_WARN}  RETRY{C_RESET} {C_GRAY}─{C_RESET} "
            f"{C_WHITE}{NF_FILE} {file_path.name:<24}{C_RESET} "
            f"{C_YELLOW}{NF_ARROW} {err} (Retained in folder){C_RESET}"
        )
        return False

    else:
        # Genuine dead/expired token: safely move to invalid
        print(
            f" {counter} {C_RED}{NF_CROSS}  DEAD{C_RESET}  {C_GRAY}─{C_RESET} "
            f"{C_WHITE}{NF_FILE} {file_path.name:<24}{C_RESET} "
            f"{C_GRAY}{NF_ARROW} {err}{C_RESET}"
        )
        dest = invalid_dir / file_path.name
        try:
            file_path.replace(dest)
        except Exception as e:
            print(f" {C_RED}{NF_WARN} Move Error:{C_RESET} {e}")
        return False


def main():
    parser = argparse.ArgumentParser(
        description="High-Speed Multi-Threaded Netscape Cookie Validator with DNS/Network Protection",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser.add_argument(
        "-f",
        "--folder",
        type=str,
        default=r"F:\scrape\cgpt 2",
        help="Target folder containing Netscape cookies/files",
    )
    parser.add_argument(
        "-w",
        "--workers",
        type=int,
        default=8,
        help="Number of concurrent worker threads",
    )
    parser.add_argument(
        "-p",
        "--proxy",
        type=str,
        default=None,
        help="Proxy string or file path containing proxies",
    )
    parser.add_argument(
        "--no-check-proxies",
        action="store_true",
        help="Skip pre-flight proxy health checking",
    )
    args = parser.parse_args()

    target_dir = Path(args.folder).resolve()

    if not target_dir.exists():
        print(f"\n{C_RED}{NF_WARN} Error: Directory does not exist:{C_RESET} {target_dir}\n")
        sys.exit(1)

    valid_dir = target_dir / "valid"
    invalid_dir = target_dir / "invalid"
    valid_dir.mkdir(parents=True, exist_ok=True)
    invalid_dir.mkdir(parents=True, exist_ok=True)

    extensions = {".txt", ".cookie", ".cookies", ".json", ""}
    target_files = [
        p for p in target_dir.iterdir()
        if p.is_file()
        and p.suffix.lower() in extensions
        and not p.name.endswith(".py")
    ]

    total_files = len(target_files)

    # Proxy pre-check
    if args.proxy and not args.no_check_proxies:
        proxies = load_and_verify_proxies(args.proxy)
    elif args.proxy:
        p_path = Path(args.proxy)
        if p_path.is_file():
            proxies = [parse_proxy_string(l) for l in p_path.read_text().splitlines() if parse_proxy_string(l)]
        else:
            p_parsed = parse_proxy_string(args.proxy)
            proxies = [p_parsed] if p_parsed else []
    else:
        proxies = []

    proxy_status = (
        f"{len(proxies)} verified alive" if proxies
        else ("All proxies dead (Using Direct)" if args.proxy else "Direct / Local")
    )
    engine_name = "curl_cffi (DNS/Network Protected)" if HAS_CURL_CFFI else "requests (Standard)"

    print(f"\n{C_CYAN}{NF_LOCK} Session & Netscape Cookie Validator {C_RESET}{C_GREEN}[DNS/Network Protected]{C_RESET}")
    print(f" {C_GRAY}├─{C_RESET} {C_WHITE}{NF_DIR} Directory :{C_RESET} {C_YELLOW}{target_dir}{C_RESET}")
    print(f" {C_GRAY}├─{C_RESET} {C_WHITE}{NF_CPU} Workers   :{C_RESET} {C_MAGENTA}{args.workers} threads{C_RESET}")
    print(f" {C_GRAY}├─{C_RESET} {C_WHITE}{NF_PROXY} Proxies   :{C_RESET} {C_WHITE if proxies else C_GRAY}{proxy_status}{C_RESET}")
    print(f" {C_GRAY}├─{C_RESET} {C_WHITE}{NF_DIRECT} Fallback  :{C_RESET} {C_GREEN}Direct Machine Connection (on 3 fails){C_RESET}")
    print(f" {C_GRAY}├─{C_RESET} {C_WHITE}{NF_NET} Engine    :{C_RESET} {C_GREEN if HAS_CURL_CFFI else C_YELLOW}{engine_name}{C_RESET}")
    print(f" {C_GRAY}└─{C_RESET} {C_WHITE}{NF_FILE} Queued    :{C_RESET} {C_WHITE}{total_files} files{C_RESET}\n")
    print(f"{C_GRAY}{'─' * 76}{C_RESET}")

    if total_files == 0:
        print(f" {C_YELLOW}{NF_WARN} No cookie files found to scan in target directory.{C_RESET}\n")
        return

    live_count = 0
    dead_count = 0
    network_err_count = 0

    with ThreadPoolExecutor(max_workers=args.workers) as executor:
        future_map = {
            executor.submit(
                process_file, f, valid_dir, invalid_dir, i + 1, total_files, proxies
            ): f
            for i, f in enumerate(target_files)
        }

        for future in as_completed(future_map):
            try:
                res = future.result()
                if res:
                    live_count += 1
                else:
                    dead_count += 1
            except Exception:
                dead_count += 1

    print(f"{C_GRAY}{'─' * 76}{C_RESET}")
    print(f"\n{C_CYAN}{NF_LOCK} Scan Results:{C_RESET}")
    print(f" {C_GRAY}├─{C_RESET} {C_GREEN}{NF_CHECK} Live Accounts :{C_RESET} {C_WHITE}{live_count}{C_RESET}")
    print(f" {C_GRAY}├─{C_RESET} {C_RED}{NF_CROSS} Dead Accounts :{C_RESET} {C_WHITE}{dead_count}{C_RESET}")
    print(f" {C_GRAY}└─{C_RESET} {C_WHITE}{NF_FILE} Total Checked :{C_RESET} {C_WHITE}{total_files}{C_RESET}\n")


if __name__ == "__main__":
    main()
