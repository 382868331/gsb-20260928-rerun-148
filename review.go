package lightclient

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ReviewVerdict is the per-requirement opinion a reviewer leaves inside one
// review round. A verdict never modifies the collected proof itself.
type ReviewVerdict string

const (
	ReviewApproved ReviewVerdict = "approved"
	ReviewReturned ReviewVerdict = "returned"
)

// RequirementReview is one reviewer's opinion about one requirement. It
// records the exact material it was formed against (evidence identity and
// state at the time), so a later evidence or chain change can tell whether
// the opinion still applies instead of silently carrying it over.
type RequirementReview struct {
	RequirementID     string        `json:"requirementId"`
	Verdict           ReviewVerdict `json:"verdict"`
	Comment           string        `json:"comment,omitempty"`
	EvidenceBlockHash Hash          `json:"evidenceBlockHash,omitempty"`
	EvidenceBatch     int           `json:"evidenceBatch,omitempty"`
	EvidenceState     EvidenceState `json:"evidenceState,omitempty"`
	ConflictState     ConflictState `json:"conflictState,omitempty"`
	DecidedAt         time.Time     `json:"decidedAt"`
}

// ReviewRound is one independent pass of review over the material collected
// so far. The round freezes the basis it relied on: the canonical tip, the
// required confirmations and the material version at open time. Opinions are
// annotations only; the underlying evidence is never edited by a review.
type ReviewRound struct {
	Sequence              int                 `json:"sequence"`
	Reviewer              string              `json:"reviewer"`
	OpenedAt              time.Time           `json:"openedAt"`
	Closed                bool                `json:"closed"`
	CanonicalTip          Hash                `json:"canonicalTip"`
	RequiredConfirmations uint64              `json:"requiredConfirmations"`
	MaterialVersion       int                 `json:"materialVersion"`
	Opinions              []RequirementReview `json:"opinions,omitempty"`
	Abandoned             bool                `json:"abandoned"`
	AbandonReason         string              `json:"abandonReason,omitempty"`
}

// OpinionApplicability tells whether an old opinion still speaks about the
// current material or must be re-reviewed.
type OpinionApplicability string

const (
	OpinionApplicable     OpinionApplicability = "applicable"
	OpinionNeedsReReview  OpinionApplicability = "needs-re-review"
	OpinionRoundAbandoned OpinionApplicability = "round-abandoned"
)

// OpinionStatus pairs one recorded opinion with its current applicability
// and the reason for that classification.
type OpinionStatus struct {
	Round    int                  `json:"round"`
	Reviewer string               `json:"reviewer"`
	Opinion  RequirementReview    `json:"opinion"`
	Status   OpinionApplicability `json:"status"`
	Reason   string               `json:"reason,omitempty"`
}

var (
	ErrReviewRoundNotFound  = errors.New("review round not found")
	ErrReviewRoundClosed    = errors.New("review round is closed")
	ErrReviewRoundAbandoned = errors.New("review round was abandoned")
	ErrUnknownRequirement   = errors.New("review opinion names an unknown requirement")
	ErrInvalidVerdict       = errors.New("review verdict must be approved or returned")
)

// OpenReview starts a new review round over the current material. The round
// captures the canonical tip, the confirmation requirement and the material
// version as its basis; later changes are detected by comparing against
// these frozen values.
func (t *CollectionTask) OpenReview(chain *Chain, reviewer string) (*ReviewRound, error) {
	if t.Abandoned {
		return nil, ErrTaskAbandoned
	}
	if reviewer == "" {
		return nil, errors.New("review round needs a reviewer label")
	}
	if err := t.checkTrustRoot(chain); err != nil {
		return nil, err
	}
	round := ReviewRound{
		Sequence:              len(t.ReviewRounds) + 1,
		Reviewer:              reviewer,
		OpenedAt:              time.Now().UTC(),
		CanonicalTip:          chain.CanonicalTip().Hash,
		RequiredConfirmations: t.RequiredConfirmations,
		MaterialVersion:       t.MaterialVersion,
	}
	t.ReviewRounds = append(t.ReviewRounds, round)
	return &t.ReviewRounds[len(t.ReviewRounds)-1], nil
}

func (t *CollectionTask) reviewRound(sequence int) (*ReviewRound, error) {
	if sequence < 1 || sequence > len(t.ReviewRounds) {
		return nil, fmt.Errorf("%w: %d", ErrReviewRoundNotFound, sequence)
	}
	return &t.ReviewRounds[sequence-1], nil
}

