package consensus

import (
	"fmt"
	"math/big"

	"github.com/example/light-client-research/types"
)

const TeachingVersion = 0

type TeachingRules struct {
	Target *big.Int
}

func NewTeachingRules() TeachingRules {
	// The first byte must be zero. This is deliberately easy for tests and examples.
	target := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 248), big.NewInt(1))
	return TeachingRules{Target: target}
}

func (rules TeachingRules) ValidateHeader(header types.BlockHeader, parent types.BlockHeader, hasParent bool) error {
	if header.Version != TeachingVersion {
		return fmt.Errorf("unsupported teaching header version %d", header.Version)
	}
	if !hasParent {
		return fmt.Errorf("missing parent %s", header.ParentHash)
	}
	if header.Height != parent.Height+1 {
		return fmt.Errorf("height %d must follow parent height %d", header.Height, parent.Height)
	}
	if header.ParentHash != types.DigestBlock(parent) {
		return fmt.Errorf("parent hash does not commit to the parent header")
	}
	if header.Timestamp <= parent.Timestamp {
		return fmt.Errorf("timestamp must strictly increase")
	}
	if header.Hash() == types.DigestBlock(parent) {
		return fmt.Errorf("block hash equals parent hash")
	}
	hash := header.Hash()
	hashValue := new(big.Int).SetBytes(hash[:])
	if hashValue.Cmp(rules.Target) > 0 {
		return fmt.Errorf("block hash %s exceeds proof-of-work target", hash)
	}
	return nil
}

func (rules TeachingRules) ChildWork(parentWork *big.Int) *big.Int {
	return new(big.Int).Add(parentWork, big.NewInt(1))
}

func (rules TeachingRules) Preferred(candidateWork, currentWork *big.Int, candidateHash, currentHash types.Hash) bool {
	comparison := candidateWork.Cmp(currentWork)
	if comparison != 0 {
		return comparison > 0
	}
	for index := range candidateHash {
		if candidateHash[index] != currentHash[index] {
			return candidateHash[index] < currentHash[index]
		}
	}
	return false
}

func MineNext(parent types.BlockHeader, txRoot types.Hash, timestamp uint64) types.BlockHeader {
	header := types.BlockHeader{
		Version:    TeachingVersion,
		Height:     parent.Height + 1,
		ParentHash: types.DigestBlock(parent),
		TxRoot:     txRoot,
		Timestamp:  timestamp,
	}
	for header.Nonce = 0; ; header.Nonce++ {
		if NewTeachingRules().ValidateHeader(header, parent, true) == nil {
			return header
		}
		if header.Nonce == ^uint64(0) {
			panic("failed to mine teaching block")
		}
	}
}

func GenesisHeader(timestamp uint64) types.BlockHeader {
	return types.BlockHeader{Version: TeachingVersion, Height: 0, Timestamp: timestamp}
}

func MineGenesis(timestamp uint64) types.BlockHeader {
	rules := NewTeachingRules()
	header := GenesisHeader(timestamp)
	for header.Nonce = 0; ; header.Nonce++ {
		hash := types.DigestBlock(header)
		if new(big.Int).SetBytes(hash[:]).Cmp(rules.Target) <= 0 {
			return header
		}
	}
}
