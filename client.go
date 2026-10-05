package lightclient

import (
	"errors"
	"fmt"
	"time"
)

const DefaultConfirmations uint64 = 2

type Chain struct {
	consensus       Consensus
	anchor          TrustRoot
	headers         map[Hash]BlockHeader
	parentByChild   map[Hash]Hash
	totalDifficulty map[Hash]uint64
	canonicalTip    Hash
	policy          ConfirmationPolicy
}

type ChainSnapshot struct {
	TrustRoot       TrustRoot
	CanonicalTip    Tip
	Headers         map[Hash]BlockHeader
	ParentByChild   map[Hash]Hash
	TotalDifficulty map[Hash]uint64
}

func NewChain(anchor TrustRoot, consensus Consensus) (*Chain, error) {
	if consensus == nil {
		consensus = TeachingProofOfWork{}
	}
	if err := validateAnchor(anchor); err != nil {
		return nil, err
	}
	if err := consensus.ValidateGenesis(anchor.Header); err != nil {
		return nil, err
	}
	hash := BlockHeaderHash(anchor.Header)
	if hash != anchor.BlockHash {
		return nil, errors.New("trust root hash does not match its header")
	}
	if anchor.Height != anchor.Header.Height {
		return nil, errors.New("trust root height does not match its header")
	}

	return &Chain{
		consensus:       consensus,
		anchor:          anchor,
		headers:         map[Hash]BlockHeader{hash: anchor.Header},
		parentByChild:   map[Hash]Hash{},
		totalDifficulty: map[Hash]uint64{hash: anchor.Header.Difficulty},
		canonicalTip:    hash,
		policy:          ConfirmationPolicy{MinConfirmations: DefaultConfirmations},
	}, nil
}

// SetConfirmationPolicy updates the default confirmation requirement. It only
// affects future verifications; previously accepted results stay historical
// evidence and must be re-checked to reflect the new policy.
func (c *Chain) SetConfirmationPolicy(policy ConfirmationPolicy) error {
	if policy.MinConfirmations == 0 {
		return errors.New("confirmation policy needs at least one confirmation")
	}
	c.policy = policy
	return nil
}

func (c *Chain) ConfirmationPolicy() ConfirmationPolicy {
	return c.policy
}

func (c *Chain) AddHeader(header BlockHeader) error {
	hash := BlockHeaderHash(header)
	if _, exists := c.headers[hash]; exists {
		return nil
	}
	parent, exists := c.headers[header.ParentRoot]
	if !exists {
		return ErrMissingAncestor
	}
	if err := c.consensus.ValidateChild(parent, header); err != nil {
		return err
	}
	c.headers[hash] = header
	c.parentByChild[hash] = header.ParentRoot
	c.totalDifficulty[hash] = c.totalDifficulty[header.ParentRoot] + header.Difficulty
	c.selectTip()
	return nil
}

// AddHeadersFromRoot connects a supplied segment only when every ancestor is
// present. A block beyond the anchor cannot bypass the trusted boundary.
func (c *Chain) AddHeadersFromRoot(root Hash, headers []BlockHeader) error {
	if root != c.anchor.BlockHash {
		return errors.New("header segment does not start at the immutable trust root")
	}
	if len(headers) == 0 {
		return nil
	}
	oldTip := c.canonicalTip
	oldHeaders := cloneHeaderSet(c)
	oldParents := make(map[Hash]Hash, len(c.parentByChild))
	oldWork := make(map[Hash]uint64, len(c.totalDifficulty))
	for child, parent := range c.parentByChild {
		oldParents[child] = parent
	}
	for hash, work := range c.totalDifficulty {
		oldWork[hash] = work
	}

	ordered := make([]BlockHeader, 0, len(headers))
	remaining := make(map[Hash]BlockHeader, len(headers))
	known := map[Hash]bool{root: true}
	for _, header := range headers {
		remaining[BlockHeaderHash(header)] = header
	}
	for len(remaining) > 0 {
		progress := false
		for hash, header := range remaining {
			parent, parentKnownInChain := c.headers[header.ParentRoot]
			if !known[header.ParentRoot] && !parentKnownInChain {
				continue
			}
			if !parentKnownInChain {
				parentFound := false
				for _, queued := range ordered {
					if BlockHeaderHash(queued) == header.ParentRoot {
						parent = queued
						parentFound = true
						break
					}
				}
				if !parentFound {
					continue
				}
			}
			if err := c.consensus.ValidateChild(parent, header); err != nil {
				c.rollbackHeaders(oldTip, oldHeaders, oldParents, oldWork)
				return err
			}
			ordered = append(ordered, header)
			known[hash] = true
			delete(remaining, hash)
			progress = true
		}
		if !progress {
			c.rollbackHeaders(oldTip, oldHeaders, oldParents, oldWork)
			return ErrMissingAncestor
		}
	}
	for _, header := range ordered {
		hash := BlockHeaderHash(header)
		if _, exists := c.headers[hash]; !exists {
			c.headers[hash] = header
			c.parentByChild[hash] = header.ParentRoot
			c.totalDifficulty[hash] = c.totalDifficulty[header.ParentRoot] + header.Difficulty
		}
	}
	c.selectTip()
	return nil
}

