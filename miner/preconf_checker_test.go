package miner

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/preconf"
	"github.com/holiman/uint256"
)

func TestIsSyncStatusOk(t *testing.T) {
	tests := []struct {
		name          string
		currentStatus *preconf.OptimismSyncStatus
		newStatus     *preconf.OptimismSyncStatus
		want          bool
	}{
		{
			name: "Normal Growth",
			currentStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},
				HeadL1:    preconf.L1BlockRef{Number: 15},
				UnsafeL2:  preconf.L2BlockRef{Number: 20, L1Origin: preconf.BlockID{Number: 10}},
			},
			newStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 11},
				HeadL1:    preconf.L1BlockRef{Number: 16},
				UnsafeL2:  preconf.L2BlockRef{Number: 21, L1Origin: preconf.BlockID{Number: 11}},
			},
			want: true,
		},
		{
			name: "Partial Growth",
			currentStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},
				HeadL1:    preconf.L1BlockRef{Number: 15},
				UnsafeL2:  preconf.L2BlockRef{Number: 20, L1Origin: preconf.BlockID{Number: 10}},
			},
			newStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},                                        // no change
				HeadL1:    preconf.L1BlockRef{Number: 16},                                        // growth
				UnsafeL2:  preconf.L2BlockRef{Number: 20, L1Origin: preconf.BlockID{Number: 10}}, // no change
			},
			want: true,
		},
		{
			name: "Reorg CurrentL1",
			currentStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},
				HeadL1:    preconf.L1BlockRef{Number: 15},
				UnsafeL2:  preconf.L2BlockRef{Number: 20, L1Origin: preconf.BlockID{Number: 10}},
			},
			newStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 9}, // decreased
				HeadL1:    preconf.L1BlockRef{Number: 16},
				UnsafeL2:  preconf.L2BlockRef{Number: 21, L1Origin: preconf.BlockID{Number: 9}},
			},
			want: false,
		},
		{
			name: "Reorg HeadL1",
			currentStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},
				HeadL1:    preconf.L1BlockRef{Number: 15},
				UnsafeL2:  preconf.L2BlockRef{Number: 20, L1Origin: preconf.BlockID{Number: 10}},
			},
			newStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 11},
				HeadL1:    preconf.L1BlockRef{Number: 14}, // decreased
				UnsafeL2:  preconf.L2BlockRef{Number: 21, L1Origin: preconf.BlockID{Number: 11}},
			},
			want: false,
		},
		{
			name: "Reorg UnsafeL2",
			currentStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},
				HeadL1:    preconf.L1BlockRef{Number: 15},
				UnsafeL2:  preconf.L2BlockRef{Number: 20, L1Origin: preconf.BlockID{Number: 10}},
			},
			newStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 11},
				HeadL1:    preconf.L1BlockRef{Number: 16},
				UnsafeL2:  preconf.L2BlockRef{Number: 19, L1Origin: preconf.BlockID{Number: 11}}, // decreased
			},
			want: false,
		},
		{
			name: "No Change",
			currentStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},
				HeadL1:    preconf.L1BlockRef{Number: 15},
				UnsafeL2:  preconf.L2BlockRef{Number: 20, L1Origin: preconf.BlockID{Number: 10}},
			},
			newStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},
				HeadL1:    preconf.L1BlockRef{Number: 15},
				UnsafeL2:  preconf.L2BlockRef{Number: 20, L1Origin: preconf.BlockID{Number: 10}},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &preconfChecker{
				optimismSyncStatus: tt.currentStatus,
			}
			got := c.isSyncStatusOk(tt.newStatus)
			if got != tt.want {
				t.Errorf("isSyncStatusOk() = %v, want %v", got, tt.want)
			}
		})
	}
}

type mockLogFilterer struct {
	FilterLogsResult struct {
		Logs []types.Log
		Err  error
	}
	SubscribeFilterLogsResult struct {
		Sub ethereum.Subscription
		Err error
	}
	WaitTime time.Duration
}

func (m *mockLogFilterer) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	time.Sleep(m.WaitTime)
	return m.FilterLogsResult.Logs, m.FilterLogsResult.Err
}

func (m *mockLogFilterer) SubscribeFilterLogs(ctx context.Context, q ethereum.FilterQuery, ch chan<- types.Log) (ethereum.Subscription, error) {
	time.Sleep(m.WaitTime)
	return m.SubscribeFilterLogsResult.Sub, m.SubscribeFilterLogsResult.Err
}

func TestUpdateOptimismSyncStatus(t *testing.T) {
	log := []types.Log{
		{
			Address: common.HexToAddress("0xa513e6e4b8f2a923d98304ec87f64353c4d5c853"),
			Topics: []common.Hash{
				common.HexToHash("0xb3813568d9991fc951961fcb4c784893574240a28925604d09fc577c55bb7c32"),
				common.HexToHash("0x0000000000000000000000001276878a594ca255338adfa4d48449f69242fca0"),
				common.HexToHash("0x0000000000000000000000004200000000000000000000000000000000000007"),
				common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001"),
			},
			Data:        common.Hex2Bytes("0000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000024d0000000000000000000000000000000000000000000000000000000000000001000000000000000000000000000000000000000000000000000000000000000100000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000024ef1200ff8daf150001000000000000000000000000000000000000000000000000000000000000000000000000000000000000dc64a140aa3e981100a9beca4e685f962f0cf6c900000000000000000000000042000000000000000000000000000000000000100000000000000000000000000000000000000000000000000000000000000001000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000001e848000000000000000000000000000000000000000000000000000000000000000e000000000000000000000000000000000000000000000000000000000000000a4f407a99e000000000000000000000000f39fd6e51aad88f6f4ce6ab8827279cfffb92266000000000000000000000000f39fd6e51aad88f6f4ce6ab8827279cfffb922660000000000000000000000000000000000000000000000000000000000000001000000000000000000000000000000000000000000000000000000000000008000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"),
			BlockHash:   common.HexToHash("0x123"),
			BlockNumber: 10,
			TxHash:      common.HexToHash("0x123"),
			TxIndex:     0,
		},
	}
	filterLogsResult := struct {
		Logs []types.Log
		Err  error
	}{
		Logs: log,
		Err:  nil,
	}
	tests := []struct {
		name                   string
		currentStatus          *preconf.OptimismSyncStatus
		newStatus              *preconf.OptimismSyncStatus
		expectStatusUpdate     bool
		expectDepositTxsUpdate bool
		mockLogFilterer        *mockLogFilterer
	}{
		{
			name:          "Initial Status",
			currentStatus: nil,
			newStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},
				HeadL1:    preconf.L1BlockRef{Number: 13},
				UnsafeL2:  preconf.L2BlockRef{Number: 30, L1Origin: preconf.BlockID{Number: 10}},
			},
			expectStatusUpdate:     true,
			expectDepositTxsUpdate: true,
			mockLogFilterer: &mockLogFilterer{
				FilterLogsResult: filterLogsResult,
				WaitTime:         10 * time.Millisecond,
			},
		},
		{
			name: "L1 Block Changed",
			currentStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},
				HeadL1:    preconf.L1BlockRef{Number: 13},
				UnsafeL2:  preconf.L2BlockRef{Number: 30, L1Origin: preconf.BlockID{Number: 10}},
			},
			newStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 11},
				HeadL1:    preconf.L1BlockRef{Number: 14},
				UnsafeL2:  preconf.L2BlockRef{Number: 30, L1Origin: preconf.BlockID{Number: 10}},
			},
			expectStatusUpdate:     true,
			expectDepositTxsUpdate: true,
			mockLogFilterer: &mockLogFilterer{
				FilterLogsResult: filterLogsResult,
				WaitTime:         10 * time.Millisecond,
			},
		},
		{
			name: "No Change",
			currentStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},
				HeadL1:    preconf.L1BlockRef{Number: 15},
				UnsafeL2:  preconf.L2BlockRef{Number: 20, L1Origin: preconf.BlockID{Number: 10}},
			},
			newStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},
				HeadL1:    preconf.L1BlockRef{Number: 15},
				UnsafeL2:  preconf.L2BlockRef{Number: 20, L1Origin: preconf.BlockID{Number: 10}},
			},
			expectStatusUpdate:     false,
			expectDepositTxsUpdate: false,
			mockLogFilterer: &mockLogFilterer{
				FilterLogsResult: filterLogsResult,
				WaitTime:         10 * time.Millisecond,
			},
		},
		{
			name: "No L1 Block Change",
			currentStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},
				HeadL1:    preconf.L1BlockRef{Number: 15},
				UnsafeL2:  preconf.L2BlockRef{Number: 20, L1Origin: preconf.BlockID{Number: 10}},
			},
			newStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},
				HeadL1:    preconf.L1BlockRef{Number: 15},
				UnsafeL2:  preconf.L2BlockRef{Number: 21, L1Origin: preconf.BlockID{Number: 10}},
			},
			expectStatusUpdate:     true,
			expectDepositTxsUpdate: false,
			mockLogFilterer: &mockLogFilterer{
				FilterLogsResult: filterLogsResult,
				WaitTime:         10 * time.Millisecond,
			},
		},
		{
			name: "L1 Reorg",
			currentStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},
				HeadL1:    preconf.L1BlockRef{Number: 15},
				UnsafeL2:  preconf.L2BlockRef{Number: 20, L1Origin: preconf.BlockID{Number: 10}},
			},
			newStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 9}, // Decreased
				HeadL1:    preconf.L1BlockRef{Number: 16},
				UnsafeL2:  preconf.L2BlockRef{Number: 21, L1Origin: preconf.BlockID{Number: 9}},
			},
			expectStatusUpdate:     false,
			expectDepositTxsUpdate: false,
			mockLogFilterer: &mockLogFilterer{
				FilterLogsResult: filterLogsResult,
				WaitTime:         10 * time.Millisecond,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			c := &preconfChecker{
				optimismSyncStatus:   tt.currentStatus,
				optimismSyncStatusOk: true,
				env:                  &environment{},
				envUpdatedAt:         time.Now(),
				depositTxs:           []*types.Transaction{},
				unSealedPreconfTxsCh: make(chan []*types.Transaction),
				minerConfig:          &preconf.DefaultMinerConfig,
			}

			c.l1ethclient = &mockLogFilterer{
				FilterLogsResult: tt.mockLogFilterer.FilterLogsResult,
				WaitTime:         tt.mockLogFilterer.WaitTime,
			}

			originalDepositTxs := c.depositTxs
			originalOptimismSyncStatus := c.optimismSyncStatus

			// Call the function
			c.UpdateOptimismSyncStatus(tt.newStatus)

			// Check if status was updated
			if tt.expectStatusUpdate && (c.optimismSyncStatus == originalOptimismSyncStatus || c.optimismSyncStatus != tt.newStatus) {
				t.Fatalf("UpdateOptimismSyncStatus() did not update status, expected %p, got %p", tt.newStatus, c.optimismSyncStatus)
			}

			if !tt.expectStatusUpdate && reflect.DeepEqual(c.optimismSyncStatus, originalDepositTxs) {
				t.Fatalf("UpdateOptimismSyncStatus() updated status when it shouldn't have, expected %p, got %p, new %p", originalOptimismSyncStatus, c.optimismSyncStatus, tt.newStatus)
			}

			// Check if depositTxs were updated
			if tt.expectDepositTxsUpdate && reflect.DeepEqual(c.depositTxs, originalDepositTxs) {
				t.Fatalf("UpdateOptimismSyncStatus() depositTxs update mismatch, expected update: %v", tt.expectDepositTxsUpdate)
			}
		})
	}
}

