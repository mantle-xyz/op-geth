package core

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

const hoodiUpgradeTime = 1_000

var hoodiSupplySlot = common.BigToHash(big.NewInt(2))

// hoodiChainConfig is an OP Stack chain config with the given chain ID, as Mantle Hoodi's genesis is:
// applying the Mantle upgrade config activates Cancun and later forks, which OP Stack chains run
// without a blob schedule.
func hoodiChainConfig(chainID *big.Int) params.ChainConfig {
	config := *params.TestChainConfig
	config.ChainID = chainID
	config.Optimism = &params.OptimismConfig{EIP1559Elasticity: 10, EIP1559Denominator: 50}
	config.BlobScheduleConfig = nil
	return config
}

func hoodiConfig(upgrade *uint64) *params.ChainConfig {
	return &params.ChainConfig{ChainID: params.MantleHoodiChainId, MantleHoodiBvmUpgradeTime: upgrade}
}

// hoodiState mirrors Hoodi before the upgrade: BVM_ETH has placeholder code, a balance and storage,
// BVM_MNT does not exist.
func hoodiState(t *testing.T) *state.StateDB {
	t.Helper()
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	statedb.SetCode(MantleBvmEthAddr, []byte{0x00}, tracing.CodeChangeUnspecified)
	statedb.SetState(MantleBvmEthAddr, hoodiSupplySlot, common.BigToHash(big.NewInt(15_349)))
	statedb.SetBalance(MantleBvmEthAddr, uint256.NewInt(7), tracing.BalanceChangeUnspecified)
	return statedb
}

func TestMantleHoodiBvmUpgradeWritesCodeAtActivation(t *testing.T) {
	ts := uint64(hoodiUpgradeTime)
	statedb := hoodiState(t)
	ApplyMantleHoodiBvmUpgrade(hoodiConfig(&ts), hoodiUpgradeTime, statedb)

	if got := statedb.GetCodeHash(MantleBvmEthAddr); got != MantleBvmEthCodeHash {
		t.Fatalf("BVM_ETH code hash = %x", got)
	}
	if got := statedb.GetCodeHash(MantleLegacyERC20MNTAddr); got != MantleLegacyERC20MNTCodeHash {
		t.Fatalf("BVM_MNT code hash = %x", got)
	}
	if got := crypto.Keccak256Hash(statedb.GetCode(MantleLegacyERC20MNTAddr)); got != MantleLegacyERC20MNTCodeHash {
		t.Fatalf("BVM_MNT code does not hash to the pinned value: %x", got)
	}
	if statedb.GetNonce(MantleLegacyERC20MNTAddr) != 0 || !statedb.GetBalance(MantleLegacyERC20MNTAddr).IsZero() {
		t.Fatal("BVM_MNT nonce and balance must stay zero")
	}
	// BVM_ETH storage and balance are preserved.
	if got := statedb.GetState(MantleBvmEthAddr, hoodiSupplySlot); got != common.BigToHash(big.NewInt(15_349)) {
		t.Fatalf("BVM_ETH storage changed: %x", got)
	}
	if statedb.GetBalance(MantleBvmEthAddr).Uint64() != 7 {
		t.Fatal("BVM_ETH balance changed")
	}
}

// The fork is written only if it is active at the block time and not one block earlier, as in
// alloy-op-evm, which saturates the previous block time at zero. An activation time between two
// block times (T=1) activates in the next block.
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
		ApplyMantleHoodiBvmUpgrade(hoodiConfig(&ts), tc.time, statedb)
		if got := statedb.Exist(MantleLegacyERC20MNTAddr); got != tc.write {
			t.Errorf("T=%d time=%d: wrote=%v, want %v", tc.upgrade, tc.time, got, tc.write)
		}
	}
}

