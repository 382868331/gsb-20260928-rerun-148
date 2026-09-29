package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/example/light-client-research/consensus"
	"github.com/example/light-client-research/store"
	"github.com/example/light-client-research/types"
)

const (
	ResultValid   = "valid"
	ResultRevoked = "revoked-by-reorg"
)

type LightClient struct {
	rules consensus.Rules
	data  *store.JSONStore
}

func New(rules consensus.Rules, data *store.JSONStore) *LightClient {
	return &LightClient{rules: rules, data: data}
}

type AddHeaderResult struct {
	Hash         types.Hash
	Reorganized  bool
	PreviousHead types.Hash
	AcceptedHead types.Hash
}

func (client *LightClient) AddHeader(header types.BlockHeader) (AddHeaderResult, error) {
	hash := types.DigestBlock(header)
	if _, exists := client.data.Header(hash); exists {
		return AddHeaderResult{Hash: hash, AcceptedHead: client.data.Accepted()}, nil
	}
	anchor := client.data.Anchor()
	trustedAnchorHeader := hash == anchor.Hash && header.Height == anchor.Height
	parent, hasParent := client.data.Header(header.ParentHash)
	if !trustedAnchorHeader {
		if !hasParent {
			return AddHeaderResult{}, fmt.Errorf("missing ancestor or parent %s; headers cannot connect to the trust anchor", header.ParentHash)
		}
		if err := client.rules.ValidateHeader(header, parent, true); err != nil {
			return AddHeaderResult{}, err
		}
	}
	if anchor.Header != nil && trustedAnchorHeader && header != *anchor.Header {
		return AddHeaderResult{}, fmt.Errorf("submitted trust anchor header differs from immutable anchor")
	}
	if header.Height < anchor.Height {
		return AddHeaderResult{}, fmt.Errorf("header height %d is below immutable trust anchor height %d", header.Height, anchor.Height)
	}
	var parentWork *big.Int
	if trustedAnchorHeader {
		parentWork = big.NewInt(0)
	} else {
		var hasWork bool
		parentWork, hasWork = client.data.Work(header.ParentHash)
		if !hasWork {
			return AddHeaderResult{}, fmt.Errorf("missing accumulated work for parent %s", header.ParentHash)
		}
	}
	work := parentWork
	if !trustedAnchorHeader {
		work = client.rules.ChildWork(parentWork)
	}
	client.data.PutHeader(header, work)
	result := AddHeaderResult{Hash: hash, PreviousHead: client.data.Accepted(), AcceptedHead: client.data.Accepted()}
	_, hasCurrent := client.data.Header(client.data.Accepted())
	currentWork, _ := client.data.Work(client.data.Accepted())
	preferred := !hasCurrent || client.rules.Preferred(work, currentWork, hash, client.data.Accepted())
	if preferred {
		result.Reorganized = hasCurrent && client.data.Accepted() != hash
		client.data.SetAccepted(hash)
	}
	result.AcceptedHead = client.data.Accepted()
	if result.Reorganized {
		client.revokeResultsOnOldChain(result.PreviousHead)
	}
	return result, client.data.Save()
}

func (client *LightClient) VerifyInclusion(proof types.InclusionProof) error {
	key := resultKey(proof)
	if record, exists := client.data.Result(key); exists {
		if record.Status == ResultValid {
			return nil
		}
	}
	block, ok := client.data.Header(proof.BlockHash)
	if !ok {
		return fmt.Errorf("proof block %s is unknown; fetch its complete header ancestry first", proof.BlockHash)
	}
	if proof.BlockHash != client.data.Accepted() && !client.ancestorOf(proof.BlockHash, client.data.Accepted()) {
		return fmt.Errorf("proof is bound to %s, which is not on the currently recognized chain", proof.BlockHash)
	}
	root, err := proof.Root()
	if err != nil {
		return err
	}
	if root != block.TxRoot {
		return fmt.Errorf("merkle proof root %s does not match header transaction root %s", root, block.TxRoot)
	}
	client.data.PutResult(key, store.VerificationRecord{Proof: proof, Status: ResultValid})
	return client.data.Save()
}

func (client *LightClient) VerifyTransactionInclusion(tx types.Transaction, proof types.InclusionProof) error {
	if types.DigestTransaction(tx) != proof.TxHash {
		return fmt.Errorf("transaction hash does not match the inclusion proof")
	}
	return client.VerifyInclusion(proof)
}

func (client *LightClient) Revoked(proof types.InclusionProof) bool {
	record, ok := client.data.Result(resultKey(proof))
	return ok && record.Status == ResultRevoked
}

func (client *LightClient) AcceptedHead() (types.Hash, types.BlockHeader, bool) {
	hash := client.data.Accepted()
	header, ok := client.data.Header(hash)
	return hash, header, ok
}

func (client *LightClient) Header(hash types.Hash) (types.BlockHeader, bool) {
	return client.data.Header(hash)
}

func (client *LightClient) ancestorOf(ancestor, descendant types.Hash) bool {
	anchorHeight := client.data.Anchor().Height
	current := descendant
	for {
		if current == ancestor {
			return true
		}
		header, ok := client.data.Header(current)
		if !ok || header.Height < anchorHeight {
			return false
		}
		current = header.ParentHash
	}
}

func (client *LightClient) revokeResultsOnOldChain(oldTip types.Hash) {
	newTip := client.data.Accepted()
	for key, record := range client.data.Results() {
		if record.Status != ResultValid {
			continue
		}
		blockHash := record.Proof.BlockHash
		if blockHash != oldTip && !client.ancestorOf(blockHash, oldTip) {
			continue
		}
		if blockHash == newTip || client.ancestorOf(blockHash, newTip) {
			continue
		}
		record.Status = ResultRevoked
		record.Reason = "recognized chain switched to a preferred branch"
		client.data.PutResult(key, record)
	}
}

func resultKey(proof types.InclusionProof) string {
	root, err := proof.Root()
	if err != nil {
		root = sha256.Sum256([]byte("invalid-proof:" + proof.BlockHash.Hex()))
	}
	var data bytes.Buffer
	data.Write(proof.BlockHash[:])
	data.Write(root[:])
	data.Write(proof.TxHash[:])
	var index [8]byte
	binary.BigEndian.PutUint64(index[:], proof.LeafIndex)
	data.Write(index[:])
	for _, sibling := range proof.Siblings {
		data.Write(sibling[:])
	}
	keyHash := sha256.Sum256(data.Bytes())
	return hex.EncodeToString(keyHash[:])
}
