package lightclient

import "fmt"

// MineGenesis searches a nonce for the fixed teaching difficulty. This is test
// and fixture code; the library itself does not contact a network.
func MineGenesis(timestamp uint64, transactions [][]byte) (Block, TrustRoot, error) {
	header := BlockHeader{
		Height:           0,
		TransactionsRoot: MerkleRoot(transactions),
		Timestamp:        timestamp,
		Difficulty:       TeachingDifficulty,
	}
	var err error
	header.Nonce, err = findNonce(header)
	if err != nil {
		return Block{}, TrustRoot{}, err
	}
	block := Block{Header: header, Transactions: transactions}
	anchor := TrustRoot{
		Name:      "manual teaching genesis",
		Height:    0,
		BlockHash: BlockHeaderHash(header),
		Header:    header,
	}
	return block, anchor, nil
}

func MineChild(parent BlockHeader, timestamp uint64, transactions [][]byte) (Block, error) {
	header := BlockHeader{
		Height:           parent.Height + 1,
		ParentRoot:       BlockHeaderHash(parent),
		TransactionsRoot: MerkleRoot(transactions),
		Timestamp:        timestamp,
		Difficulty:       TeachingDifficulty,
	}
	nonce, err := findNonce(header)
	if err != nil {
		return Block{}, err
	}
	header.Nonce = nonce
	return Block{Header: header, Transactions: transactions}, nil
}

func findNonce(template BlockHeader) (uint64, error) {
	for nonce := uint64(0); nonce < ^uint64(0); nonce++ {
		template.Nonce = nonce
		if meetsDifficulty(BlockHeaderHash(template), template.Difficulty) {
			return nonce, nil
		}
	}
	return 0, fmt.Errorf("no nonce found for height %d", template.Height)
}
