package api

import (
	"net/http"
	"strings"
)

const (
	HeaderAgentID = "X-Idemio-Agent-Id"
	HeaderRole    = "X-Idemio-Role"

	RoleAgent = "agent"
)

type identity struct {
	AgentID string
	Role    string
}

func readIdentity(r *http.Request) (identity, bool) {
	agentID := strings.TrimSpace(r.Header.Get(HeaderAgentID))
	role := strings.TrimSpace(r.Header.Get(HeaderRole))

	if agentID == "" || role == "" {
		return identity{}, false
	}
	return identity{AgentID: agentID, Role: role}, true
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
