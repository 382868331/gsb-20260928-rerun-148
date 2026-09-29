package store

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"

	"github.com/example/light-client-research/types"
)

type TrustAnchor struct {
	Height uint64             `json:"height"`
	Hash   types.Hash         `json:"hash"`
	Header *types.BlockHeader `json:"header,omitempty"`
}

type storedHeader struct {
	Header types.BlockHeader `json:"header"`
	Work   string            `json:"work"`
}

type VerificationRecord struct {
	Proof  types.InclusionProof `json:"proof"`
	Status string               `json:"status"`
	Reason string               `json:"reason,omitempty"`
}

const (
	ResultValid   = "valid"
	ResultRevoked = "revoked-by-reorg"
)

type HeaderValidator interface {
	ValidateHeader(header types.BlockHeader, parent types.BlockHeader, hasParent bool) error
	ChildWork(parentWork *big.Int) *big.Int
	Preferred(candidateWork, currentWork *big.Int, candidateHash, currentHash types.Hash) bool
}

type chainFile struct {
	Headers  map[string]storedHeader `json:"headers"`
	Children map[string][]types.Hash `json:"children"`
	Tips     []types.Hash            `json:"tips"`
	Accepted types.Hash              `json:"accepted"`
}

type resultsFile struct {
	Results map[string]VerificationRecord `json:"results"`
}

type JSONStore struct {
	dir      string
	anchor   TrustAnchor
	headers  map[types.Hash]types.BlockHeader
	work     map[types.Hash]*big.Int
	children map[types.Hash][]types.Hash
	tips     map[types.Hash]struct{}
	accepted types.Hash
	results  map[string]VerificationRecord
}

