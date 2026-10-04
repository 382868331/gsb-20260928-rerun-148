package lightclient

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// EvidenceRequirement names one record a collection task must obtain. The
// requirement set is fixed when the task is created and never grows or
// shrinks afterwards, so the missing scope is always exact.
type EvidenceRequirement struct {
	ID              string `json:"id"`
	TransactionHash Hash   `json:"transactionHash"`
}

type EvidenceState string

const (
	EvidenceAccepted     EvidenceState = "accepted"
	EvidenceNeedsRecheck EvidenceState = "needs-recheck"
)

type ConflictState string

const (
	ConflictUnresolved ConflictState = "unresolved"
	ConflictResolved   ConflictState = "resolved"
)

// ConflictingEvidence records material that arrived later for a requirement
// that is already satisfied. It never replaces the first accepted evidence;
// it is kept so the contradiction stays explainable.
type ConflictingEvidence struct {
	Source        string             `json:"source"`
	Batch         int                `json:"batch"`
	Result        VerificationResult `json:"result"`
	CurrentResult VerificationResult `json:"currentResult,omitempty"`
	State         EvidenceState      `json:"state"`
	RecheckReason string             `json:"recheckReason,omitempty"`
	Reason        string             `json:"reason"`
}

// CollectedEvidence is the first accepted proof for a requirement together
// with its provenance and any later contradictory material.
type CollectedEvidence struct {
	RequirementID string                `json:"requirementId"`
	Source        string                `json:"source"`
	Batch         int                   `json:"batch"`
	Result        VerificationResult    `json:"result"`
	CurrentResult VerificationResult    `json:"currentResult,omitempty"`
	State         EvidenceState         `json:"state"`
	RecheckReason string                `json:"recheckReason,omitempty"`
	ConflictState ConflictState         `json:"conflictState,omitempty"`
	Conflicts     []ConflictingEvidence `json:"conflicts,omitempty"`
}

type BatchItemOutcome string

const (
	ItemAccepted    BatchItemOutcome = "accepted"
	ItemDuplicate   BatchItemOutcome = "duplicate"
	ItemConflict    BatchItemOutcome = "conflict"
	ItemRejected    BatchItemOutcome = "rejected"
	ItemUnrequested BatchItemOutcome = "unrequested"
)

// BatchItem records the per-proof outcome of one arrival, so out-of-order
// cross-block deliveries stay auditable.
type BatchItem struct {
	TransactionHash Hash               `json:"transactionHash"`
	RequirementID   string             `json:"requirementId,omitempty"`
	Outcome         BatchItemOutcome   `json:"outcome"`
	Result          VerificationResult `json:"result"`
}

// EvidenceBatch is one delivery of material. Its source label and observed
// chain tip are kept with every item it contributed.
type EvidenceBatch struct {
	Sequence     int         `json:"sequence"`
	Source       string      `json:"source"`
	ObservedAt   time.Time   `json:"observedAt"`
	CanonicalTip Hash        `json:"canonicalTip"`
	Items        []BatchItem `json:"items"`
}

// TaskDecision is one versioned conclusion of the task. Old versions keep
// their original basis (canonical tip and required confirmations at the
// time); a reorg or policy change produces a new version instead of
// rewriting history.
type TaskDecision struct {
	Version               int       `json:"version"`
	ObservedAt            time.Time `json:"observedAt"`
	CanonicalTip          Hash      `json:"canonicalTip"`
	RequiredConfirmations uint64    `json:"requiredConfirmations"`
	Complete              bool      `json:"complete"`
	Missing               []string  `json:"missing"`
	Reason                string    `json:"reason"`
}

type TaskStatus string

const (
	TaskCollecting TaskStatus = "collecting"
	TaskComplete   TaskStatus = "complete"
	TaskAbandoned  TaskStatus = "abandoned"
)

