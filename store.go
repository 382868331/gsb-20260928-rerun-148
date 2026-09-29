package lightclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	trustFile   = "trust-root.json"
	headersFile = "header-chain.json"
	resultsFile = "verification-results.json"
)

type FileStore struct {
	dir string
}

type storedHeader struct {
	Header BlockHeader `json:"header"`
	Hash   Hash        `json:"hash"`
}

type headerStore struct {
	CanonicalTip Hash           `json:"canonicalTip"`
	Headers      []storedHeader `json:"headers"`
}

type resultStore struct {
	Results []VerificationResult `json:"results"`
}

func NewFileStore(dir string) *FileStore {
	return &FileStore{dir: dir}
}

// SaveTrustRoot writes the trust boundary exactly once. Results and headers are
// never allowed to replace it.
func (s *FileStore) SaveTrustRoot(anchor TrustRoot) error {
	path := filepath.Join(s.dir, trustFile)
	if _, err := os.Stat(path); err == nil {
		var existing TrustRoot
		if readErr := readJSON(path, &existing); readErr != nil {
			return fmt.Errorf("existing trust root is unreadable: %w", readErr)
		}
		if !sameTrustRoot(existing, anchor) {
			return errors.New("cannot replace an existing trust root")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeJSON(path, anchor)
}

func (s *FileStore) LoadTrustRoot() (TrustRoot, error) {
	var anchor TrustRoot
	if err := readJSON(filepath.Join(s.dir, trustFile), &anchor); err != nil {
		return TrustRoot{}, err
	}
	return anchor, nil
}

func (s *FileStore) SaveChain(chain *Chain) error {
	if chain == nil {
		return errors.New("chain is nil")
	}
	anchor, err := s.LoadTrustRoot()
	if err != nil {
		return err
	}
	if !sameTrustRoot(anchor, chain.anchor) {
		return errors.New("refusing to save a chain with a different trust root")
	}

	stored := headerStore{CanonicalTip: chain.canonicalTip}
	for hash, header := range chain.headers {
		stored.Headers = append(stored.Headers, storedHeader{Hash: hash, Header: header})
	}
	return writeJSON(filepath.Join(s.dir, headersFile), stored)
}

func (s *FileStore) LoadChain(consensus Consensus) (*Chain, error) {
	anchor, err := s.LoadTrustRoot()
	if err != nil {
		return nil, err
	}
	var stored headerStore
	if err := readJSON(filepath.Join(s.dir, headersFile), &stored); err != nil {
		return nil, err
	}
	chain, err := NewChain(anchor, consensus)
	if err != nil {
		return nil, err
	}

	pending := make([]BlockHeader, 0, len(stored.Headers))
	for _, item := range stored.Headers {
		if BlockHeaderHash(item.Header) != item.Hash {
			return nil, fmt.Errorf("stored header %x has been modified", item.Hash)
		}
		if item.Hash == anchor.BlockHash {
			continue
		}
		pending = append(pending, item.Header)
	}
	if err := chain.AddHeadersFromRoot(anchor.BlockHash, pending); err != nil {
		return nil, err
	}
	if chain.canonicalTip != stored.CanonicalTip {
		return nil, errors.New("stored canonical tip does not follow the configured chain selection rule")
	}
	return chain, nil
}

func (s *FileStore) AppendResult(chain *Chain, result VerificationResult) error {
	if chain == nil {
		return errors.New("chain is nil")
	}
	if result.TrustRootHash != chain.anchor.BlockHash {
		return errors.New("result is not bound to the stored trust root")
	}
	path := filepath.Join(s.dir, resultsFile)
	var stored resultStore
	if err := readJSON(path, &stored); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, existing := range stored.Results {
		if existing.ID == result.ID {
			return nil
		}
	}
	stored.Results = append(stored.Results, result)
	return writeJSON(path, stored)
}

func (s *FileStore) LoadResults() ([]VerificationResult, error) {
	var stored resultStore
	if err := readJSON(filepath.Join(s.dir, resultsFile), &stored); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return stored.Results, nil
}

func sameTrustRoot(left, right TrustRoot) bool {
	return left.Name == right.Name &&
		left.Height == right.Height &&
		left.BlockHash == right.BlockHash &&
		bytes.Equal(left.Header.ParentRoot[:], right.Header.ParentRoot[:]) &&
		BlockHeaderHash(left.Header) == BlockHeaderHash(right.Header) &&
		left.CreatedAt.Equal(right.CreatedAt)
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
