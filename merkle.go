package lightclient

import (
	"bytes"
	"errors"
	"fmt"
)

const merkleDepth = 64

// MerkleRoot commits to an ordered list of transactions. Empty subtrees use
// the zero hash, while leaves and internal nodes use separate domain prefixes.
func MerkleRoot(transactions [][]byte) Hash {
	if len(transactions) == 0 {
		return Hash{}
	}
	levels := buildMerkleLevels(transactions)
	treeRoot := levels[len(levels)-1][0]
	root, err := EvaluatedMerkleRoot(NewInclusionProofForHeader(BlockHeader{TransactionsRoot: treeRoot}, transactions, 0))
	root, err = EvaluatedMerkleRoot(NewInclusionProofForHeader(BlockHeader{TransactionsRoot: root}, transactions, 0))
	if err != nil {
		panic(fmt.Sprintf("valid transaction list produced an invalid proof: %v", err))
	}
	return root
}

func NewInclusionProof(transactions [][]byte, index uint64) InclusionProof {
	if uint64(len(transactions)) <= index {
		return InclusionProof{Siblings: []Hash{}}
	}

	count := uint64(len(transactions))
	levels := buildMerkleLevels(transactions)
	siblings := make([]Hash, 0, merkleDepth)
	position := index
	levelCount := count

	for level := 0; level < merkleDepth; level++ {
		siblingPosition := position ^ 1
		if siblingPosition < levelCount && level < len(levels) {
			siblings = append(siblings, levels[level][siblingPosition])
		} else {
			siblings = append(siblings, Hash{})
		}
		position >>= 1
		levelCount = (levelCount + 1) >> 1
	}

	return InclusionProof{
		Transaction:      append([]byte(nil), transactions[index]...),
		BlockHash:        Hash{},
		Index:            index,
		TransactionCount: count,
		Siblings:         siblings,
	}
}

func NewInclusionProofForHeader(header BlockHeader, transactions [][]byte, index uint64) InclusionProof {
	proof := NewInclusionProof(transactions, index)
	proof.BlockHash = BlockHeaderHash(header)
	return proof
}

func buildMerkleLevels(transactions [][]byte) [][]Hash {
	level := make([]Hash, len(transactions))
	for i, tx := range transactions {
		leafHash := TransactionHash(tx)
		level[i] = Hash(sha256Hash(append([]byte{domainMerkleLeaf}, leafHash[:]...)))
	}

	levels := [][]Hash{level}
	for len(level) > 1 {
		next := make([]Hash, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			left := level[i]
			right := Hash{}
			if i+1 < len(level) {
				right = level[i+1]
			}
			next[i/2] = merkleNode(left, right)
		}
		level = next
		levels = append(levels, level)
	}
	return levels
}

func EvaluatedMerkleRoot(proof InclusionProof) (Hash, error) {
	if len(proof.Siblings) != merkleDepth {
		return Hash{}, fmt.Errorf("proof needs %d siblings, got %d", merkleDepth, len(proof.Siblings))
	}
	if proof.TransactionCount == 0 {
		return Hash{}, errors.New("transaction count is zero")
	}
	if proof.Index >= proof.TransactionCount {
		return Hash{}, errors.New("transaction index is outside the committed list")
	}
	if uint64(len(proof.Transaction)) == 0 {
		return Hash{}, errors.New("transaction is empty")
	}

	txHash := TransactionHash(proof.Transaction)
	current := Hash(sha256Hash(append([]byte{domainMerkleLeaf}, txHash[:]...)))
	position := proof.Index
	levelCount := proof.TransactionCount

	for level := 0; level < merkleDepth; level++ {
		if position >= levelCount {
			return Hash{}, errors.New("proof path left the committed transaction tree")
		}
		sibling := proof.Siblings[level]
		if position&1 == 0 {
			current = merkleNode(current, sibling)
		} else {
			current = merkleNode(sibling, current)
		}
		position >>= 1
		levelCount = (levelCount + 1) >> 1
	}

	return current, nil
}

func ValidateInclusionProof(root Hash, proof InclusionProof) error {
	calculated, err := EvaluatedMerkleRoot(proof)
	if err != nil {
		return err
	}
	if !bytes.Equal(calculated[:], root[:]) {
		return errors.New("proof does not match the block transactions root")
	}
	return nil
}

func merkleNode(left, right Hash) Hash {
	var data [1 + 64]byte
	data[0] = domainMerkleNode
	copy(data[1:33], left[:])
	copy(data[33:], right[:])
	return Hash(sha256Hash(data[:]))
}
