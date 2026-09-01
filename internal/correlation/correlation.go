package correlation

import (
	"crypto/sha256"
	"encoding/hex"
)

func ID(agentID, idempotencyKey string) string {
	sum := sha256.Sum256([]byte(agentID + "\x00" + idempotencyKey))
	return hex.EncodeToString(sum[:16])
}
