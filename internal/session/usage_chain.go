package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// usageChain is continuity evidence for one journal record. A deleted middle
// record breaks the chain even when every remaining line is valid JSON.
// The record bytes are wrapped, not remarshaled, so the checksum is the hash
// of the exact attempt that was appended.
type usageChain struct {
	Version  int    `json:"version"`
	Sequence int64  `json:"sequence"`
	Previous string `json:"previous"`
	Record   string `json:"record"`
}

type sealedUsageRecord struct {
	Chain  *usageChain     `json:"chain,omitempty"`
	Record json.RawMessage `json:"record,omitempty"`
}

func hashUsageRecord(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sealUsageRecord(data []byte, sequence int64, previous string) ([]byte, string, error) {
	if sequence < 1 || (sequence == 1) != (previous == "") {
		return nil, "", fmt.Errorf("invalid usage chain position")
	}
	if !json.Valid(data) {
		return nil, "", fmt.Errorf("usage record is not valid JSON")
	}
	sealed, err := json.Marshal(sealedUsageRecord{
		Chain:  &usageChain{Version: 1, Sequence: sequence, Previous: previous, Record: hashUsageRecord(data)},
		Record: json.RawMessage(data),
	})
	if err != nil {
		return nil, "", err
	}
	return sealed, hashUsageRecord(sealed), nil
}

// unwrapUsageRecord returns the attempt JSON and, when the line is chained,
// its chain and the digest of the sealed line. Lines written before chaining
// are returned unchanged with a nil chain.
func unwrapUsageRecord(line []byte) (record []byte, chain *usageChain, digest string, err error) {
	var sealed sealedUsageRecord
	if err := json.Unmarshal(line, &sealed); err != nil {
		return nil, nil, "", fmt.Errorf("invalid usage journal record")
	}
	if sealed.Chain == nil {
		return line, nil, "", nil
	}
	if sealed.Chain.Version != 1 || sealed.Chain.Sequence < 1 || len(sealed.Record) == 0 || !json.Valid(sealed.Record) {
		return nil, sealed.Chain, "", fmt.Errorf("invalid usage chain")
	}
	if sealed.Chain.Record != hashUsageRecord(sealed.Record) {
		return nil, sealed.Chain, "", fmt.Errorf("usage record checksum mismatch")
	}
	return sealed.Record, sealed.Chain, hashUsageRecord(line), nil
}

func linkUsageChain(chain *usageChain, want int64, previous string, chainStarted bool) error {
	if chain == nil {
		if chainStarted {
			return fmt.Errorf("usage journal discontinuity at sequence %d", want)
		}
		return nil
	}
	if !chainStarted {
		if chain.Sequence != 1 || chain.Previous != "" {
			return fmt.Errorf("usage journal discontinuity at sequence %d", chain.Sequence)
		}
		return nil
	}
	if chain.Sequence != want || chain.Previous != previous {
		return fmt.Errorf("usage journal discontinuity at sequence %d", chain.Sequence)
	}
	return nil
}
