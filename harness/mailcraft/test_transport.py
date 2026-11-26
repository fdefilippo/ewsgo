# SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
# SPDX-License-Identifier: AGPL-3.0-or-later

import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

from transport import exchange


class TransportTests(unittest.TestCase):
    def test_redirect_is_not_followed_and_body_is_bounded(self):
        paths = []
        class Endpoint(BaseHTTPRequestHandler):
            def do_POST(self):
                paths.append(self.path)
                if self.path == "/redirect":
                    self.send_response(302)
                    self.send_header("Location", "/private")
                else:
                    self.send_response(200)
                self.end_headers()
                self.wfile.write(b"synthetic")
            def log_message(self, *args):
                pass
        server = HTTPServer(("127.0.0.1", 0), Endpoint)
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        try:
            request = {"url": f"http://127.0.0.1:{server.server_port}/redirect", "fields": [], "body": [], "limit": 100, "ca": ""}
            self.assertEqual(exchange(request)["status"], 302)
            self.assertEqual(paths, ["/redirect"])
            request.update(url=request["url"].replace("redirect", "body"), limit=2)
            self.assertEqual(exchange(request), {"error": "limit"})
            request["url"] = "http://example.invalid/"
            with self.assertRaises(ValueError):
                exchange(request)
        finally:
            server.shutdown()
            thread.join()
            server.server_close()


if __name__ == "__main__":
    unittest.main()
