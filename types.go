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

type ResultStatus string

const (
	ResultAccepted ResultStatus = "accepted"
	ResultRejected ResultStatus = "rejected"
)

// VerificationResult is an audit record. A previously accepted record is not a
// current credential: reopening the store forces fresh canonical-chain checks.
type VerificationResult struct {
	ID               Hash         `json:"id"`
	Status           ResultStatus `json:"status"`
	ObservedAt       time.Time    `json:"observedAt"`
	TrustRootHash    Hash         `json:"trustRootHash"`
	CanonicalTip     Hash         `json:"canonicalTip"`
	BlockHash        Hash         `json:"blockHash"`
	BlockHeight      uint64       `json:"blockHeight"`
	TransactionHash  Hash         `json:"transactionHash"`
	TransactionIndex uint64       `json:"transactionIndex"`
	Confirmations    uint64       `json:"confirmations"`
	Reason           string       `json:"reason"`
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
