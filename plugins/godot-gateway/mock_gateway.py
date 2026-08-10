#!/usr/bin/env python3
"""Mock Godot Tiny MMO gateway for live-capture demos.

Implements the 8 /v1/* REST endpoints the game client hits, returning
fixture-shaped JSON. Run it on 127.0.0.1:8088 so gta-mcp can capture the real
loopback client<->gateway HTTP traffic and decode it with the godot-gateway
plugin. This stands in for the real Godot gateway (http_server.gd), which isn't
running in this environment.

    python3 mock_gateway.py
"""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import sys

WORLDS = {
    "1": {"info": {"name": "Westreach", "motd": "Welcome, traveler!", "pvp": False}},
    "2": {"info": {"name": "Frostpeak", "motd": "", "pvp": True}},
}


def j(body, code=200):
    data = json.dumps(body).encode()
    return code, {"Content-Type": "application/json", "Content-Length": str(len(data))}, data


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):  # quiet
        pass

    def _body(self):
        n = int(self.headers.get("Content-Length", 0) or 0)
        return json.loads(self.rfile.read(n) or b"{}") if n else {}

    def _send(self, code, headers, data):
        self.send_response(code)
        for k, v in headers.items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        b = self._body()
        p = self.path
        if p == "/v1/handshake":
            cv = b.get("c-v", "")
            if cv != "0.28.0":
                return self._send(*j({"error": 70, "msg": f"outdated ({cv})"}, 200))
            return self._send(*j({"ok": True}))
        if p == "/v1/login":
            if b.get("a-u") == "baduser":
                return self._send(*j({"error": 50}, 200))
            return self._send(*j({"session_id": "AbCdEfGhIj0123456789",
                                   "name": b.get("a-u", "hero"), "id": 42, "w": WORLDS}))
        if p == "/v1/guest":
            return self._send(*j({"session_id": "GuEsT0000000000000000",
                                   "name": "guest", "id": 99, "w": WORLDS}))
        if p == "/v1/account/create":
            return self._send(*j({"name": b.get("a-u"), "id": 43, "w": WORLDS}))
        if p == "/v1/worlds":
            return self._send(*j({"w": WORLDS}))
        if p == "/v1/world/characters":
            return self._send(*j({"7": {"name": "Knightly", "level": 12, "skin": 3},
                                      "8": {"name": "Rogue", "level": 5, "skin": 1}}))
        if p == "/v1/world/enter":
            return self._send(*j({"address": "127.0.0.1", "port": 27015,
                                   "auth-token": "at-xyz123"}))
        if p == "/v1/world/character/create":
            d = b.get("data", {})
            return self._send(*j({"address": "127.0.0.1", "port": 27015,
                                   "auth-token": "at-abc456"}))
        self._send(*j({"error": "invalid_payload"}, 200))


if __name__ == "__main__":
    srv = ThreadingHTTPServer(("127.0.0.1", 8088), H)
    print("mock gateway listening on http://127.0.0.1:8088", flush=True)
    srv.serve_forever()