// CollectionTask gathers a fixed set of proofs against a fixed trusted
// basis. Material may arrive in any number of batches and any order.
type CollectionTask struct {
	ID                    string                        `json:"id"`
	TrustRootHash         Hash                          `json:"trustRootHash"`
	CreatedAt             time.Time                     `json:"createdAt"`
	RequiredConfirmations uint64                        `json:"requiredConfirmations"`
	Requirements          []EvidenceRequirement         `json:"requirements"`
	Batches               []EvidenceBatch               `json:"batches"`
	Evidence              map[string]*CollectedEvidence `json:"evidence"`
	Decisions             []TaskDecision                `json:"decisions"`
	Abandoned             bool                          `json:"abandoned"`
	AbandonReason         string                        `json:"abandonReason,omitempty"`
}

var (
	ErrEmptyRequirements     = errors.New("collection task needs at least one requirement")
	ErrDuplicateRequirement  = errors.New("collection task requirements must be unique")
	ErrInvalidConfirmations  = errors.New("collection task needs at least one required confirmation")
	ErrTaskAbandoned         = errors.New("collection task was abandoned")
	ErrTaskTrustRootMismatch = errors.New("collection task is bound to a different trust root")
)

// NewCollectionTask fixes the required record set and the trusted basis for
// the whole collection. requiredConfirmations is the task-level policy; it
// is recorded on every decision so later chain policy changes cannot
// silently rewrite old conclusions.
func NewCollectionTask(id string, requirements []EvidenceRequirement, requiredConfirmations uint64) (*CollectionTask, error) {
	if id == "" {
		return nil, errors.New("collection task needs an id")
	}
	if len(requirements) == 0 {
		return nil, ErrEmptyRequirements
	}
	if requiredConfirmations == 0 {
		return nil, ErrInvalidConfirmations
	}
	seen := map[string]bool{}
	for _, requirement := range requirements {
		if requirement.ID == "" {
			return nil, errors.New("requirement needs an id")
		}
		if seen[requirement.ID] {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateRequirement, requirement.ID)
		}
		seen[requirement.ID] = true
	}
	copied := append([]EvidenceRequirement(nil), requirements...)
	return &CollectionTask{
		ID:                    id,
		CreatedAt:             time.Now().UTC(),
		RequiredConfirmations: requiredConfirmations,
		Requirements:          copied,
		Evidence:              map[string]*CollectedEvidence{},
	}, nil
}

func (t *CollectionTask) checkTrustRoot(chain *Chain) error {
	anchor := chainAnchorHash(chain)
	if t.TrustRootHash != anchor {
		return ErrTaskTrustRootMismatch
	}
	return nil
}

func chainAnchorHash(chain *Chain) Hash {
	return chain.anchor.BlockHash
}

func (t *CollectionTask) requirementFor(txHash Hash) (EvidenceRequirement, bool) {
	for _, requirement := range t.Requirements {
		if requirement.TransactionHash == txHash {
			return requirement, true
		}
	}
	return EvidenceRequirement{}, false
}