func TestUpdateOptimismSyncStatusDelay(t *testing.T) {
	log := []types.Log{
		{
			Address: common.HexToAddress("0xa513e6e4b8f2a923d98304ec87f64353c4d5c853"),
			Topics: []common.Hash{
				common.HexToHash("0xb3813568d9991fc951961fcb4c784893574240a28925604d09fc577c55bb7c32"),
				common.HexToHash("0x0000000000000000000000001276878a594ca255338adfa4d48449f69242fca0"),
				common.HexToHash("0x0000000000000000000000004200000000000000000000000000000000000007"),
				common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001"),
			},
			Data:        common.Hex2Bytes("0000000000000000000000000000000000000000000000000000000000000020000000000000000000000000000000000000000000000000000000000000024d0000000000000000000000000000000000000000000000000000000000000001000000000000000000000000000000000000000000000000000000000000000100000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000024ef1200ff8daf150001000000000000000000000000000000000000000000000000000000000000000000000000000000000000dc64a140aa3e981100a9beca4e685f962f0cf6c900000000000000000000000042000000000000000000000000000000000000100000000000000000000000000000000000000000000000000000000000000001000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000001e848000000000000000000000000000000000000000000000000000000000000000e000000000000000000000000000000000000000000000000000000000000000a4f407a99e000000000000000000000000f39fd6e51aad88f6f4ce6ab8827279cfffb92266000000000000000000000000f39fd6e51aad88f6f4ce6ab8827279cfffb922660000000000000000000000000000000000000000000000000000000000000001000000000000000000000000000000000000000000000000000000000000008000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"),
			BlockHash:   common.HexToHash("0x123"),
			BlockNumber: 10,
			TxHash:      common.HexToHash("0x123"),
			TxIndex:     0,
		},
	}
	filterLogsResult := struct {
		Logs []types.Log
		Err  error
	}{
		Logs: log,
		Err:  nil,
	}
	tests := []struct {
		name                   string
		currentStatus          *preconf.OptimismSyncStatus
		newStatus              *preconf.OptimismSyncStatus
		expectStatusUpdate     bool
		expectDepositTxsUpdate bool
		mockLogFilterer        *mockLogFilterer
	}{
		{
			name: "L1 Delay",
			currentStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 10},
				HeadL1:    preconf.L1BlockRef{Number: 15},
				UnsafeL2:  preconf.L2BlockRef{Number: 20, L1Origin: preconf.BlockID{Number: 10}},
			},
			newStatus: &preconf.OptimismSyncStatus{
				CurrentL1: preconf.L1BlockRef{Number: 11},
				HeadL1:    preconf.L1BlockRef{Number: 15},
				UnsafeL2:  preconf.L2BlockRef{Number: 26, L1Origin: preconf.BlockID{Number: 11}},
			},
			expectStatusUpdate:     true,
			expectDepositTxsUpdate: false,
			mockLogFilterer: &mockLogFilterer{
				FilterLogsResult: filterLogsResult,
				WaitTime:         2 * time.Second,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			c := &preconfChecker{
				optimismSyncStatus:   tt.currentStatus,
				optimismSyncStatusOk: true,
				env:                  &environment{},
				envUpdatedAt:         time.Now(),
				depositTxs:           []*types.Transaction{},
				unSealedPreconfTxsCh: make(chan []*types.Transaction),
				minerConfig:          &preconf.DefaultMinerConfig,
			}

			c.l1ethclient = &mockLogFilterer{
				FilterLogsResult: tt.mockLogFilterer.FilterLogsResult,
				WaitTime:         tt.mockLogFilterer.WaitTime,
			}

			originalDepositTxs := c.depositTxs
			originalOptimismSyncStatus := c.optimismSyncStatus

			// Call the function
			c.UpdateOptimismSyncStatus(tt.newStatus)

			// Check if status was updated
			if tt.expectStatusUpdate && (c.optimismSyncStatus == originalOptimismSyncStatus || c.optimismSyncStatus != tt.newStatus) {
				t.Fatalf("UpdateOptimismSyncStatus() did not update status, expected %p, got %p", tt.newStatus, c.optimismSyncStatus)
			}

			if !tt.expectStatusUpdate && reflect.DeepEqual(c.optimismSyncStatus, originalDepositTxs) {
				t.Fatalf("UpdateOptimismSyncStatus() updated status when it shouldn't have, expected %p, got %p, new %p", originalOptimismSyncStatus, c.optimismSyncStatus, tt.newStatus)
			}

			// Check if depositTxs were updated
			if tt.expectDepositTxsUpdate && reflect.DeepEqual(c.depositTxs, originalDepositTxs) {
				t.Fatalf("UpdateOptimismSyncStatus() depositTxs update mismatch, expected update: %v", tt.expectDepositTxsUpdate)
			}
		})
	}
}

// --- Test helpers for baseFee / block-full tests ---

func newU64(v uint64) *uint64 { return &v }

// newArsiaBlockchain creates a *core.BlockChain with MantleArsiaTime=0 (always active)
// and Arsia EIP-1559 params encoded in genesis ExtraData.
func newArsiaBlockchain(t *testing.T) *core.BlockChain {
	t.Helper()
	zero := uint64(0)
	chainConfig := &params.ChainConfig{
		ChainID:             big.NewInt(1337),
		HomesteadBlock:      big.NewInt(0),
		EIP150Block:         big.NewInt(0),
		EIP155Block:         big.NewInt(0),
		EIP158Block:         big.NewInt(0),
		ByzantiumBlock:      big.NewInt(0),
		ConstantinopleBlock: big.NewInt(0),
		PetersburgBlock:     big.NewInt(0),
		IstanbulBlock:       big.NewInt(0),
		MuirGlacierBlock:    big.NewInt(0),
		BerlinBlock:         big.NewInt(0),
		LondonBlock:         big.NewInt(0),
		Ethash:              new(params.EthashConfig),
		MantleArsiaTime:     &zero,
		Optimism: &params.OptimismConfig{
			EIP1559Elasticity:        50,
			EIP1559Denominator:       50,
			EIP1559DenominatorCanyon: newU64(50),
		},
	}
	genesis := &core.Genesis{
		Config:     chainConfig,
		ExtraData:  eip1559.EncodeMinBaseFeeExtraData(50, 50, 0),
		GasLimit:   30_000_000,
		BaseFee:    big.NewInt(params.InitialBaseFee),
		Difficulty: big.NewInt(1),
	}
	chainDB := rawdb.NewMemoryDatabase()
	bc, err := core.NewBlockChain(chainDB, genesis, ethash.NewFaker(), nil)
	if err != nil {
		t.Fatalf("NewBlockChain: %v", err)
	}
	t.Cleanup(bc.Stop)
	return bc
}

// newTestEnv creates a minimal *environment backed by the genesis state of bc.
// gasPoolGas controls the available gas (pass header.GasLimit for a full pool).
func newTestEnv(t *testing.T, bc *core.BlockChain, header *types.Header, gasPoolGas uint64) *environment {
	t.Helper()
	statedb, err := bc.StateAt(bc.Genesis().Header())
	if err != nil {
		t.Fatalf("StateAt: %v", err)
	}
	chainConfig := bc.Config()
	hdr := types.CopyHeader(header)
	blockCtx := core.NewEVMBlockContext(hdr, bc, nil, chainConfig, statedb)
	evm := vm.NewEVM(blockCtx, statedb, chainConfig, vm.Config{})
	gp := core.NewGasPool(gasPoolGas)
	return &environment{
		signer:  types.MakeSigner(chainConfig, hdr.Number, hdr.Time),
		state:   statedb,
		evm:     evm,
		gasPool: gp,
		header:  hdr,
	}
}

// --- Block-full tests: verify virtual block advance is removed ---

