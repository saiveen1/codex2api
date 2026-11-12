# SPDX-License-Identifier: AGPL-3.0-only
# Adapted from yukkcat/chatgpt2api utils/pow.py at 4336d0594dd4fe2962bca5b0b55bc95a24686e59.
# Changes: stdlib base64, reduced fingerprint choices, bounded CPU/deadline, fail closed.
import base64
import hashlib
import json
import random
import re
import time
import uuid
from datetime import datetime, timedelta, timezone
from html.parser import HTMLParser

from contract import SafeError

SDK = "https://chatgpt.com/backend-api/sentinel/sdk.js"

class Resources(HTMLParser):
    def __init__(self, html):
        super().__init__()
        self.scripts, self.build = [], ""
        self.feed(html)
        if not self.build:
            match = re.search(r'<html[^>]*data-build="([^"]*)"', html)
            self.build = match.group(1) if match else ""

    def handle_starttag(self, tag, attrs):
        if tag == "script" and dict(attrs).get("src"):
            src = dict(attrs)["src"]
            self.scripts.append(src)
            match = re.search(r"c/[^/]*/_", src)
            if match:
                self.build = match.group(0)


def config(user_agent, resources):
    now = datetime.now(timezone(timedelta(hours=-5)))
    return [3000, now.strftime("%a %b %d %Y %H:%M:%S") + " GMT-0500 (Eastern Standard Time)",
            4294705152, 1, user_agent, random.choice(resources.scripts or [SDK]), resources.build,
            "en-US", "en-US,es-US,en,es", random.random(), "webdriver−false", "location", "chrome",
            time.perf_counter() * 1000, str(uuid.uuid4()), "", 16,
            time.time() * 1000 - time.perf_counter() * 1000, 0, 0, 0, 0, 0, 0, 0]


def encoded(value):
    return base64.b64encode(json.dumps(value, separators=(",", ":"), ensure_ascii=False).encode())


def requirements_token(user_agent, resources):
    return "gAAAAAC" + encoded(config(user_agent, resources)).decode()


def proof_token(info, user_agent, resources, deadline):
    if not info.get("required"):
        return ""
    try:
        difficulty, seed = info["difficulty"], info["seed"]
        if not isinstance(seed, str) or len(seed) > 4096 or not re.fullmatch(r"(?:[0-9a-fA-F]{2}){1,64}", difficulty):
            raise ValueError()
        target = bytes.fromhex(difficulty)
    except Exception:
        raise SafeError("environment_challenge", 403) from None
    value = config(user_agent, resources)
    limit = min(deadline, time.monotonic() + 3)
    for nonce in range(500000):
        if nonce % 256 == 0 and time.monotonic() >= limit:
            break
        value[3], value[9] = nonce, nonce >> 1
        answer = encoded(value)
        if hashlib.sha3_512(seed.encode() + answer).digest()[:len(target)] <= target:
            return "gAAAAAB" + answer.decode()
    raise SafeError("environment_challenge", 403)
