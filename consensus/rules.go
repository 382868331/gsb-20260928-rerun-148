package consensus

import (
	"math/big"

	"github.com/example/light-client-research/types"
)

type Rules interface {
	ValidateHeader(header types.BlockHeader, parent types.BlockHeader, hasParent bool) error
	ChildWork(parentWork *big.Int) *big.Int
	Preferred(candidateWork, currentWork *big.Int, candidateHash, currentHash types.Hash) bool
}