func TestApplyTxWithResetEnv_GasLimitReturnsBlockFull(t *testing.T) {
	bc := newArsiaBlockchain(t)

	blobGasUsed := uint64(0)
	header := &types.Header{
		Number:      big.NewInt(11),
		GasLimit:    30_000_000,
		BaseFee:     big.NewInt(params.InitialBaseFee),
		Time:        0,
		Extra:       eip1559.EncodeMinBaseFeeExtraData(50, 50, 0),
		BlobGasUsed: &blobGasUsed,
	}
	// gasPool = 0 → any tx will trigger ErrGasLimitReached
	env := newTestEnv(t, bc, header, 0)
	originalNumber := new(big.Int).Set(env.header.Number)

	checker := &preconfChecker{
		blockchain:  bc,
		env:         env,
		minerConfig: &preconf.DefaultMinerConfig,
	}

	// Create a simple tx (gas required > 0 but pool has 0 gas)
	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	// Fund sender so it passes balance check and hits the gas pool check
	env.state.AddBalance(sender, uint256.NewInt(1e18), tracing.BalanceChangeUnspecified)
	signer := types.NewEIP155Signer(bc.Config().ChainID)
	tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce:    0,
		Gas:      21_000,
		GasPrice: big.NewInt(params.InitialBaseFee),
	}), signer, key)

	_, _, err := checker.applyTxWithResetEnv(env, tx)

	// Must get ErrPreconfBlockFull (wrapping ErrGasLimitReached)
	if !errors.Is(err, ErrPreconfBlockFull) {
		t.Fatalf("expected ErrPreconfBlockFull, got %v", err)
	}
	if !errors.Is(err, core.ErrGasLimitReached) {
		t.Fatalf("expected wrapped ErrGasLimitReached, got %v", err)
	}
	// header.Number must NOT have advanced (no virtual block advance)
	if env.header.Number.Cmp(originalNumber) != 0 {
		t.Fatalf("header.Number must not advance: got %v, want %v", env.header.Number, originalNumber)
	}
}

func TestApplyTxWithResetEnv_DASpaceReturnsBlockFull(t *testing.T) {
	bc := newArsiaBlockchain(t)

	blobGasUsed := uint64(30_000_000) // DA completely saturated
	header := &types.Header{
		Number:      big.NewInt(11),
		GasLimit:    30_000_000,
		BaseFee:     big.NewInt(params.InitialBaseFee),
		Time:        0,
		Extra:       eip1559.EncodeMinBaseFeeExtraData(50, 50, 0),
		BlobGasUsed: &blobGasUsed,
	}
	// Gas pool is fine, but DA is exhausted
	env := newTestEnv(t, bc, header, header.GasLimit)
	env.daFootprintGasScalar = 1 // activate DA check (scalar=0 disables it)
	originalNumber := new(big.Int).Set(env.header.Number)

	checker := &preconfChecker{
		blockchain:  bc,
		env:         env,
		minerConfig: &preconf.DefaultMinerConfig,
	}

	key, _ := crypto.GenerateKey()
	signer := types.NewEIP155Signer(bc.Config().ChainID)
	tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce:    0,
		Gas:      21_000,
		GasPrice: big.NewInt(params.InitialBaseFee),
	}), signer, key)

	_, _, err := checker.applyTxWithResetEnv(env, tx)

	// Must get ErrPreconfBlockFull (wrapping ErrDAFootprintLimitReached)
	if !errors.Is(err, ErrPreconfBlockFull) {
		t.Fatalf("expected ErrPreconfBlockFull, got %v", err)
	}
	if !errors.Is(err, core.ErrDAFootprintLimitReached) {
		t.Fatalf("expected wrapped ErrDAFootprintLimitReached, got %v", err)
	}
	// header.Number must NOT have advanced
	if env.header.Number.Cmp(originalNumber) != 0 {
		t.Fatalf("header.Number must not advance: got %v, want %v", env.header.Number, originalNumber)
	}
}

// --- UnpausePreconf baseFee recalculation test ---

func TestUnpausePreconf_BaseFeeRecalculated(t *testing.T) {
	bc := newArsiaBlockchain(t)
	chainConfig := bc.Config()

	gasLimit := uint64(30_000_000)
	daUsed := gasLimit // BlobGasUsed == GasLimit → parentGasMetered = max(GasUsed, BlobGasUsed)
	arsiaExtra := eip1559.EncodeMinBaseFeeExtraData(50, 50, 0)

	sealedHeader := &types.Header{
		Number:      big.NewInt(10),
		GasLimit:    gasLimit,
		GasUsed:     gasLimit, // 100% utilisation → baseFee must increase
		BlobGasUsed: &daUsed,
		BaseFee:     big.NewInt(params.InitialBaseFee),
		Time:        0,
		Extra:       arsiaExtra,
		Difficulty:  big.NewInt(1),
	}

	// Ground truth: what CalcBaseFee computes for the next block
	expectedBaseFee := eip1559.CalcBaseFee(chainConfig, sealedHeader)
	if expectedBaseFee.Cmp(sealedHeader.BaseFee) == 0 {
		t.Fatal("test setup error: baseFee unchanged — gas params may be wrong")
	}

	// Build environment from the sealed header
	env := newTestEnv(t, bc, sealedHeader, gasLimit)
	// BlobGasUsed must match for CalcBaseFee to read the correct parentGasMetered
	*env.header.BlobGasUsed = daUsed
	env.header.GasUsed = gasLimit

	// Set up the unsealed preconf txs channel (empty, no overflow)
	unSealedCh := make(chan []*types.Transaction, 1)
	unSealedCh <- []*types.Transaction{}

	checker := &preconfChecker{
		blockchain:           bc,
		depositTxs:           nil,
		unSealedPreconfTxsCh: unSealedCh,
		minerConfig:          &preconf.DefaultMinerConfig,
	}

	readyCalled := false
	// UnpausePreconf defers mu.Unlock(), so we must hold the lock
	checker.mu.Lock()
	checker.UnpausePreconf(env, func() { readyCalled = true })

	// preconfReady callback was called
	if !readyCalled {
		t.Error("preconfReady was not called")
	}

	// Block number advanced by 1
	wantNumber := new(big.Int).Add(sealedHeader.Number, common.Big1)
	if checker.env.header.Number.Cmp(wantNumber) != 0 {
		t.Errorf("header.Number: got %v, want %v", checker.env.header.Number, wantNumber)
	}

	// BaseFee on the header is recalculated (not stale)
	if checker.env.header.BaseFee.Cmp(expectedBaseFee) != 0 {
		t.Errorf("header.BaseFee: got %v, want %v",
			checker.env.header.BaseFee, expectedBaseFee)
	}

	// EVM BlockContext.BaseFee is also updated (EVM was rebuilt)
	if checker.env.evm.Context.BaseFee.Cmp(expectedBaseFee) != 0 {
		t.Errorf("evm.Context.BaseFee: got %v, want %v",
			checker.env.evm.Context.BaseFee, expectedBaseFee)
	}

	// BlobGasUsed reset to 0 for new block
	if *checker.env.header.BlobGasUsed != 0 {
		t.Errorf("BlobGasUsed not reset: got %d", *checker.env.header.BlobGasUsed)
	}

	// Gas pool reset to full
	if checker.env.gasPool.Gas() != gasLimit {
		t.Errorf("gasPool.Gas(): got %d, want %d", checker.env.gasPool.Gas(), gasLimit)
	}
}

// --- C1 supplement: non gas/DA errors pass through without wrapping ---

func TestApplyTxWithResetEnv_OtherErrorsPassThrough(t *testing.T) {
	bc := newArsiaBlockchain(t)

	blobGasUsed := uint64(0)
	header := &types.Header{
		Number:      big.NewInt(11),
		GasLimit:    30_000_000,
		BaseFee:     big.NewInt(params.InitialBaseFee),
		Time:        0,
		Extra:       eip1559.EncodeMinBaseFeeExtraData(50, 50, 0),
		BlobGasUsed: &blobGasUsed,
	}
	env := newTestEnv(t, bc, header, header.GasLimit) // gas pool is full

	checker := &preconfChecker{
		blockchain:  bc,
		env:         env,
		minerConfig: &preconf.DefaultMinerConfig,
	}

	// Create a tx with nonce=999 (way ahead of actual nonce 0) → triggers nonce-too-high error
	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	env.state.AddBalance(sender, uint256.NewInt(1e18), tracing.BalanceChangeUnspecified)
	signer := types.NewEIP155Signer(bc.Config().ChainID)
	tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce:    999, // nonce too high
		Gas:      21_000,
		GasPrice: big.NewInt(params.InitialBaseFee),
	}), signer, key)

	_, _, err := checker.applyTxWithResetEnv(env, tx)

	// Must NOT be ErrPreconfBlockFull — should be original error
	if err == nil {
		t.Fatal("expected error for nonce-too-high tx, got nil")
	}
	if errors.Is(err, ErrPreconfBlockFull) {
		t.Fatalf("non gas/DA error should not be wrapped as ErrPreconfBlockFull, got %v", err)
	}
}

// --- C2 supplement: baseFee decreases on low utilisation ---

