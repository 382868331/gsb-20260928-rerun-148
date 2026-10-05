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

	// The final missing piece does not resolve the independently recorded contradiction.
	proofA := NewInclusionProofForHeader(blockA.Header, blockA.Transactions, 0)
	if _, err := task.SubmitBatch(chain, "relay-north", []InclusionProof{proofA}); err != nil {
		t.Fatal(err)
	}
	if task.Complete() || task.Status() != TaskCollecting {
		t.Fatalf("unresolved contradiction counted as a complete pass: %s", task.Explain())
	}
	last := task.Decisions[len(task.Decisions)-1]
	if last.Complete || len(last.Missing) != 1 || last.Missing[0] != "need-b" {
		t.Fatalf("final decision ignored the conflict: %+v", last)
	}
}

func TestCollectionTaskConflictSurvivesRestartRecheckAndAbandon(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	chain, anchor, blocks := buildLinearChain(t, 1)
	blockA := mineBlockWith(t, chain, blocks[1].Header, 7001, [][]byte{[]byte("tx-a")})
	blockB := mineBlockWith(t, chain, blockA.Header, 7002, [][]byte{[]byte("tx-b")})

	task, err := NewCollectionTask("audit-conflict", []EvidenceRequirement{
		{ID: "need-a", TransactionHash: TransactionHash([]byte("tx-a"))},
		{ID: "need-b", TransactionHash: TransactionHash([]byte("tx-b"))},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}

	proofB := NewInclusionProofForHeader(blockB.Header, blockB.Transactions, 0)
	if _, err := task.SubmitBatch(chain, "source-b-first", []InclusionProof{proofB}); err != nil {
		t.Fatal(err)
	}
	proofA := NewInclusionProofForHeader(blockA.Header, blockA.Transactions, 0)
	if _, err := task.SubmitBatch(chain, "source-a", []InclusionProof{proofA}); err != nil {
		t.Fatal(err)
	}

	otherB := mineBlockWith(t, chain, blockA.Header, 8001, [][]byte{[]byte("tx-b")})
	mineBlockWith(t, chain, otherB.Header, 8002, [][]byte{[]byte("fork-filler")})
	conflictProof := NewInclusionProofForHeader(otherB.Header, otherB.Transactions, 0)
	conflictBatch, err := task.SubmitBatch(chain, "source-b-other-block", []InclusionProof{conflictProof})
	if err != nil {
		t.Fatal(err)
	}
	if conflictBatch.Items[0].Outcome != ItemConflict {
		t.Fatalf("alternative proof was not recorded as a conflict: %+v", conflictBatch.Items[0])
	}
	if task.Complete() {
		t.Fatalf("task passed with an unhandled conflict: %s", task.Explain())
	}
	missing := task.Missing()
	if len(missing) != 1 || missing[0].ID != "need-b" {
		t.Fatalf("conflicting requirement missing from overall scope: %+v", missing)
	}
	beforeReorg := len(task.Decisions)
	oldDecision := task.Decisions[beforeReorg-1]

	if err := store.SaveTrustRoot(anchor); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveChain(chain); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTask(task); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.LoadTask("audit-conflict")
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := store.LoadChain(TeachingProofOfWork{})
	if err != nil {
		t.Fatal(err)
	}
	kept := reopened.Evidence["need-b"]
	if kept.Source != "source-b-first" || kept.Batch != 1 || len(kept.Conflicts) != 1 {
		t.Fatalf("original provenance or conflict lost on reopen: %+v", kept)
	}
	if kept.Conflicts[0].Source != "source-b-other-block" || kept.Conflicts[0].Result.BlockHash != BlockHeaderHash(otherB.Header) {
		t.Fatalf("conflicting source lost on reopen: %+v", kept.Conflicts[0])
	}
	if reopened.Complete() {
		t.Fatalf("reopened task passed with an unhandled conflict: %s", reopened.Explain())
	}

	mineBlockWith(t, rebuilt, otherB.Header, 8003, [][]byte{[]byte("later-fork-tx")})
	stale, err := reopened.Revalidate(rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != "need-b" {
		t.Fatalf("revalidation did not keep the conflict in scope: %v", stale)
	}
	if !decisionEqual(reopened.Decisions[beforeReorg-1], oldDecision) {
		t.Fatal("revalidation rewrote the old decision basis")
	}
	newDecision := reopened.Decisions[len(reopened.Decisions)-1]
	if newDecision.Complete || len(newDecision.Missing) != 1 || newDecision.CanonicalTip == oldDecision.CanonicalTip {
		t.Fatalf("new decision did not reflect the current chain and conflict: %+v", newDecision)
	}
	if !strings.Contains(newDecision.Reason, "invalid primary proof") ||
		!strings.Contains(newDecision.Reason, "source-b-first") ||
		!strings.Contains(newDecision.Reason, "unresolved conflicts: need-b") {
		t.Fatalf("new decision does not identify the failed proof and remaining contradiction: %s", newDecision.Reason)
	}
	if kept.State != EvidenceNeedsRecheck || !strings.Contains(kept.RecheckReason, "left the canonical chain") {
		t.Fatalf("primary proof was not marked invalid: %+v", kept)
	}
	if kept.Conflicts[0].State != EvidenceAccepted || kept.ConflictState != ConflictUnresolved {
		t.Fatalf("valid alternative proof was not left for explicit handling: %+v", kept)
	}

	reopened.Abandon("cannot choose between contradictory sources without reviewer instruction")
	explanation := reopened.Explain()
	if !strings.Contains(explanation, "source-b-first") ||
		!strings.Contains(explanation, "source-b-other-block") ||
		!strings.Contains(explanation, "needs-recheck") ||
		!strings.Contains(explanation, "unresolved") ||
		!strings.Contains(explanation, "cannot choose between contradictory sources") {
		t.Fatalf("abandonment lost evidence or the concrete unfinished reason:\n%s", explanation)
	}
}

func TestCollectionTaskLoadsLegacyUnresolvedConflict(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	_, anchor, _ := buildLinearChain(t, 1)
	txHash := TransactionHash([]byte("legacy-tx"))
	firstHash := Hash{1}
	otherHash := Hash{2}
	task := CollectionTask{
		ID:                    "legacy-conflict",
		TrustRootHash:         anchor.BlockHash,
		RequiredConfirmations: 1,
		Requirements:          []EvidenceRequirement{{ID: "need-legacy", TransactionHash: txHash}},
		Evidence: map[string]*CollectedEvidence{
			"need-legacy": {
				RequirementID: "need-legacy",
				Source:        "legacy-source",
				Batch:         1,
				Result: VerificationResult{
					Status:          ResultAccepted,
					BlockHash:       firstHash,
					TransactionHash: txHash,
				},
				State: EvidenceAccepted,
				Conflicts: []ConflictingEvidence{
					{
						Source: "legacy-other-source",
						Batch:  2,
						Result: VerificationResult{
							Status:          ResultAccepted,
							BlockHash:       otherHash,
							TransactionHash: txHash,
						},
					},
				},
			},
		},
	}
	if err := store.SaveTrustRoot(anchor); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTask(&task); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.LoadTask("legacy-conflict")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Complete() {
		t.Fatal("legacy conflict was silently treated as resolved")
	}
	missing := reopened.Missing()
	if len(missing) != 1 || missing[0].ID != "need-legacy" {
		t.Fatalf("legacy conflict missing from reopened scope: %+v", missing)
	}
	legacy := reopened.Evidence["need-legacy"]
	if legacy.ConflictState != ConflictUnresolved || legacy.Conflicts[0].State != EvidenceAccepted {
		t.Fatalf("legacy conflict state not migrated: %+v", legacy)
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
		!strings.Contains(explanation, "incomplete: still missing or unresolved: want") {
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

func TestCollectionReviewRoundsKeepBasisAndTrackChangedMaterials(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	chain, anchor, blocks := buildLinearChain(t, 1)
	blockA := mineBlockWith(t, chain, blocks[1].Header, 7101, [][]byte{[]byte("review-a")})
	blockB := mineBlockWith(t, chain, blockA.Header, 7102, [][]byte{[]byte("review-b")})

	task, err := NewCollectionTask("review-audit", []EvidenceRequirement{
		{ID: "need-a", TransactionHash: TransactionHash([]byte("review-a"))},
		{ID: "need-b", TransactionHash: TransactionHash([]byte("review-b"))},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	proofA := NewInclusionProofForHeader(blockA.Header, blockA.Transactions, 0)
	proofB := NewInclusionProofForHeader(blockB.Header, blockB.Transactions, 0)
	if _, err := task.SubmitBatch(chain, "source-main", []InclusionProof{proofA, proofB}); err != nil {
		t.Fatal(err)
	}

	round, err := task.OpenReviewRound(chain, "reviewer-1")
	if err != nil {
		t.Fatal(err)
	}
	if round.Number != 1 || round.State != ReviewOpen || round.BatchCount != 1 || len(round.Materials) != 2 {
		t.Fatalf("review basis not frozen: %+v", round)
	}
	if len(round.Basis.CanonicalChain) != int(chain.CanonicalTip().Height)+1 {
		t.Fatalf("full canonical chain not recorded: %+v", round.Basis)
	}
	if round.Basis.TrustRootHash != anchor.BlockHash || round.Basis.RequiredConfirmations != 1 {
		t.Fatalf("review basis misses trust root or policy: %+v", round.Basis)
	}
	if _, err := task.RecordReviewOpinion(chain, 1, "need-a", ReviewPassed, "valid primary proof"); err != nil {
		t.Fatal(err)
	}
	if _, err := task.RecordReviewOpinion(chain, 1, "need-b", ReviewReturned, "needs an additional source statement"); err != nil {
		t.Fatal(err)
	}
	if _, err := task.CompleteReviewRound(chain, 1); err != nil {
		t.Fatal(err)
	}
	assessment, err := task.AssessReviewRound(chain, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(assessment.MissingDecisions) != 0 {
		t.Fatalf("completed round still reports missing item decisions: %+v", assessment.MissingDecisions)
	}
	for _, item := range assessment.Items {
		if item.Applicability != ReviewStillApplicable {
			t.Fatalf("fresh review was not applicable: %+v", item)
		}
	}

	// A new contradictory proof changes only need-b's material version. The old
	// pass for need-a remains applicable; the old need-b opinion cannot move.
	otherB := mineBlockWith(t, chain, blockA.Header, 8101, [][]byte{[]byte("review-b")})
	mineBlockWith(t, chain, otherB.Header, 8102, [][]byte{[]byte("fork-filler")})
	conflictB := NewInclusionProofForHeader(otherB.Header, otherB.Transactions, 0)
	if _, err := task.SubmitBatch(chain, "source-alt", []InclusionProof{conflictB}); err != nil {
		t.Fatal(err)
	}
	assessment, err = task.AssessReviewRound(chain, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range assessment.Items {
		want := ReviewStillApplicable
		if item.RequirementID == "need-b" {
			want = ReviewNeedsReview
		}
		if item.Applicability != want {
			t.Fatalf("conflicting material was classified incorrectly: %+v", item)
		}
	}
	if _, err := task.OpenReviewRound(chain, "reviewer-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := task.RecordReviewOpinion(chain, 2, "need-b", ReviewPassed, "must not pass conflict"); !errors.Is(err, ErrInvalidReviewOpinion) {
		t.Fatalf("conflicting material could receive a pass: %v", err)
	}
	if _, err := task.RecordReviewOpinion(chain, 2, "need-b", ReviewReturned, "conflicting proofs must be resolved"); err != nil {
		t.Fatal(err)
	}
	if err := task.AbandonReviewRound(2, "waiting for source clarification"); err != nil {
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
	reopened, err := store.LoadTask("review-audit")
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := store.LoadChain(TeachingProofOfWork{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.ReviewRounds) != 2 {
		t.Fatalf("review rounds were not persisted: %+v", reopened.ReviewRounds)
	}
	if reopened.ReviewRounds[0].Basis.CanonicalTip != task.ReviewRounds[0].Basis.CanonicalTip ||
		reopened.ReviewRounds[1].AbandonReason != "waiting for source clarification" {
		t.Fatalf("review basis or abandonment was lost: %+v", reopened.ReviewRounds)
	}
	assessment, err = reopened.AssessReviewRound(rebuilt, 1)
	if err != nil {
		t.Fatal(err)
	}
	if assessment.Items[0].Applicability != ReviewStillApplicable || assessment.Items[1].Applicability != ReviewNeedsReview {
		t.Fatalf("reopened assessment did not preserve applicability: %+v", assessment.Items)
	}
	explanation := reopened.Explain()
	if !strings.Contains(explanation, "review-round 1") ||
		!strings.Contains(explanation, "review-round 2") ||
		!strings.Contains(explanation, "source-alt") ||
		!strings.Contains(explanation, "waiting for source clarification") {
		t.Fatalf("abandoned review deleted audit material:\n%s", explanation)
	}
}

func TestCollectionReviewRejectsStaleOpenRoundAfterChainChange(t *testing.T) {
	chain, genesis, blocks := buildLinearChain(t, 3)
	block := mineBlockWith(t, chain, blocks[3].Header, 7201, [][]byte{[]byte("watched-review-tx")})
	mineBlockWith(t, chain, block.Header, 7202, [][]byte{[]byte("filler")})
	task, err := NewCollectionTask("review-reorg", []EvidenceRequirement{
		{ID: "need-watched", TransactionHash: TransactionHash([]byte("watched-review-tx"))},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	proof := NewInclusionProofForHeader(block.Header, block.Transactions, 0)
	if _, err := task.SubmitBatch(chain, "source-1", []InclusionProof{proof}); err != nil {
		t.Fatal(err)
	}
	if _, err := task.OpenReviewRound(chain, "reviewer-1"); err != nil {
		t.Fatal(err)
	}

	parent := genesis.Header
	for i := 0; i < 7; i++ {
		fork, err := MineChild(parent, uint64(7300+i), [][]byte{[]byte("fork-tx")})
		if err != nil {
			t.Fatal(err)
		}
		if err := chain.AddHeader(fork.Header); err != nil {
			t.Fatal(err)
		}
		parent = fork.Header
	}
	if _, err := task.RecordReviewOpinion(chain, 1, "need-watched", ReviewPassed, "late pass"); !errors.Is(err, ErrReviewBasisChanged) {
		t.Fatalf("stale open review accepted an opinion: %v", err)
	}
	if _, err := task.CompleteReviewRound(chain, 1); !errors.Is(err, ErrReviewBasisChanged) {
		t.Fatalf("stale open review could be closed: %v", err)
	}
	if err := task.AbandonReviewRound(1, "canonical chain changed"); err != nil {
		t.Fatal(err)
	}
	assessment, err := task.AssessReviewRound(chain, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(assessment.Items) != 1 || assessment.Items[0].Applicability != ReviewNeedsReview {
		t.Fatalf("old opinion survived a conflicting canonical chain: %+v", assessment)
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
