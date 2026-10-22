package tracers

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// hoodiSymbolSelector is `symbol()`.
var hoodiSymbolSelector = []byte{0x95, 0xd8, 0x9b, 0x41}

// newHoodiBvmUpgradeBackend builds a Mantle Hoodi chain whose block 2 is the Hoodi BVM upgrade
// activation block (GenerateChain spaces blocks 10s apart). Block 2 calls BVM_MNT `symbol()`, which
// only returns "MNT" if the upgrade code was written before the block's transactions.
func newHoodiBvmUpgradeBackend(t *testing.T) *testBackend {
	t.Helper()
	upgradeTime := uint64(20)
	config := *params.TestChainConfig
	config.ChainID = params.MantleHoodiChainId
	config.MantleHoodiBvmUpgradeTime = &upgradeTime

	accounts := newAccounts(1)
	genesis := &core.Genesis{
		Config: &config,
		Alloc:  types.GenesisAlloc{accounts[0].addr: {Balance: big.NewInt(params.Ether)}},
	}
	signer := types.LatestSigner(&config)
	to := core.MantleLegacyERC20MNTAddr
	backend := newTestBackend(t, 3, genesis, func(i int, b *core.BlockGen) {
		if i != 1 {
			return
		}
		if b.Timestamp() != upgradeTime {
			t.Fatalf("block 2 time = %d, want %d", b.Timestamp(), upgradeTime)
		}
		b.AddTx(types.MustSignNewTx(accounts[0].key, signer, &types.LegacyTx{
			Nonce:    0,
			To:       &to,
			Gas:      100_000,
			GasPrice: b.BaseFee(),
			Data:     hoodiSymbolSelector,
		}))
	})
	t.Cleanup(backend.teardown)
	return backend
}

// decodeSymbolTrace extracts the ABI-decoded string returned in a struct logger trace result.
func decodeSymbolTrace(t *testing.T, result interface{}) string {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var trace struct {
		Failed      bool          `json:"failed"`
		ReturnValue hexutil.Bytes `json:"returnValue"`
	}
	if err := json.Unmarshal(raw, &trace); err != nil {
		t.Fatal(err)
	}
	if trace.Failed {
		t.Fatalf("symbol() call failed: %s", raw)
	}
	stringType, _ := abi.NewType("string", "", nil)
	out, err := abi.Arguments{{Type: stringType}}.Unpack(trace.ReturnValue)
	if err != nil {
		t.Fatalf("decode symbol() output %x: %v", trace.ReturnValue, err)
	}
	return out[0].(string)
}

// IntermediateRoots re-executes the activation block on the parent state; its root after the last
// transaction must equal the imported post-state before the ethash block reward, which only holds if
// the upgrade code is written before the transactions.
func TestHoodiBvmUpgradeIntermediateRoots(t *testing.T) {
	backend := newHoodiBvmUpgradeBackend(t)
	api := NewAPI(backend)
	block := backend.chain.GetBlockByNumber(2)
	roots, err := api.IntermediateRoots(context.Background(), block.Hash(), nil)
	if err != nil {
		t.Fatal(err)
	}
	statedb, err := backend.chain.StateAt(block.Header())
	if err != nil {
		t.Fatal(err)
	}
	if statedb.GetCodeHash(core.MantleLegacyERC20MNTAddr) != core.MantleLegacyERC20MNTCodeHash {
		t.Fatal("activation block state has no BVM_MNT upgrade code")
	}
	statedb.SubBalance(block.Coinbase(), ethash.ConstantinopleBlockReward, tracing.BalanceChangeUnspecified)
	if want := statedb.IntermediateRoot(true); len(roots) != 1 || roots[0] != want {
		t.Fatalf("intermediate roots %v, want [%x] (imported state before the block reward)", roots, want)
	}
}

// debug_traceBlockByNumber traces the activation block against the upgraded code.
func TestHoodiBvmUpgradeTraceBlock(t *testing.T) {
	backend := newHoodiBvmUpgradeBackend(t)
	api := NewAPI(backend)
	results, err := api.TraceBlockByNumber(context.Background(), rpc.BlockNumber(2), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Error != "" {
		t.Fatalf("unexpected trace results: %+v", results)
	}
	if got := decodeSymbolTrace(t, results[0].Result); got != "MNT" {
		t.Fatalf("symbol() = %q in the activation block, want MNT", got)
	}
}

// debug_traceChain re-executes each block on the previous one; the activation block must still
// see the upgraded code.
func TestHoodiBvmUpgradeTraceChain(t *testing.T) {
	backend := newHoodiBvmUpgradeBackend(t)
	api := NewAPI(backend)
	from, to := backend.chain.GetBlockByNumber(1), backend.chain.GetBlockByNumber(3)
	var found bool
	for result := range api.traceChain(from, to, nil, nil) {
		if result.Block != 2 {
			continue
		}
		if len(result.Traces) != 1 {
			t.Fatalf("block 2: %d traces, want 1", len(result.Traces))
		}
		if got := decodeSymbolTrace(t, result.Traces[0].Result); got != "MNT" {
			t.Fatalf("symbol() = %q in the activation block, want MNT", got)
		}
		found = true
	}
	if !found {
		t.Fatal("block 2 was not traced")
	}
}
