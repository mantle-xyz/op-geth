package ethapi

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/ethapi/override"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// eth_simulateV1 builds new blocks on top of the chain. A simulated Mantle Hoodi BVM upgrade
// activation block must write the upgrade code before its calls, like a real block.
func TestHoodiBvmUpgradeSimulateV1(t *testing.T) {
	upgradeTime := uint64(20) // GenerateChain spaces blocks 10s apart: the base block has time 10.
	config := *params.MergedTestChainConfig
	config.ChainID = params.MantleHoodiChainId
	config.MantleHoodiBvmUpgradeTime = &upgradeTime

	acc := newTestAccount()
	gspec := &core.Genesis{
		Config: &config,
		Alloc:  types.GenesisAlloc{acc.addr: {Balance: big.NewInt(params.Ether)}},
	}
	backend := newTestBackend(t, 1, gspec, beacon.New(ethash.NewFaker()), func(i int, b *core.BlockGen) {})
	api := NewBlockChainAPI(backend)

	mnt := misc.MantleLegacyERC20MNTAddr
	symbol := hexutil.Bytes{0x95, 0xd8, 0x9b, 0x41}
	call := TransactionArgs{From: &acc.addr, To: &mnt, Input: &symbol}
	timeOf := func(t uint64) *override.BlockOverrides {
		ts := hexutil.Uint64(t)
		return &override.BlockOverrides{Time: &ts}
	}
	latest := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
	results, err := api.SimulateV1(context.Background(), simOpts{BlockStateCalls: []simBlock{
		{BlockOverrides: timeOf(upgradeTime - 2), Calls: []TransactionArgs{call}},
		{BlockOverrides: timeOf(upgradeTime), Calls: []TransactionArgs{call}},
	}}, &latest)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d simulated blocks, want 2", len(results))
	}
	if out := results[0].Calls[0].ReturnValue; len(out) != 0 {
		t.Fatalf("symbol() before activation returned %x, want empty (no code yet)", out)
	}
	stringType, _ := abi.NewType("string", "", nil)
	out, err := abi.Arguments{{Type: stringType}}.Unpack(results[1].Calls[0].ReturnValue)
	if err != nil {
		t.Fatalf("decode symbol() output %x: %v", results[1].Calls[0].ReturnValue, err)
	}
	if got := out[0].(string); got != "MNT" {
		t.Fatalf("symbol() in the simulated activation block = %q, want MNT", got)
	}
}