func TestMantleHoodiBvmUpgradeNoop(t *testing.T) {
	ts := uint64(hoodiUpgradeTime)
	for name, tc := range map[string]struct {
		config *params.ChainConfig
		time   uint64
	}{
		"before activation":      {hoodiConfig(&ts), hoodiUpgradeTime - 1},
		"after activation block": {hoodiConfig(&ts), hoodiUpgradeTime + params.MantleBlockTime},
		"not scheduled":          {hoodiConfig(nil), 1 << 62},
		"mantle mainnet":         {&params.ChainConfig{ChainID: params.MantleMainnetChainId, MantleHoodiBvmUpgradeTime: &ts}, hoodiUpgradeTime},
		"mantle sepolia":         {&params.ChainConfig{ChainID: params.MantleSepoliaChainId, MantleHoodiBvmUpgradeTime: &ts}, hoodiUpgradeTime},
	} {
		t.Run(name, func(t *testing.T) {
			statedb := hoodiState(t)
			ApplyMantleHoodiBvmUpgrade(tc.config, tc.time, statedb)
			if statedb.GetCodeHash(MantleBvmEthAddr) != crypto.Keccak256Hash([]byte{0x00}) {
				t.Fatal("BVM_ETH code must be untouched")
			}
			if statedb.Exist(MantleLegacyERC20MNTAddr) {
				t.Fatal("BVM_MNT must not be created")
			}
		})
	}
}

// The Mantle Hoodi BVM upgrade must be applied identically by block production
// (GenerateChain, like the miner) and by block import (StateProcessor): the imported chain is only
// accepted if both compute the same state root. The upgrade is applied inside message execution;
// every L2 block starts with the L1 info deposit, so each generated block carries a transaction,
// and the activation block's first transaction performs the write.
func TestMantleHoodiBvmUpgradeChain(t *testing.T) {
	upgradeTime := uint64(20) // GenerateChain spaces blocks 10s apart: block 2 activates.
	config := *params.TestChainConfig
	config.ChainID = params.MantleHoodiChainId
	config.MantleHoodiBvmUpgradeTime = &upgradeTime

	supplySlot := common.BigToHash(big.NewInt(2))
	key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	sender := crypto.PubkeyToAddress(key.PublicKey)
	gspec := &Genesis{
		Config: &config,
		Alloc: types.GenesisAlloc{
			sender: {Balance: big.NewInt(params.Ether)},
			MantleBvmEthAddr: {
				Code:    []byte{0x00},
				Storage: map[common.Hash]common.Hash{supplySlot: common.BigToHash(big.NewInt(15_349))},
				Balance: big.NewInt(0),
			},
		},
	}
	signer := types.LatestSigner(&config)
	_, blocks, _ := GenerateChainWithGenesis(gspec, ethash.NewFaker(), 3, func(i int, b *BlockGen) {
		b.AddTx(types.MustSignNewTx(key, signer, &types.LegacyTx{
			Nonce: uint64(i), To: &sender, Gas: params.TxGas, GasPrice: b.BaseFee(),
		}))
	})
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
	if got := codeHashAt(0, MantleBvmEthAddr); got != crypto.Keccak256Hash([]byte{0x00}) {
		t.Fatalf("pre-fork BVM_ETH code hash = %x", got)
	}
	if got := codeHashAt(0, MantleLegacyERC20MNTAddr); got != (common.Hash{}) {
		t.Fatalf("pre-fork BVM_MNT must not exist, code hash = %x", got)
	}
	for _, number := range []int{1, 2} {
		if got := codeHashAt(number, MantleBvmEthAddr); got != MantleBvmEthCodeHash {
			t.Fatalf("block %d BVM_ETH code hash = %x", number+1, got)
		}
		if got := codeHashAt(number, MantleLegacyERC20MNTAddr); got != MantleLegacyERC20MNTCodeHash {
			t.Fatalf("block %d BVM_MNT code hash = %x", number+1, got)
		}
	}
	statedb, _ := chain.StateAt(blocks[2].Header())
	if got := statedb.GetState(MantleBvmEthAddr, supplySlot); got != common.BigToHash(big.NewInt(15_349)) {
		t.Fatalf("BVM_ETH storage changed: %x", got)
	}

	// The upgrade is written by message execution: the state before the activation block's first
	// transaction has no upgrade code and the state after it has. Block 1 does not write.
	for _, tc := range []struct {
		number int
		write  bool
	}{{0, false}, {1, true}} {
		block := blocks[tc.number]
		header := block.Header()
		statedb, err := chain.StateAt(chain.GetHeaderByHash(block.ParentHash()))
		if err != nil {
			t.Fatal(err)
		}
		if statedb.Exist(MantleLegacyERC20MNTAddr) {
			t.Fatalf("block %d: BVM_MNT exists before the first transaction", tc.number+1)
		}
		evm := vm.NewEVM(NewEVMBlockContext(header, chain, nil, &config, statedb), statedb, &config, vm.Config{})
		rules := config.Rules(header.Number, false, header.Time)
		msg, err := TransactionToMessage(block.Transactions()[0], types.MakeSigner(&config, header.Number, header.Time), header.BaseFee, &rules)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyMessage(evm, msg, NewGasPool(header.GasLimit)); err != nil {
			t.Fatal(err)
		}
		if got := statedb.GetCodeHash(MantleLegacyERC20MNTAddr) == MantleLegacyERC20MNTCodeHash; got != tc.write {
			t.Fatalf("block %d: BVM_MNT has the upgrade code after the first transaction = %v, want %v", tc.number+1, got, tc.write)
		}
		if got := statedb.GetCodeHash(MantleBvmEthAddr) == MantleBvmEthCodeHash; got != tc.write {
			t.Fatalf("block %d: BVM_ETH has the upgrade code after the first transaction = %v, want %v", tc.number+1, got, tc.write)
		}
	}
}

