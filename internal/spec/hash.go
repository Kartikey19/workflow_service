package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Hash is sha256 over canonical JSON. encoding/json sorts map keys and struct
// fields have a fixed order, so equal specs always hash equally.
func (s *Spec) Hash() (string, error) {
	encoded, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode spec for hashing: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Node returns the normalized node with id.
func (s *Spec) Node(id string) (NodeSpec, bool) {
	for _, node := range s.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return NodeSpec{}, false
}