func TestUnpausePreconf_BaseFeeDecreasesOnLowUtil(t *testing.T) {
	bc := newArsiaBlockchain(t)
	chainConfig := bc.Config()

	gasLimit := uint64(30_000_000)
	daUsed := uint64(0) // empty block
	arsiaExtra := eip1559.EncodeMinBaseFeeExtraData(50, 50, 0)

	// Start with a high baseFee so there's room to decrease
	highBaseFee := new(big.Int).Mul(big.NewInt(params.InitialBaseFee), big.NewInt(10))

	sealedHeader := &types.Header{
		Number:      big.NewInt(10),
		GasLimit:    gasLimit,
		GasUsed:     0, // 0% utilisation → baseFee must decrease
		BlobGasUsed: &daUsed,
		BaseFee:     highBaseFee,
		Time:        0,
		Extra:       arsiaExtra,
		Difficulty:  big.NewInt(1),
	}

	expectedBaseFee := eip1559.CalcBaseFee(chainConfig, sealedHeader)
	if expectedBaseFee.Cmp(sealedHeader.BaseFee) >= 0 {
		t.Fatal("test setup error: baseFee did not decrease — expected lower baseFee for empty block")
	}

	env := newTestEnv(t, bc, sealedHeader, gasLimit)
	*env.header.BlobGasUsed = daUsed
	env.header.GasUsed = 0

	unSealedCh := make(chan []*types.Transaction, 1)
	unSealedCh <- []*types.Transaction{}

	checker := &preconfChecker{
		blockchain:           bc,
		depositTxs:           nil,
		unSealedPreconfTxsCh: unSealedCh,
		minerConfig:          &preconf.DefaultMinerConfig,
	}

	checker.mu.Lock()
	checker.UnpausePreconf(env, func() {})

	// BaseFee must have decreased
	if checker.env.header.BaseFee.Cmp(highBaseFee) >= 0 {
		t.Errorf("header.BaseFee should decrease: got %v, parent was %v",
			checker.env.header.BaseFee, highBaseFee)
	}
	if checker.env.header.BaseFee.Cmp(expectedBaseFee) != 0 {
		t.Errorf("header.BaseFee: got %v, want %v",
			checker.env.header.BaseFee, expectedBaseFee)
	}

	// EVM must also reflect the decreased baseFee
	if checker.env.evm.Context.BaseFee.Cmp(expectedBaseFee) != 0 {
		t.Errorf("evm.Context.BaseFee: got %v, want %v",
			checker.env.evm.Context.BaseFee, expectedBaseFee)
	}
}

// =============================================================================
// Shared harness & helpers
// =============================================================================
//
// Every preconf boundary bug (size, DA-footprint, FIFO) is "admission promised
// something packing cannot deliver". compareAdmitVsPack runs the SAME tx slice
// through both paths so any test can assert: admission (A) == packing (B), same
// txs in the same order, not merely the same count.
//
//   A) admission : preconfChecker.applyTxWithResetEnv  — real-time status to the client
//   B) packing   : Miner.commitFIFOTransactions        — the canonical block at seal

// --- shared helpers ---------------------------------------------------------

// newPreconfCalldataTx builds + signs a fresh-key calldata tx with an explicit gas
// limit (caller owns the gas policy). Funding is separate (see fundSenders).
// data==nil yields a plain value transfer.
func newPreconfCalldataTx(t *testing.T, chainID *big.Int, gas uint64, data []byte) *types.Transaction {
	t.Helper()
	key, _ := crypto.GenerateKey()
	to := common.Address{0x42}
	tx, err := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce:    0,
		To:       &to,
		Value:    big.NewInt(0),
		Gas:      gas,
		GasPrice: big.NewInt(params.InitialBaseFee),
		Data:     data,
	}), types.NewEIP155Signer(chainID), key)
	if err != nil {
		t.Fatalf("SignTx: %v", err)
	}
	return tx
}

// fundSenders gives every tx's sender enough balance to execute, in the given env.
func fundSenders(t *testing.T, env *environment, txs ...*types.Transaction) {
	t.Helper()
	signer := types.NewEIP155Signer(env.evm.ChainConfig().ChainID)
	for _, tx := range txs {
		from, err := types.Sender(signer, tx)
		if err != nil {
			t.Fatalf("recover sender: %v", err)
		}
		env.state.AddBalance(from, uint256.NewInt(1e18), tracing.BalanceChangeUnspecified)
	}
}

// daFootprint is the DA footprint (in gas) a tx consumes under the given scalar —
// what checkTxDAFootprint compares against the budget.
func daFootprint(tx *types.Transaction, scalar uint16) uint64 {
	return tx.RollupCostData().EstimatedDASize().Uint64() * uint64(scalar)
}

// incompressibleCalldata returns n bytes of deterministic pseudo-random data so
// the fastlz-based EstimatedDASize stays above the MinTransactionSize floor
// (zeros would compress down to it).
func incompressibleCalldata(n int) []byte {
	b := make([]byte, n)
	x := uint32(0x12345678)
	for i := range b {
		x = x*1664525 + 1013904223 // LCG
		b[i] = byte(x >> 24)
	}
	return b
}

// txHashSet builds a membership set of the hashes of the txs packed into env.
func txHashSet(env *environment) map[common.Hash]bool {
	set := make(map[common.Hash]bool, len(env.txs))
	for _, tx := range env.txs {
		set[tx.Hash()] = true
	}
	return set
}

func sumSize(txs []*types.Transaction) uint64 {
	var b uint64
	for _, tx := range txs {
		b += tx.Size()
	}
	return b
}

// requireArsiaActive guards against a mis-built header silently disabling the
// Arsia DA-footprint path, which would make a DA/FIFO test pass vacuously.
func requireArsiaActive(t *testing.T, bc *core.BlockChain, env *environment) {
	t.Helper()
	if !bc.Config().IsMantleArsia(env.header.Time) {
		t.Fatalf("setup invalid: MantleArsia not active at Time=%d — the DA-footprint path won't run, the test would be vacuous", env.header.Time)
	}
	if env.header.BlobGasUsed == nil {
		t.Fatal("setup invalid: header.BlobGasUsed is nil — DA accounting requires it")
	}
}

// --- the admit-vs-pack invariant probe --------------------------------------

// pathCompare records which txs admission (A) accepted and packing (B) included,
// so callers can assert identity, not just count.
type pathCompare struct {
	admitted      []*types.Transaction // txs admission accepted, in feed order
	packed        []*types.Transaction // txs packing included, in block order
	admitFirstErr error                // first rejection from admission (nil if none)
	unsealed      int                  // txs packing deferred to the next block
}

// compareAdmitVsPack feeds the SAME txs through preconf admission (path A) and
// block packing (path B) on two independent envs built from the same header,
// gasPool and DA-footprint scalar. Senders are auto-funded in both envs.
func compareAdmitVsPack(t *testing.T, bc *core.BlockChain, header *types.Header, gasPool uint64, scalar uint16, txs []*types.Transaction) pathCompare {
	t.Helper()
	mkEnv := func() *environment {
		env := newTestEnv(t, bc, types.CopyHeader(header), gasPool)
		env.daFootprintGasScalar = scalar
		fundSenders(t, env, txs...)
		return env
	}

	var c pathCompare

	// Path A — admission: feed one-by-one, record what is accepted.
	envA := mkEnv()
	checker := &preconfChecker{blockchain: bc, env: envA, minerConfig: &preconf.DefaultMinerConfig}
	for _, tx := range txs {
		if _, _, err := checker.applyTxWithResetEnv(envA, tx); err == nil {
			c.admitted = append(c.admitted, tx)
		} else if c.admitFirstErr == nil {
			c.admitFirstErr = err
		}
	}

	// Path B — packing: hand the whole slice to the canonical packer.
	envB := mkEnv()
	m := &Miner{chainConfig: bc.Config()}
	unsealed, err := m.commitFIFOTransactions(context.Background(), envB, txs, nil)
	if err != nil {
		t.Fatalf("commitFIFOTransactions: %v", err)
	}
	c.packed = envB.txs
	c.unsealed = len(unsealed)
	return c
}

// assertConsistent fails unless admission (A) accepted exactly the same txs, in
// the same order, that packing (B) included — catching "same count, different
// txs" that a count comparison would miss.
func assertConsistent(t *testing.T, c pathCompare) {
	t.Helper()
	if len(c.admitted) != len(c.packed) {
		t.Fatalf("preconf inconsistency: admission accepted %d tx (%d bytes) but packing included %d tx (%d bytes); %d deferred — A over-promised %d tx the block cannot keep (admitFirstErr=%v)",
			len(c.admitted), sumSize(c.admitted), len(c.packed), sumSize(c.packed), c.unsealed, len(c.admitted)-len(c.packed), c.admitFirstErr)
	}
	for i := range c.admitted {
		if c.admitted[i].Hash() != c.packed[i].Hash() {
			t.Fatalf("preconf inconsistency at index %d: admission accepted tx %s but packing included tx %s — A and B disagree on WHICH txs, not just how many",
				i, c.admitted[i].Hash().Hex(), c.packed[i].Hash().Hex())
		}
	}
}

// =============================================================================
// block-size cap
// =============================================================================
//
// These feed real txs through the admission path (no hand-set env.size) so the
// size cap must trip organically, modelling an unbounded-speed sequencer.
//
// Regression guarded: env.size is accumulated in packing (B) but not admission
// (A), so the size cap never trips in admission and A over-promises.

func sizeTestHeader() *types.Header {
	blobGasUsed := uint64(0)
	return &types.Header{
		Number:      big.NewInt(11),
		GasLimit:    30_000_000,
		BaseFee:     big.NewInt(params.InitialBaseFee),
		Difficulty:  big.NewInt(0), // header.Size() dereferences Difficulty; real headers always set it
		Time:        0,
		Extra:       eip1559.EncodeMinBaseFeeExtraData(50, 50, 0),
		BlobGasUsed: &blobGasUsed,
	}
}

// T1 — after N admitted preconf txs, env.size must equal the sum of their sizes,
// else the cumulative size cap can never trip in admission. Regresses to FAIL if
// applyTx stops doing env.size += tx.Size() (or environment.copy stops carrying it).
func TestPreconf_SizeCap_AccumulatesAcrossTxs(t *testing.T) {
	bc := newArsiaBlockchain(t)
	env := newTestEnv(t, bc, sizeTestHeader(), 30_000_000)
	checker := &preconfChecker{blockchain: bc, env: env, minerConfig: &preconf.DefaultMinerConfig}
	chainID := bc.Config().ChainID

	const n = 5
	var wantSize uint64
	for i := 0; i < n; i++ {
		tx := newPreconfCalldataTx(t, chainID, 21_000, nil) // plain transfer
		fundSenders(t, env, tx)
		if _, _, err := checker.applyTxWithResetEnv(env, tx); err != nil {
			t.Fatalf("tx %d should be admitted, got %v", i, err)
		}
		wantSize += tx.Size()
	}
	if env.size != wantSize {
		t.Fatalf("env.size did not track %d admitted preconf txs: env.size=%d want=%d (admission never accumulates size)",
			n, env.size, wantSize)
	}
}

