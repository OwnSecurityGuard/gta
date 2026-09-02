package main

import "fmt"

// protocol.go — knowledge of the Godot Tiny MMO (Ekonia) client→gateway protocol.
//
// Source of truth (in-repo):
//   - source/common/network/gateway_api.gd      (endpoint URLs + short key names + error codes)
//   - source/client/gateway/gateway.gd          (client request builders)
//   - source/server/gateway/http_server.gd      (server handlers + response shapes)
//
// The client serializes a Dictionary with JSON.stringify and POSTs it with
// Content-Type: application/json. The gateway (http_server.gd) listens on
// 127.0.0.1:8088 locally and ws.ekoniaonline.com in release.

// Wire-short key names sent by the client (GatewayAPI.*).
const (
	keyRequestID     = "r-id"
	keyTokenID       = "t-id"
	keyAccountID     = "a-id"
	keyAccountUser   = "a-u"
	keyAccountPass   = "a-p"
	keyWorldID       = "w-id"
	keyCharID        = "c-id"
	keyClientVersion = "c-v"
)

// GatewayAuth error codes (GatewayAPI.ERR_*).
const (
	errGeneric             = 1
	errAccountCreateFailed = 30
	errBadCredentials      = 50
	errAlreadyConnected    = 51
	errRateLimited         = 60
	errOutdatedVersion     = 70
)

// errorName maps a raw gateway error value to its symbolic name.
// GatewayAPI error codes are integers; "invalid_payload"/"connection_failed"
// are short string tokens; anything else is an already-human server message.
func errorName(v any) string {
	switch x := v.(type) {
	case int64:
		switch x {
		case errGeneric:
			return "ERR_GENERIC"
		case errAccountCreateFailed:
			return "ERR_ACCOUNT_CREATE_FAILED"
		case errBadCredentials:
			return "ERR_BAD_CREDENTIALS"
		case errAlreadyConnected:
			return "ERR_ALREADY_CONNECTED"
		case errRateLimited:
			return "ERR_RATE_LIMITED"
		case errOutdatedVersion:
			return "ERR_OUTDATED_VERSION"
		default:
			return fmt.Sprintf("ERR_%d", x)
		}
	case float64:
		return errorName(int64(x))
	case int:
		return errorName(int64(x))
	case string:
		return x
	default:
		return ""
	}
}

// endpointFromPath returns the symbolic action for a gateway request path.
func endpointFromPath(path string) string {
	switch {
	case endsWith(path, "/v1/handshake"):
		return "handshake"
	case endsWith(path, "/v1/login"):
		return "login"
	case endsWith(path, "/v1/guest"):
		return "guest"
	case endsWith(path, "/v1/account/create"):
		return "account_create"
	case endsWith(path, "/v1/worlds"):
		return "worlds"
	case endsWith(path, "/v1/world/characters"):
		return "world_characters"
	case endsWith(path, "/v1/world/enter"):
		return "world_enter"
	case endsWith(path, "/v1/world/character/create"):
		return "character_create"
	default:
		return "unknown"
	}
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func copyField(dest, src map[string]any, from, to string) {
	if v, ok := src[from]; ok {
		dest[to] = v
	}
}
