// Package a2a builds requests for kagent's A2A endpoint (protocol 0.3).
package a2a

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"

	"github.com/jhyoong/RANK-proxy-webapp/internal/config"
)

// Path is the Rancher-relative URL path of the agent's A2A endpoint inside
// clusterID. It ends in "/". url.PathEscape leaves ':' alone, so a service
// like http:kagent-controller:8083 reaches Rancher unescaped. It mirrors the
// path rancher.Client.AgentCard builds; a test keeps the two in step.
func Path(clusterID string, k config.KagentConfig) string {
	e := url.PathEscape
	return "/k8s/clusters/" + e(clusterID) + "/api/v1/namespaces/" + e(k.Namespace) + "/services/" + e(k.Service) +
		"/proxy/api/a2a/" + e(k.AgentNamespace) + "/" + e(k.AgentName) + "/"
}

// StreamRequest is the JSON-RPC message/stream body for one user message.
func StreamRequest(text, contextID string) any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      NewUUID(),
		"method":  "message/stream",
		"params": map[string]any{
			"message": map[string]any{
				"kind":      "message",
				"messageId": NewUUID(),
				"contextId": contextID,
				"role":      "user",
				"parts":     []map[string]any{{"kind": "text", "text": text}},
			},
		},
	}
}

// NewUUID returns a random (v4) UUID from crypto/rand.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:], b[10:])
	return string(out[:])
}

// ValidUUID reports whether s is 8-4-4-4-12 lowercase hex.
func ValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}
