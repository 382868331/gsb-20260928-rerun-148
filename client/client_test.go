package client_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/light-client-research/client"
	"github.com/example/light-client-research/consensus"
	"github.com/example/light-client-research/store"
	"github.com/example/light-client-research/types"
)

type fixture struct {
	genesis types.BlockHeader
	main    []types.BlockHeader
	alt     []types.BlockHeader
	txs     []types.Transaction
	altTxs  []types.Transaction
}

func buildFixture(t *testing.T) fixture {
	t.Helper()
	genesis := consensus.MineGenesis(1)
	txs := []types.Transaction{{Payload: []byte("main-a")}, {Payload: []byte("main-b")}}
	altTxs := []types.Transaction{{Payload: []byte("alt-a")}}
	main1 := consensus.MineNext(genesis, types.TransactionMerkleRoot(txs[:1]), 2)
	main2 := consensus.MineNext(main1, types.TransactionMerkleRoot(txs), 3)
	main3 := consensus.MineNext(main2, types.MerkleRootFromHashes(nil), 4)
	alt1 := consensus.MineNext(genesis, types.TransactionMerkleRoot(altTxs), 5)
	alt2 := consensus.MineNext(alt1, types.MerkleRootFromHashes(nil), 6)
	alt3 := consensus.MineNext(alt2, types.MerkleRootFromHashes(nil), 7)
	alt4 := consensus.MineNext(alt3, types.MerkleRootFromHashes(nil), 8)
	return fixture{
		genesis: genesis,
		main:    []types.BlockHeader{main1, main2, main3},
		alt:     []types.BlockHeader{alt1, alt2, alt3, alt4},
		txs:     txs,
		altTxs:  altTxs,
	}
}

func newClient(t *testing.T, dir string, genesis types.BlockHeader) *client.LightClient {
	t.Helper()
	anchor := store.TrustAnchor{Height: genesis.Height, Hash: genesis.Hash(), Header: &genesis}
	data, err := store.InitializeJSON(dir, anchor)
	if err != nil {
		t.Fatal(err)
	}
	return client.New(consensus.NewTeachingRules(), data)
}

func TestValidatesHeaderAndInclusionProof(t *testing.T) {
	fx := buildFixture(t)
	lc := newClient(t, t.TempDir(), fx.genesis)
	for _, header := range fx.main {
		if _, err := lc.AddHeader(header); err != nil {
			t.Fatalf("add main header: %v", err)
		}
	}
	proof, err := types.BuildInclusionProof(fx.main[1].Hash(), fx.txs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := lc.VerifyInclusion(proof); err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}
	if err := lc.VerifyTransactionInclusion(fx.txs[1], proof); err != nil {
		t.Fatalf("transaction-bound proof rejected: %v", err)
	}
	wrongTx := types.Transaction{Payload: []byte("wrong")}
	if err := lc.VerifyTransactionInclusion(wrongTx, proof); err == nil {
		t.Fatal("transaction with mismatching hash accepted")
	}
}

