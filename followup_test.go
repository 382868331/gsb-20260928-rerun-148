package lightclient

import (
	"errors"
	"testing"
)

func TestBatchInclusionSharesBlockButBindsEachTransaction(t *testing.T) {
	chain, _, blocks := buildLinearChain(t, 1)
	txs := [][]byte{[]byte("tx-a"), []byte("tx-b"), []byte("tx-c"), []byte("tx-d")}
	block, err := MineChild(blocks[1].Header, 1002, txs)
	if err != nil {
		t.Fatal(err)
	}
	if err := chain.AddHeader(block.Header); err != nil {
		t.Fatal(err)
	}

	batch := NewBatchInclusionProofForHeader(block.Header, txs, 0, 2, 3)
	results, err := chain.VerifyBatchInclusion(batch, 1)
	if err != nil {
		t.Fatalf("valid joint proof rejected: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected one result per item, got %d", len(results))
	}
	for i, result := range results {
		if result.Status != ResultAccepted {
			t.Fatalf("item %d not accepted: %+v", i, result)
		}
		if result.TransactionIndex != batch.Items[i].Index {
			t.Fatalf("item %d lost its position binding: %+v", i, result)
		}
	}

	forged := NewBatchInclusionProofForHeader(block.Header, txs, 0, 1)
	forged.Items[1].Siblings = append([]Hash(nil), forged.Items[1].Siblings...)
	forged.Items[1].Siblings[0][0] ^= 1
	results, err = chain.VerifyBatchInclusion(forged, 1)
	if !errors.Is(err, ErrBatchIncomplete) {
		t.Fatalf("tampered item did not fail the batch: %v", err)
	}
	if results[0].Status != ResultAccepted || results[1].Status != ResultRejected {
		t.Fatalf("per-item outcomes not isolated: %+v", results)
	}

	missing := NewBatchInclusionProofForHeader(block.Header, txs, 0)
	missing.Items = append(missing.Items, BatchProofItem{
		Transaction: []byte("tx-not-in-block"),
		Index:       7,
		Siblings:    make([]Hash, merkleDepth),
	})
	results, err = chain.VerifyBatchInclusion(missing, 1)
	if !errors.Is(err, ErrBatchIncomplete) {
		t.Fatalf("missing item counted as success: %v", err)
	}
	if results[0].Status != ResultAccepted || results[1].Status != ResultRejected {
		t.Fatalf("missing item polluted sibling results: %+v", results)
	}

	if _, err := chain.VerifyBatchInclusion(BatchInclusionProof{}, 1); !errors.Is(err, ErrEmptyBatch) {
		t.Fatalf("empty batch accepted: %v", err)
	}
	if _, err := chain.VerifyInclusionBatch(nil, 1); !errors.Is(err, ErrEmptyBatch) {
		t.Fatalf("empty proof list accepted: %v", err)
	}
}

func TestDuplicateTransactionsBindToTheirOwnIndex(t *testing.T) {
	chain, _, blocks := buildLinearChain(t, 1)
	txs := [][]byte{[]byte("same-tx"), []byte("same-tx")}
	block, err := MineChild(blocks[1].Header, 1002, txs)
	if err != nil {
		t.Fatal(err)
	}
	if err := chain.AddHeader(block.Header); err != nil {
		t.Fatal(err)
	}

	first := NewInclusionProofForHeader(block.Header, txs, 0)
	second := NewInclusionProofForHeader(block.Header, txs, 1)
	results, err := chain.VerifyInclusionBatch([]InclusionProof{first, second}, 1)
	if err != nil {
		t.Fatalf("duplicate transactions at their own indexes rejected: %v", err)
	}
	if results[0].TransactionIndex != 0 || results[1].TransactionIndex != 1 {
		t.Fatalf("duplicate transactions lost index binding: %+v", results)
	}

	outOfRange := first
	outOfRange.Index = 2
	result, err := chain.VerifyInclusion(outOfRange, 1)
	if !errors.Is(err, ErrProofNotBound) || result.Status != ResultRejected {
		t.Fatalf("proof moved outside the committed positions: %+v, %v", result, err)
	}
}

