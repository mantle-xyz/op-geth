package types

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestHeaderBlockAccessListHashJSONKey guards the JSON key of the EIP-7928
// block-access-list hash header field. go-ethereum serializes it as
// "blockAccessListHash"; a divergent key (e.g. "balHash") would make a node
// silently drop the field when unmarshaling an upstream header and compute a
// mismatching block hash.
func TestHeaderBlockAccessListHashJSONKey(t *testing.T) {
	bal := common.HexToHash("0xabc0000000000000000000000000000000000000000000000000000000000123")
	h := &Header{
		Number:              big.NewInt(1),
		Difficulty:          big.NewInt(0),
		BaseFee:             big.NewInt(7),
		BlockAccessListHash: &bal,
	}

	enc, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	if !strings.Contains(string(enc), `"blockAccessListHash"`) {
		t.Fatalf("expected canonical key blockAccessListHash, got: %s", enc)
	}
	if strings.Contains(string(enc), `"balHash"`) {
		t.Fatalf("stale balHash key still present: %s", enc)
	}

	var got Header
	if err := json.Unmarshal(enc, &got); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if got.BlockAccessListHash == nil || *got.BlockAccessListHash != bal {
		t.Fatalf("BlockAccessListHash not round-tripped: got %v want %v", got.BlockAccessListHash, bal)
	}
}