func cloneHeaderSet(c *Chain) map[Hash]BlockHeader {
	clone := make(map[Hash]BlockHeader, len(c.headers))
	for hash, header := range c.headers {
		clone[hash] = header
	}
	return clone
}

func (c *Chain) rollbackHeaders(tip Hash, headers map[Hash]BlockHeader, parents map[Hash]Hash, work map[Hash]uint64) {
	c.canonicalTip = tip
	c.headers = headers
	c.parentByChild = parents
	c.totalDifficulty = work
}

func (c *Chain) selectTip() {
	candidate := Tip{Hash: c.anchor.BlockHash, Height: c.anchor.Height, TotalDifficulty: c.anchor.Header.Difficulty}
	for hash, header := range c.headers {
		tip := Tip{Hash: hash, Height: header.Height, TotalDifficulty: c.totalDifficulty[hash]}
		if c.consensus.CompareTips(tip, candidate) > 0 && c.isAttached(hash) {
			candidate = tip
		}
	}
	c.canonicalTip = candidate.Hash
}

func (c *Chain) isAttached(hash Hash) bool {
	seen := map[Hash]bool{}
	for hash != c.anchor.BlockHash {
		if seen[hash] {
			return false
		}
		seen[hash] = true
		parent, exists := c.parentByChild[hash]
		if !exists {
			return false
		}
		hash = parent
	}
	return true
}

func (c *Chain) CanonicalTip() Tip {
	header := c.headers[c.canonicalTip]
	return Tip{Hash: c.canonicalTip, Height: header.Height, TotalDifficulty: c.totalDifficulty[c.canonicalTip]}
}

func (c *Chain) IsCanonical(hash Hash) bool {
	if _, exists := c.headers[hash]; !exists {
		return false
	}
	for tip := c.canonicalTip; tip != c.anchor.BlockHash; tip = c.parentByChild[tip] {
		if tip == hash {
			return true
		}
	}
	return hash == c.anchor.BlockHash
}

// CanonicalChain returns the complete current canonical chain from the
// immutable trust root to the selected tip. Review snapshots use it rather
// than trusting a tip hash alone.
func (c *Chain) CanonicalChain() []Hash {
	var reversed []Hash
	for hash := c.canonicalTip; ; hash = c.parentByChild[hash] {
		reversed = append(reversed, hash)
		if hash == c.anchor.BlockHash {
			break
		}
	}
	chain := make([]Hash, len(reversed))
	for i, hash := range reversed {
		chain[len(reversed)-1-i] = hash
	}
	return chain
}

func (c *Chain) Confirmations(hash Hash) (uint64, error) {
	if !c.IsCanonical(hash) {
		return 0, ErrNotCanonical
	}
	return c.headers[c.canonicalTip].Height - c.headers[hash].Height + 1, nil
}

func (c *Chain) VerifyInclusion(proof InclusionProof, requiredConfirmations uint64) (result VerificationResult, err error) {
	if requiredConfirmations == 0 {
		requiredConfirmations = c.policy.MinConfirmations
	}
	txHash := TransactionHash(proof.Transaction)
	now := time.Now().UTC()
	result = VerificationResult{
		Status:                ResultRejected,
		ObservedAt:            now,
		TrustRootHash:         c.anchor.BlockHash,
		CanonicalTip:          c.canonicalTip,
		TransactionHash:       txHash,
		TransactionIndex:      proof.Index,
		RequiredConfirmations: requiredConfirmations,
		Reason:                "unknown",
	}
	defer func() {
		result.ID = resultIdentityHash(result)
	}()

	headerHash := proof.BlockHash
	header, headerExists := c.headers[headerHash]
	if !headerExists {
		result.Reason = "proof does not name a recognized block"
		return result, ErrProofNotBound
	}
	if err := ValidateInclusionProof(header.TransactionsRoot, proof); err != nil {
		result.Reason = err.Error()
		return result, ErrProofNotBound
	}
	result.BlockHash = headerHash
	result.BlockHeight = header.Height
	if !c.IsCanonical(headerHash) {
		result.Reason = "proof is bound to a stale fork"
		return result, ErrStaleFork
	}
	confirmations, err := c.Confirmations(headerHash)
	if err != nil {
		result.Reason = err.Error()
		return result, err
	}
	result.Confirmations = confirmations
	if confirmations < requiredConfirmations {
		result.Reason = fmt.Sprintf("confirmations %d below required %d", confirmations, requiredConfirmations)
		return result, ErrInsufficientConfirmations
	}
	result.Status = ResultAccepted
	result.Reason = "proof binds a canonical block with enough confirmations"
	return result, nil
}

