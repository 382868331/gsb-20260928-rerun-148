package lightclient

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var ErrCollectionAbandoned = errors.New("collection task has been abandoned")

type CollectionItemState string

const (
	CollectionItemPending     CollectionItemState = "pending"
	CollectionItemAccepted    CollectionItemState = "accepted"
	CollectionItemRejected    CollectionItemState = "rejected"
	CollectionItemConflicting CollectionItemState = "conflicting"
	CollectionItemNeedsReview CollectionItemState = "needs-review"
	CollectionItemAbandoned   CollectionItemState = "abandoned"
)

type CollectionTaskState string

const (
	CollectionTaskIncomplete CollectionTaskState = "incomplete"
	CollectionTaskComplete   CollectionTaskState = "complete"
	CollectionTaskAbandoned  CollectionTaskState = "abandoned"
)

type CollectionRequirement struct {
	ID                    string `json:"id"`
	TransactionHash       Hash   `json:"transactionHash"`
	RequiredConfirmations uint64 `json:"requiredConfirmations,omitempty"`
}

type CollectionBasis struct {
	TrustRootHash         Hash      `json:"trustRootHash"`
	CanonicalTip          Hash      `json:"canonicalTip"`
	RequiredConfirmations uint64    `json:"requiredConfirmations"`
	ObservedAt            time.Time `json:"observedAt"`
}

type ProofDelivery struct {
	ID            string             `json:"id"`
	SourceID      string             `json:"sourceId"`
	ReceivedAt    time.Time          `json:"receivedAt"`
	Proof         InclusionProof     `json:"proof"`
	Result        VerificationResult `json:"result"`
	RequirementID string             `json:"requirementId,omitempty"`
	DuplicateOf   string             `json:"duplicateOf,omitempty"`
}

type CollectionItemStatus struct {
	RequirementID string              `json:"requirementId"`
	State         CollectionItemState `json:"state"`
	DeliveryIDs   []string            `json:"deliveryIds"`
	AcceptedID    string              `json:"acceptedId,omitempty"`
	Reason        string              `json:"reason,omitempty"`
}

type CollectionDecision struct {
	Version    int                    `json:"version"`
	State      CollectionTaskState    `json:"state"`
	Basis      CollectionBasis        `json:"basis"`
	Items      []CollectionItemStatus `json:"items"`
	MissingIDs []string               `json:"missingIds"`
	RecheckIDs []string               `json:"recheckIds,omitempty"`
	Reason     string                 `json:"reason,omitempty"`
}

type ProofCollectionTask struct {
	ID                   Hash                    `json:"id"`
	Name                 string                  `json:"name"`
	TrustRootHash        Hash                    `json:"trustRootHash"`
	DefaultConfirmations uint64                  `json:"defaultConfirmations"`
	Requirements         []CollectionRequirement `json:"requirements"`
	Deliveries           []ProofDelivery         `json:"deliveries"`
	Decisions            []CollectionDecision    `json:"decisions"`
	Abandoned            bool                    `json:"abandoned"`
	AbandonedReason      string                  `json:"abandonedReason,omitempty"`
	AbandonedAt          time.Time               `json:"abandonedAt,omitempty"`
}

func NewProofCollectionTask(chain *Chain, name string, requirements []CollectionRequirement, requiredConfirmations uint64) (*ProofCollectionTask, error) {
	if chain == nil {
		return nil, errors.New("collection task needs a chain")
	}
	if name == "" {
		return nil, errors.New("collection task needs a name")
	}
	if len(requirements) == 0 {
		return nil, errors.New("collection task needs at least one requirement")
	}
	seen := map[string]bool{}
	txSeen := map[Hash]bool{}
	cloned := make([]CollectionRequirement, len(requirements))
	for i, requirement := range requirements {
		if requirement.ID == "" {
			return nil, fmt.Errorf("requirement %d has no id", i)
		}
		if seen[requirement.ID] {
			return nil, fmt.Errorf("duplicate requirement id %q", requirement.ID)
		}
		if requirement.TransactionHash == (Hash{}) {
			return nil, fmt.Errorf("requirement %q has no transaction hash", requirement.ID)
		}
		if txSeen[requirement.TransactionHash] {
			return nil, fmt.Errorf("transaction %x is required more than once", requirement.TransactionHash)
		}
		seen[requirement.ID] = true
		txSeen[requirement.TransactionHash] = true
		req := requirement
		cloned[i] = req
	}
	task := &ProofCollectionTask{
		Name:                 name,
		TrustRootHash:        chain.anchor.BlockHash,
		DefaultConfirmations: requiredConfirmations,
		Requirements:         cloned,
	}
	task.ID = task.identityHash()
	return task, nil
}

