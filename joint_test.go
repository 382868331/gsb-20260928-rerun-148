package lightclient

import (
	"errors"
	"testing"
	"time"
)

type fixtureBlocks map[Hash]Block

func (f fixtureBlocks) BlockByHash(hash Hash) (Block, bool) {
	block, ok := f[hash]
	return block, ok
}

func buildForkFixture(t *testing.T) (*Chain, TrustRoot, []Block, []Block, fixtureBlocks) {
	t.Helper()
	genesis, anchor, err := MineGenesis(1000, [][]byte{[]byte("genesis-tx")})
	if err != nil {
		t.Fatal(err)
	}
	anchor.CreatedAt = time.Unix(1700000000, 0).UTC()
	chain, err := NewChain(anchor, TeachingProofOfWork{})
	if err != nil {
		t.Fatal(err)
	}

	original := []Block{genesis}
	parent := genesis.Header
	for h := uint64(1); h <= 3; h++ {
		transactions := [][]byte{[]byte("shared"), []byte("dup-tx")}
		block, err := MineChild(parent, 1000+h, transactions)
		if err != nil {
			t.Fatal(err)
		}
		original = append(original, block)
		if err := chain.AddHeader(block.Header); err != nil {
			t.Fatal(err)
		}
		parent = block.Header
	}

	fork := []Block{}
	parent = genesis.Header
	for i := 0; i < 4; i++ {
		block, err := MineChild(parent, uint64(2000+i), [][]byte{[]byte("fork-tx"), []byte("dup-tx")})
		if err != nil {
			t.Fatal(err)
		}
		fork = append(fork, block)
		parent = block.Header
	}

	blocks := fixtureBlocks{}
	for _, block := range original {
		blocks[BlockHeaderHash(block.Header)] = block
	}
	for _, block := range fork {
		blocks[BlockHeaderHash(block.Header)] = block
	}
	return chain, anchor, original, fork, blocks
}

func proofRequest(anchor TrustRoot, block Block, index uint64) ProofRequest {
	return ProofRequest{
		Transaction:   block.Transactions[index],
		BlockHash:     BlockHeaderHash(block.Header),
		Index:         index,
		TrustRootHash: anchor.BlockHash,
	}
}

func TestJointProofSharesLevelsButBindsEachLocation(t *testing.T) {
	chain, anchor, original, _, blocks := buildForkFixture(t)

	requests := []ProofRequest{
		proofRequest(anchor, original[2], 0),
		proofRequest(anchor, original[2], 1),
		proofRequest(anchor, original[3], 1),
	}
	packet, err := BuildJointProofPacket(anchor.BlockHash, requests, blocks)
	if err != nil {
		t.Fatal(err)
	}
	if len(packet.Items) != 3 {
		t.Fatalf("expected 3 independently bound items, got %d", len(packet.Items))
	}
	for _, item := range packet.Items {
		if !item.Found || len(item.Siblings) != merkleDepth {
			t.Fatalf("shared levels should produce a complete path per item: %+v", item)
		}
	}

	batch, err := chain.VerifyJointProof(packet, 1)
	if err != nil || !batch.AllAccepted() {
		t.Fatalf("joint proof on canonical chain should pass: %+v %v", batch, err)
	}
	if batch.AcceptedCount() != 3 {
		t.Fatalf("missing items must not count as success: %+v", batch)
	}

	// The duplicated transaction cannot satisfy a request at a different index.
	duplicate := packet
	duplicate.Items[0].Request.Index = 1
	dBatch, dErr := chain.VerifyJointProof(duplicate, 1)
	if dErr == nil || dBatch.AllAccepted() {
		t.Fatalf("duplicate transaction moved to another index was accepted: %+v %v", dBatch, dErr)
	}
}

func TestJointProofMissingItemAndUntrustedClaimedRoot(t *testing.T) {
	chain, anchor, original, _, blocks := buildForkFixture(t)
	requests := []ProofRequest{
		proofRequest(anchor, original[1], 0),
		{Transaction: []byte("ghost"), BlockHash: Hash{9}, Index: 0, TrustRootHash: anchor.BlockHash},
	}
	packet, err := BuildJointProofPacket(anchor.BlockHash, requests, blocks)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := chain.VerifyJointProof(packet, 1)
	if !errors.Is(err, ErrProofMissing) || batch.AllAccepted() {
		t.Fatalf("a missing item must fail the batch: %+v %v", batch, err)
	}
	if batch.AcceptedCount() != 1 {
		t.Fatalf("present item can still be audited, missing one rejected: %+v", batch)
	}

	// A self-reported root in the data packet is ignored: tampering with it does
	// not make verification use it, and verification still uses the local header.
	packet.Items[0].ClaimedTransactionsRoot = Hash{0xff}
	batch, err = chain.VerifyJointProof(packet, 1)
	if !errors.Is(err, ErrProofMissing) || batch.Results[0].Result.Status != ResultAccepted {
		t.Fatalf("claimed root must not extend or damage root-based verification: %+v %v", batch, err)
	}
}