func TestConfirmationBoundaryAndUpdatablePolicy(t *testing.T) {
	chain, _, blocks := buildLinearChain(t, 3)
	proof := NewInclusionProofForHeader(blocks[1].Header, blocks[1].Transactions, 0)

	result, err := chain.VerifyInclusion(proof, 3)
	if err != nil || result.Confirmations != 3 {
		t.Fatalf("exact confirmation boundary rejected: %+v, %v", result, err)
	}
	result, err = chain.VerifyInclusion(proof, 4)
	if !errors.Is(err, ErrInsufficientConfirmations) || result.Status != ResultRejected {
		t.Fatalf("one below the boundary accepted: %+v, %v", result, err)
	}

	if got := chain.ConfirmationPolicy().MinConfirmations; got != DefaultConfirmations {
		t.Fatalf("unexpected default policy: %d", got)
	}
	if err := chain.SetConfirmationPolicy(ConfirmationPolicy{MinConfirmations: 0}); err == nil {
		t.Fatal("zero-confirmation policy accepted")
	}
	if err := chain.SetConfirmationPolicy(ConfirmationPolicy{MinConfirmations: 4}); err != nil {
		t.Fatal(err)
	}
	result, err = chain.VerifyInclusion(proof, 0)
	if !errors.Is(err, ErrInsufficientConfirmations) {
		t.Fatalf("updated policy not applied to default verification: %+v, %v", result, err)
	}
	if result.RequiredConfirmations != 4 {
		t.Fatalf("result did not record the applied policy: %+v", result)
	}
	if _, err := chain.VerifyInclusion(proof, 3); err != nil {
		t.Fatalf("explicit per-call requirement must still win: %v", err)
	}
}

func TestReorgRecheckKeepsHistoryAsEvidenceOnly(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	chain, genesis, canonical := buildLinearChain(t, 3)
	proof := NewInclusionProofForHeader(canonical[1].Header, canonical[1].Transactions, 0)
	accepted, err := chain.VerifyInclusion(proof, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTrustRoot(genesis); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveChain(chain); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendResult(chain, accepted); err != nil {
		t.Fatal(err)
	}

	parent := genesis.Header
	for i := 0; i < 4; i++ {
		block, err := MineChild(parent, uint64(3000+i), [][]byte{[]byte("fork-tx")})
		if err != nil {
			t.Fatal(err)
		}
		if err := chain.AddHeader(block.Header); err != nil {
			t.Fatal(err)
		}
		parent = block.Header
	}

	fresh, err := chain.RecheckResult(accepted, 2)
	if !errors.Is(err, ErrStaleFork) || fresh.Status != ResultRejected {
		t.Fatalf("historical acceptance survived the reorg: %+v, %v", fresh, err)
	}
	history, err := store.LoadResults()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Status != ResultAccepted {
		t.Fatalf("historical evidence was rewritten: %+v", history)
	}

	if err := store.SaveChain(chain); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.LoadChain(TeachingProofOfWork{})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.CanonicalTip() != chain.CanonicalTip() {
		t.Fatal("reopen did not rebuild the canonical judgment from verified headers")
	}
	fresh, err = reopened.RecheckResult(accepted, 2)
	if !errors.Is(err, ErrStaleFork) {
		t.Fatalf("reopened chain trusted the old conclusion: %+v, %v", fresh, err)
	}

	fabricated := accepted
	fabricated.BlockHash = Hash{9}
	fabricated.Status = ResultAccepted
	fresh, err = reopened.RecheckResult(fabricated, 1)
	if !errors.Is(err, ErrProofNotBound) || fresh.Status != ResultRejected {
		t.Fatalf("self-reported historical root extended trust: %+v, %v", fresh, err)
	}
}
