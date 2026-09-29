package lightclient

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestInclusionRequiresCanonicalConfirmedBlockAndValidPath(t *testing.T) {
	chain, _, blocks := buildLinearChain(t, 2)

	proof := NewInclusionProofForHeader(blocks[1].Header, blocks[1].Transactions, 0)
	result, err := chain.VerifyInclusion(proof, 2)
	if err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}
	if result.Status != ResultAccepted || result.Confirmations != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}

	fake := proof
	fake.Siblings = append([]Hash(nil), proof.Siblings...)
	fake.Siblings[0][0] ^= 1
	result, err = chain.VerifyInclusion(fake, 2)
	if !errors.Is(err, ErrProofNotBound) && result.Status != ResultRejected {
		t.Fatalf("forged path was accepted: %+v, %v", result, err)
	}
}

func TestMissingAncestorIsRejected(t *testing.T) {
	chain, _, _ := buildLinearChain(t, 0)
	distant := BlockHeader{
		Height:     3,
		ParentRoot: Hash{1},
		Timestamp:  1003,
		Difficulty: TeachingDifficulty,
	}
	if err := chain.AddHeader(distant); !errors.Is(err, ErrMissingAncestor) {
		t.Fatalf("expected missing ancestor, got %v", err)
	}
}

func TestForkSwitchWithdrawsOldConfirmation(t *testing.T) {
	chain, genesis, canonical := buildLinearChain(t, 3)

	oldProof := NewInclusionProofForHeader(canonical[1].Header, canonical[1].Transactions, 0)
	if _, err := chain.VerifyInclusion(oldProof, 2); err != nil {
		t.Fatalf("old-chain proof should initially verify: %v", err)
	}

	forked := make([]Block, 4)
	parent := genesis.Header
	for i := range forked {
		block, err := MineChild(parent, uint64(2000+i), [][]byte{[]byte("fork-tx")})
		if err != nil {
			t.Fatal(err)
		}
		forked[i] = block
		parent = block.Header
	}
	for _, block := range forked {
		if err := chain.AddHeader(block.Header); err != nil {
			t.Fatal(err)
		}
	}

	tip := chain.CanonicalTip()
	if tip.Hash != BlockHeaderHash(forked[3].Header) {
		t.Fatal("heaviest fork did not become the canonical tip")
	}
	result, err := chain.VerifyInclusion(oldProof, 2)
	if !errors.Is(err, ErrStaleFork) || result.Status != ResultRejected {
		t.Fatalf("old fork proof was not withdrawn: %+v, %v", result, err)
	}

	forkProof := NewInclusionProofForHeader(forked[1].Header, forked[1].Transactions, 0)
	result, err = chain.VerifyInclusion(forkProof, 2)
	if err != nil {
		t.Fatalf("current fork proof rejected: %v", err)
	}
	if result.BlockHash != BlockHeaderHash(forked[1].Header) || result.Status != ResultAccepted {
		t.Fatalf("current fork proof did not bind correctly: %+v", result)
	}
}

func TestRestartPreservesImmutableTrustBoundary(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	chain, genesis, blocks := buildLinearChain(t, 2)
	proof := NewInclusionProofForHeader(blocks[1].Header, blocks[1].Transactions, 0)
	result, err := chain.VerifyInclusion(proof, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTrustRoot(genesis); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveChain(chain); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendResult(chain, result); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.LoadChain(TeachingProofOfWork{})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.anchor.BlockHash != genesis.BlockHash {
		t.Fatal("reopened trust root changed")
	}
	if _, err := reopened.VerifyInclusion(proof, 2); err != nil {
		t.Fatalf("proof should still verify after restart: %v", err)
	}

	fake := genesis
	fake.Name = "attacker replacement"
	if err := store.SaveTrustRoot(fake); err == nil {
		t.Fatal("trust root was replaced after restart")
	}
	if _, err := store.LoadResults(); err != nil {
		t.Fatalf("results should remain independently readable: %v", err)
	}
}

func TestProofMustNameCurrentBlock(t *testing.T) {
	chain, _, blocks := buildLinearChain(t, 2)
	proof := NewInclusionProofForHeader(blocks[1].Header, blocks[1].Transactions, 0)
	proof.BlockHash = BlockHeaderHash(blocks[0].Header)
	result, err := chain.VerifyInclusion(proof, 1)
	if !errors.Is(err, ErrProofNotBound) || result.Status != ResultRejected {
		t.Fatalf("proof bound to another block was accepted: %+v, %v", result, err)
	}
}

func buildLinearChain(t *testing.T, height uint64) (*Chain, TrustRoot, []Block) {
	t.Helper()
	genesisBlock, anchor, err := MineGenesis(1000, [][]byte{[]byte("genesis-tx")})
	if err != nil {
		t.Fatal(err)
	}
	anchor.CreatedAt = time.Unix(1700000000, 0).UTC()
	chain, err := NewChain(anchor, TeachingProofOfWork{})
	if err != nil {
		t.Fatal(err)
	}
	blocks := []Block{genesisBlock}
	parent := genesisBlock.Header
	for h := uint64(1); h <= height; h++ {
		block, err := MineChild(parent, 1000+h, [][]byte{[]byte("tx-")})
		if err != nil {
			t.Fatal(err)
		}
		if err := chain.AddHeader(block.Header); err != nil {
			t.Fatal(err)
		}
		blocks = append(blocks, block)
		parent = block.Header
	}
	return chain, anchor, blocks
}

func TestStoreRejectsOrphanedPersistedHeader(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	chain, anchor, _ := buildLinearChain(t, 0)
	if err := store.SaveTrustRoot(anchor); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveChain(chain); err != nil {
		t.Fatal(err)
	}

	orphan := BlockHeader{Height: 9, Difficulty: TeachingDifficulty, Timestamp: 1}
	payload := headerStore{
		CanonicalTip: BlockHeaderHash(orphan),
		Headers:      []storedHeader{{Hash: BlockHeaderHash(orphan), Header: orphan}},
	}
	if err := writeJSON(filepath.Join(dir, headersFile), payload); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadChain(TeachingProofOfWork{}); err == nil {
		t.Fatal("store loaded a chain with a missing ancestor")
	}
}
