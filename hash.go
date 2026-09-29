package lightclient

import "crypto/sha256"

const (
	domainBlockHeader = 0
	domainTransaction = 1
	domainMerkleLeaf  = 2
	domainMerkleNode  = 3
)

func sha256Hash(data []byte) [32]byte {
	return sha256.Sum256(data)
}
