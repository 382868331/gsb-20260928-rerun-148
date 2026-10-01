package lightclient

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// JointProofItem is one slot of an on-demand multi-transaction packet. The
// item keeps the request's own position and block: a duplicated transaction
// cannot silently satisfy a different request, and a missing item stays a
// rejected slot instead of being dropped.
type JointProofItem struct {
	Request ProofRequest `json:"request"`

	// ClaimedTransactionsRoot is packet metadata supplied by the data source.
	// Verification ignores it and uses the root of the locally verified header.
	ClaimedTransactionsRoot Hash `json:"claimedTransactionsRoot"`

	Found            bool   `json:"found"`
	MissingReason    string `json:"missingReason,omitempty"`
	TransactionCount uint64 `json:"transactionCount"`
	Siblings         []Hash `json:"siblings"`
}

// JointProofPacket shares the expensive Merkle levels once while keeping every
// transaction bound to its own location, block, and named trust root.
type JointProofPacket struct {
	ID            Hash             `json:"id"`
	TrustRootHash Hash             `json:"trustRootHash"`
	ObservedAt    time.Time        `json:"observedAt"`
	Items         []JointProofItem `json:"items"`
}

// BlockSource supplies complete, manually built or fixture blocks on demand.
type BlockSource interface {
	BlockByHash(hash Hash) (Block, bool)
}

// BuildJointProofPacket gathers proofs for several requests. Blocks and Merkle
// levels are fetched once per block, while each resulting item remains
// independently bound. An unsatisfiable request becomes a missing item rather
// than stopping the whole packet.
func BuildJointProofPacket(trustRoot Hash, requests []ProofRequest, source BlockSource) (*JointProofPacket, error) {
	if source == nil {
		return nil, errors.New("joint proof needs a block source")
	}
	packet := &JointProofPacket{
		TrustRootHash: trustRoot,
		ObservedAt:    time.Now().UTC(),
		Items:         make([]JointProofItem, 0, len(requests)),
	}
	levelsByBlock := map[Hash][][]Hash{}
	blocks := map[Hash]Block{}

	for _, request := range requests {
		item := JointProofItem{Request: cloneProofRequest(request), Siblings: []Hash{}}
		if request.TrustRootHash != trustRoot {
			item.MissingReason = "request names a different trust root"
			packet.Items = append(packet.Items, item)
			continue
		}
		block, cached := blocks[request.BlockHash]
		if !cached {
			var found bool
			block, found = source.BlockByHash(request.BlockHash)
			blocks[request.BlockHash] = block
			if !found {
				item.MissingReason = "requested block is unavailable"
				packet.Items = append(packet.Items, item)
				continue
			}
		}
		if BlockHeaderHash(block.Header) != request.BlockHash {
			item.MissingReason = "requested block is unavailable"
			packet.Items = append(packet.Items, item)
			continue
		}
		item.ClaimedTransactionsRoot = block.Header.TransactionsRoot
		if request.Index >= uint64(len(block.Transactions)) {
			item.MissingReason = fmt.Sprintf("transaction index %d is outside the block of %d transactions", request.Index, len(block.Transactions))
			packet.Items = append(packet.Items, item)
			continue
		}
		levels, ok := levelsByBlock[request.BlockHash]
		if !ok {
			if len(block.Transactions) == 0 {
				item.MissingReason = "block has no transactions"
				packet.Items = append(packet.Items, item)
				continue
			}
			levels = buildMerkleLevels(block.Transactions)
			levelsByBlock[request.BlockHash] = levels
		}
		item.Found = true
		item.TransactionCount = uint64(len(block.Transactions))
		item.Siblings = siblingsForIndex(levels, request.Index, uint64(len(block.Transactions)))
		packet.Items = append(packet.Items, item)
	}
	packet.ID = JointProofPacketIdentity(packet)
	return packet, nil
}
func cloneProofRequest(request ProofRequest) ProofRequest {
	return ProofRequest{
		Transaction:   append([]byte(nil), request.Transaction...),
		BlockHash:     request.BlockHash,
		Index:         request.Index,
		TrustRootHash: request.TrustRootHash,
	}
}

