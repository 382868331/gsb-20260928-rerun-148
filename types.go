package lightclient

import (
	"encoding/binary"
	"fmt"
	"time"
)

// Hash is the uniform digest used by the teaching protocol.
type Hash [32]byte

// BlockHeader contains only fields a light client needs. Its serialization is
// independent of Go map ordering and JSON formatting.
type BlockHeader struct {
	Height           uint64 `json:"height"`
	ParentRoot       Hash   `json:"parentRoot"`
	TransactionsRoot Hash   `json:"transactionsRoot"`
	Timestamp        uint64 `json:"timestamp"`
	Difficulty       uint64 `json:"difficulty"`
	Nonce            uint64 `json:"nonce"`
}

type Block struct {
	Header       BlockHeader `json:"header"`
	Transactions [][]byte    `json:"transactions"`
}

// TrustRoot is an explicitly supplied starting point. It is never inferred from
// a block announcement and never replaced by a fork.
type TrustRoot struct {
	Name      string      `json:"name"`
	Height    uint64      `json:"height"`
	BlockHash Hash        `json:"blockHash"`
	Header    BlockHeader `json:"header"`
	CreatedAt time.Time   `json:"createdAt"`
}

type InclusionProof struct {
	Transaction      []byte `json:"transaction"`
	BlockHash        Hash   `json:"blockHash"`
	Index            uint64 `json:"index"`
	TransactionCount uint64 `json:"transactionCount"`
	Siblings         []Hash `json:"siblings"`
}

// BatchProofItem carries the per-transaction part of a joint proof. Each item
// is still bound to its own index inside the committed transaction list.
type BatchProofItem struct {
	Transaction []byte `json:"transaction"`
	Index       uint64 `json:"index"`
	Siblings    []Hash `json:"siblings"`
}

// BatchInclusionProof shares the block binding and transaction count across
// several transactions proved against the same block.
type BatchInclusionProof struct {
	BlockHash        Hash             `json:"blockHash"`
	TransactionCount uint64           `json:"transactionCount"`
	Items            []BatchProofItem `json:"items"`
}

// Proofs expands the joint proof into one fully bound proof per transaction.
func (b BatchInclusionProof) Proofs() []InclusionProof {
	proofs := make([]InclusionProof, 0, len(b.Items))
	for _, item := range b.Items {
		proofs = append(proofs, InclusionProof{
			Transaction:      item.Transaction,
			BlockHash:        b.BlockHash,
			Index:            item.Index,
			TransactionCount: b.TransactionCount,
			Siblings:         item.Siblings,
		})
	}
	return proofs
}

// ConfirmationPolicy is the chain-level default for how many confirmations a
// proof must have. Callers can still demand more per verification.
type ConfirmationPolicy struct {
	MinConfirmations uint64 `json:"minConfirmations"`
}

type ResultStatus string

const (
	ResultAccepted ResultStatus = "accepted"
	ResultRejected ResultStatus = "rejected"
)

// VerificationResult is an audit record. A previously accepted record is not a
// current credential: reopening the store forces fresh canonical-chain checks.
type VerificationResult struct {
	ID                    Hash         `json:"id"`
	Status                ResultStatus `json:"status"`
	ObservedAt            time.Time    `json:"observedAt"`
	TrustRootHash         Hash         `json:"trustRootHash"`
	CanonicalTip          Hash         `json:"canonicalTip"`
	BlockHash             Hash         `json:"blockHash"`
	BlockHeight           uint64       `json:"blockHeight"`
	TransactionHash       Hash         `json:"transactionHash"`
	TransactionIndex      uint64       `json:"transactionIndex"`
	Confirmations         uint64       `json:"confirmations"`
	RequiredConfirmations uint64       `json:"requiredConfirmations"`
	Reason                string       `json:"reason"`
}

func SerializeHeader(header BlockHeader) []byte {
	buf := make([]byte, 0, 32*2+8*4)
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], header.Height)
	buf = append(buf, raw[:]...)
	buf = append(buf, header.ParentRoot[:]...)
	buf = append(buf, header.TransactionsRoot[:]...)
	binary.BigEndian.PutUint64(raw[:], header.Timestamp)
	buf = append(buf, raw[:]...)
	binary.BigEndian.PutUint64(raw[:], header.Difficulty)
	buf = append(buf, raw[:]...)
	binary.BigEndian.PutUint64(raw[:], header.Nonce)
	buf = append(buf, raw[:]...)
	return buf
}

func BlockHeaderHash(header BlockHeader) Hash {
	return Hash(sha256Hash(append([]byte{domainBlockHeader}, SerializeHeader(header)...)))
}

func TransactionHash(transaction []byte) Hash {
	return Hash(sha256Hash(append([]byte{domainTransaction}, transaction...)))
}

func (h Hash) Hex() string {
	return fmt.Sprintf("%x", h[:])
}
