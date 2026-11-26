#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
# SPDX-License-Identifier: AGPL-3.0-or-later

"""One bounded HTTP exchange. Private data only travels through anonymous pipes."""
import json
import socket
import ssl
import sys
import urllib.error
import urllib.parse
import urllib.request
import ipaddress


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def exchange(v):
    url = urllib.parse.urlsplit(v["url"])
    if url.username or url.password or url.fragment:
        raise ValueError()
    if url.scheme != "https":
        if url.scheme != "http" or not ipaddress.ip_address(url.hostname).is_loopback:
            raise ValueError()
    context = ssl.create_default_context(cafile=v["ca"] or None)
    opener = urllib.request.build_opener(NoRedirect(), urllib.request.HTTPSHandler(context=context))
    req = urllib.request.Request(v["url"], bytes(v["body"]), dict(v["fields"]), method="POST")
    try:
        response = opener.open(req, timeout=20)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        data = response.read(min(int(v["limit"]), 16 << 20) + 1)
        if len(data) > min(int(v["limit"]), 16 << 20):
            return {"error": "limit"}
        return {"status": response.code, "body": list(data), "fields": [(k, val) for k, val in response.headers.items() if k.lower() in ("www-authenticate", "location", "retry-after", "x-ews-policy-reason")]}


if __name__ == "__main__":
    try:
        result = exchange(json.load(sys.stdin))
    except (ssl.SSLError, urllib.error.URLError) as error:
        result = {"error": "tls" if isinstance(error, ssl.SSLError) or isinstance(getattr(error, "reason", None), ssl.SSLError) else "connection"}
    except (TimeoutError, socket.timeout):
        result = {"error": "timeout"}
    except Exception:
        result = {"error": "transport"}
    json.dump(result, sys.stdout)
