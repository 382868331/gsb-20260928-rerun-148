package types

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/bits"
	"strings"
)

type Hash [32]byte

func HashFromHex(value string) (Hash, error) {
	var result Hash
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "0x"))
	if err != nil {
		return result, err
	}
	if len(decoded) != len(result) {
		return result, fmt.Errorf("hash must be 32 bytes, got %d", len(decoded))
	}
	copy(result[:], decoded)
	return result, nil
}

func (h Hash) Hex() string {
	return hex.EncodeToString(h[:])
}

func (h Hash) String() string {
	return h.Hex()
}

type BlockHeader struct {
	Version    uint32 `json:"version"`
	Height     uint64 `json:"height"`
	ParentHash Hash   `json:"parentHash"`
	TxRoot     Hash   `json:"txRoot"`
	Timestamp  uint64 `json:"timestamp"`
	Nonce      uint64 `json:"nonce"`
}

func (header BlockHeader) Hash() Hash {
	return DigestBlock(header)
}

func DigestBlock(header BlockHeader) Hash {
	buffer := make([]byte, 0, 96)
	var word [8]byte
	binary.BigEndian.PutUint32(word[:4], header.Version)
	buffer = append(buffer, word[:4]...)
	binary.BigEndian.PutUint64(word[:], header.Height)
	buffer = append(buffer, word[:]...)
	buffer = append(buffer, header.ParentHash[:]...)
	buffer = append(buffer, header.TxRoot[:]...)
	binary.BigEndian.PutUint64(word[:], header.Timestamp)
	buffer = append(buffer, word[:]...)
	binary.BigEndian.PutUint64(word[:], header.Nonce)
	buffer = append(buffer, word[:]...)
	return sha256.Sum256(buffer)
}

type Transaction struct {
	Payload []byte `json:"payload"`
}

func DigestTransaction(tx Transaction) Hash {
	return sha256.Sum256(append([]byte("teaching-tx\x00"), tx.Payload...))
}

var emptyLeafHash = sha256.Sum256([]byte("teaching-empty-leaf"))

func leafHash(txHash Hash) Hash {
	return sha256.Sum256(append([]byte("teaching-leaf\x00"), txHash[:]...))
}

func nodeHash(left, right Hash) Hash {
	var buffer []byte
	buffer = append(buffer, "teaching-node\x00"...)
	buffer = append(buffer, left[:]...)
	buffer = append(buffer, right[:]...)
	return sha256.Sum256(buffer)
}

func paddedLeafHashes(txHashes []Hash) []Hash {
	if len(txHashes) == 0 {
		return nil
	}
	size := 1
	for size < len(txHashes) || size < 2 {
		size *= 2
	}
	leaves := make([]Hash, size)
	for index, txHash := range txHashes {
		leaves[index] = leafHash(txHash)
	}
	for index := len(txHashes); index < size; index++ {
		leaves[index] = emptyLeafHash
	}
	return leaves
}

func TransactionMerkleRoot(transactions []Transaction) Hash {
	var txHashes []Hash
	for _, tx := range transactions {
		txHashes = append(txHashes, DigestTransaction(tx))
	}
	return MerkleRootFromHashes(txHashes)
}

func MerkleRootFromHashes(txHashes []Hash) Hash {
	level := paddedLeafHashes(txHashes)
	for len(level) > 1 {
		next := make([]Hash, 0, len(level)/2)
		for index := 0; index < len(level); index += 2 {
			next = append(next, nodeHash(level[index], level[index+1]))
		}
		level = next
	}
	if len(level) == 0 {
		return Hash{}
	}
	return level[0]
}

type InclusionProof struct {
	BlockHash Hash   `json:"blockHash"`
	TxHash    Hash   `json:"txHash"`
	LeafIndex uint64 `json:"leafIndex"`
	Siblings  []Hash `json:"siblings"`
}

func BuildInclusionProof(blockHash Hash, transactions []Transaction, leafIndex uint64) (InclusionProof, error) {
	if int(leafIndex) >= len(transactions) {
		return InclusionProof{}, fmt.Errorf("leaf index %d is outside transaction count %d", leafIndex, len(transactions))
	}
	var txHashes []Hash
	for _, tx := range transactions {
		txHashes = append(txHashes, DigestTransaction(tx))
	}
	level := paddedLeafHashes(txHashes)
	depth := bits.TrailingZeros(uint(len(level)))
	proof := InclusionProof{BlockHash: blockHash, TxHash: txHashes[leafIndex], LeafIndex: leafIndex}
	index := int(leafIndex)
	for levelSize := len(level); levelSize > 1; levelSize /= 2 {
		sibling := index ^ 1
		proof.Siblings = append(proof.Siblings, level[sibling])
		current := make([]Hash, 0, levelSize/2)
		for pair := 0; pair < levelSize; pair += 2 {
			current = append(current, nodeHash(level[pair], level[pair+1]))
		}
		level = current
		index /= 2
	}
	if len(proof.Siblings) != depth {
		return InclusionProof{}, fmt.Errorf("internal proof construction error")
	}
	return proof, nil
}

func (proof InclusionProof) Root() (Hash, error) {
	leafCount := uint64(1) << len(proof.Siblings)
	if leafCount < 2 || proof.LeafIndex >= leafCount {
		return Hash{}, fmt.Errorf("proof path does not fit a valid binary merkle tree")
	}
	current := leafHash(proof.TxHash)
	index := proof.LeafIndex
	for level := 0; level < len(proof.Siblings); level++ {
		sibling := proof.Siblings[level]
		if index&1 == 0 {
			current = nodeHash(current, sibling)
		} else {
			current = nodeHash(sibling, current)
		}
		index >>= 1
	}
	return current, nil
}