// SubmitBatch verifies one delivery of proofs against the current chain and
// folds the outcome into the task. Duplicates of already accepted evidence
// are not counted twice; contradictory material for a satisfied requirement
// is recorded as a conflict and never overrides the first arrival.
func (t *CollectionTask) SubmitBatch(chain *Chain, source string, proofs []InclusionProof) (EvidenceBatch, error) {
	if t.Abandoned {
		return EvidenceBatch{}, ErrTaskAbandoned
	}
	if source == "" {
		return EvidenceBatch{}, errors.New("batch needs a source label")
	}
	if t.TrustRootHash == (Hash{}) {
		t.TrustRootHash = chainAnchorHash(chain)
	} else if err := t.checkTrustRoot(chain); err != nil {
		return EvidenceBatch{}, err
	}

	batch := EvidenceBatch{
		Sequence:     len(t.Batches) + 1,
		Source:       source,
		ObservedAt:   time.Now().UTC(),
		CanonicalTip: chain.CanonicalTip().Hash,
	}
	for _, proof := range proofs {
		txHash := TransactionHash(proof.Transaction)
		requirement, wanted := t.requirementFor(txHash)
		result, verifyErr := chain.VerifyInclusion(proof, t.RequiredConfirmations)
		item := BatchItem{TransactionHash: txHash, Result: result}
		if !wanted {
			item.Outcome = ItemUnrequested
			batch.Items = append(batch.Items, item)
			continue
		}
		item.RequirementID = requirement.ID
		if verifyErr != nil || result.Status != ResultAccepted {
			item.Outcome = ItemRejected
			batch.Items = append(batch.Items, item)
			continue
		}
		existing, collected := t.Evidence[requirement.ID]
		if !collected {
			t.Evidence[requirement.ID] = &CollectedEvidence{
				RequirementID: requirement.ID,
				Source:        source,
				Batch:         batch.Sequence,
				Result:        result,
				CurrentResult: result,
				State:         EvidenceAccepted,
			}
			item.Outcome = ItemAccepted
			batch.Items = append(batch.Items, item)
			continue
		}
		if sameEvidence(existing.Result, result) {
			item.Outcome = ItemDuplicate
			batch.Items = append(batch.Items, item)
			continue
		}
		existing.Conflicts = append(existing.Conflicts, ConflictingEvidence{
			Source:        source,
			Batch:         batch.Sequence,
			Result:        result,
			CurrentResult: result,
			State:         EvidenceAccepted,
			Reason:        "contradicts the evidence already collected for this requirement; first arrival is kept",
		})
		existing.setConflictUnresolved()
		item.Outcome = ItemConflict
		batch.Items = append(batch.Items, item)
	}
	t.Batches = append(t.Batches, batch)
	t.recordDecision(chain, "batch "+source+" processed")
	return batch, nil
}

func sameEvidence(a, b VerificationResult) bool {
	return a.Status == b.Status &&
		a.BlockHash == b.BlockHash &&
		a.TransactionIndex == b.TransactionIndex &&
		a.TransactionHash == b.TransactionHash
}

// Missing returns the requirements that do not currently hold accepted,
// still-valid evidence. The scope is exact: partial validity never counts
// as overall completeness.
func (t *CollectionTask) Missing() []EvidenceRequirement {
	var missing []EvidenceRequirement
	for _, requirement := range t.Requirements {
		evidence, ok := t.Evidence[requirement.ID]
		if !ok || evidence.State != EvidenceAccepted || t.conflictIsUnresolved(evidence) {
			missing = append(missing, requirement)
		}
	}
	return missing
}

func (t *CollectionTask) Complete() bool {
	return !t.Abandoned && len(t.Missing()) == 0
}

func (t *CollectionTask) Status() TaskStatus {
	switch {
	case t.Abandoned:
		return TaskAbandoned
	case t.Complete():
		return TaskComplete
	default:
		return TaskCollecting
	}
}

func (t *CollectionTask) recordDecision(chain *Chain, reason string) {
	missing := t.Missing()
	missingIDs := make([]string, 0, len(missing))
	for _, requirement := range missing {
		missingIDs = append(missingIDs, requirement.ID)
	}
	sort.Strings(missingIDs)
	complete := len(missing) == 0 && !t.Abandoned
	decision := TaskDecision{
		Version:               len(t.Decisions) + 1,
		ObservedAt:            time.Now().UTC(),
		CanonicalTip:          chain.CanonicalTip().Hash,
		RequiredConfirmations: t.RequiredConfirmations,
		Complete:              complete,
		Missing:               missingIDs,
		Reason:                reason,
	}
	if complete {
		decision.Reason = reason + ": all requirements satisfied"
	} else if unresolved := t.unresolvedConflictIDs(); len(unresolved) > 0 && !strings.Contains(reason, "unresolved conflicts") {
		decision.Reason = reason + ": unresolved conflicts: " + strings.Join(unresolved, ", ")
	}
	t.Decisions = append(t.Decisions, decision)
}