func (t *ProofCollectionTask) identityHash() Hash {
	var data []byte
	data = append(data, []byte(t.Name)...)
	data = append(data, t.TrustRootHash[:]...)
	data = binary.BigEndian.AppendUint64(data, t.DefaultConfirmations)
	for _, requirement := range t.Requirements {
		data = append(data, []byte(requirement.ID)...)
		data = append(data, requirement.TransactionHash[:]...)
		data = binary.BigEndian.AppendUint64(data, requirement.RequiredConfirmations)
	}
	return Hash(sha256.Sum256(data))
}

func (t *ProofCollectionTask) requirementForHash(hash Hash) (CollectionRequirement, bool) {
	for _, requirement := range t.Requirements {
		if requirement.TransactionHash == hash {
			return requirement, true
		}
	}
	return CollectionRequirement{}, false
}

func (t *ProofCollectionTask) confirmationsForProof(proof InclusionProof) uint64 {
	if requirement, ok := t.requirementForHash(TransactionHash(proof.Transaction)); ok && requirement.RequiredConfirmations != 0 {
		return requirement.RequiredConfirmations
	}
	if t.DefaultConfirmations != 0 {
		return t.DefaultConfirmations
	}
	return 0
}

func (t *ProofCollectionTask) AddProofBatch(chain *Chain, sourceID string, receivedAt time.Time, proofs ...InclusionProof) ([]ProofDelivery, error) {
	if t == nil {
		return nil, errors.New("collection task is nil")
	}
	if t.Abandoned {
		return nil, ErrCollectionAbandoned
	}
	if chain == nil {
		return nil, errors.New("chain is nil")
	}
	if chain.anchor.BlockHash != t.TrustRootHash {
		return nil, errors.New("delivery is checked against a different trust root")
	}
	if sourceID == "" {
		return nil, errors.New("delivery needs a source id")
	}
	if len(proofs) == 0 {
		return nil, ErrEmptyBatch
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now().UTC()
	}
	deliveries := make([]ProofDelivery, 0, len(proofs))
	for i, proof := range proofs {
		result, _ := chain.VerifyInclusion(proof, t.confirmationsForProof(proof))
		txHash := TransactionHash(proof.Transaction)
		requirement, _ := t.requirementForHash(txHash)
		delivery := ProofDelivery{
			ID:            deliveryIdentityHash(sourceID, receivedAt, i, proof),
			SourceID:      sourceID,
			ReceivedAt:    receivedAt.UTC(),
			Proof:         proof,
			Result:        result,
			RequirementID: requirement.ID,
		}
		if existingID, ok := t.findDuplicate(delivery); ok {
			delivery.DuplicateOf = existingID
		}
		t.Deliveries = append(t.Deliveries, delivery)
		deliveries = append(deliveries, delivery)
	}
	return deliveries, nil
}

