package correlation

import (
	"crypto/sha256"
	"encoding/hex"
)

// Derived from the key rather than being the key, because the same idempotency key may
// exist under two agents and a probe must not confuse them.
func ID(agentID, idempotencyKey string) string {
	sum := sha256.Sum256([]byte(agentID + "\x00" + idempotencyKey))
	return hex.EncodeToString(sum[:16])
}