func TestUpdatableConfirmationPolicyBoundary(t *testing.T) {
	chain, anchor, original, _, blocks := buildForkFixture(t)
	packet, err := BuildJointProofPacket(anchor.BlockHash, []ProofRequest{
		proofRequest(anchor, original[3], 0),
	}, blocks)
	if err != nil {
		t.Fatal(err)
	}

	// Tip height is 3, block height 3 has exactly 1 confirmation: boundary is
	// inclusive, so requirement 1 passes and requirement 2 fails.
	batch, err := chain.VerifyJointProof(packet, 1)
	if err != nil || !batch.AllAccepted() {
		t.Fatalf("one confirmation should pass the boundary: %+v %v", batch, err)
	}
	batch, err = chain.VerifyJointProof(packet, 2)
	if !errors.Is(err, ErrInsufficientConfirmations) {
		t.Fatalf("two confirmations should fail at the boundary: %+v %v", batch, err)
	}
	if batch.Results[0].Result.RequiredConfirmations != 2 || batch.Results[0].Result.Confirmations != 1 {
		t.Fatalf("result should record required vs observed confirmations: %+v", batch.Results[0].Result)
	}

	chain.SetConfirmationPolicy(ConstantConfirmations(1))
	batch, err = chain.VerifyJointProof(packet, 0)
	if err != nil || !batch.AllAccepted() {
		t.Fatalf("updated policy should be used on the next check: %+v %v", batch, err)
	}

	chain.SetConfirmationPolicy(HeightConfirmations{ByHeight: map[uint64]uint64{3: 2}})
	if _, err := chain.VerifyJointProof(packet, 0); !errors.Is(err, ErrInsufficientConfirmations) {
		t.Fatalf("height-specific policy should tighten the boundary: %v", err)
	}
}

func TestManualForkRechecksCurrentValidityAndKeepsHistory(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	chain, anchor, original, fork, blocks := buildForkFixture(t)

	requests := []ProofRequest{
		proofRequest(anchor, original[2], 0),
		proofRequest(anchor, original[3], 0),
	}
	packet, err := BuildJointProofPacket(anchor.BlockHash, requests, blocks)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.RecheckJointProof(chain, packet, 1)
	if err != nil || !before.AllAccepted() {
		t.Fatalf("original branch should initially pass: %+v %v", before, err)
	}

	if err := store.SaveTrustRoot(anchor); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveChain(chain); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveJointProof(chain, packet); err != nil {
		t.Fatal(err)
	}

	// Manually introduce a strictly heavier fork. Reopened chain selection must
	// be rebuilt from headers, not inherited from a stored/self-reported tip.
	for _, block := range fork {
		if err := chain.AddHeader(block.Header); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveChain(chain); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.LoadChain(TeachingProofOfWork{})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.CanonicalTip().Hash != BlockHeaderHash(fork[3].Header) {
		t.Fatal("reopened store did not rebuild the heavier canonical chain")
	}

	storedPackets, err := store.LoadJointProofs()
	if err != nil || len(storedPackets) != 1 {
		t.Fatalf("packet should remain available as historical evidence: %v", err)
	}
	after, err := store.RecheckJointProof(reopened, &storedPackets[0], 1)
	if !errors.Is(err, ErrStaleFork) || after.AllAccepted() {
		t.Fatalf("old accepted packet must be rechecked and withdrawn: %+v %v", after, err)
	}
	results, err := store.LoadResults()
	if err != nil {
		t.Fatal(err)
	}
	accepted, rejected := 0, 0
	for _, result := range results {
		if result.Status == ResultAccepted {
			accepted++
		} else {
			rejected++
		}
	}
	if accepted != 2 || rejected != 2 {
		t.Fatalf("historical accepted records remain while fresh rejections are added, got accepted=%d rejected=%d", accepted, rejected)
	}

	// A packet claiming another trust root cannot extend trust after restart.
	foreign := storedPackets[0]
	foreign.TrustRootHash = Hash{7}
	foreign.Items[0].Request.TrustRootHash = Hash{7}
	if _, err := reopened.VerifyJointProof(&foreign, 1); !errors.Is(err, ErrProofNotBound) {
		t.Fatalf("foreign trust root was accepted: %v", err)
	}

	// Persisted packet identities are audit references, not trust anchors.
	tampered := storedPackets[0]
	tampered.ID = Hash{13}
	store2 := NewFileStore(dir)
	if err := store2.SaveJointProof(reopened, &tampered); err != nil {
		t.Fatalf("packet ID should be recomputed on save: %v", err)
	}
}
