package misc_test

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

const upgradeTime = 1_000

var supplySlot = common.BigToHash(big.NewInt(2))

func hoodiConfig(upgradeTime *uint64) *params.ChainConfig {
	return &params.ChainConfig{ChainID: params.MantleHoodiChainId, MantleHoodiBvmUpgradeTime: upgradeTime}
}

// hoodiState mirrors Hoodi before the upgrade: BVM_ETH has placeholder code, a balance and storage,
// BVM_MNT does not exist.
func hoodiState(t *testing.T) *state.StateDB {
	t.Helper()
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	statedb.SetCode(misc.MantleBvmEthAddr, []byte{0x00}, tracing.CodeChangeUnspecified)
	statedb.SetState(misc.MantleBvmEthAddr, supplySlot, common.BigToHash(big.NewInt(15_349)))
	statedb.SetBalance(misc.MantleBvmEthAddr, uint256.NewInt(7), tracing.BalanceChangeUnspecified)
	return statedb
}

func TestMantleHoodiBvmUpgradeWritesCodeAtActivation(t *testing.T) {
	ts := uint64(upgradeTime)
	statedb := hoodiState(t)
	misc.ApplyMantleHoodiBvmUpgrade(hoodiConfig(&ts), upgradeTime, statedb)

	if got := statedb.GetCodeHash(misc.MantleBvmEthAddr); got != misc.MantleBvmEthCodeHash {
		t.Fatalf("BVM_ETH code hash = %x", got)
	}
	if got := statedb.GetCodeHash(misc.MantleLegacyERC20MNTAddr); got != misc.MantleLegacyERC20MNTCodeHash {
		t.Fatalf("BVM_MNT code hash = %x", got)
	}
	if got := crypto.Keccak256Hash(statedb.GetCode(misc.MantleLegacyERC20MNTAddr)); got != misc.MantleLegacyERC20MNTCodeHash {
		t.Fatalf("BVM_MNT code does not hash to the pinned value: %x", got)
	}
	if statedb.GetNonce(misc.MantleLegacyERC20MNTAddr) != 0 || !statedb.GetBalance(misc.MantleLegacyERC20MNTAddr).IsZero() {
		t.Fatal("BVM_MNT nonce and balance must stay zero")
	}
	// BVM_ETH storage and balance are preserved.
	if got := statedb.GetState(misc.MantleBvmEthAddr, supplySlot); got != common.BigToHash(big.NewInt(15_349)) {
		t.Fatalf("BVM_ETH storage changed: %x", got)
	}
	if statedb.GetBalance(misc.MantleBvmEthAddr).Uint64() != 7 {
		t.Fatal("BVM_ETH balance changed")
	}
}

// An activation time between two blocks activates in the next block, as in alloy-op-evm.
func TestMantleHoodiBvmUpgradeUnalignedActivation(t *testing.T) {
	ts := uint64(upgradeTime + 1)
	statedb := hoodiState(t)
	misc.ApplyMantleHoodiBvmUpgrade(hoodiConfig(&ts), upgradeTime+params.MantleHoodiBlockTime, statedb)
	if statedb.GetCodeHash(misc.MantleLegacyERC20MNTAddr) != misc.MantleLegacyERC20MNTCodeHash {
		t.Fatal("the first block at or after an unaligned activation time must write")
	}
}

// Times near zero follow alloy-op-evm, which saturates the previous block time at zero: the
// fork is written only if it is active at the block time and not at the saturated previous time.
func TestMantleHoodiBvmUpgradeNearZero(t *testing.T) {
	for _, tc := range []struct {
		upgrade, time uint64
		write         bool
	}{
		{0, 0, false},
		{0, 1, false},
		{0, 2, false},
		{1, 1, true},
		{1, 2, true},
		{2, 2, true},
		{2, 3, true},
		{2, 4, false},
	} {
		ts := tc.upgrade
		statedb := hoodiState(t)
		misc.ApplyMantleHoodiBvmUpgrade(hoodiConfig(&ts), tc.time, statedb)
		if got := statedb.Exist(misc.MantleLegacyERC20MNTAddr); got != tc.write {
			t.Errorf("T=%d time=%d: wrote=%v, want %v", tc.upgrade, tc.time, got, tc.write)
		}
	}
}

func TestMantleHoodiBvmUpgradeNoop(t *testing.T) {
	ts := uint64(upgradeTime)
	for name, tc := range map[string]struct {
		config *params.ChainConfig
		time   uint64
	}{
		"before activation":      {hoodiConfig(&ts), upgradeTime - 1},
		"after activation block": {hoodiConfig(&ts), upgradeTime + params.MantleHoodiBlockTime},
		"not scheduled":          {hoodiConfig(nil), 1 << 62},
		"mantle mainnet":         {&params.ChainConfig{ChainID: params.MantleMainnetChainId, MantleHoodiBvmUpgradeTime: &ts}, upgradeTime},
		"mantle sepolia":         {&params.ChainConfig{ChainID: params.MantleSepoliaChainId, MantleHoodiBvmUpgradeTime: &ts}, upgradeTime},
	} {
		t.Run(name, func(t *testing.T) {
			statedb := hoodiState(t)
			misc.ApplyMantleHoodiBvmUpgrade(tc.config, tc.time, statedb)
			if statedb.GetCodeHash(misc.MantleBvmEthAddr) != crypto.Keccak256Hash([]byte{0x00}) {
				t.Fatal("BVM_ETH code must be untouched")
			}
			if statedb.Exist(misc.MantleLegacyERC20MNTAddr) {
				t.Fatal("BVM_MNT must not be created")
			}
		})
	}
}

func TestMantleHoodiBvmUpgradeOnlyActivationBlock(t *testing.T) {
	ts := uint64(upgradeTime)
	statedb := hoodiState(t)
	misc.ApplyMantleHoodiBvmUpgrade(hoodiConfig(&ts), upgradeTime, statedb)
	root := statedb.IntermediateRoot(true)
	// A later change to the code is not reverted by blocks after the activation block.
	statedb.SetCode(misc.MantleBvmEthAddr, []byte{0x01}, tracing.CodeChangeUnspecified)
	changed := statedb.IntermediateRoot(true)
	for time := upgradeTime + params.MantleHoodiBlockTime; time <= upgradeTime+3*params.MantleHoodiBlockTime; time += params.MantleHoodiBlockTime {
		misc.ApplyMantleHoodiBvmUpgrade(hoodiConfig(&ts), time, statedb)
	}
	if got := statedb.IntermediateRoot(true); got != changed || got == root {
		t.Fatalf("blocks after the activation block must not write: %x", got)
	}
}

func TestMantleHoodiBvmUpgradeConfig(t *testing.T) {
	ts := uint64(upgradeTime)
	cfg := hoodiConfig(&ts)
	if cfg.IsMantleHoodiBvmUpgrade(upgradeTime-1) || !cfg.IsMantleHoodiBvmUpgrade(upgradeTime) {
		t.Fatal("IsMantleHoodiBvmUpgrade must switch at the activation time")
	}
	if hoodiConfig(nil).IsMantleHoodiBvmUpgrade(1 << 62) {
		t.Fatal("unscheduled fork must never be active")
	}
}