func TestRejectsForgedProofPath(t *testing.T) {
	fx := buildFixture(t)
	lc := newClient(t, t.TempDir(), fx.genesis)
	for _, header := range fx.main {
		if _, err := lc.AddHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	proof, err := types.BuildInclusionProof(fx.main[1].Hash(), fx.txs, 1)
	if err != nil {
		t.Fatal(err)
	}
	proof.Siblings[0][0] ^= 0xff
	err = lc.VerifyInclusion(proof)
	if err == nil || !strings.Contains(err.Error(), "does not match header transaction root") {
		t.Fatalf("forged path accepted, err=%v", err)
	}
	proof.LeafIndex = 2
	if err := lc.VerifyInclusion(proof); err == nil {
		t.Fatal("out-of-range proof path accepted")
	}
}

func TestRejectsOldBranchAndMissingAncestor(t *testing.T) {
	fx := buildFixture(t)
	dir := t.TempDir()
	lc := newClient(t, dir, fx.genesis)
	if _, err := lc.AddHeader(fx.alt[0]); err != nil {
		t.Fatal(err)
	}
	proof, err := types.BuildInclusionProof(fx.alt[0].Hash(), fx.altTxs, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range fx.main {
		if _, err := lc.AddHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	head, _, _ := lc.AcceptedHead()
	if head != fx.main[2].Hash() {
		t.Fatalf("expected longer main chain, got %s", head)
	}
	err = lc.VerifyInclusion(proof)
	if err == nil || !strings.Contains(err.Error(), "not on the currently recognized chain") {
		t.Fatalf("old branch proof accepted, err=%v", err)
	}

	other := newClient(t, filepath.Join(t.TempDir(), "fresh"), fx.genesis)
	_, err = other.AddHeader(fx.main[1])
	if err == nil || !strings.Contains(err.Error(), "missing ancestor") {
		t.Fatalf("header with missing ancestor accepted, err=%v", err)
	}
}

func TestForkSwitchRevokesConfirmationAndPersistsBoundary(t *testing.T) {
	fx := buildFixture(t)
	dir := t.TempDir()
	lc := newClient(t, dir, fx.genesis)
	for _, header := range fx.main {
		if _, err := lc.AddHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	proof, err := types.BuildInclusionProof(fx.main[1].Hash(), fx.txs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := lc.VerifyInclusion(proof); err != nil {
		t.Fatal(err)
	}
	for _, header := range fx.alt {
		if _, err := lc.AddHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if !lc.Revoked(proof) {
		t.Fatal("old-chain confirmation was not revoked")
	}
	if err := lc.VerifyInclusion(proof); err == nil {
		t.Fatal("revoked proof accepted while it was still on an old branch")
	}
	main4 := consensus.MineNext(fx.main[2], types.MerkleRootFromHashes(nil), 9)
	main5 := consensus.MineNext(main4, types.MerkleRootFromHashes(nil), 10)
	for _, header := range []types.BlockHeader{main4, main5} {
		if _, err := lc.AddHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := lc.VerifyInclusion(proof); err != nil {
		t.Fatalf("proof on restored recognized chain rejected: %v", err)
	}
	head, _, _ := lc.AcceptedHead()
	if head != main5.Hash() {
		t.Fatalf("expected main chain to regain preference, got %s", head)
	}
	anchor := store.TrustAnchor{Height: fx.genesis.Height, Hash: fx.genesis.Hash(), Header: &fx.genesis}
	data, err := store.OpenJSON(dir, anchor, consensus.NewTeachingRules())
	if err != nil {
		t.Fatal(err)
	}
	reopened := client.New(consensus.NewTeachingRules(), data)
	reopenedHead, _, _ := reopened.AcceptedHead()
	if reopenedHead != main5.Hash() || reopened.Revoked(proof) {
		t.Fatal("reopened client expanded or lost its chain/result boundary")
	}
	if err := reopened.VerifyInclusion(proof); err != nil {
		t.Fatalf("restored proof should remain valid after reopening: %v", err)
	}

	differentGenesis := fx.genesis
	differentGenesis.Nonce++
	wrongAnchor := store.TrustAnchor{Height: 0, Hash: differentGenesis.Hash(), Header: &differentGenesis}
	if _, err := store.OpenJSON(dir, wrongAnchor, consensus.NewTeachingRules()); err == nil {
		t.Fatal("reopen accepted a different trust anchor")
	}
}

func TestRejectsInvalidConsensusHeader(t *testing.T) {
	fx := buildFixture(t)
	lc := newClient(t, t.TempDir(), fx.genesis)
	invalid := fx.main[0]
	invalid.Nonce++
	_, err := lc.AddHeader(invalid)
	if err == nil {
		t.Fatal("header with invalid proof of work and parent commitment accepted")
	}
}

func TestReopenRejectsTamperedAcceptedHead(t *testing.T) {
	fx := buildFixture(t)
	dir := t.TempDir()
	lc := newClient(t, dir, fx.genesis)
	for _, header := range append(append([]types.BlockHeader{}, fx.main...), fx.alt...) {
		if _, err := lc.AddHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "chain.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	data["accepted"] = fx.main[2].Hash().Hex()
	tampered, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	anchor := store.TrustAnchor{Height: fx.genesis.Height, Hash: fx.genesis.Hash(), Header: &fx.genesis}
	if _, err := store.OpenJSON(dir, anchor, consensus.NewTeachingRules()); err == nil {
		t.Fatal("tampered recognized head was accepted on reopen")
	}
}
