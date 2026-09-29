package types_test

import (
	"strings"
	"testing"

	"github.com/example/light-client-research/types"
)

func TestMerkleProofRejectsMalformedPath(t *testing.T) {
	txs := []types.Transaction{{Payload: []byte("a")}, {Payload: []byte("b")}}
	proof, err := types.BuildInclusionProof(types.Hash{}, txs, 0)
	if err != nil {
		t.Fatal(err)
	}
	proof.LeafIndex = 4
	if _, err := proof.Root(); err == nil {
		t.Fatal("leaf index outside tree was accepted")
	}
	proof.LeafIndex = 0
	proof.Siblings = nil
	if _, err := proof.Root(); err == nil || !strings.Contains(err.Error(), "binary merkle tree") {
		t.Fatalf("short path was accepted: %v", err)
	}
}

func TestMerkleRootMatchesProof(t *testing.T) {
	txs := []types.Transaction{{Payload: []byte("a")}, {Payload: []byte("b")}, {Payload: []byte("c")}}
	proof, err := types.BuildInclusionProof(types.Hash{}, txs, 2)
	if err != nil {
		t.Fatal(err)
	}
	root, err := proof.Root()
	if err != nil {
		t.Fatal(err)
	}
	if root != types.TransactionMerkleRoot(txs) {
		t.Fatal("proof root differs from transaction merkle root")
	}
}
