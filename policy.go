package lightclient

// ConfirmationPolicy lets callers update how many confirmations a proof must
// have without changing consensus, Merkle, or storage rules. The policy is
// evaluated against the current canonical chain at verification time, so a
// reorg automatically tightens or relaxes the check on the next verification.
type ConfirmationPolicy interface {
	RequiredConfirmations(blockHeight uint64) uint64
}

// ConstantConfirmations asks for the same number of confirmations at every
// height. A zero value falls back to DefaultConfirmations.
type ConstantConfirmations uint64

func (c ConstantConfirmations) RequiredConfirmations(uint64) uint64 {
	if c == 0 {
		return DefaultConfirmations
	}
	return uint64(c)
}

// ConfirmationPolicyFunc adapts a plain height->requirement function.
type ConfirmationPolicyFunc func(blockHeight uint64) uint64

func (f ConfirmationPolicyFunc) RequiredConfirmations(blockHeight uint64) uint64 {
	required := f(blockHeight)
	if required == 0 {
		return DefaultConfirmations
	}
	return required
}

// HeightConfirmations stores per-height requirements and falls back to a
// default for unlisted heights. It is a convenient updatable policy for tests.
type HeightConfirmations struct {
	ByHeight map[uint64]uint64
	Fallback uint64
}

func (p HeightConfirmations) RequiredConfirmations(blockHeight uint64) uint64 {
	if required, ok := p.ByHeight[blockHeight]; ok && required > 0 {
		return required
	}
	if p.Fallback == 0 {
		return DefaultConfirmations
	}
	return p.Fallback
}
