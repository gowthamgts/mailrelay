#!/usr/bin/env python3
"""Verify a captured webhook: MAILRELAY_SIGNING_SECRET=... ./scripts/verify-webhook.py TIMESTAMP SIGNATURE < body.json"""
import hashlib
import hmac
import os
import re
import sys
import time


def verify(secret, body, timestamp, signature):
    if not re.fullmatch(r"[0-9]+", timestamp):
        return False
    if abs(int(time.time()) - int(timestamp)) > 300:
        return False
    if not re.fullmatch(r"sha256=[0-9a-f]{64}", signature):
        return False
    expected = "sha256=" + hmac.new(
        secret.encode("utf-8"), timestamp.encode("ascii") + b"." + body,
        hashlib.sha256,
    ).hexdigest()
    return hmac.compare_digest(expected, signature)


if __name__ == "__main__":
    if len(sys.argv) != 3 or not os.environ.get("MAILRELAY_SIGNING_SECRET"):
        sys.exit(__doc__)
    valid = verify(os.environ["MAILRELAY_SIGNING_SECRET"], sys.stdin.buffer.read(), *sys.argv[1:])
    print("valid" if valid else "invalid or expired signature")
    sys.exit(0 if valid else 1)
