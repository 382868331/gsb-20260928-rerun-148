package lightclient

import (
	"encoding/binary"
	"time"
)

func resultIdentityHash(result VerificationResult) Hash {
	var data []byte
	data = append(data, result.Status...)
	data = append(data, result.TrustRootHash[:]...)
	data = append(data, result.CanonicalTip[:]...)
	data = append(data, result.BlockHash[:]...)
	data = append(data, result.TransactionHash[:]...)
	data = binary.BigEndian.AppendUint64(data, result.BlockHeight)
	data = binary.BigEndian.AppendUint64(data, result.TransactionIndex)
	data = binary.BigEndian.AppendUint64(data, result.Confirmations)
	data = binary.BigEndian.AppendUint64(data, result.RequiredConfirmations)
	data = appendTime(data, result.ObservedAt)
	data = append(data, []byte(result.Reason)...)
	return Hash(sha256Hash(data))
}

func appendTime(data []byte, value time.Time) []byte {
	return binary.BigEndian.AppendUint64(data, uint64(value.UnixNano()))
}
