package core

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// The Mantle Hoodi BVM upgrade must be applied identically by block production
// (GenerateChain, like the miner) and by block import (StateProcessor): the imported chain is only
// accepted if both compute the same state root.
func TestMantleHoodiBvmUpgradeChain(t *testing.T) {
	upgradeTime := uint64(20) // GenerateChain spaces blocks 10s apart: block 2 activates.
	config := *params.TestChainConfig
	config.ChainID = params.MantleHoodiChainId
	config.MantleHoodiBvmUpgradeTime = &upgradeTime

	supplySlot := common.BigToHash(big.NewInt(2))
	gspec := &Genesis{
		Config: &config,
		Alloc: types.GenesisAlloc{
			misc.MantleBvmEthAddr: {
				Code:    []byte{0x00},
				Storage: map[common.Hash]common.Hash{supplySlot: common.BigToHash(big.NewInt(15_349))},
				Balance: big.NewInt(0),
			},
		},
	}
	_, blocks, _ := GenerateChainWithGenesis(gspec, ethash.NewFaker(), 3, nil)
	if blocks[1].Time() != upgradeTime {
		t.Fatalf("block 2 time = %d, want %d", blocks[1].Time(), upgradeTime)
	}

	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, ethash.NewFaker(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer chain.Stop()
	if n, err := chain.InsertChain(blocks); err != nil {
		t.Fatalf("block %d import failed: %v", n, err)
	}

	codeHashAt := func(number int, addr common.Address) common.Hash {
		statedb, err := chain.StateAt(blocks[number].Header())
		if err != nil {
			t.Fatal(err)
		}
		return statedb.GetCodeHash(addr)
	}
	if got := codeHashAt(0, misc.MantleBvmEthAddr); got != crypto.Keccak256Hash([]byte{0x00}) {
		t.Fatalf("pre-fork BVM_ETH code hash = %x", got)
	}
	if got := codeHashAt(0, misc.MantleLegacyERC20MNTAddr); got != (common.Hash{}) {
		t.Fatalf("pre-fork BVM_MNT must not exist, code hash = %x", got)
	}
	for _, number := range []int{1, 2} {
		if got := codeHashAt(number, misc.MantleBvmEthAddr); got != misc.MantleBvmEthCodeHash {
			t.Fatalf("block %d BVM_ETH code hash = %x", number+1, got)
		}
		if got := codeHashAt(number, misc.MantleLegacyERC20MNTAddr); got != misc.MantleLegacyERC20MNTCodeHash {
			t.Fatalf("block %d BVM_MNT code hash = %x", number+1, got)
		}
	}
	statedb, _ := chain.StateAt(blocks[2].Header())
	if got := statedb.GetState(misc.MantleBvmEthAddr, supplySlot); got != common.BigToHash(big.NewInt(15_349)) {
		t.Fatalf("BVM_ETH storage changed: %x", got)
	}
}

// The fork time can be scheduled on an existing Hoodi database through the override, which is how
// op-geth nodes (initialised from genesis once) pick it up.
func TestMantleHoodiBvmUpgradeOverride(t *testing.T) {
	config := *params.TestChainConfig
	config.ChainID = params.MantleHoodiChainId
	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, triedb.HashDefaults)
	gspec := &Genesis{Config: &config}
	if _, _, _, err := SetupGenesisBlockWithOverride(db, tdb, gspec, nil, nil); err != nil {
		t.Fatal(err)
	}
	upgradeTime := uint64(1_000)
	cfg, _, _, err := SetupGenesisBlockWithOverride(db, tdb, gspec, &ChainOverrides{
		ApplyMantleUpgrades:           true,
		OverrideMantleHoodiBvmUpgrade: &upgradeTime,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MantleHoodiBvmUpgradeTime == nil || *cfg.MantleHoodiBvmUpgradeTime != upgradeTime {
		t.Fatalf("override not applied: %v", cfg.MantleHoodiBvmUpgradeTime)
	}
}

// Once scheduled, the bundled Hoodi time replaces the stored one on an existing database, and the
// override still takes precedence over it.
func TestMantleHoodiBvmUpgradeBundled(t *testing.T) {
	bundled := uint64(2_000)
	prev := params.BundledMantleHoodiBvmUpgradeTime
	params.BundledMantleHoodiBvmUpgradeTime = &bundled
	t.Cleanup(func() { params.BundledMantleHoodiBvmUpgradeTime = prev })

	setup := func(chainID *big.Int, override *uint64) *params.ChainConfig {
		stored := uint64(1_000)
		config := *params.TestChainConfig
		config.ChainID = chainID
		config.MantleHoodiBvmUpgradeTime = &stored
		db := rawdb.NewMemoryDatabase()
		tdb := triedb.NewDatabase(db, triedb.HashDefaults)
		gspec := &Genesis{Config: &config}
		if _, _, _, err := SetupGenesisBlockWithOverride(db, tdb, gspec, nil, nil); err != nil {
			t.Fatal(err)
		}
		cfg, _, _, err := SetupGenesisBlockWithOverride(db, tdb, nil, &ChainOverrides{
			ApplyMantleUpgrades:           true,
			OverrideMantleHoodiBvmUpgrade: override,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	if got := setup(params.MantleHoodiChainId, nil).MantleHoodiBvmUpgradeTime; got == nil || *got != bundled {
		t.Fatalf("hoodi: got %v, want bundled %d", got, bundled)
	}
	override := uint64(3_000)
	if got := setup(params.MantleHoodiChainId, &override).MantleHoodiBvmUpgradeTime; got == nil || *got != override {
		t.Fatalf("hoodi with override: got %v, want %d", got, override)
	}
	if got := setup(params.TestChainConfig.ChainID, nil).MantleHoodiBvmUpgradeTime; got == nil || *got != 1_000 {
		t.Fatalf("other chain: got %v, want stored 1000", got)
	}
}
