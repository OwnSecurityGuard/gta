#!/usr/bin/env python3
"""Replay the game client's client->gateway REST calls against the mock gateway.

Mirrors the order the real Godot client uses (gateway.gd): handshake, login,
worlds, world_characters, world_enter, a failed login, character_create, guest.
Generates real loopback HTTP traffic on 127.0.0.1:8088 for gta-mcp to capture.

    python3 replay_client.py [count]
"""
import json
import sys
import urllib.request

URL = "http://127.0.0.1:8088"
CV = "0.28.0"

CALLS = [
    ("/v1/handshake", {"c-v": CV}),
    ("/v1/login", {"a-u": "hero123", "a-p": "s3cret!", "c-v": CV}),
    ("/v1/worlds", {}),
    ("/v1/world/characters", {"w-id": 1, "a-id": 42, "a-u": "hero123", "t-id": "AbCdEfGhIj"}),
    ("/v1/world/enter", {"t-id": "AbCdEfGhIj", "a-u": "hero123", "w-id": 1, "c-id": 7}),
    ("/v1/login", {"a-u": "baduser", "a-p": "x", "c-v": CV}),  # -> error 50
    ("/v1/world/character/create", {"t-id": "AbCdEfGhIj",
                                     "data": {"name": "Knightly", "skin": 3},
                                     "a-u": "hero123", "w-id": 1}),
    ("/v1/guest", {}),
]


def post(path, body):
    req = urllib.request.Request(URL + path, data=json.dumps(body).encode(),
                                  headers={"Content-Type": "application/json"}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=5) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode())


def run():
    count = int(sys.argv[1]) if len(sys.argv) > 1 else 1
    for i in range(count):
        if i:
            print(f"--- round {i+1} ---", flush=True)
        for path, body in CALLS:
            code, resp = post(path, body)
            print(f"POST {path} -> {code} {json.dumps(resp, ensure_ascii=False)[:80]}",
                  flush=True)


if __name__ == "__main__":
    run()
