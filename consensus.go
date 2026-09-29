package lightclient

import (
	"errors"
	"fmt"
)

const TeachingDifficulty uint64 = 8

// Consensus is the replaceable boundary for other consensus rules.
type Consensus interface {
	ValidateGenesis(header BlockHeader) error
	ValidateChild(parent, child BlockHeader) error
	CompareTips(left Tip, right Tip) int
}

type Tip struct {
	Hash            Hash
	Height          uint64
	TotalDifficulty uint64
}

// TeachingProofOfWork is intentionally simple and explicit:
// - every block has the fixed TeachingDifficulty;
// - heights and parent hashes are consecutive;
// - timestamps strictly increase;
// - hash(header) has at least Difficulty leading zero bits;
// - heaviest total difficulty wins, with smaller hash as the only tie-break.
type TeachingProofOfWork struct{}

func (TeachingProofOfWork) ValidateGenesis(header BlockHeader) error {
	if header.Height != 0 {
		return errors.New("genesis height must be zero")
	}
	if header.ParentRoot != (Hash{}) {
		return errors.New("genesis parent root must be zero")
	}
	if header.Difficulty != TeachingDifficulty {
		return errors.New("genesis has wrong difficulty")
	}
	if !meetsDifficulty(BlockHeaderHash(header), header.Difficulty) {
		return errors.New("genesis does not satisfy proof of work")
	}
	return nil
}

func (TeachingProofOfWork) ValidateChild(parent, child BlockHeader) error {
	if child.Height != parent.Height+1 {
		return fmt.Errorf("child height %d does not extend parent height %d", child.Height, parent.Height)
	}
	if child.ParentRoot != BlockHeaderHash(parent) {
		return errors.New("child parent root does not bind the full parent header")
	}
	if child.Timestamp <= parent.Timestamp {
		return errors.New("child timestamp must be strictly greater than parent timestamp")
	}
	if child.Difficulty != TeachingDifficulty {
		return errors.New("child has wrong difficulty")
	}
	if !meetsDifficulty(BlockHeaderHash(child), child.Difficulty) {
		return errors.New("child does not satisfy proof of work")
	}
	return nil
}

func (TeachingProofOfWork) CompareTips(left, right Tip) int {
	if left.TotalDifficulty > right.TotalDifficulty {
		return 1
	}
	if left.TotalDifficulty < right.TotalDifficulty {
		return -1
	}
	if left.Height > right.Height {
		return 1
	}
	if left.Height < right.Height {
		return -1
	}
	return compareHashes(left.Hash, right.Hash)
}

func compareHashes(left, right Hash) int {
	for i := 0; i < len(left); i++ {
		if left[i] < right[i] {
			return 1
		}
		if left[i] > right[i] {
			return -1
		}
	}
	return 0
}

func meetsDifficulty(hash Hash, difficulty uint64) bool {
	if difficulty > 255 {
		return false
	}
	fullBytes := difficulty / 8
	remaining := difficulty % 8
	for i := uint64(0); i < fullBytes; i++ {
		if hash[i] != 0 {
			return false
		}
	}
	if remaining != 0 && hash[fullBytes]&(0xff<<(8-remaining)) != 0 {
		return false
	}
	return true
}