func InitializeJSON(dir string, anchor TrustAnchor) (*JSONStore, error) {
	if anchor.Height == 0 && anchor.Header == nil {
		return nil, fmt.Errorf("trust anchor must include the genesis header or a nonzero height")
	}
	if anchor.Header != nil {
		hash := types.DigestBlock(*anchor.Header)
		if hash != anchor.Hash || anchor.Header.Height != anchor.Height {
			return nil, fmt.Errorf("trust anchor header does not match its declared height and hash")
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for _, name := range []string{"anchor.json", "chain.json", "results.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return nil, fmt.Errorf("light-client data already exists in %s", dir)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	s := &JSONStore{
		dir:      dir,
		anchor:   anchor,
		headers:  make(map[types.Hash]types.BlockHeader),
		work:     make(map[types.Hash]*big.Int),
		children: make(map[types.Hash][]types.Hash),
		tips:     make(map[types.Hash]struct{}),
		results:  make(map[string]VerificationRecord),
	}
	if anchor.Header != nil {
		hash := anchor.Hash
		s.headers[hash] = *anchor.Header
		s.work[hash] = big.NewInt(0)
		s.tips[hash] = struct{}{}
		s.accepted = hash
	}
	if err := s.Save(); err != nil {
		return nil, err
	}
	return s, nil
}

func OpenJSON(dir string, expectedAnchor TrustAnchor, rules HeaderValidator) (*JSONStore, error) {
	anchorBytes, err := os.ReadFile(filepath.Join(dir, "anchor.json"))
	if err != nil {
		return nil, err
	}
	var savedAnchor TrustAnchor
	if err := json.Unmarshal(anchorBytes, &savedAnchor); err != nil {
		return nil, err
	}
	if !anchorsEqual(savedAnchor, expectedAnchor) {
		return nil, fmt.Errorf("provided trust anchor does not match immutable stored anchor")
	}
	dataBytes, err := os.ReadFile(filepath.Join(dir, "chain.json"))
	if err != nil {
		return nil, err
	}
	var data chainFile
	if err := json.Unmarshal(dataBytes, &data); err != nil {
		return nil, err
	}
	s := &JSONStore{
		dir:      dir,
		anchor:   savedAnchor,
		headers:  make(map[types.Hash]types.BlockHeader),
		work:     make(map[types.Hash]*big.Int),
		children: make(map[types.Hash][]types.Hash),
		tips:     make(map[types.Hash]struct{}),
		results:  make(map[string]VerificationRecord),
	}
	for hash, item := range data.Headers {
		parsedHash, err := types.HashFromHex(hash)
		if err != nil {
			return nil, err
		}
		s.headers[parsedHash] = item.Header
		work, ok := new(big.Int).SetString(item.Work, 10)
		if !ok {
			return nil, fmt.Errorf("invalid stored work for %s", parsedHash)
		}
		s.work[parsedHash] = work
	}
	for parent, children := range data.Children {
		parentHash, err := types.HashFromHex(parent)
		if err != nil {
			return nil, err
		}
		s.children[parentHash] = children
	}
	for _, tip := range data.Tips {
		s.tips[tip] = struct{}{}
	}
	s.accepted = data.Accepted
	resultBytes, err := os.ReadFile(filepath.Join(dir, "results.json"))
	if err != nil {
		return nil, err
	}
	var persistedResults resultsFile
	if err := json.Unmarshal(resultBytes, &persistedResults); err != nil {
		return nil, err
	}
	s.results = persistedResults.Results
	if s.results == nil {
		s.results = make(map[string]VerificationRecord)
	}
	return s, s.validate(rules)
}

func (s *JSONStore) validate(rules HeaderValidator) error {
	if rules == nil {
		return fmt.Errorf("header validation rules are required when reopening stored data")
	}
	anchorHeader, ok := s.headers[s.anchor.Hash]
	if !ok || anchorHeader.Height != s.anchor.Height {
		return fmt.Errorf("stored data does not contain the immutable trust anchor")
	}
	if s.anchor.Header != nil && anchorHeader != *s.anchor.Header {
		return fmt.Errorf("stored trust anchor header was modified")
	}
	if s.work[s.anchor.Hash] == nil || s.work[s.anchor.Hash].Sign() != 0 {
		return fmt.Errorf("trust anchor accumulated work must start at zero")
	}
	hashes := make([]types.Hash, 0, len(s.headers))
	for hash := range s.headers {
		hashes = append(hashes, hash)
	}
	sort.Slice(hashes, func(i, j int) bool {
		left, right := s.headers[hashes[i]], s.headers[hashes[j]]
		if left.Height != right.Height {
			return left.Height < right.Height
		}
		return hashes[i].Hex() < hashes[j].Hex()
	})
	childrenSeen := make(map[types.Hash]map[types.Hash]struct{})
	for _, hash := range hashes {
		header := s.headers[hash]
		if types.DigestBlock(header) != hash {
			return fmt.Errorf("stored header %s was modified", hash)
		}
		if header.Height < s.anchor.Height {
			return fmt.Errorf("stored header %s is below the trust anchor", hash)
		}
		if hash == s.anchor.Hash {
			continue
		}
		parent, parentOK := s.headers[header.ParentHash]
		if !parentOK {
			return fmt.Errorf("stored header %s has a missing ancestor", hash)
		}
		if err := rules.ValidateHeader(header, parent, true); err != nil {
			return fmt.Errorf("stored header %s failed validation: %w", hash, err)
		}
		expectedWork := rules.ChildWork(s.work[header.ParentHash])
		if s.work[hash] == nil || s.work[hash].Cmp(expectedWork) != 0 {
			return fmt.Errorf("stored accumulated work for %s is invalid", hash)
		}
		if childrenSeen[header.ParentHash] == nil {
			childrenSeen[header.ParentHash] = make(map[types.Hash]struct{})
		}
		if _, duplicated := childrenSeen[header.ParentHash][hash]; duplicated {
			return fmt.Errorf("duplicate child edge for %s -> %s", header.ParentHash, hash)
		}
		childrenSeen[header.ParentHash][hash] = struct{}{}
	}
	for parent, children := range s.children {
		if _, ok := s.headers[parent]; !ok {
			return fmt.Errorf("stored child edge references unknown parent %s", parent)
		}
		seen := make(map[types.Hash]struct{})
		for _, child := range children {
			childHeader, ok := s.headers[child]
			if !ok || childHeader.ParentHash != parent {
				return fmt.Errorf("stored child edge %s -> %s is invalid", parent, child)
			}
			if _, duplicated := seen[child]; duplicated {
				return fmt.Errorf("duplicate stored child edge to %s", child)
			}
			seen[child] = struct{}{}
		}
	}
	for hash := range s.headers {
		isTip := len(s.children[hash]) == 0
		_, markedTip := s.tips[hash]
		if isTip != markedTip {
			return fmt.Errorf("stored tip set for %s is inconsistent", hash)
		}
	}
	_, acceptedOK := s.headers[s.accepted]
	if !acceptedOK {
		return fmt.Errorf("stored accepted head is unknown")
	}
	acceptedWork := s.work[s.accepted]
	for tip := range s.tips {
		rules.Preferred(s.work[tip], acceptedWork, tip, s.accepted)
		if tip != s.accepted && rules.Preferred(s.work[tip], acceptedWork, tip, s.accepted) {
			return fmt.Errorf("stored accepted head %s loses chain selection to %s", s.accepted, tip)
		}
	}
	for _, record := range s.results {
		if record.Status != ResultValid && record.Status != ResultRevoked {
			return fmt.Errorf("stored result has unknown status %q", record.Status)
		}
		block, ok := s.headers[record.Proof.BlockHash]
		if !ok {
			return fmt.Errorf("stored proof references unknown block")
		}
		root, err := record.Proof.Root()
		if err != nil || root != block.TxRoot {
			return fmt.Errorf("stored proof is malformed or does not match its block")
		}
		if record.Status == ResultValid && !s.isAncestor(record.Proof.BlockHash, s.accepted) {
			return fmt.Errorf("stored valid proof is not bound to the recognized chain")
		}
	}
	return nil
}

func (s *JSONStore) isAncestor(ancestor, descendant types.Hash) bool {
	current := descendant
	for {
		if current == ancestor {
			return true
		}
		header, ok := s.headers[current]
		if !ok || header.Height < s.anchor.Height {
			return false
		}
		current = header.ParentHash
	}
}

func anchorsEqual(left, right TrustAnchor) bool {
	if left.Height != right.Height || left.Hash != right.Hash {
		return false
	}
	if (left.Header == nil) != (right.Header == nil) {
		return false
	}
	return left.Header == nil || *left.Header == *right.Header
}

func (s *JSONStore) Save() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	anchorBytes, err := json.MarshalIndent(s.anchor, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.dir, "anchor.json"), append(anchorBytes, '\n'), 0o600); err != nil {
		return err
	}
	data := chainFile{Headers: make(map[string]storedHeader), Children: make(map[string][]types.Hash)}
	parents := make([]types.Hash, 0, len(s.children))
	for parent := range s.children {
		parents = append(parents, parent)
	}
	sort.Slice(parents, func(i, j int) bool { return parents[i].Hex() < parents[j].Hex() })
	for _, parent := range parents {
		data.Children[parent.Hex()] = s.children[parent]
	}
	hashes := make([]types.Hash, 0, len(s.headers))
	for hash := range s.headers {
		hashes = append(hashes, hash)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].Hex() < hashes[j].Hex() })
	for _, hash := range hashes {
		data.Headers[hash.Hex()] = storedHeader{Header: s.headers[hash], Work: s.work[hash].String()}
	}
	for tip := range s.tips {
		data.Tips = append(data.Tips, tip)
	}
	sort.Slice(data.Tips, func(i, j int) bool { return data.Tips[i].Hex() < data.Tips[j].Hex() })
	data.Accepted = s.accepted
	dataBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.dir, "chain.json"), append(dataBytes, '\n'), 0o600); err != nil {
		return err
	}
	persistedResults := resultsFile{Results: s.results}
	resultBytes, err := json.MarshalIndent(persistedResults, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, "results.json"), append(resultBytes, '\n'), 0o600)
}

