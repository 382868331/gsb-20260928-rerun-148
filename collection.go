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

// ConflictingEvidence records material that arrived later for a requirement
// that is already satisfied. It never replaces the first accepted evidence;
// it is kept so the contradiction stays explainable. A conflict stays
// unresolved until Revalidate finds the contradictory proof no longer holds
// or ResolveConflict settles it explicitly; unresolved conflicts keep the
// requirement out of the satisfied set.
type ConflictingEvidence struct {
	Source     string             `json:"source"`
	Batch      int                `json:"batch"`
	Result     VerificationResult `json:"result"`
	Reason     string             `json:"reason"`
	Resolved   bool               `json:"resolved"`
	Resolution string             `json:"resolution,omitempty"`
}

// CollectedEvidence is the first accepted proof for a requirement together
// with its provenance and any later contradictory material.
type CollectedEvidence struct {
	RequirementID string                `json:"requirementId"`
	Source        string                `json:"source"`
	Batch         int                   `json:"batch"`
	Result        VerificationResult    `json:"result"`
	State         EvidenceState         `json:"state"`
	RecheckReason string                `json:"recheckReason,omitempty"`
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
	Conflicts             []string  `json:"conflicts,omitempty"`
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
			Source: source,
			Batch:  batch.Sequence,
			Result: result,
			Reason: "contradicts the evidence already collected for this requirement; first arrival is kept",
		})
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

// PendingConflicts returns the requirements whose collected evidence still
// carries a contradiction nobody has settled. An unresolved conflict means
// the requirement is not clearly satisfied, so it cannot count toward a
// complete pass.
func (t *CollectionTask) PendingConflicts() []string {
	var ids []string
	for _, requirement := range t.Requirements {
		evidence, ok := t.Evidence[requirement.ID]
		if !ok {
			continue
		}
		for _, conflict := range evidence.Conflicts {
			if !conflict.Resolved {
				ids = append(ids, requirement.ID)
				break
			}
		}
	}
	sort.Strings(ids)
	return ids
}

func hasPendingConflict(evidence *CollectedEvidence) bool {
	for _, conflict := range evidence.Conflicts {
		if !conflict.Resolved {
			return true
		}
	}
	return false
}

// Missing returns the requirements that do not currently hold accepted,
// still-valid and undisputed evidence. The scope is exact: partial validity
// never counts as overall completeness, and a requirement with an
// unresolved conflict is reported here instead of being silently treated
// as satisfied.
func (t *CollectionTask) Missing() []EvidenceRequirement {
	var missing []EvidenceRequirement
	for _, requirement := range t.Requirements {
		evidence, ok := t.Evidence[requirement.ID]
		if !ok || evidence.State != EvidenceAccepted || hasPendingConflict(evidence) {
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
	conflicts := t.PendingConflicts()
	complete := len(missing) == 0 && !t.Abandoned
	decision := TaskDecision{
		Version:               len(t.Decisions) + 1,
		ObservedAt:            time.Now().UTC(),
		CanonicalTip:          chain.CanonicalTip().Hash,
		RequiredConfirmations: t.RequiredConfirmations,
		Complete:              complete,
		Missing:               missingIDs,
		Conflicts:             conflicts,
		Reason:                reason,
	}
	if complete {
		decision.Reason = reason + ": all requirements satisfied"
	} else if len(conflicts) > 0 {
		decision.Reason = reason + ": unresolved conflicts on " + strings.Join(conflicts, ", ")
	}
	t.Decisions = append(t.Decisions, decision)
}

// ResolveConflict explicitly settles every pending contradiction for one
// requirement, keeping the first accepted evidence. The resolution note is
// stored on each settled conflict and the settlement becomes its own
// decision version, so the task can only pass once every contradiction has
// been dealt with openly.
func (t *CollectionTask) ResolveConflict(chain *Chain, requirementID string, resolution string) error {
	if t.Abandoned {
		return ErrTaskAbandoned
	}
	if err := t.checkTrustRoot(chain); err != nil {
		return err
	}
	evidence, ok := t.Evidence[requirementID]
	if !ok {
		return fmt.Errorf("no evidence collected for requirement %s", requirementID)
	}
	pending := false
	for i := range evidence.Conflicts {
		if evidence.Conflicts[i].Resolved {
			continue
		}
		evidence.Conflicts[i].Resolved = true
		evidence.Conflicts[i].Resolution = resolution
		pending = true
	}
	if !pending {
		return fmt.Errorf("requirement %s has no unresolved conflict", requirementID)
	}
	t.recordDecision(chain, fmt.Sprintf("conflicts resolved for %s: %s", requirementID, resolution))
	return nil
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
	for _, requirement := range t.Requirements {
		evidence, ok := t.Evidence[requirement.ID]
		if !ok || evidence.State != EvidenceAccepted {
			continue
		}
		if _, err := chain.RecheckResult(evidence.Result, t.RequiredConfirmations); err != nil {
			evidence.State = EvidenceNeedsRecheck
			evidence.RecheckReason = err.Error()
			stale = append(stale, requirement.ID)
		}
	}
	sort.Strings(stale)
	for _, requirement := range t.Requirements {
		evidence, ok := t.Evidence[requirement.ID]
		if !ok {
			continue
		}
		for i := range evidence.Conflicts {
			conflict := &evidence.Conflicts[i]
			if conflict.Resolved {
				continue
			}
			if _, err := chain.RecheckResult(conflict.Result, t.RequiredConfirmations); err != nil {
				conflict.Resolved = true
				conflict.Resolution = "conflicting proof no longer holds on the current chain: " + err.Error()
			}
		}
	}
	if len(stale) > 0 {
		t.recordDecision(chain, fmt.Sprintf("revalidation invalidated: %s", strings.Join(stale, ", ")))
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
			pending := 0
			for _, conflict := range evidence.Conflicts {
				if !conflict.Resolved {
					pending++
				}
			}
			if pending > 0 {
				line += fmt.Sprintf(" (%d unresolved conflicting arrival(s), requirement not settled)", pending)
			} else {
				line += fmt.Sprintf(" (%d conflicting later arrival(s) recorded, first kept)", len(evidence.Conflicts))
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
		lines = append(lines, "  incomplete: still missing "+strings.Join(ids, ", "))
		if conflicts := t.PendingConflicts(); len(conflicts) > 0 {
			lines = append(lines, "  incomplete: unresolved conflicts on "+strings.Join(conflicts, ", "))
		}
	}
	return strings.Join(lines, "\n")
}
