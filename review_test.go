package lightclient

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func newReviewTask(t *testing.T, chain *Chain, blocks []Block) (*CollectionTask, Block, Block) {
	t.Helper()
	blockA := mineBlockWith(t, chain, blocks[1].Header, 2001, [][]byte{[]byte("tx-a")})
	blockB := mineBlockWith(t, chain, blockA.Header, 2002, [][]byte{[]byte("tx-b")})
	task, err := NewCollectionTask("review-task", []EvidenceRequirement{
		{ID: "need-a", TransactionHash: TransactionHash([]byte("tx-a"))},
		{ID: "need-b", TransactionHash: TransactionHash([]byte("tx-b"))},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	proofA := NewInclusionProofForHeader(blockA.Header, blockA.Transactions, 0)
	proofB := NewInclusionProofForHeader(blockB.Header, blockB.Transactions, 0)
	if _, err := task.SubmitBatch(chain, "relay", []InclusionProof{proofA, proofB}); err != nil {
		t.Fatal(err)
	}
	return task, blockA, blockB
}

func TestReviewRoundPartialVerdictsAndPendingDecisions(t *testing.T) {
	chain, _, blocks := buildLinearChain(t, 1)
	task, blockA, _ := newReviewTask(t, chain, blocks)
	evidenceBefore := *task.Evidence["need-a"]

	round, err := task.OpenReview(chain, "auditor-1")
	if err != nil {
		t.Fatal(err)
	}
	if round.Sequence != 1 || round.CanonicalTip != chain.CanonicalTip().Hash {
		t.Fatalf("round did not freeze the chain basis: %+v", round)
	}
	if round.RequiredConfirmations != 1 || round.MaterialVersion != task.MaterialVersion {
		t.Fatalf("round did not record the material basis: %+v", round)
	}

	if err := task.RecordOpinion(1, "need-a", ReviewApproved, "proof matches the canonical block"); err != nil {
		t.Fatal(err)
	}
	if err := task.RecordOpinion(1, "need-b", ReviewReturned, "need an independent source"); err != nil {
		t.Fatal(err)
	}
	if err := task.RecordOpinion(1, "need-x", ReviewApproved, ""); !errors.Is(err, ErrUnknownRequirement) {
		t.Fatalf("opinion on unknown requirement accepted: %v", err)
	}
	if err := task.RecordOpinion(1, "need-a", "maybe", ""); !errors.Is(err, ErrInvalidVerdict) {
		t.Fatalf("invalid verdict accepted: %v", err)
	}

	// Reviewing never edits the collected proof.
	if !reflect.DeepEqual(*task.Evidence["need-a"], evidenceBefore) {
		t.Fatal("reviewing mutated the collected evidence")
	}
	opinion := task.ReviewRounds[0].Opinions[0]
	if opinion.EvidenceBlockHash != BlockHeaderHash(blockA.Header) || opinion.EvidenceState != EvidenceAccepted {
		t.Fatalf("opinion did not snapshot the reviewed material: %+v", opinion)
	}

	// A second round may close with decisions still missing.
	if _, err := task.OpenReview(chain, "auditor-2"); err != nil {
		t.Fatal(err)
	}
	if err := task.RecordOpinion(2, "need-a", ReviewApproved, ""); err != nil {
		t.Fatal(err)
	}
	pending, err := task.PendingDecisions(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != "need-b" {
		t.Fatalf("pending decisions wrong: %+v", pending)
	}
	if err := task.CloseReview(chain, 2); err != nil {
		t.Fatal(err)
	}
	last := task.Decisions[len(task.Decisions)-1]
	if !strings.Contains(last.Reason, "decisions still missing: need-b") {
		t.Fatalf("closing decision did not record the missing decisions: %s", last.Reason)
	}
	if err := task.RecordOpinion(2, "need-b", ReviewApproved, ""); !errors.Is(err, ErrReviewRoundClosed) {
		t.Fatalf("closed round accepted new opinions: %v", err)
	}
	if err := task.CloseReview(chain, 1); err != nil {
		t.Fatal(err)
	}
	last = task.Decisions[len(task.Decisions)-1]
	if !strings.Contains(last.Reason, "approved: need-a") || !strings.Contains(last.Reason, "returned for supplement: need-b") {
		t.Fatalf("closing decision missed the partial verdicts: %s", last.Reason)
	}
}

func TestReviewOpinionsNotMigratedAcrossNewMaterial(t *testing.T) {
	chain, _, blocks := buildLinearChain(t, 1)
	task, blockA, blockB := newReviewTask(t, chain, blocks)

	if _, err := task.OpenReview(chain, "auditor-1"); err != nil {
		t.Fatal(err)
	}
	if err := task.RecordOpinion(1, "need-a", ReviewApproved, ""); err != nil {
		t.Fatal(err)
	}
	if err := task.RecordOpinion(1, "need-b", ReviewApproved, ""); err != nil {
		t.Fatal(err)
	}
	if err := task.CloseReview(chain, 1); err != nil {
		t.Fatal(err)
	}

	// A conflicting proof for need-a arrives: the old approval must not
	// follow the new material.
	forkA := mineBlockWith(t, chain, blocks[1].Header, 9001, [][]byte{[]byte("tx-a")})
	mineBlockWith(t, chain, forkA.Header, 9002, [][]byte{[]byte("filler")})
	conflicting := NewInclusionProofForHeader(forkA.Header, forkA.Transactions, 0)
	batch, err := task.SubmitBatch(chain, "relay-2", []InclusionProof{conflicting})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Items[0].Outcome != ItemConflict {
		t.Fatalf("contradictory material not flagged: %+v", batch.Items[0])
	}

	// Extend the original branch again so it retakes the canonical seat;
	// the conflicting arrival stays recorded but its proof goes stale.
	restored := mineBlockWith(t, chain, blockB.Header, 2003, [][]byte{[]byte("tx-c")})
	mineBlockWith(t, chain, restored.Header, 2004, [][]byte{[]byte("tx-d")})

	statuses, err := task.ReviewStatuses(chain)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 {
		t.Fatalf("expected one status per opinion, got %+v", statuses)
	}
	byRequirement := map[string]OpinionStatus{}
	for _, status := range statuses {
		byRequirement[status.Opinion.RequirementID] = status
	}
	if byRequirement["need-a"].Status != OpinionNeedsReReview {
		t.Fatalf("approval silently carried onto contradicted material: %+v", byRequirement["need-a"])
	}
	if byRequirement["need-b"].Status != OpinionApplicable {
		t.Fatalf("untouched opinion wrongly invalidated: %+v", byRequirement["need-b"])
	}
	if task.Evidence["need-a"].Result.BlockHash != BlockHeaderHash(blockA.Header) {
		t.Fatal("conflicting arrival replaced the first evidence")
	}

	// A reorg that overtakes the reviewed branch invalidates every opinion
	// formed against it.
	parent := blocks[1].Header
	for i := 0; i < 7; i++ {
		block, err := MineChild(parent, uint64(3000+i), [][]byte{[]byte("reorg-tx")})
		if err != nil {
			t.Fatal(err)
		}
		if err := chain.AddHeader(block.Header); err != nil {
			t.Fatal(err)
		}
		parent = block.Header
	}
	statuses, err = task.ReviewStatuses(chain)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.Status != OpinionNeedsReReview {
			t.Fatalf("opinion survived a canonical-chain change: %+v", status)
		}
	}
}

func TestReviewRoundsSurviveReopenAndStayComparable(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	chain, genesis, blocks := buildLinearChain(t, 1)
	task, _, _ := newReviewTask(t, chain, blocks)

	if _, err := task.OpenReview(chain, "auditor-1"); err != nil {
		t.Fatal(err)
	}
	if err := task.RecordOpinion(1, "need-a", ReviewApproved, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := task.RecordOpinion(1, "need-b", ReviewReturned, "supplement needed"); err != nil {
		t.Fatal(err)
	}
	if err := task.CloseReview(chain, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := task.OpenReview(chain, "auditor-2"); err != nil {
		t.Fatal(err)
	}
	if err := task.RecordOpinion(2, "need-b", ReviewApproved, "supplement arrived"); err != nil {
		t.Fatal(err)
	}

	if err := store.SaveTrustRoot(genesis); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTask(task); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.LoadTask("review-task")
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.ReviewRounds) != 2 {
		t.Fatalf("review rounds lost across reopen: %+v", reopened.ReviewRounds)
	}
	if !reopened.ReviewRounds[0].Closed || reopened.ReviewRounds[1].Closed {
		t.Fatalf("round states not preserved: %+v", reopened.ReviewRounds)
	}

	comparisons, err := reopened.CompareRounds(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	byRequirement := map[string]RoundComparison{}
	for _, comparison := range comparisons {
		byRequirement[comparison.RequirementID] = comparison
	}
	needB := byRequirement["need-b"]
	if !needB.Changed || needB.First.Verdict != ReviewReturned || needB.Second.Verdict != ReviewApproved {
		t.Fatalf("round comparison missed the verdict change: %+v", needB)
	}
	needA := byRequirement["need-a"]
	if !needA.Changed || needA.First.Verdict != ReviewApproved || needA.Second != nil {
		t.Fatalf("unchanged requirement misreported: %+v", needA)
	}

	// Abandoning a round keeps the round, its opinions and the evidence.
	if _, err := reopened.OpenReview(chain, "auditor-3"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.RecordOpinion(3, "need-a", ReviewReturned, "doubt"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.AbandonReview(3, "reviewer reassigned"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.RecordOpinion(3, "need-b", ReviewApproved, ""); !errors.Is(err, ErrReviewRoundAbandoned) {
		t.Fatalf("abandoned round accepted opinions: %v", err)
	}
	if len(reopened.Evidence) != 2 || len(reopened.ReviewRounds) != 3 {
		t.Fatal("abandoning a review deleted material or rounds")
	}
	statuses, err := reopened.ReviewStatuses(chain)
	if err != nil {
		t.Fatal(err)
	}
	var abandonedSeen bool
	for _, status := range statuses {
		if status.Round == 3 {
			abandonedSeen = true
			if status.Status != OpinionRoundAbandoned {
				t.Fatalf("abandoned round opinion misclassified: %+v", status)
			}
		}
	}
	if !abandonedSeen {
		t.Fatal("abandoned round opinions vanished from the status report")
	}
}
