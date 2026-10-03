package lightclient

import (
	"errors"
	"strings"
	"testing"
)

func mineBlockWith(t *testing.T, chain *Chain, parent BlockHeader, seed uint64, txs [][]byte) Block {
	t.Helper()
	block, err := MineChild(parent, seed, txs)
	if err != nil {
		t.Fatal(err)
	}
	if err := chain.AddHeader(block.Header); err != nil {
		t.Fatal(err)
	}
	return block
}

func TestCollectionTaskOutOfOrderBatchesDuplicatesAndConflicts(t *testing.T) {
	chain, _, blocks := buildLinearChain(t, 1)
	blockA := mineBlockWith(t, chain, blocks[1].Header, 2001, [][]byte{[]byte("tx-a")})
	blockB := mineBlockWith(t, chain, blockA.Header, 2002, [][]byte{[]byte("tx-b")})

	task, err := NewCollectionTask("audit-1", []EvidenceRequirement{
		{ID: "need-a", TransactionHash: TransactionHash([]byte("tx-a"))},
		{ID: "need-b", TransactionHash: TransactionHash([]byte("tx-b"))},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}

	// Material arrives out of order across blocks: block B first.
	proofB := NewInclusionProofForHeader(blockB.Header, blockB.Transactions, 0)
	batch, err := task.SubmitBatch(chain, "relay-north", []InclusionProof{proofB})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Items[0].Outcome != ItemAccepted || batch.Source != "relay-north" {
		t.Fatalf("first batch lost provenance: %+v", batch)
	}
	if task.Complete() {
		t.Fatal("partial evidence counted as a full pass")
	}
	if missing := task.Missing(); len(missing) != 1 || missing[0].ID != "need-a" {
		t.Fatalf("missing scope wrong after partial arrival: %+v", missing)
	}

	// Duplicate of the same record is not counted twice.
	batch, err = task.SubmitBatch(chain, "relay-south", []InclusionProof{proofB})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Items[0].Outcome != ItemDuplicate {
		t.Fatalf("duplicate delivery double counted: %+v", batch.Items[0])
	}
	if task.Evidence["need-b"].Source != "relay-north" {
		t.Fatal("duplicate overwrote the original provenance")
	}

	// Contradictory material for the satisfied requirement is recorded, not applied.
	otherB := mineBlockWith(t, chain, blockA.Header, 9001, [][]byte{[]byte("tx-b")})
	mineBlockWith(t, chain, otherB.Header, 9002, [][]byte{[]byte("fork-filler")})
	conflicting := NewInclusionProofForHeader(otherB.Header, otherB.Transactions, 0)
	batch, err = task.SubmitBatch(chain, "relay-east", []InclusionProof{conflicting})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Items[0].Outcome != ItemConflict {
		t.Fatalf("contradictory material not flagged: %+v", batch.Items[0])
	}
	kept := task.Evidence["need-b"]
	if kept.Result.BlockHash != BlockHeaderHash(blockB.Header) || len(kept.Conflicts) != 1 {
		t.Fatalf("last arrival overrode the first evidence: %+v", kept)
	}

	// Unrequested material is logged but never fills the requirement set.
	stray := InclusionProof{Transaction: []byte("tx-stray"), BlockHash: BlockHeaderHash(blockB.Header)}
	batch, err = task.SubmitBatch(chain, "relay-west", []InclusionProof{stray})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Items[0].Outcome != ItemUnrequested {
		t.Fatalf("unrequested material leaked into the task: %+v", batch.Items[0])
	}

	// Final missing piece completes the task exactly once.
	proofA := NewInclusionProofForHeader(blockA.Header, blockA.Transactions, 0)
	if _, err := task.SubmitBatch(chain, "relay-north", []InclusionProof{proofA}); err != nil {
		t.Fatal(err)
	}
	if !task.Complete() || task.Status() != TaskComplete {
		t.Fatalf("full evidence did not complete the task: %s", task.Explain())
	}
	last := task.Decisions[len(task.Decisions)-1]
	if !last.Complete || len(last.Missing) != 0 {
		t.Fatalf("final decision is not a complete pass: %+v", last)
	}
}

func TestCollectionTaskReorgAndPolicyChangeVersionDecisions(t *testing.T) {
	chain, genesis, blocks := buildLinearChain(t, 3)
	tx := []byte("watched-tx")
	block := mineBlockWith(t, chain, blocks[3].Header, 3001, [][]byte{tx})
	mineBlockWith(t, chain, block.Header, 3002, [][]byte{[]byte("filler")})

	task, err := NewCollectionTask("audit-2", []EvidenceRequirement{
		{ID: "need-watched", TransactionHash: TransactionHash(tx)},
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	proof := NewInclusionProofForHeader(block.Header, block.Transactions, 0)
	if _, err := task.SubmitBatch(chain, "source-1", []InclusionProof{proof}); err != nil {
		t.Fatal(err)
	}
	if !task.Complete() {
		t.Fatalf("task should be complete before the reorg: %s", task.Explain())
	}
	before := len(task.Decisions)
	oldDecision := task.Decisions[before-1]

	// Reorg: a heavier fork from genesis replaces the canonical chain.
	parent := genesis.Header
	for i := 0; i < 7; i++ {
		fork, err := MineChild(parent, uint64(4000+i), [][]byte{[]byte("fork-tx")})
		if err != nil {
			t.Fatal(err)
		}
		if err := chain.AddHeader(fork.Header); err != nil {
			t.Fatal(err)
		}
		parent = fork.Header
	}
	stale, err := task.Revalidate(chain)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != "need-watched" {
		t.Fatalf("reorg did not flag the collected evidence for recheck: %v", stale)
	}
	if task.Complete() {
		t.Fatal("stale evidence still counted as a complete pass")
	}
	if task.Evidence["need-watched"].State != EvidenceNeedsRecheck {
		t.Fatal("invalidated evidence not marked needs-recheck")
	}
	if !decisionEqual(task.Decisions[before-1], oldDecision) {
		t.Fatal("revalidation rewrote an old decision version")
	}
	if len(task.Decisions) != before+1 || task.Decisions[before].Complete {
		t.Fatalf("new decision version missing or wrong: %+v", task.Decisions)
	}
	if task.Decisions[before].CanonicalTip == oldDecision.CanonicalTip {
		t.Fatal("new decision kept the old canonical basis")
	}

	// Confirmation-policy change on a fresh task: stricter requirement
	// invalidates previously accepted evidence on recheck.
	chain2, _, blocks2 := buildLinearChain(t, 2)
	block2 := mineBlockWith(t, chain2, blocks2[2].Header, 5001, [][]byte{tx})
	task2, err := NewCollectionTask("audit-3", []EvidenceRequirement{
		{ID: "need-watched", TransactionHash: TransactionHash(tx)},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	proof2 := NewInclusionProofForHeader(block2.Header, block2.Transactions, 0)
	if _, err := task2.SubmitBatch(chain2, "source-1", []InclusionProof{proof2}); err != nil {
		t.Fatal(err)
	}
	if !task2.Complete() {
		t.Fatal("task should pass under the original policy")
	}
	task2.RequiredConfirmations = 3
	stale, err = task2.Revalidate(chain2)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || task2.Complete() {
		t.Fatalf("policy change did not reopen the collected evidence: %v", stale)
	}
}

func TestCollectionTaskSaveReopenAndAbandon(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	chain, anchor, blocks := buildLinearChain(t, 2)
	block := mineBlockWith(t, chain, blocks[2].Header, 6001, [][]byte{[]byte("tx-kept")})

	task, err := NewCollectionTask("audit-4", []EvidenceRequirement{
		{ID: "have", TransactionHash: TransactionHash([]byte("tx-kept"))},
		{ID: "want", TransactionHash: TransactionHash([]byte("tx-absent"))},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	proof := NewInclusionProofForHeader(block.Header, block.Transactions, 0)
	if _, err := task.SubmitBatch(chain, "source-1", []InclusionProof{proof}); err != nil {
		t.Fatal(err)
	}

	if err := store.SaveTrustRoot(anchor); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveChain(chain); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTask(task); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.LoadTask("audit-4")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Complete() {
		t.Fatal("reopened task lost its missing scope")
	}
	missing := reopened.Missing()
	if len(missing) != 1 || missing[0].ID != "want" {
		t.Fatalf("missing scope inaccurate after reopen: %+v", missing)
	}
	if reopened.Evidence["have"].Source != "source-1" {
		t.Fatal("reopen lost evidence provenance")
	}

	// The reopened task still validates against the rebuilt chain only.
	rebuilt, err := store.LoadChain(TeachingProofOfWork{})
	if err != nil {
		t.Fatal(err)
	}
	if stale, err := reopened.Revalidate(rebuilt); err != nil || len(stale) != 0 {
		t.Fatalf("reopened task did not revalidate against the rebuilt chain: %v %v", stale, err)
	}

	// Abandoning keeps the checked evidence explainable.
	reopened.Abandon("requester cancelled the audit")
	if _, err := reopened.SubmitBatch(rebuilt, "late", nil); !errors.Is(err, ErrTaskAbandoned) {
		t.Fatalf("abandoned task accepted new material: %v", err)
	}
	explanation := reopened.Explain()
	if !strings.Contains(explanation, "have: accepted") ||
		!strings.Contains(explanation, "want: missing") ||
		!strings.Contains(explanation, "requester cancelled the audit") ||
		!strings.Contains(explanation, "incomplete: still missing want") {
		t.Fatalf("abandoned task cannot explain itself:\n%s", explanation)
	}
	if reopened.Status() != TaskAbandoned {
		t.Fatal("abandoned task reports the wrong status")
	}
}

func TestCollectionTaskRejectsInvalidDefinition(t *testing.T) {
	if _, err := NewCollectionTask("x", nil, 1); !errors.Is(err, ErrEmptyRequirements) {
		t.Fatalf("empty requirement set accepted: %v", err)
	}
	if _, err := NewCollectionTask("x", []EvidenceRequirement{{ID: "a"}, {ID: "a"}}, 1); !errors.Is(err, ErrDuplicateRequirement) {
		t.Fatalf("duplicate requirement accepted: %v", err)
	}
	if _, err := NewCollectionTask("x", []EvidenceRequirement{{ID: "a"}}, 0); !errors.Is(err, ErrInvalidConfirmations) {
		t.Fatalf("zero-confirmation task accepted: %v", err)
	}
}

func decisionEqual(a, b TaskDecision) bool {
	if a.Version != b.Version || a.CanonicalTip != b.CanonicalTip ||
		a.RequiredConfirmations != b.RequiredConfirmations || a.Complete != b.Complete ||
		a.Reason != b.Reason || len(a.Missing) != len(b.Missing) {
		return false
	}
	for i := range a.Missing {
		if a.Missing[i] != b.Missing[i] {
			return false
		}
	}
	return true
}