// Revalidate re-checks every accepted evidence item against the current
// chain after a reorg or a confirmation-policy change. Items that no longer
// hold are marked needs-recheck and returned; the original evidence and all
// earlier decisions are kept, and the new evaluation becomes a separate
// decision version.
func (t *CollectionTask) Revalidate(chain *Chain) ([]string, error) {
	if t.Abandoned {
		return nil, ErrTaskAbandoned
	}
	if err := t.checkTrustRoot(chain); err != nil {
		return nil, err
	}
	var stale []string
	var invalidPrimary []string
	var invalidConflicts []string
	var unresolvedConflicts []string
	var resolvedConflicts []string
	for _, requirement := range t.Requirements {
		evidence, ok := t.Evidence[requirement.ID]
		if !ok {
			continue
		}
		freshPrimary, err := chain.RecheckResult(evidence.Result, t.RequiredConfirmations)
		evidence.CurrentResult = freshPrimary
		if err != nil {
			evidence.State = EvidenceNeedsRecheck
			evidence.RecheckReason = freshPrimary.Reason
			invalidPrimary = append(invalidPrimary, fmt.Sprintf("%s primary from %q batch %d: %s", requirement.ID, evidence.Source, evidence.Batch, freshPrimary.Reason))
		} else {
			evidence.State = EvidenceAccepted
			evidence.RecheckReason = ""
		}

		hasAcceptedConflict := false
		for i := range evidence.Conflicts {
			conflict := &evidence.Conflicts[i]
			freshConflict, conflictErr := chain.RecheckResult(conflict.Result, t.RequiredConfirmations)
			conflict.CurrentResult = freshConflict
			if conflictErr != nil {
				conflict.State = EvidenceNeedsRecheck
				conflict.RecheckReason = freshConflict.Reason
				invalidConflicts = append(invalidConflicts, fmt.Sprintf("%s conflict from %q batch %d: %s", requirement.ID, conflict.Source, conflict.Batch, freshConflict.Reason))
				continue
			}
			conflict.State = EvidenceAccepted
			conflict.RecheckReason = ""
			hasAcceptedConflict = true
		}

		if len(evidence.Conflicts) > 0 && hasAcceptedConflict {
			evidence.ConflictState = ConflictUnresolved
			unresolvedConflicts = append(unresolvedConflicts, requirement.ID)
		} else if len(evidence.Conflicts) > 0 {
			evidence.ConflictState = ConflictResolved
			resolvedConflicts = append(resolvedConflicts, requirement.ID)
		}
		if evidence.State != EvidenceAccepted || evidence.ConflictState == ConflictUnresolved {
			stale = append(stale, requirement.ID)
		}
	}
	sort.Strings(stale)
	sort.Strings(invalidPrimary)
	sort.Strings(invalidConflicts)
	sort.Strings(unresolvedConflicts)
	sort.Strings(resolvedConflicts)

	reasonParts := make([]string, 0, 3)
	if len(invalidPrimary) > 0 {
		reasonParts = append(reasonParts, "invalid primary proof: "+strings.Join(invalidPrimary, "; "))
	}
	if len(invalidConflicts) > 0 {
		reasonParts = append(reasonParts, "invalid conflicting proof: "+strings.Join(invalidConflicts, "; "))
	}
	if len(unresolvedConflicts) > 0 {
		reasonParts = append(reasonParts, "unresolved conflicts: "+strings.Join(unresolvedConflicts, ", "))
	}
	if len(resolvedConflicts) > 0 {
		reasonParts = append(reasonParts, "resolved contradictions: "+strings.Join(resolvedConflicts, ", "))
	}
	if len(stale) > 0 {
		reasonParts = append(reasonParts, "requirements needing attention: "+strings.Join(stale, ", "))
	}
	if len(reasonParts) > 0 {
		t.recordDecision(chain, "revalidation: "+strings.Join(reasonParts, "; "))
	} else {
		t.recordDecision(chain, "revalidation confirmed all collected evidence")
	}
	return stale, nil
}