// T2 — high-throughput overflow via the compareAdmitVsPack probe. Feeds 65 real
// 120KB-calldata txs (~8.0MB, just past the 7.4MB ceiling) through both paths with
// gas headroom so SIZE binds. Correct admission accepts exactly what packing
// includes; without the fix it over-promises. Regresses if admission stops
// accumulating env.size.
func TestPreconf_SizeCap_HighThroughputOverflow(t *testing.T) {
	bc := newArsiaBlockchain(t)
	sizeCap := uint64(params.MaxBlockSize - maxBlockSizeBufferZone) // 7,388,608

	header := sizeTestHeader()
	header.GasLimit = 500_000_000 // gas headroom so SIZE is the binding limit

	const n = 65 // 65 × ~120KB ≈ 8.0MB, just past the 7.4MB ceiling
	calldata := make([]byte, 120*1024)
	txs := make([]*types.Transaction, n)
	for i := range txs {
		// 1M gas: covers the ~512K intrinsic of 120KB calldata, small vs the 500M
		// pool so the FIFO gas reservation never binds.
		txs[i] = newPreconfCalldataTx(t, bc.Config().ChainID, 1_000_000, calldata)
	}

	c := compareAdmitVsPack(t, bc, header, 500_000_000, 0 /*DA disabled*/, txs)
	t.Logf("admission accepted %d tx (%d bytes); packing included %d tx; size cap %d bytes",
		len(c.admitted), sumSize(c.admitted), len(c.packed), sizeCap)

	// admission must accept exactly the txs packing can hold.
	assertConsistent(t, c)
}

// txWithSize builds a preconf calldata tx whose RLP size is ~targetSize bytes
// (calldata len = targetSize - measured per-tx RLP overhead). Used to land the
// cumulative block size precisely on the cap boundary.
func txWithSize(t *testing.T, chainID *big.Int, gas, targetSize uint64) *types.Transaction {
	t.Helper()
	const probeLen = 2048
	overhead := uint64(newPreconfCalldataTx(t, chainID, gas, make([]byte, probeLen)).Size()) - probeLen
	var dataLen uint64
	if targetSize > overhead {
		dataLen = targetSize - overhead
	}
	return newPreconfCalldataTx(t, chainID, gas, make([]byte, dataLen))
}

// Admission and packing must use the same block-size base. Packing seeds size =
// header.Size() via makeEnv; environment.copy drops size, so UnpausePreconf must
// reseed it the same way — a lower preconf base would admit a boundary tx that
// packing defers. Fills to the cap boundary and asserts admission never accepts a
// tx packing rejects. Regresses if the preconf size base diverges from header.Size().
func TestPreconf_SizeCap_AdmissionBaseMatchesPacking(t *testing.T) {
	bc := newArsiaBlockchain(t)
	chainID := bc.Config().ChainID
	sizeCap := uint64(params.MaxBlockSize - maxBlockSizeBufferZone) // 7,388,608
	const (
		hugeGas = uint64(20_000_000_000) // model Mantle's high gas limit → SIZE binds, not gas
		txGas   = uint64(12_000_000)     // per-tx ceiling; covers the calldata floor of 120KB
	)

	// --- admission env: built as production (copy then UnpausePreconf reset). ---
	// admitEnv.size is the preconf size base under test; must equal header.Size().
	sealedHdr := sizeTestHeader()
	sealedHdr.GasLimit = hugeGas
	sealed := newTestEnv(t, bc, sealedHdr, hugeGas)
	// Empty unsealed-tx channel so UnpausePreconf returns immediately (no 1s timeout).
	ch := make(chan []*types.Transaction, 1)
	ch <- nil
	checker := &preconfChecker{blockchain: bc, unSealedPreconfTxsCh: ch, minerConfig: &preconf.DefaultMinerConfig}
	checker.mu.Lock()
	checker.UnpausePreconf(sealed.copy(bc), func() {})
	admitEnv := checker.env
	requireArsiaActive(t, bc, admitEnv)
	preconfBase := admitEnv.size

	// --- packing env: built as makeEnv seeds it — size base = header.Size(). ---
	hb := uint64(admitEnv.header.Size())
	packEnv := newTestEnv(t, bc, types.CopyHeader(admitEnv.header), hugeGas)
	packEnv.size = hb // model makeEnv's size base: uint64(header.Size())

	// --- build a tx sequence that fills to the size-cap boundary ---
	const targetRoom = uint64(6000) // cap headroom left over the tx bodies, before the boundary tx
	calldata120 := make([]byte, 120*1024)
	var seq []*types.Transaction
	for sizeCap-sumSize(seq)-targetRoom >= 125*1024 { // coarse fills; never overshoots the approach window
		seq = append(seq, newPreconfCalldataTx(t, chainID, txGas, calldata120))
	}
	seq = append(seq, txWithSize(t, chainID, txGas, sizeCap-sumSize(seq)-targetRoom)) // approach → room≈targetRoom
	room := sizeCap - sumSize(seq)
	if room < hb*2 || room > targetRoom+2_000 {
		t.Fatalf("setup: room=%d outside workable window (hb=%d, targetRoom=%d)", room, hb, targetRoom)
	}
	boundary := txWithSize(t, chainID, txGas, room-hb/2) // window [room-hb, room): a size-base-0 admission would over-admit here
	if uint64(boundary.Size()) < room-hb || uint64(boundary.Size()) >= room {
		t.Fatalf("setup: boundary size %d not in window [%d,%d)", boundary.Size(), room-hb, room)
	}
	seq = append(seq, boundary)
	boundaryHash := boundary.Hash()

	fundSenders(t, admitEnv, seq...)
	fundSenders(t, packEnv, seq...)

	// --- path A: admission (base = preconfBase) ---
	var admitted []*types.Transaction
	admittedBoundary := false
	for _, tx := range seq {
		if _, _, err := checker.applyTxWithResetEnv(admitEnv, tx); err == nil {
			admitted = append(admitted, tx)
			if tx.Hash() == boundaryHash {
				admittedBoundary = true
			}
		}
	}
	// --- path B: packing (base = hb) ---
	m := &Miner{chainConfig: bc.Config()}
	unsealed, err := m.commitFIFOTransactions(context.Background(), packEnv, seq, nil)
	if err != nil {
		t.Fatalf("commitFIFOTransactions: %v", err)
	}
	packSet := txHashSet(packEnv)

	t.Logf("preconf base=%d  packing base=header.Size()=%d  seq=%d (admitted=%d packed=%d deferred=%d)  boundary: admitted=%v packed=%v",
		preconfBase, hb, len(seq), len(admitted), len(packEnv.txs), len(unsealed), admittedBoundary, packSet[boundaryHash])

	// setup guards: the fill reached the cap (packing deferred the boundary) and
	// admission cleared everything before it.
	if packSet[boundaryHash] {
		t.Fatalf("setup: packing INCLUDED the boundary tx — fill didn't reach the cap (room=%d, hb=%d)", room, hb)
	}
	if len(admitted) < len(seq)-1 {
		t.Fatalf("setup: admission rejected %d tx before the boundary — non-size limit bound? (admitted %d/%d)",
			len(seq)-1-len(admitted), len(admitted), len(seq))
	}

	// admission must not accept a tx that packing rejects.
	for _, tx := range admitted {
		if !packSet[tx.Hash()] {
			t.Fatalf("size-base divergence: admission ACCEPTED tx %s (size %d) that packing REJECTED. "+
				"preconf size base=%d but packing base=header.Size()=%d — admission's size cap is %d bytes more lenient; "+
				"admission over-promised a tx the sealed block defers.",
				tx.Hash().Hex()[:10], tx.Size(), preconfBase, hb, hb-preconfBase)
		}
	}
}

// CROSS-BLOCK GUARD: each block's preconf env must start fresh — no leak of the
// previous block's size/txs/tcount/receipts/sidecars/blobs. environment.copy drops
// size but carries the rest forward, so UnpausePreconf must reset them (else preconf
// receipts inherit the previous block's tx index and cumulative gas). After reset,
// size is reseeded to header.Size() and the rest are empty. Regresses if reset is dropped.
func TestPreconf_UnpausePreconf_ResetsStateAcrossBlocks(t *testing.T) {
	bc := newArsiaBlockchain(t)

	// Block N whose preconf env accumulated state. Set every per-block accumulator
	// environment.copy carries forward (plus size) so the reset is verified field-by-field.
	sealed := newTestEnv(t, bc, sizeTestHeader(), 30_000_000)
	sealed.size = 6_000_000
	sealed.tcount = 7
	sealed.txs = []*types.Transaction{newPreconfCalldataTx(t, bc.Config().ChainID, 21_000, nil)}
	sealed.receipts = []*types.Receipt{{Status: types.ReceiptStatusSuccessful}}
	sealed.sidecars = []*types.BlobTxSidecar{{}}
	sealed.blobs = 3
	sealed.header.GasUsed = 5_000_000 // P2: the CumulativeGasUsed accumulator carried in the header must not leak into N+1

	// Pre-load an empty unsealed-tx channel, mirroring the real Pause→channel→Unpause path,
	// so UnpausePreconf's select returns immediately instead of hitting the 1s timeout branch.
	ch := make(chan []*types.Transaction, 1)
	ch <- nil
	checker := &preconfChecker{blockchain: bc, unSealedPreconfTxsCh: ch, minerConfig: &preconf.DefaultMinerConfig}
	// Advance to the next block. (UnpausePreconf defers mu.Unlock(), so hold the lock first.)
	checker.mu.Lock()
	checker.UnpausePreconf(sealed.copy(bc), func() {})

	// No deposit txs, so the new env must start fresh: size reseeded to header.Size(),
	// every other accumulator empty.
	wantSize := uint64(checker.env.header.Size())
	if checker.env.size != wantSize {
		t.Fatalf("size leaked across blocks: env.size=%d (want header.Size()=%d; block N's accumulation must be gone)", checker.env.size, wantSize)
	}
	if checker.env.tcount != 0 {
		t.Fatalf("tcount leaked across blocks: tcount=%d (want 0)", checker.env.tcount)
	}
	if len(checker.env.txs) != 0 {
		t.Fatalf("tx list leaked across blocks: %d tx(s) carried from block N (want 0)", len(checker.env.txs))
	}
	if len(checker.env.receipts) != 0 {
		t.Fatalf("receipts leaked across blocks: %d carried from block N (want 0)", len(checker.env.receipts))
	}
	if len(checker.env.sidecars) != 0 {
		t.Fatalf("blob sidecars leaked across blocks: %d carried from block N (want 0)", len(checker.env.sidecars))
	}
	if checker.env.blobs != 0 {
		t.Fatalf("blob count leaked across blocks: blobs=%d (want 0)", checker.env.blobs)
	}
	if checker.env.header.GasUsed != 0 {
		t.Fatalf("header.GasUsed leaked across blocks: GasUsed=%d (want 0; block N's total gas must not seed N+1's first-tx receipt CumulativeGasUsed)", checker.env.header.GasUsed)
	}
}