// ItemResult pairs a request slot with its fresh, current-chain verdict.
type ItemResult struct {
	Request ProofRequest       `json:"request"`
	Result  VerificationResult `json:"result"`
	Err     error              `json:"-"`
}

// BatchVerificationResult is never an aggregate credential: inspect every
// item. AllAccepted is true only when every slot passed on the current chain.
type BatchVerificationResult struct {
	Results []ItemResult `json:"results"`
}

func (b BatchVerificationResult) AcceptedCount() int {
	count := 0
	for _, item := range b.Results {
		if item.Result.Status == ResultAccepted {
			count++
		}
	}
	return count
}

func (b BatchVerificationResult) AllAccepted() bool {
	if len(b.Results) == 0 {
		return false
	}
	return b.AcceptedCount() == len(b.Results)
}

// VerifyJointProof re-evaluates every item against the current canonical chain.
// Packet roots, packet IDs, and historical conclusions are never trusted.
func (c *Chain) VerifyJointProof(packet *JointProofPacket, requiredConfirmations uint64) (BatchVerificationResult, error) {
	if packet == nil {
		return BatchVerificationResult{}, errors.New("joint proof packet is nil")
	}
	batch := BatchVerificationResult{Results: make([]ItemResult, 0, len(packet.Items))}
	var failures []error
	for _, item := range packet.Items {
		request := cloneProofRequest(item.Request)
		if packet.TrustRootHash != c.anchor.BlockHash || request.TrustRootHash != c.anchor.BlockHash {
			result := c.rejectedItemResult(request, "proof is not bound to the configured trust root")
			batch.Results = append(batch.Results, ItemResult{Request: request, Result: result, Err: ErrProofNotBound})
			failures = append(failures, ErrProofNotBound)
			continue
		}
		if !item.Found {
			reason := item.MissingReason
			if reason == "" {
				reason = "joint proof is missing a requested item"
			}
			result := c.rejectedItemResult(request, reason)
			batch.Results = append(batch.Results, ItemResult{Request: request, Result: result, Err: ErrProofMissing})
			failures = append(failures, ErrProofMissing)
			continue
		}
		proof := InclusionProof{
			Transaction:      append([]byte(nil), request.Transaction...),
			BlockHash:        request.BlockHash,
			Index:            request.Index,
			TransactionCount: item.TransactionCount,
			Siblings:         append([]Hash(nil), item.Siblings...),
		}
		result, err := c.VerifyInclusion(proof, requiredConfirmations)
		batch.Results = append(batch.Results, ItemResult{Request: request, Result: result, Err: err})
		if err != nil {
			failures = append(failures, err)
		}
	}
	return batch, errors.Join(failures...)
}

func (c *Chain) rejectedItemResult(request ProofRequest, reason string) VerificationResult {
	now := time.Now().UTC()
	txHash := TransactionHash(request.Transaction)
	result := VerificationResult{
		Status:           ResultRejected,
		ObservedAt:       now,
		TrustRootHash:    c.anchor.BlockHash,
		CanonicalTip:     c.canonicalTip,
		BlockHash:        request.BlockHash,
		TransactionHash:  txHash,
		TransactionIndex: request.Index,
		Reason:           reason,
	}
	result.ID = resultIdentityHash(result)
	return result
}

// JointProofPacketIdentity commits the packet to one trust root and an ordered
// list of requested locations. It is used only for storage de-duplication.
func JointProofPacketIdentity(packet *JointProofPacket) Hash {
	var data []byte
	data = append(data, packet.TrustRootHash[:]...)
	data = binary.BigEndian.AppendUint64(data, uint64(packet.ObservedAt.UnixNano()))
	for _, item := range packet.Items {
		data = append(data, item.Request.TrustRootHash[:]...)
		data = append(data, item.Request.BlockHash[:]...)
		data = binary.BigEndian.AppendUint64(data, item.Request.Index)
		data = append(data, item.Request.Transaction...)
	}
	return Hash(sha256Hash(data))
}