// Abandon closes the task. Collected evidence and decisions stay available
// for explanation; no further material is accepted.
func (t *CollectionTask) Abandon(reason string) {
	t.Abandoned = true
	t.AbandonReason = reason
}

func (t *CollectionTask) unresolvedConflictIDs() []string {
	var ids []string
	for _, requirement := range t.Requirements {
		evidence, ok := t.Evidence[requirement.ID]
		if ok && t.conflictIsUnresolved(evidence) {
			ids = append(ids, requirement.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func (t *CollectionTask) conflictIsUnresolved(evidence *CollectedEvidence) bool {
	if len(evidence.Conflicts) == 0 {
		return false
	}
	return evidence.ConflictState == "" || evidence.ConflictState == ConflictUnresolved
}

func (e *CollectedEvidence) setConflictUnresolved() {
	e.ConflictState = ConflictUnresolved
	if e.CurrentResult == (VerificationResult{}) {
		e.CurrentResult = e.Result
	}
}

// Explain renders what was checked, with provenance, and why the task did
// not (or did) reach a complete pass. It works for abandoned tasks too.
func (t *CollectionTask) Explain() string {
	var lines []string
	lines = append(lines, fmt.Sprintf("task %s: status=%s required-confirmations=%d", t.ID, t.Status(), t.RequiredConfirmations))
	for _, requirement := range t.Requirements {
		evidence, ok := t.Evidence[requirement.ID]
		if !ok {
			lines = append(lines, fmt.Sprintf("  %s: missing (tx %s)", requirement.ID, requirement.TransactionHash.Hex()))
			continue
		}
		line := fmt.Sprintf("  %s: %s from source %q batch %d block %s",
			requirement.ID, evidence.State, evidence.Source, evidence.Batch, evidence.Result.BlockHash.Hex())
		if evidence.State == EvidenceNeedsRecheck {
			line += " recheck-reason: " + evidence.RecheckReason
		}
		if len(evidence.Conflicts) > 0 {
			state := evidence.ConflictState
			if state == "" {
				state = ConflictUnresolved
			}
			line += fmt.Sprintf(" (%d conflicting later arrival(s), %s, first kept)", len(evidence.Conflicts), state)
			for _, conflict := range evidence.Conflicts {
				conflictState := conflict.State
				if conflictState == "" {
					conflictState = EvidenceAccepted
				}
				line += fmt.Sprintf("\n    conflict source %q batch %d: %s block %s", conflict.Source, conflict.Batch, conflictState, conflict.Result.BlockHash.Hex())
				if conflict.RecheckReason != "" {
					line += " recheck-reason: " + conflict.RecheckReason
				}
			}
		}
		lines = append(lines, line)
	}
	if t.Abandoned {
		lines = append(lines, "  abandoned: "+t.AbandonReason)
	}
	if !t.Complete() {
		missing := t.Missing()
		ids := make([]string, 0, len(missing))
		for _, requirement := range missing {
			ids = append(ids, requirement.ID)
		}
		sort.Strings(ids)
		var reasons []string
		if len(ids) > 0 {
			reasons = append(reasons, "still missing or unresolved: "+strings.Join(ids, ", "))
		}
		if unresolved := t.unresolvedConflictIDs(); len(unresolved) > 0 {
			reasons = append(reasons, "unresolved conflict: "+strings.Join(unresolved, ", "))
		}
		if len(reasons) > 0 {
			lines = append(lines, "  incomplete: "+strings.Join(reasons, "; "))
		}
	}
	return strings.Join(lines, "\n")
}