// TestEnvironment_FieldGuard is a tripwire: environment has 14 fields, and every one
// must be considered in BOTH environment.copy (per-tx preconf snapshot) and nextBlockEnv
// (cross-block preconf env). When upstream adds/removes a field this fails, forcing a
// human to route the new field through both — turning the "two places to keep in sync"
// human memory into a machine-enforced check. It touches no real sealing path.
func TestEnvironment_FieldGuard(t *testing.T) {
	const want = 14
	if n := reflect.TypeOf(environment{}).NumField(); n != want {
		t.Fatalf("environment now has %d fields (was %d): route the change through both environment.copy and nextBlockEnv, then update this guard to %d", n, want, n)
	}
}

// TestNextBlockEnv_DoesNotMutateReceiver pins the read-only contract worker.go relies on when
// it hands the just-sealed env to UnpausePreconf by reference (no .copy()). nextBlockEnv must
// build the N+1 env purely from reads; a future edit that mutates env.header in place (as the
// old UnpausePreconf did, e.g. *header.BlobGasUsed = 0) would silently corrupt the sealing env.
func TestNextBlockEnv_DoesNotMutateReceiver(t *testing.T) {
	bc := newArsiaBlockchain(t)
	env := newTestEnv(t, bc, sizeTestHeader(), 30_000_000)
	env.header.GasUsed = 5_000_000
	env.size = 6_000_000
	env.header.Time = 1000
	*env.header.BlobGasUsed = 700_000

	wantNumber := new(big.Int).Set(env.header.Number)
	wantGasUsed := env.header.GasUsed
	wantSize := env.size
	wantTime := env.header.Time
	wantBlobGasUsed := *env.header.BlobGasUsed

	next := env.nextBlockEnv(bc)

	// Child is the next block: number advanced, gas accumulator zeroed, time +interval.
	if got, want := next.header.Number, new(big.Int).Add(wantNumber, common.Big1); got.Cmp(want) != 0 {
		t.Fatalf("child header.Number=%s, want %s", got, want)
	}
	if next.header.GasUsed != 0 {
		t.Fatalf("child header.GasUsed=%d, want 0", next.header.GasUsed)
	}
	if got, want := next.header.Time, wantTime+preconfL2BlockTime; got != want {
		t.Fatalf("child header.Time=%d, want %d (parent+%d)", got, want, preconfL2BlockTime)
	}

	// Receiver must be untouched.
	if env.header.Number.Cmp(wantNumber) != 0 {
		t.Fatalf("nextBlockEnv mutated receiver header.Number: got %s want %s", env.header.Number, wantNumber)
	}
	if env.header.Time != wantTime {
		t.Fatalf("nextBlockEnv mutated receiver header.Time: got %d want %d", env.header.Time, wantTime)
	}
	if env.header.GasUsed != wantGasUsed {
		t.Fatalf("nextBlockEnv mutated receiver header.GasUsed: got %d want %d", env.header.GasUsed, wantGasUsed)
	}
	if env.size != wantSize {
		t.Fatalf("nextBlockEnv mutated receiver size: got %d want %d", env.size, wantSize)
	}
	if *env.header.BlobGasUsed != wantBlobGasUsed {
		t.Fatalf("nextBlockEnv mutated receiver header.BlobGasUsed: got %d want %d (in-place *ptr mutation leaks into the sealed env)", *env.header.BlobGasUsed, wantBlobGasUsed)
	}
}

// TestNextBlockEnv_TimeAdvancesByBlockInterval locks the admission-env timestamp: the env
// represents block N+1, so its timestamp must be sealed N's time + the fixed L2 block interval.
// Otherwise CopyHeader carries N's time verbatim, making admission's TIMESTAMP one block (2s)
// early relative to where the tx actually lands. Behaviour-level: also asserts the EVM block
// context the tx simulates against sees the advanced time (TIMESTAMP opcode reads
// evm.Context.Time). Regresses to FAIL if the +interval is dropped.
func TestNextBlockEnv_TimeAdvancesByBlockInterval(t *testing.T) {
	bc := newArsiaBlockchain(t)
	env := newTestEnv(t, bc, sizeTestHeader(), 30_000_000)
	env.header.Time = 1000 // arbitrary sealed-N timestamp

	next := env.nextBlockEnv(bc)

	want := uint64(1000 + preconfL2BlockTime)
	if got := next.header.Time; got != want {
		t.Fatalf("admission env header.Time=%d, want %d (sealed N.Time + %d)", got, want, preconfL2BlockTime)
	}
	// The EVM block context the admission simulates against must see the advanced time,
	// otherwise the TIMESTAMP opcode would return N's time instead of N+1's.
	if got := next.evm.Context.Time; got != want {
		t.Fatalf("EVM block context Time=%d, want %d (TIMESTAMP opcode would read the wrong block's time)", got, want)
	}
}

// TestPreconf_SizeCap_AccumulatesAcrossCopies mirrors production Preconf, which snapshots
// the admission env with environment.copy() before every tx. copy() dropping the size field
// zeroed the cumulative block-size accumulator on every tx, so the size cap could never trip
// across txs (only a single >7.4MB tx would). Asserts size survives the per-tx copy and keeps
// accumulating from the seeded base. Regresses to FAIL if copy() stops carrying size.
func TestPreconf_SizeCap_AccumulatesAcrossCopies(t *testing.T) {
	bc := newArsiaBlockchain(t)
	env := newTestEnv(t, bc, sizeTestHeader(), 30_000_000)
	checker := &preconfChecker{blockchain: bc, env: env, minerConfig: &preconf.DefaultMinerConfig}
	chainID := bc.Config().ChainID

	base := uint64(env.header.Size()) // makeEnv/UnpausePreconf seed size = header.Size()
	checker.env.size = base

	const n = 5
	wantSize := base
	for i := 0; i < n; i++ {
		tx := newPreconfCalldataTx(t, chainID, 21_000, nil) // unique sender, plain transfer
		// Production Preconf snapshots the env per tx via copy() before applying.
		checker.env = checker.env.copy(bc)
		fundSenders(t, checker.env, tx)
		if _, _, err := checker.applyTxWithResetEnv(checker.env, tx); err != nil {
			t.Fatalf("tx %d should be admitted, got %v", i, err)
		}
		wantSize += tx.Size()
	}
	if checker.env.size != wantSize {
		t.Fatalf("env.size did not accumulate across per-tx copy() snapshots: env.size=%d want=%d (copy() dropping size resets the accumulator every tx)", checker.env.size, wantSize)
	}
}

// TestPreconf_UnpausePreconf_ReceiptCumulativeStartsFromZero locks the downstream effect of
// P2 on the preconf receipt's CumulativeGasUsed field: after advancing to N+1, the first
// admitted tx's receipt CumulativeGasUsed must be its own gas, not block N's total gas plus
// its own. Before the fix, header.GasUsed leaked across blocks and the *usedGas accumulator
// started from block N's total (5,000,000 here).
func TestPreconf_UnpausePreconf_ReceiptCumulativeStartsFromZero(t *testing.T) {
	bc := newArsiaBlockchain(t)

	sealed := newTestEnv(t, bc, sizeTestHeader(), 30_000_000)
	sealed.header.GasUsed = 5_000_000 // block N's total gas — must not seed N+1

	// Empty unsealed-tx channel so UnpausePreconf returns immediately (no 1s timeout).
	ch := make(chan []*types.Transaction, 1)
	ch <- nil
	checker := &preconfChecker{blockchain: bc, unSealedPreconfTxsCh: ch, minerConfig: &preconf.DefaultMinerConfig}
	checker.mu.Lock()
	checker.UnpausePreconf(sealed.copy(bc), func() {})

	if checker.env.header.GasUsed != 0 {
		t.Fatalf("header.GasUsed did not reset entering N+1: GasUsed=%d (want 0)", checker.env.header.GasUsed)
	}

	// Plain transfer (exactly 21,000 gas used, regardless of price). Price it above the
	// recalculated base fee — block N's 5,000,000 gas raises N+1's base fee over InitialBaseFee.
	key, _ := crypto.GenerateKey()
	to := common.Address{0x42}
	tx, err := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce:    0,
		To:       &to,
		Value:    big.NewInt(0),
		Gas:      21_000,
		GasPrice: new(big.Int).Mul(big.NewInt(params.InitialBaseFee), big.NewInt(10)),
	}), types.NewEIP155Signer(bc.Config().ChainID), key)
	if err != nil {
		t.Fatalf("SignTx: %v", err)
	}
	fundSenders(t, checker.env, tx)
	receipt, _, err := checker.applyTxWithResetEnv(checker.env, tx)
	if err != nil {
		t.Fatalf("first tx of N+1 should be admitted, got %v", err)
	}
	if receipt.CumulativeGasUsed != 21_000 {
		t.Fatalf("receipt CumulativeGasUsed=%d, want 21000 (block N's 5,000,000 gas leaked into N+1's first receipt)", receipt.CumulativeGasUsed)
	}
}