// RecordOpinion leaves or replaces the reviewer's verdict for one
// requirement inside an open round. The opinion snapshots the identity of
// the evidence it refers to; it never touches the evidence itself.
func (t *CollectionTask) RecordOpinion(sequence int, requirementID string, verdict ReviewVerdict, comment string) error {
	if verdict != ReviewApproved && verdict != ReviewReturned {
		return ErrInvalidVerdict
	}
	round, err := t.reviewRound(sequence)
	if err != nil {
		return err
	}
	if round.Abandoned {
		return ErrReviewRoundAbandoned
	}
	if round.Closed {
		return ErrReviewRoundClosed
	}
	known := false
	for _, requirement := range t.Requirements {
		if requirement.ID == requirementID {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("%w: %s", ErrUnknownRequirement, requirementID)
	}
	opinion := RequirementReview{
		RequirementID: requirementID,
		Verdict:       verdict,
		Comment:       comment,
		DecidedAt:     time.Now().UTC(),
	}
	if evidence, ok := t.Evidence[requirementID]; ok {
		opinion.EvidenceBlockHash = evidence.Result.BlockHash
		opinion.EvidenceBatch = evidence.Batch
		opinion.EvidenceState = evidence.State
		opinion.ConflictState = evidence.ConflictState
		if opinion.ConflictState == "" && len(evidence.Conflicts) > 0 {
			opinion.ConflictState = ConflictUnresolved
		}
	}
	for i, existing := range round.Opinions {
		if existing.RequirementID == requirementID {
			round.Opinions[i] = opinion
			return nil
		}
	}
	round.Opinions = append(round.Opinions, opinion)
	return nil
}

// PendingDecisions lists the requirements the round has not decided yet.
// A round may close with pending decisions; they stay visible here and in
// the decision log entry recorded at close time.
func (t *CollectionTask) PendingDecisions(sequence int) ([]string, error) {
	round, err := t.reviewRound(sequence)
	if err != nil {
		return nil, err
	}
	decided := map[string]bool{}
	for _, opinion := range round.Opinions {
		decided[opinion.RequirementID] = true
	}
	var pending []string
	for _, requirement := range t.Requirements {
		if !decided[requirement.ID] {
			pending = append(pending, requirement.ID)
		}
	}
	sort.Strings(pending)
	return pending, nil
}

// CloseReview ends the round and appends a task decision recording the
// per-requirement outcome and the decisions the round still lacks. Closing
// never alters the collected evidence.
func (t *CollectionTask) CloseReview(chain *Chain, sequence int) error {
	round, err := t.reviewRound(sequence)
	if err != nil {
		return err
	}
	if round.Abandoned {
		return ErrReviewRoundAbandoned
	}
	if round.Closed {
		return ErrReviewRoundClosed
	}
	round.Closed = true
	var approved, returned []string
	for _, opinion := range round.Opinions {
		switch opinion.Verdict {
		case ReviewApproved:
			approved = append(approved, opinion.RequirementID)
		case ReviewReturned:
			returned = append(returned, opinion.RequirementID)
		}
	}
	sort.Strings(approved)
	sort.Strings(returned)
	pending, err := t.PendingDecisions(sequence)
	if err != nil {
		return err
	}
	parts := []string{fmt.Sprintf("review round %d by %q closed", sequence, round.Reviewer)}
	if len(approved) > 0 {
		parts = append(parts, "approved: "+strings.Join(approved, ", "))
	}
	if len(returned) > 0 {
		parts = append(parts, "returned for supplement: "+strings.Join(returned, ", "))
	}
	if len(pending) > 0 {
		parts = append(parts, "decisions still missing: "+strings.Join(pending, ", "))
	}
	t.recordDecision(chain, strings.Join(parts, "; "))
	return nil
}

// AbandonReview gives up a round. The round, its opinions and the material
// it reviewed are all kept; the round simply stops accepting new opinions.
func (t *CollectionTask) AbandonReview(sequence int, reason string) error {
	round, err := t.reviewRound(sequence)
	if err != nil {
		return err
	}
	if round.Closed {
		return ErrReviewRoundClosed
	}
	round.Abandoned = true
	round.AbandonReason = reason
	return nil
}

// opinionApplicability classifies one opinion against the current task and
// chain state. An approval is never migrated onto different material: if the
// evidence identity, its validity state, its conflict state or the canonical
// basis changed, the opinion must be re-reviewed.
func (t *CollectionTask) opinionApplicability(chain *Chain, round ReviewRound, opinion RequirementReview) OpinionStatus {
	status := OpinionStatus{Round: round.Sequence, Reviewer: round.Reviewer, Opinion: opinion}
	if round.Abandoned {
		status.Status = OpinionRoundAbandoned
		status.Reason = "round was abandoned: " + round.AbandonReason
		return status
	}
	evidence, ok := t.Evidence[opinion.RequirementID]
	if !ok {
		if opinion.EvidenceBlockHash == (Hash{}) {
			status.Status = OpinionApplicable
			return status
		}
		status.Status = OpinionNeedsReReview
		status.Reason = "the reviewed evidence is no longer present"
		return status
	}
	conflictState := evidence.ConflictState
	if conflictState == "" && len(evidence.Conflicts) > 0 {
		conflictState = ConflictUnresolved
	}
	switch {
	case evidence.Result.BlockHash != opinion.EvidenceBlockHash || evidence.Batch != opinion.EvidenceBatch:
		status.Status = OpinionNeedsReReview
		status.Reason = "a different proof now holds this requirement"
	case evidence.State != opinion.EvidenceState:
		status.Status = OpinionNeedsReReview
		status.Reason = "the reviewed evidence changed validity state: " + evidence.RecheckReason
	case conflictState != opinion.ConflictState:
		status.Status = OpinionNeedsReReview
		status.Reason = "the conflict state of the reviewed evidence changed"
	default:
		if _, err := chain.RecheckResult(evidence.Result, round.RequiredConfirmations); err != nil {
			status.Status = OpinionNeedsReReview
			status.Reason = "the reviewed proof no longer verifies on the current canonical chain"
			return status
		}
		status.Status = OpinionApplicable
	}
	return status
}

// ReviewStatuses reports, for every recorded round, which opinions still
// apply to the current material and which must be re-reviewed after new
// evidence or a canonical-chain change. Old approvals are reported, never
// re-attributed to different proofs.
func (t *CollectionTask) ReviewStatuses(chain *Chain) ([]OpinionStatus, error) {
	if err := t.checkTrustRoot(chain); err != nil {
		return nil, err
	}
	var statuses []OpinionStatus
	for _, round := range t.ReviewRounds {
		for _, opinion := range round.Opinions {
			statuses = append(statuses, t.opinionApplicability(chain, round, opinion))
		}
	}
	return statuses, nil
}

// RoundComparison is the per-requirement diff between two review rounds.
type RoundComparison struct {
	RequirementID string
	First         *RequirementReview
	Second        *RequirementReview
	Changed       bool
}

// CompareRounds lines up two rounds opinion by opinion so reopened tasks can
// show how verdicts, basis and gaps moved between rounds.
func (t *CollectionTask) CompareRounds(first, second int) ([]RoundComparison, error) {
	firstRound, err := t.reviewRound(first)
	if err != nil {
		return nil, err
	}
	secondRound, err := t.reviewRound(second)
	if err != nil {
		return nil, err
	}
	firstByID := map[string]RequirementReview{}
	for _, opinion := range firstRound.Opinions {
		firstByID[opinion.RequirementID] = opinion
	}
	secondByID := map[string]RequirementReview{}
	for _, opinion := range secondRound.Opinions {
		secondByID[opinion.RequirementID] = opinion
	}
	var comparisons []RoundComparison
	for _, requirement := range t.Requirements {
		comparison := RoundComparison{RequirementID: requirement.ID}
		if opinion, ok := firstByID[requirement.ID]; ok {
			copied := opinion
			comparison.First = &copied
		}
		if opinion, ok := secondByID[requirement.ID]; ok {
			copied := opinion
			comparison.Second = &copied
		}
		comparison.Changed = !sameOpinion(comparison.First, comparison.Second)
		comparisons = append(comparisons, comparison)
	}
	return comparisons, nil
}

func sameOpinion(a, b *RequirementReview) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Verdict == b.Verdict &&
		a.EvidenceBlockHash == b.EvidenceBlockHash &&
		a.EvidenceState == b.EvidenceState &&
		a.ConflictState == b.ConflictState
}