// The override schedules the fork on an existing Hoodi database (initialised from genesis once).
// This is for local testing only.
func TestMantleHoodiBvmUpgradeOverride(t *testing.T) {
	config := hoodiChainConfig(params.MantleHoodiChainId)
	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, triedb.HashDefaults)
	gspec := &Genesis{Config: &config}
	if _, _, _, err := SetupGenesisBlockWithOverride(db, tdb, gspec, nil, nil); err != nil {
		t.Fatal(err)
	}
	upgradeTime := uint64(1_000)
	cfg, _, _, err := SetupGenesisBlockWithOverride(db, tdb, nil, &ChainOverrides{
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

// Mantle Hoodi's fork times come from params.MantleHoodiUpgradeConfig, as Mantle mainnet's and
// Sepolia's do: with ApplyMantleUpgrades the configured upgrade time replaces the stored one and the
// override wins over it; without ApplyMantleUpgrades the stored value is kept. Other chains keep
// their stored value.
func TestMantleHoodiBvmUpgradeConfigured(t *testing.T) {
	configured := uint64(2_000)
	prev := params.MantleHoodiUpgradeConfig.MantleHoodiBvmUpgradeTime
	params.MantleHoodiUpgradeConfig.MantleHoodiBvmUpgradeTime = &configured
	t.Cleanup(func() { params.MantleHoodiUpgradeConfig.MantleHoodiBvmUpgradeTime = prev })

	setup := func(chainID *big.Int, overrides *ChainOverrides) *params.ChainConfig {
		stored := uint64(1_000)
		config := hoodiChainConfig(chainID)
		config.MantleHoodiBvmUpgradeTime = &stored
		db := rawdb.NewMemoryDatabase()
		tdb := triedb.NewDatabase(db, triedb.HashDefaults)
		gspec := &Genesis{Config: &config}
		if _, _, _, err := SetupGenesisBlockWithOverride(db, tdb, gspec, nil, nil); err != nil {
			t.Fatal(err)
		}
		cfg, _, _, err := SetupGenesisBlockWithOverride(db, tdb, nil, overrides, nil)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	override := uint64(3_000)
	for _, tc := range []struct {
		name      string
		chainID   *big.Int
		overrides *ChainOverrides
		want      uint64
	}{
		{"hoodi, mantle upgrades", params.MantleHoodiChainId, &ChainOverrides{ApplyMantleUpgrades: true}, configured},
		{"hoodi, override", params.MantleHoodiChainId, &ChainOverrides{ApplyMantleUpgrades: true, OverrideMantleHoodiBvmUpgrade: &override}, override},
		{"hoodi, mantle upgrades disabled", params.MantleHoodiChainId, &ChainOverrides{ApplyMantleUpgrades: false}, 1_000},
		{"hoodi, no overrides", params.MantleHoodiChainId, nil, 1_000},
		{"other chain", params.TestChainConfig.ChainID, &ChainOverrides{ApplyMantleUpgrades: true}, 1_000},
	} {
		cfg := setup(tc.chainID, tc.overrides)
		if got := cfg.MantleHoodiBvmUpgradeTime; got == nil || *got != tc.want {
			t.Fatalf("%s: got %v, want %d", tc.name, fmtTime(got), tc.want)
		}
	}
	if got := setup(params.MantleHoodiChainId, &ChainOverrides{ApplyMantleUpgrades: true}).MantleArsiaTime; got == nil || *got != 0 {
		t.Fatalf("hoodi Arsia time = %v, want 0 from MantleHoodiUpgradeConfig", fmtTime(got))
	}
}

func fmtTime(t *uint64) any {
	if t == nil {
		return nil
	}
	return *t
}