// VerifyInclusionBatch verifies several proofs against their named blocks.
// Header lookups are shared, but every transaction is checked against its own
// index, block and trust anchor. A missing or invalid item never counts as a
// success: the per-item results are returned together with ErrBatchIncomplete.
func (c *Chain) VerifyInclusionBatch(proofs []InclusionProof, requiredConfirmations uint64) ([]VerificationResult, error) {
	if len(proofs) == 0 {
		return nil, ErrEmptyBatch
	}
	results := make([]VerificationResult, len(proofs))
	var firstErr error
	for i, proof := range proofs {
		result, err := c.VerifyInclusion(proof, requiredConfirmations)
		results[i] = result
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return results, fmt.Errorf("%w: %w", ErrBatchIncomplete, firstErr)
	}
	return results, nil
}

// VerifyBatchInclusion verifies a joint proof that shares its block binding
// across several transactions of the same block.
func (c *Chain) VerifyBatchInclusion(batch BatchInclusionProof, requiredConfirmations uint64) ([]VerificationResult, error) {
	if len(batch.Items) == 0 {
		return nil, ErrEmptyBatch
	}
	return c.VerifyInclusionBatch(batch.Proofs(), requiredConfirmations)
}

// RecheckResult re-evaluates a historical result against the current chain
// state. A previously accepted result is only evidence of a past observation;
// after a reorg or a policy change it must be re-checked to mean anything now.
func (c *Chain) RecheckResult(past VerificationResult, requiredConfirmations uint64) (VerificationResult, error) {
	if requiredConfirmations == 0 {
		requiredConfirmations = c.policy.MinConfirmations
	}
	fresh := VerificationResult{
		Status:                ResultRejected,
		ObservedAt:            time.Now().UTC(),
		TrustRootHash:         c.anchor.BlockHash,
		CanonicalTip:          c.canonicalTip,
		BlockHash:             past.BlockHash,
		TransactionHash:       past.TransactionHash,
		TransactionIndex:      past.TransactionIndex,
		RequiredConfirmations: requiredConfirmations,
	}
	header, exists := c.headers[past.BlockHash]
	if !exists {
		fresh.Reason = "block is not recognized by the current chain"
		fresh.ID = resultIdentityHash(fresh)
		return fresh, ErrProofNotBound
	}
	fresh.BlockHeight = header.Height
	if !c.IsCanonical(past.BlockHash) {
		fresh.Reason = "block left the canonical chain after a reorg"
		fresh.ID = resultIdentityHash(fresh)
		return fresh, ErrStaleFork
	}
	confirmations, err := c.Confirmations(past.BlockHash)
	if err != nil {
		fresh.Reason = err.Error()
		fresh.ID = resultIdentityHash(fresh)
		return fresh, err
	}
	fresh.Confirmations = confirmations
	if confirmations < requiredConfirmations {
		fresh.Reason = fmt.Sprintf("confirmations %d below required %d", confirmations, requiredConfirmations)
		fresh.ID = resultIdentityHash(fresh)
		return fresh, ErrInsufficientConfirmations
	}
	fresh.Status = ResultAccepted
	fresh.Reason = "historical result still holds on the current canonical chain"
	fresh.ID = resultIdentityHash(fresh)
	return fresh, nil
}

func validateAnchor(anchor TrustRoot) error {
	if anchor.Name == "" {
		return errors.New("trust root needs a human-readable name")
	}
	if anchor.CreatedAt.IsZero() {
		return errors.New("trust root needs a creation timestamp")
	}
	if anchor.BlockHash == (Hash{}) {
		return errors.New("trust root block hash is zero")
	}
	return nil
}

var (
	ErrMissingAncestor           = errors.New("missing ancestor header")
	ErrNotCanonical              = errors.New("block is not on the current canonical chain")
	ErrProofNotBound             = errors.New("proof is not bound to a recognized block")
	ErrStaleFork                 = errors.New("proof binds an old branch")
	ErrInsufficientConfirmations = errors.New("not enough confirmations")
	ErrEmptyBatch                = errors.New("batch verification needs at least one proof")
	ErrBatchIncomplete           = errors.New("one or more proofs in the batch failed verification")
)