// TestPreconf_SizeCap_AdmissionRejectsOverflowWithBlockFull pins the size-cap
// error contract: an overflow tx must be rejected by admission with
// ErrPreconfBlockFull (wrapping core.ErrBlockOversized) — the consistency tests
// check WHICH txs, not the rejection reason. Regresses if admission stops
// accumulating env.size or the error stops wrapping the expected sentinels.
func TestPreconf_SizeCap_AdmissionRejectsOverflowWithBlockFull(t *testing.T) {
	bc := newArsiaBlockchain(t)
	header := sizeTestHeader()
	header.GasLimit = 500_000_000 // gas headroom so SIZE is the binding limit
	env := newTestEnv(t, bc, header, 500_000_000)
	checker := &preconfChecker{blockchain: bc, env: env, minerConfig: &preconf.DefaultMinerConfig}
	chainID := bc.Config().ChainID

	const n = 65 // 65 × ~120KB ≈ 8MB > the 7.4MB cap
	calldata := make([]byte, 120*1024)
	var firstReject error
	rejected := 0
	for i := 0; i < n; i++ {
		tx := newPreconfCalldataTx(t, chainID, 1_000_000, calldata)
		fundSenders(t, env, tx)
		if _, _, err := checker.applyTxWithResetEnv(env, tx); err != nil {
			if firstReject == nil {
				firstReject = err
			}
			rejected++
		}
	}
	if rejected == 0 {
		t.Fatalf("admission accepted all %d×120KB txs (~8MB) — it never rejected the size overflow (env.size not accumulated)", n)
	}
	if !errors.Is(firstReject, ErrPreconfBlockFull) {
		t.Fatalf("overflow rejected with %v, want ErrPreconfBlockFull", firstReject)
	}
	if !errors.Is(firstReject, core.ErrBlockOversized) {
		t.Fatalf("overflow err must wrap core.ErrBlockOversized, got %v", firstReject)
	}
}

// =============================================================================
// DA-footprint cap
// =============================================================================
//
// Multi-tx boundary tests for the preconf DA-footprint cap. Unlike the single-tx,
// pre-saturated TestApplyTxWithResetEnv_DASpaceReturnsBlockFull, these start from an
// empty DA budget (BlobGasUsed=0) and feed many real calldata txs so BlobGasUsed
// accumulates until DA (not gas) binds — the high-throughput sequencer scenario.
//
//   A) admission : preconfChecker.applyTxWithResetEnv
//   B) packing   : Miner.commitFIFOTransactions
//
// Contract: past the block budget the boundary tx must return ErrPreconfBlockFull
// (wrapping core.ErrDAFootprintLimitReached), not a bare internal error, and
// *env.header.BlobGasUsed must never exceed GasLimit.

// daBoundaryHeader is a sizeTestHeader with a GasLimit large enough that gas
// never binds (each calldata tx burns ~intrinsic gas only) so DA is the only
// limit under test. BlobGasUsed starts at 0 (empty DA budget).
func daBoundaryHeader() *types.Header {
	h := sizeTestHeader()
	gl := uint64(60_000_000)
	h.GasLimit = gl
	zero := uint64(0)
	h.BlobGasUsed = &zero
	return h
}

// daPickScalar returns a daFootprintGasScalar whose perTxFootprint binds the 60M
// budget within ~5-15 txs, plus the count K that fits before the boundary.
// budget = GasLimit (BlobGasUsed=0).
func daPickScalar(estimatedDASize, gasLimit uint64) (scalar uint16, fitBefore uint64) {
	// Aim for ~12 txs before the boundary, the 13th overflows. perTx ~= gasLimit/13.
	targetPerTx := gasLimit / 13
	s := targetPerTx / estimatedDASize
	if s == 0 {
		s = 1
	}
	if s > 65535 {
		s = 65535
	}
	scalar = uint16(s)
	perTx := estimatedDASize * uint64(scalar)
	fitBefore = gasLimit / perTx
	return scalar, fitBefore
}

// daBuildTxs builds m identical-calldata txs (so EstimatedDASize is identical) and
// funds their senders in env.
func daBuildTxs(t *testing.T, env *environment, chainID *big.Int, m int) []*types.Transaction {
	t.Helper()
	// 120KB all-0xff calldata: compresses to EstimatedDASize ~1218 (above floor);
	// daPickScalar scales it to the intended ~5M-per-tx DA footprint.
	calldata := bytes.Repeat([]byte{0xff}, 120*1024)
	txs := make([]*types.Transaction, m)
	for i := range txs {
		txs[i] = newPreconfCalldataTx(t, chainID, 5_000_000, calldata) // 5M covers the ~2M intrinsic of 120KB calldata
	}
	fundSenders(t, env, txs...)
	return txs
}

// TestPreconf_DABoundary_AdmissionReturnsBlockFull — PATH A (admission walk).
// Feeds calldata txs one by one through applyTxWithResetEnv; the first K succeed
// (BlobGasUsed grows by estimatedDASize*scalar each) and the boundary tx must
// return an err wrapping both ErrPreconfBlockFull and core.ErrDAFootprintLimitReached.
func TestPreconf_DABoundary_AdmissionReturnsBlockFull(t *testing.T) {
	bc := newArsiaBlockchain(t)

	header := daBoundaryHeader()
	gasLimit := header.GasLimit
	env := newTestEnv(t, bc, header, gasLimit) // full gas pool → DA binds, not gas
	requireArsiaActive(t, bc, env)

	// Build txs first to measure a real EstimatedDASize, then pick a scalar from it.
	// Identical calldata → identical DA size.
	const m = 15 // 15 txs
	txs := daBuildTxs(t, env, bc.Config().ChainID, m)
	estDA := txs[0].RollupCostData().EstimatedDASize().Uint64()
	if estDA < types.MinTransactionSize.Uint64() {
		t.Fatalf("estimatedDASize %d below floor; calldata too small", estDA)
	}
	scalar, fitBefore := daPickScalar(estDA, gasLimit)
	env.daFootprintGasScalar = scalar
	perTx := daFootprint(txs[0], scalar)

	t.Logf("estimatedDASize=%d scalar=%d perTxFootprint=%d gasLimit=%d expectFitBefore=%d",
		estDA, scalar, perTx, gasLimit, fitBefore)
	if fitBefore < 3 || fitBefore > 20 {
		t.Fatalf("boundary not in fast range: fitBefore=%d (tune calldata/scalar)", fitBefore)
	}
	if int(fitBefore)+1 >= m {
		t.Fatalf("not enough txs (%d) to reach boundary at %d", m, fitBefore)
	}

	checker := &preconfChecker{blockchain: bc, env: env, minerConfig: &preconf.DefaultMinerConfig}

	var admitted uint64
	var boundaryErr error
	boundaryIdx := -1
	for i, tx := range txs {
		blobBefore := *env.header.BlobGasUsed
		_, _, err := checker.applyTxWithResetEnv(env, tx)
		if err == nil {
			admitted++
			blobAfter := *env.header.BlobGasUsed
			// Each admitted tx grows BlobGasUsed by ITS OWN footprint — per-tx
			// EstimatedDASize varies by a byte (signature length), so don't compare
			// against txs[0]'s footprint.
			want := daFootprint(tx, scalar)
			if blobAfter-blobBefore != want {
				t.Fatalf("tx %d admitted but BlobGasUsed grew by %d, want %d", i, blobAfter-blobBefore, want)
			}
			// And must never exceed the budget.
			if blobAfter > gasLimit {
				t.Fatalf("tx %d: BlobGasUsed %d exceeded GasLimit %d after admit", i, blobAfter, gasLimit)
			}
			continue
		}
		boundaryErr = err
		boundaryIdx = i
		break
	}

	if boundaryIdx < 0 {
		t.Fatalf("DA boundary never hit after %d txs (admitted=%d, perTx=%d, gasLimit=%d)",
			m, admitted, perTx, gasLimit)
	}
	t.Logf("admitted=%d txs, boundary at idx=%d, BlobGasUsed=%d/%d, boundaryErr=%v",
		admitted, boundaryIdx, *env.header.BlobGasUsed, gasLimit, boundaryErr)

	// The number admitted before the boundary must match the budget math.
	if admitted != fitBefore {
		t.Logf("note: admitted=%d differs from naive fitBefore=%d (min-footprint guard at the tail); still valid", admitted, fitBefore)
	}

	// CORE DA-footprint ASSERTIONS.
	if !errors.Is(boundaryErr, ErrPreconfBlockFull) {
		t.Fatalf("DA-saturation boundary must wrap ErrPreconfBlockFull, got %v (internal/other error instead)", boundaryErr)
	}
	if !errors.Is(boundaryErr, core.ErrDAFootprintLimitReached) {
		t.Fatalf("DA-saturation boundary err must also wrap core.ErrDAFootprintLimitReached, got %v", boundaryErr)
	}
	// Invariant: accumulated DA must never have exceeded the budget.
	if *env.header.BlobGasUsed > gasLimit {
		t.Fatalf("BlobGasUsed %d exceeded GasLimit %d", *env.header.BlobGasUsed, gasLimit)
	}
}

