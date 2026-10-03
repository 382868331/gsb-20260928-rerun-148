package lightclient

import (
	"errors"
	"testing"
	"time"
)

func TestProofCollectionBatchesDeduplicatesAndCompletesAllRequirements(t *testing.T) {
	chain, _, blocks := buildLinearChain(t, 4)
	first := blocks[2].Transactions[0]
	secondBlock, err := MineChild(blocks[4].Header, 2000, [][]byte{[]byte("unique-second")})
	if err != nil {
		t.Fatal(err)
	}
	if err := chain.AddHeader(secondBlock.Header); err != nil {
		t.Fatal(err)
	}
	second := secondBlock.Transactions[0]
	requirements := []CollectionRequirement{
		{ID: "first", TransactionHash: TransactionHash(first)},
		{ID: "second", TransactionHash: TransactionHash(second)},
	}
	task, err := NewProofCollectionTask(chain, "batch", requirements, 1)
	if err != nil {
		t.Fatal(err)
	}

	delivered, err := task.AddProofBatch(chain, "late-source", time.Unix(100, 0).UTC(),
		NewInclusionProofForHeader(secondBlock.Header, secondBlock.Transactions, 0))
	if err != nil {
		t.Fatal(err)
	}
	decision, err := task.Review(chain, time.Unix(101, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if decision.State != CollectionTaskIncomplete || len(decision.MissingIDs) != 1 || decision.MissingIDs[0] != "first" {
		t.Fatalf("partial delivery reported the wrong missing scope: %+v", decision)
	}

	validProof := NewInclusionProofForHeader(blocks[2].Header, blocks[2].Transactions, 0)
	if _, err := task.AddProofBatch(chain, "early-source", time.Unix(102, 0).UTC(), validProof); err != nil {
		t.Fatal(err)
	}
	duplicates, err := task.AddProofBatch(chain, "duplicate-source", time.Unix(103, 0).UTC(), validProof, delivered[0].Proof)
	if err != nil {
		t.Fatal(err)
	}
	if duplicates[0].DuplicateOf == "" || duplicates[1].DuplicateOf == "" {
		t.Fatalf("duplicate proofs were counted as new evidence: %+v", duplicates)
	}
	decision, err = task.Review(chain, time.Unix(104, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if decision.State != CollectionTaskComplete || len(decision.MissingIDs) != 0 {
		t.Fatalf("all requirements were not reported complete: %+v", decision)
	}
	for _, item := range decision.Items {
		if len(item.DeliveryIDs) != 1 {
			t.Fatalf("duplicate evidence was counted for %s: %+v", item.RequirementID, item)
		}
	}
	if task.Deliveries[0].SourceID != "late-source" || task.Deliveries[2].SourceID != "duplicate-source" {
		t.Fatal("batch source provenance was not retained")
	}
}

func TestProofCollectionReorgAndPolicyCreateNewRecheckVersion(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	genesis, anchor := mineCollectionGenesis(t)
	chain, err := NewChain(anchor, TeachingProofOfWork{})
	if err != nil {
		t.Fatal(err)
	}
	oldBlock, err := MineChild(genesis.Header, 1001, [][]byte{[]byte("old-tx")})
	if err != nil {
		t.Fatal(err)
	}
	if err := chain.AddHeader(oldBlock.Header); err != nil {
		t.Fatal(err)
	}
	other, err := MineChild(oldBlock.Header, 1002, [][]byte{[]byte("other-tx")})
	if err != nil {
		t.Fatal(err)
	}
	if err := chain.AddHeader(other.Header); err != nil {
		t.Fatal(err)
	}
	requirements := []CollectionRequirement{{ID: "old", TransactionHash: TransactionHash(oldBlock.Transactions[0])}}
	task, err := NewProofCollectionTask(chain, "reorg", requirements, 0)
	if err != nil {
		t.Fatal(err)
	}
	proof := NewInclusionProofForHeader(oldBlock.Header, oldBlock.Transactions, 0)
	if _, err := task.AddProofBatch(chain, "source-a", time.Unix(200, 0).UTC(), proof); err != nil {
		t.Fatal(err)
	}
	first, err := task.Review(chain, time.Unix(201, 0).UTC())
	if err != nil || first.State != CollectionTaskComplete {
		t.Fatalf("initial review did not complete: %+v, %v", first, err)
	}
	if err := store.SaveTrustRoot(anchor); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveChain(chain); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProofCollectionTask(chain, task); err != nil {
		t.Fatal(err)
	}

	if err := chain.SetConfirmationPolicy(ConfirmationPolicy{MinConfirmations: 4}); err != nil {
		t.Fatal(err)
	}
	policyDecision, err := task.Review(chain, time.Unix(202, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if policyDecision.State == CollectionTaskComplete || len(policyDecision.RecheckIDs) != 1 {
		t.Fatalf("policy change did not request a recheck: %+v", policyDecision)
	}
	if first.Version == policyDecision.Version || first.State != CollectionTaskComplete {
		t.Fatal("old decision was rewritten by the new policy")
	}

	parent := genesis.Header
	for i := 0; i < 3; i++ {
		forkBlock, err := MineChild(parent, uint64(3000+i), [][]byte{[]byte("fork-tx")})
		if err != nil {
			t.Fatal(err)
		}
		if err := chain.AddHeader(forkBlock.Header); err != nil {
			t.Fatal(err)
		}
		parent = forkBlock.Header
	}
	reorgDecision, err := task.Review(chain, time.Unix(203, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if reorgDecision.State != CollectionTaskIncomplete || reorgDecision.MissingIDs[0] != "old" {
		t.Fatalf("stale requirement remained satisfied after reorg: %+v", reorgDecision)
	}
	if reorgDecision.Items[0].State != CollectionItemNeedsReview {
		t.Fatalf("stale evidence was not marked for recheck: %+v", reorgDecision.Items[0])
	}
	if reorgDecision.Basis.CanonicalTip == first.Basis.CanonicalTip || task.Decisions[0].Basis.CanonicalTip != first.Basis.CanonicalTip {
		t.Fatal("decision bases were not versioned")
	}

	if err := store.SaveChain(chain); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProofCollectionTask(chain, task); err != nil {
		t.Fatal(err)
	}
	reopenedChain, err := store.LoadChain(TeachingProofOfWork{})
	if err != nil {
		t.Fatal(err)
	}
	reopenedTask, err := store.LoadProofCollectionTask(reopenedChain, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	reopenedDecision, err := reopenedTask.Review(reopenedChain, time.Unix(204, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if reopenedDecision.State != CollectionTaskIncomplete || len(reopenedDecision.MissingIDs) != 1 {
		t.Fatalf("reopened task trusted an old complete decision: %+v", reopenedDecision)
	}
	if len(reopenedTask.Decisions) != 4 || reopenedTask.Decisions[0].State != CollectionTaskComplete {
		t.Fatal("reload did not preserve old evidence and decision versions")
	}
}

func TestProofCollectionAbandonmentExplainsEvidenceAndMissingScope(t *testing.T) {
	chain, _, blocks := buildLinearChain(t, 2)
	validTx := blocks[1].Transactions[0]
	missingTx := []byte("never-delivered")
	task, err := NewProofCollectionTask(chain, "abandon", []CollectionRequirement{
		{ID: "present", TransactionHash: TransactionHash(validTx)},
		{ID: "absent", TransactionHash: TransactionHash(missingTx)},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := task.AddProofBatch(chain, "source", time.Unix(400, 0).UTC(),
		NewInclusionProofForHeader(blocks[1].Header, blocks[1].Transactions, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := task.Review(chain, time.Unix(401, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	decision, err := task.Abandon("requestor stopped collection", time.Unix(402, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if decision.State != CollectionTaskAbandoned || len(decision.MissingIDs) != 1 || decision.MissingIDs[0] != "absent" {
		t.Fatalf("abandonment lost the accurate missing scope: %+v", decision)
	}
	if decision.Items[0].State != CollectionItemAccepted || decision.Items[1].Reason != "requestor stopped collection" {
		t.Fatalf("abandonment did not preserve checked evidence and explain incompleteness: %+v", decision.Items)
	}
	if _, err := task.AddProofBatch(chain, "late", time.Unix(403, 0).UTC(), InclusionProof{}); !errors.Is(err, ErrCollectionAbandoned) {
		t.Fatalf("late delivery after abandonment was accepted: %v", err)
	}
}

func TestProofCollectionDoesNotLetLaterConflictingEvidenceWin(t *testing.T) {
	chain, _, blocks := buildLinearChain(t, 1)
	block, err := MineChild(blocks[1].Header, 1002, [][]byte{[]byte("dup"), []byte("dup")})
	if err != nil {
		t.Fatal(err)
	}
	if err := chain.AddHeader(block.Header); err != nil {
		t.Fatal(err)
	}
	task, err := NewProofCollectionTask(chain, "conflict", []CollectionRequirement{{
		ID:              "dup",
		TransactionHash: TransactionHash([]byte("dup")),
	}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := task.AddProofBatch(chain, "first", time.Unix(500, 0).UTC(),
		NewInclusionProofForHeader(block.Header, block.Transactions, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := task.AddProofBatch(chain, "second", time.Unix(501, 0).UTC(),
		NewInclusionProofForHeader(block.Header, block.Transactions, 1)); err != nil {
		t.Fatal(err)
	}
	decision, err := task.Review(chain, time.Unix(502, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if decision.State == CollectionTaskComplete || decision.Items[0].State != CollectionItemConflicting {
		t.Fatalf("later conflicting proof overrode the earlier one: %+v", decision)
	}
	if len(decision.MissingIDs) != 1 || decision.MissingIDs[0] != "dup" {
		t.Fatalf("conflicting requirement was removed from the missing scope: %+v", decision)
	}
	if len(decision.Items[0].DeliveryIDs) != 2 {
		t.Fatalf("conflict did not preserve both sources of evidence: %+v", decision.Items[0])
	}
}

func mineCollectionGenesis(t *testing.T) (Block, TrustRoot) {
	t.Helper()
	genesisBlock, anchor, err := MineGenesis(1000, [][]byte{[]byte("genesis-tx")})
	if err != nil {
		t.Fatal(err)
	}
	anchor.CreatedAt = time.Unix(1700000000, 0).UTC()
	return genesisBlock, anchor
}