func (s *JSONStore) Anchor() TrustAnchor { return s.anchor }

func (s *JSONStore) Header(hash types.Hash) (types.BlockHeader, bool) {
	header, ok := s.headers[hash]
	return header, ok
}

func (s *JSONStore) Work(hash types.Hash) (*big.Int, bool) {
	work, ok := s.work[hash]
	if !ok {
		return nil, false
	}
	return new(big.Int).Set(work), true
}

func (s *JSONStore) Children(parent types.Hash) []types.Hash {
	return append([]types.Hash(nil), s.children[parent]...)
}

func (s *JSONStore) Tips() []types.Hash {
	tips := make([]types.Hash, 0, len(s.tips))
	for tip := range s.tips {
		tips = append(tips, tip)
	}
	return tips
}

func (s *JSONStore) Accepted() types.Hash { return s.accepted }

func (s *JSONStore) PutHeader(header types.BlockHeader, work *big.Int) {
	hash := types.DigestBlock(header)
	s.headers[hash] = header
	s.work[hash] = new(big.Int).Set(work)
	s.children[header.ParentHash] = append(s.children[header.ParentHash], hash)
	delete(s.tips, header.ParentHash)
	s.tips[hash] = struct{}{}
}

func (s *JSONStore) SetAccepted(hash types.Hash) { s.accepted = hash }

func (s *JSONStore) Result(key string) (VerificationRecord, bool) {
	record, ok := s.results[key]
	return record, ok
}

func (s *JSONStore) PutResult(key string, record VerificationRecord) {
	s.results[key] = record
}

func (s *JSONStore) Results() map[string]VerificationRecord {
	return s.results
}