// TestPreconf_DABoundary_PackingStopsAtBudget — PATH B (packing).
// Feeds the same slice to commitFIFOTransactions; it must stop at the DA boundary
// (non-empty unsealed remainder) and BlobGasUsed must never exceed GasLimit.
func TestPreconf_DABoundary_PackingStopsAtBudget(t *testing.T) {
	bc := newArsiaBlockchain(t)

	header := daBoundaryHeader()
	gasLimit := header.GasLimit
	env := newTestEnv(t, bc, header, gasLimit)
	requireArsiaActive(t, bc, env)

	const m = 15 // 15 txs
	txs := daBuildTxs(t, env, bc.Config().ChainID, m)
	estDA := txs[0].RollupCostData().EstimatedDASize().Uint64()
	scalar, fitBefore := daPickScalar(estDA, gasLimit)
	env.daFootprintGasScalar = scalar
	perTx := daFootprint(txs[0], scalar)

	t.Logf("estimatedDASize=%d scalar=%d perTxFootprint=%d gasLimit=%d expectFitBefore=%d",
		estDA, scalar, perTx, gasLimit, fitBefore)
	if fitBefore < 3 || fitBefore > 20 || int(fitBefore)+1 >= m {
		t.Fatalf("boundary not in fast range: fitBefore=%d m=%d", fitBefore, m)
	}

	m2 := &Miner{chainConfig: bc.Config()}
	unsealed, err := m2.commitFIFOTransactions(context.Background(), env, txs, nil)
	if err != nil {
		t.Fatalf("commitFIFOTransactions returned error: %v", err)
	}
	packed := len(txs) - len(unsealed)
	t.Logf("packed=%d unsealed=%d BlobGasUsed=%d/%d", packed, len(unsealed), *env.header.BlobGasUsed, gasLimit)

	// Packing must stop before consuming all txs (DA boundary), leaving a remainder.
	if len(unsealed) == 0 {
		t.Fatalf("packing consumed ALL %d txs but DA budget (%d) should bind at ~%d txs (perTx=%d); nothing unsealed",
			m, gasLimit, fitBefore, perTx)
	}
	// And it must never have over-filled the DA budget.
	if *env.header.BlobGasUsed > gasLimit {
		t.Fatalf("BlobGasUsed %d exceeded GasLimit %d during packing", *env.header.BlobGasUsed, gasLimit)
	}
	// Sanity: the number packed should be in the same ballpark as the admission walk.
	if uint64(packed) > fitBefore {
		t.Fatalf("packed %d > naive budget capacity %d (over-filled DA)", packed, fitBefore)
	}
}

// =============================================================================
// FIFO ordering at the DA-footprint boundary
// =============================================================================
//
// When a large tx (txA) does NOT fit the remaining DA budget, does the sequencer
// BREAK (strict FIFO — txA and everything after it, including a later fitting txB,
// defer to the next block) or SKIP txA and pack txB (a FIFO violation)?
//
// Code under test: commitFIFOTransactions (path B). The DA-fail branch sets
// breakIndex=i and breaks, so unsealedTxs = txs[i:] (txA and everything after).
// A violation would `continue` past txA and pack txB, leaving txA absent.

// TestPreconf_FIFO_DABoundary_StrictBreak feeds an ORDERED FIFO slice:
//
//	[ fill... , txA(large calldata, does NOT fit) , txB(small calldata, WOULD fit) ]
//
// and asserts strict FIFO: neither txA nor txB is sealed, both are returned
// unsealed, and the violation signature (txB packed, txA skipped) never occurs.
func TestPreconf_FIFO_DABoundary_StrictBreak(t *testing.T) {
	bc := newArsiaBlockchain(t)
	chainID := bc.Config().ChainID

	// Large scalar so footprints are big and well-separated for bracketing the DA
	// budget. (footprint = EstimatedDASize * scalar.)
	const scalar = uint16(50_000)

	// covers the largest tx's intrinsic (250KB calldata ~4.1M), small vs the pool
	// so the FIFO gas reservation never binds.
	const txGas = uint64(10_000_000)

	const (
		fillCount  = 10         // fill_0..fill_9
		fillBytes  = 100 * 1024 // 100KB calldata each
		largeBytes = 250 * 1024 // txA: large calldata
		smallBytes = 2 * 1024   // txB: small calldata (footprint fits on its own)
	)

	header := sizeTestHeader()                    // BlobGasUsed ptr to 0
	env := newTestEnv(t, bc, header, 600_000_000) // gas headroom >> any tx gas
	env.daFootprintGasScalar = scalar             // activate DA check (0 disables)
	requireArsiaActive(t, bc, env)

	// --- build the ordered slice first so the budget can be sized to the real
	// measured footprints (FastLZ compression is data-dependent). ---
	txs := make([]*types.Transaction, 0, fillCount+2)
	for i := 0; i < fillCount; i++ {
		txs = append(txs, newPreconfCalldataTx(t, chainID, txGas, incompressibleCalldata(fillBytes)))
	}
	txA := newPreconfCalldataTx(t, chainID, txGas, incompressibleCalldata(largeBytes)) // large — must NOT fit
	txB := newPreconfCalldataTx(t, chainID, txGas, incompressibleCalldata(smallBytes)) // small — WOULD fit on its own
	txs = append(txs, txA, txB)
	fundSenders(t, env, txs...)

	// --- size the DA budget (= GasLimit - BlobGasUsed, controlled via GasLimit) so
	// that after the fills remaining lands between fpB and fpA: txA does NOT fit,
	// txB WOULD. ---
	var fillTotal uint64
	for i := 0; i < fillCount; i++ {
		fillTotal += daFootprint(txs[i], scalar)
	}
	fpA := daFootprint(txA, scalar)
	fpB := daFootprint(txB, scalar)
	if fpB >= fpA {
		t.Fatalf("setup: expected footprint(txB)=%d < footprint(txA)=%d", fpB, fpA)
	}
	remainingTarget := fpB + (fpA-fpB)/2 // between fpB and fpA
	// Mutate env.header, not the local `header` (newTestEnv copies it);
	// commitFIFOTransactions reads env.header for the DA budget at runtime.
	env.header.GasLimit = fillTotal + remainingTarget

	budget := env.header.GasLimit - *env.header.BlobGasUsed
	remainingAfterFill := int64(budget) - int64(fillTotal)

	t.Logf("DA budget=%d  fillTotal=%d (x%d)  remainingAfterFill=%d  footprint(txA)=%d  footprint(txB)=%d",
		budget, fillTotal, fillCount, remainingAfterFill, fpA, fpB)

	// Meaningful only if after the fills txA does NOT fit but txB WOULD. A trip here
	// means the calldata sizing needs adjusting, not a code verdict.
	if remainingAfterFill <= 0 {
		t.Fatalf("setup: fills already exhausted the DA budget (remaining=%d); txA can't be the boundary tx", remainingAfterFill)
	}
	if uint64(remainingAfterFill) >= fpA {
		t.Fatalf("setup: txA (footprint=%d) still fits in remaining budget %d — not a boundary; enlarge txA", fpA, remainingAfterFill)
	}
	if uint64(remainingAfterFill) < fpB {
		t.Fatalf("setup: txB (footprint=%d) would NOT fit either (remaining=%d) — shrink txB so the FIFO/skip distinction is observable", fpB, remainingAfterFill)
	}

	// --- run path B (packing) ---
	m := &Miner{chainConfig: bc.Config()}
	unsealed, err := m.commitFIFOTransactions(context.Background(), env, txs, nil)
	if err != nil {
		t.Fatalf("commitFIFOTransactions: %v", err)
	}

	sealed := txHashSet(env)
	txAHash := txA.Hash()
	txBHash := txB.Hash()
	inSealedA := sealed[txAHash]
	inSealedB := sealed[txBHash]

	inUnsealed := func(h common.Hash) bool {
		for _, tx := range unsealed {
			if tx.Hash() == h {
				return true
			}
		}
		return false
	}

	t.Logf("packed %d/%d txs; unsealed=%d; txA in sealed=%v in unsealed=%v; txB in sealed=%v in unsealed=%v",
		len(env.txs), len(txs), len(unsealed), inSealedA, inUnsealed(txAHash), inSealedB, inUnsealed(txBHash))

	// FIFO-VIOLATION signature: txB packed while txA skipped.
	if inSealedB && !inSealedA {
		t.Fatalf("FIFO violation: txB was packed into the sealed block while the earlier txA was SKIPPED (txA does not fit DA budget). Strict FIFO requires txA's non-fit to defer txB too.")
	}

	// Strict-FIFO expectation: both deferred, both in the unsealed remainder.
	if inSealedA {
		t.Fatalf("txA was packed despite footprint %d > remaining budget %d (DA check should have broken)", fpA, remainingAfterFill)
	}
	if inSealedB {
		t.Fatalf("txB was packed; strict FIFO requires it to be deferred behind the non-fitting txA")
	}
	if !inUnsealed(txAHash) {
		t.Fatalf("txA neither sealed nor unsealed — lost tx")
	}
	if !inUnsealed(txBHash) {
		t.Fatalf("txB neither sealed nor unsealed — strict FIFO must carry it forward with txA")
	}

	// Exact-suffix: exactly the fills are packed and the remainder is exactly
	// [txA, txB] in order — proves the boundary fell at txA (an early fill failure
	// would leave a different suffix yet still pass the membership checks above).
	if len(env.txs) != fillCount {
		t.Fatalf("expected exactly %d fill txs packed, got %d", fillCount, len(env.txs))
	}
	if len(unsealed) != 2 {
		t.Fatalf("expected exactly [txA, txB] unsealed (2), got %d", len(unsealed))
	}
	if unsealed[0].Hash() != txA.Hash() || unsealed[1].Hash() != txB.Hash() {
		t.Fatalf("unsealed suffix is not [txA, txB]")
	}
}
