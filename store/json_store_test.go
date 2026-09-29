package store_test

import (
	"testing"

	"github.com/example/light-client-research/client"
	"github.com/example/light-client-research/consensus"
	"github.com/example/light-client-research/store"
	"github.com/example/light-client-research/types"
)

func TestNonGenesisAnchorHeaderWorkStartsAtZero(t *testing.T) {
	genesis := consensus.MineGenesis(1)
	anchorHeader := consensus.MineNext(genesis, types.MerkleRootFromHashes(nil), 2)
	dir := t.TempDir()
	anchor := store.TrustAnchor{Height: anchorHeader.Height, Hash: anchorHeader.Hash(), Header: &anchorHeader}
	data, err := store.InitializeJSON(dir, anchor)
	if err != nil {
		t.Fatal(err)
	}
	lc := client.New(consensus.NewTeachingRules(), data)
	next := consensus.MineNext(anchorHeader, types.MerkleRootFromHashes(nil), 3)
	if _, err := lc.AddHeader(next); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.OpenJSON(dir, anchor, consensus.NewTeachingRules())
	if err != nil {
		t.Fatal(err)
	}
	reopenedClient := client.New(consensus.NewTeachingRules(), reopened)
	head, _, ok := reopenedClient.AcceptedHead()
	if !ok || head != next.Hash() {
		t.Fatalf("accepted head after reopen = %s, want %s", head, next.Hash())
	}
}