func deliveryIdentityHash(source string, received time.Time, index int, proof InclusionProof) string {
	var data []byte
	data = append(data, []byte(source)...)
	data = binary.BigEndian.AppendUint64(data, uint64(received.UTC().UnixNano()))
	data = binary.BigEndian.AppendUint64(data, uint64(index))
	data = append(data, proof.BlockHash[:]...)
	data = append(data, proof.Transaction...)
	data = binary.BigEndian.AppendUint64(data, proof.Index)
	data = binary.BigEndian.AppendUint64(data, proof.TransactionCount)
	for _, sibling := range proof.Siblings {
		data = append(data, sibling[:]...)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (t *ProofCollectionTask) findDuplicate(candidate ProofDelivery) (string, bool) {
	candidateMaterial := proofMaterialKey(candidate.Proof)
	for _, delivery := range t.Deliveries {
		if delivery.RequirementID == candidate.RequirementID && proofMaterialKey(delivery.Proof) == candidateMaterial {
			return delivery.ID, true
		}
	}
	return "", false
}

func proofMaterialKey(proof InclusionProof) string {
	data := make([]byte, 0, 32+len(proof.Transaction)+len(proof.Siblings)*32+16)
	data = append(data, proof.BlockHash[:]...)
	data = append(data, proof.Transaction...)
	data = binary.BigEndian.AppendUint64(data, proof.Index)
	data = binary.BigEndian.AppendUint64(data, proof.TransactionCount)
	for _, sibling := range proof.Siblings {
		data = append(data, sibling[:]...)
	}
	return string(data)
}

// Review re-evaluates every non-duplicate delivery against the supplied chain
// and appends an immutable decision. The latest decision is never inferred from
// stored historical results.
func (t *ProofCollectionTask) Review(chain *Chain, observedAt time.Time) (CollectionDecision, error) {
	if t == nil {
		return CollectionDecision{}, errors.New("collection task is nil")
	}
	if chain == nil {
		return CollectionDecision{}, errors.New("chain is nil")
	}
	if chain.anchor.BlockHash != t.TrustRootHash {
		return CollectionDecision{}, errors.New("review uses a different trust root")
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	previous := t.latestDecision()
	decision := CollectionDecision{
		Version: len(t.Decisions) + 1,
		State:   CollectionTaskIncomplete,
		Basis: CollectionBasis{
			TrustRootHash:         chain.anchor.BlockHash,
			CanonicalTip:          chain.canonicalTip,
			RequiredConfirmations: chain.policy.MinConfirmations,
			ObservedAt:            observedAt.UTC(),
		},
		Items: make([]CollectionItemStatus, 0, len(t.Requirements)),
	}
	if t.Abandoned {
		decision.State = CollectionTaskAbandoned
	}
	for _, requirement := range t.Requirements {
		item := t.reviewRequirement(chain, requirement)
		previousAccepted, hadPrevious := previousItem(previous, requirement.ID)
		basisChanged := previous.Version != 0 && previous.Basis.CanonicalTip != chain.canonicalTip
		if t.DefaultConfirmations == 0 && previous.Version != 0 {
			basisChanged = basisChanged || previous.Basis.RequiredConfirmations != chain.policy.MinConfirmations
		}
		if hadPrevious && previousAccepted.State == CollectionItemAccepted {
			if basisChanged || item.State != CollectionItemAccepted || previousAccepted.AcceptedID != item.AcceptedID {
				decision.RecheckIDs = appendUniqueString(decision.RecheckIDs, requirement.ID)
			}
		}
		if len(previous.RecheckIDs) > 0 && containsString(previous.RecheckIDs, requirement.ID) && item.State == CollectionItemAccepted {
			decision.RecheckIDs = appendUniqueString(decision.RecheckIDs, requirement.ID)
		}
		if item.State == CollectionItemNeedsReview {
			decision.RecheckIDs = appendUniqueString(decision.RecheckIDs, requirement.ID)
		}
		if item.State == CollectionItemPending || item.State == CollectionItemRejected || item.State == CollectionItemNeedsReview || item.State == CollectionItemConflicting {
			decision.MissingIDs = append(decision.MissingIDs, requirement.ID)
		}
		decision.Items = append(decision.Items, item)
	}
	if t.Abandoned {
		decision.State = CollectionTaskAbandoned
		decision.Reason = t.AbandonedReason
	} else if len(decision.MissingIDs) == 0 && !t.hasUnresolvedConflict(decision) {
		decision.State = CollectionTaskComplete
	}
	t.Decisions = append(t.Decisions, decision)
	return decision, nil
}

func (t *ProofCollectionTask) reviewRequirement(chain *Chain, requirement CollectionRequirement) CollectionItemStatus {
	item := CollectionItemStatus{
		RequirementID: requirement.ID,
		State:         CollectionItemPending,
		Reason:        "no non-duplicate proof has been received",
	}
	distinct := map[string]bool{}
	var accepted []ProofDelivery
	var rejected []ProofDelivery
	for _, delivery := range t.Deliveries {
		if delivery.RequirementID != requirement.ID || delivery.DuplicateOf != "" {
			continue
		}
		material := proofMaterialKey(delivery.Proof)
		if distinct[material] {
			continue
		}
		distinct[material] = true
		item.DeliveryIDs = append(item.DeliveryIDs, delivery.ID)
		result, _ := chain.VerifyInclusion(delivery.Proof, t.requiredConfirmationsFor(requirement))
		if result.Status == ResultAccepted {
			accepted = append(accepted, delivery)
		} else {
			rejected = append(rejected, delivery)
		}
	}
	switch {
	case len(accepted) > 1:
		item.State = CollectionItemConflicting
		item.Reason = "multiple distinct proofs currently satisfy the same requirement"
	case len(accepted) == 1:
		item.State = CollectionItemAccepted
		item.AcceptedID = accepted[0].ID
		item.Reason = "proof binds a canonical block with enough confirmations"
	case len(distinct) > 0:
		item.State = CollectionItemRejected
		item.Reason = rejected[0].Result.Reason
	}
	if item.State == CollectionItemRejected {
		for _, delivery := range t.Deliveries {
			if delivery.RequirementID == requirement.ID && delivery.Result.Status == ResultAccepted {
				item.State = CollectionItemNeedsReview
				item.Reason = "previously accepted evidence no longer satisfies the current chain or confirmation agreement"
				break
			}
		}
	}
	if t.Abandoned && item.State != CollectionItemAccepted {
		item.State = CollectionItemAbandoned
		if item.Reason == "" || item.Reason == "no non-duplicate proof has been received" {
			item.Reason = "task was abandoned before a valid proof was accepted"
		}
	}
	return item
}

func (t *ProofCollectionTask) requiredConfirmationsFor(requirement CollectionRequirement) uint64 {
	if requirement.RequiredConfirmations != 0 {
		return requirement.RequiredConfirmations
	}
	return t.DefaultConfirmations
}

func (t *ProofCollectionTask) hasUnresolvedConflict(decision CollectionDecision) bool {
	for _, item := range decision.Items {
		if item.State == CollectionItemConflicting {
			return true
		}
	}
	return false
}

func (t *ProofCollectionTask) latestDecision() CollectionDecision {
	if len(t.Decisions) == 0 {
		return CollectionDecision{}
	}
	return t.Decisions[len(t.Decisions)-1]
}

func previousItem(decision CollectionDecision, requirementID string) (CollectionItemStatus, bool) {
	for _, item := range decision.Items {
		if item.RequirementID == requirementID {
			return item, true
		}
	}
	return CollectionItemStatus{}, false
}

func (t *ProofCollectionTask) LatestDecision() (CollectionDecision, bool) {
	if len(t.Decisions) == 0 {
		return CollectionDecision{}, false
	}
	return t.latestDecision(), true
}

func (t *ProofCollectionTask) MissingRequirements(decision CollectionDecision) []CollectionRequirement {
	missing := make([]CollectionRequirement, 0, len(decision.MissingIDs))
	for _, id := range decision.MissingIDs {
		for _, requirement := range t.Requirements {
			if requirement.ID == id {
				missing = append(missing, requirement)
			}
		}
	}
	return missing
}

func (t *ProofCollectionTask) Abandon(reason string, at time.Time) (CollectionDecision, error) {
	if t == nil {
		return CollectionDecision{}, errors.New("collection task is nil")
	}
	if reason == "" {
		return CollectionDecision{}, errors.New("abandonment needs a reason")
	}
	t.Abandoned = true
	t.AbandonedReason = reason
	if at.IsZero() {
		at = time.Now().UTC()
	}
	t.AbandonedAt = at.UTC()
	decision := CollectionDecision{
		Version: len(t.Decisions) + 1,
		State:   CollectionTaskAbandoned,
		Basis: CollectionBasis{
			TrustRootHash: t.TrustRootHash,
			ObservedAt:    t.AbandonedAt,
		},
		Items:  make([]CollectionItemStatus, 0, len(t.Requirements)),
		Reason: reason,
	}
	if latest, ok := t.LatestDecision(); ok {
		decision.Basis = latest.Basis
		decision.Basis.ObservedAt = t.AbandonedAt
		decision.RecheckIDs = append(decision.RecheckIDs, latest.RecheckIDs...)
		for _, item := range latest.Items {
			if item.State != CollectionItemAccepted {
				item.State = CollectionItemAbandoned
				item.Reason = reason
			}
			if item.State != CollectionItemAccepted {
				decision.MissingIDs = append(decision.MissingIDs, item.RequirementID)
			}
			decision.Items = append(decision.Items, item)
		}
	} else {
		for _, requirement := range t.Requirements {
			decision.Items = append(decision.Items, CollectionItemStatus{
				RequirementID: requirement.ID,
				State:         CollectionItemAbandoned,
				Reason:        reason,
			})
			decision.MissingIDs = append(decision.MissingIDs, requirement.ID)
		}
	}
	t.Decisions = append(t.Decisions, decision)
	return decision, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func appendUniqueString(values []string, value string) []string {
	if containsString(values, value) {
		return values
	}
	return append(values, value)
}
