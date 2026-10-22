package miner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/preconf"
)

// Errors
var (
	ErrEnvNil                                               = errors.New("env is nil")
	ErrOptimismSyncNil                                      = errors.New("optimism sync status is nil")
	ErrOptimismSyncNotOk                                    = errors.New("optimism sync status is not ok")
	ErrEnvTooOld                                            = errors.New("env is too old")
	ErrHeadL1BlockTooOld                                    = errors.New("head l1 block is too old")
	ErrCurrentL1NumberAndHeadL1NumberDistanceTooLarge       = errors.New("current l1 number and head l1 number distance is too large")
	ErrEnvBlockNumberLessThanUnsafeL2BlockNumber            = errors.New("env block number is less than unsafe l2 block number")
	ErrEnvBlockNumberAndUnsafeL2BlockNumberDistanceTooLarge = errors.New("env block number and unsafe l2 block number distance is too large")
	ErrPreconfNotAvailable                                  = errors.New("preconf is not available")
	ErrPreconfBlockFull                                     = errors.New("preconf block is full")
)

const (
	RequestTimeout = 5 * time.Second
	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096
)

type preconfChecker struct {
	mu sync.RWMutex

	minerConfig *preconf.MinerConfig
	blockchain  *core.BlockChain

	// clients
	opnodeClient *http.Client
	l1ethclient  ethereum.LogFilterer

	snapEnv      *environment
	snapTxHash   common.Hash
	env          *environment
	envUpdatedAt time.Time
	lastPauseNow time.Time

	optimismSyncStatus   *preconf.OptimismSyncStatus
	optimismSyncStatusOk bool

	// need pre apply to env
	depositTxs           []*types.Transaction
	unSealedPreconfTxsCh chan []*types.Transaction
}

type updateDepositTxsHeader struct {
	currentL1 uint64
	headL1    uint64
}

func NewPreconfChecker(chain *core.BlockChain, minerConfig *preconf.MinerConfig) *preconfChecker {
	checker := &preconfChecker{
		minerConfig:  minerConfig,
		blockchain:   chain,
		opnodeClient: &http.Client{Timeout: RequestTimeout},
	}
	log.Info("preconf checker", "minner.config", checker.minerConfig.String())
	go checker.loop()
	return checker
}

func (c *preconfChecker) loop() {
	if !c.minerConfig.EnablePreconfChecker {
		log.Info("preconf checker is disabled, skip loop")
		return
	}
	for {
		if err := c.syncOptimismStatus(); err != nil {
			log.Error("Failed to sync optimism status", "err", err)
		}

		preconf.MetricsOpNodeSyncStatus(c.optimismSyncStatus, c.optimismSyncStatusOk)

		time.Sleep(1 * time.Second)
	}
}

func (c *preconfChecker) syncOptimismStatus() error {
	resp, err := c.opnodeClient.Post(c.minerConfig.OptimismNodeHTTP, "application/json", strings.NewReader(`{"jsonrpc":"2.0","method":"optimism_syncStatus","params":[],"id":1}`))
	if err != nil {
		return fmt.Errorf("failed to get optimism sync status from opNode: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read optimism sync status from opNode: %w", err)
	}
	response := &preconf.OptimismSyncStatusResponse{}
	if err := json.Unmarshal(body, response); err != nil {
		return fmt.Errorf("failed to unmarshal optimism sync status: %w", err)
	}

	if response.Error != nil {
		return fmt.Errorf("failed to get optimism sync status from opNode: %v", response.Error)
	}

	// update optimism sync status
	c.UpdateOptimismSyncStatus(response.Result)
	return nil
}

func (c *preconfChecker) GetDepositTxs(start, end uint64) ([]*types.Transaction, error) {
	if c.l1ethclient == nil {
		dialCtx, dialCancel := context.WithTimeout(context.Background(), RequestTimeout)
		defer dialCancel()
		l1ethclient, err := ethclient.DialContext(dialCtx, c.minerConfig.L1RPCHTTP)
		if err != nil {
			return nil, fmt.Errorf("failed to dial l1 rpc: %w", err)
		}
		c.l1ethclient = l1ethclient
	}

	// filter deposit logs from start to end, but not include end
	filterCtx, filterCancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer filterCancel()
	logs, err := c.l1ethclient.FilterLogs(filterCtx, ethereum.FilterQuery{
		FromBlock: big.NewInt(int64(start)),
		ToBlock:   big.NewInt(int64(end)),
		Addresses: []common.Address{common.HexToAddress(c.minerConfig.L1DepositAddress)},
		Topics:    [][]common.Hash{{preconf.DepositEventABIHash}},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to filter logs: %w", err)
	}
	log.Trace("filter deposit tx logs", "start", start, "end", end, "logs", len(logs))

	depositTxs := make([]*types.Transaction, 0)
	for _, log := range logs {
		depositTx, err := preconf.UnmarshalDepositLogEvent(&log)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal deposit log event: %w", err)
		}
		depositTxs = append(depositTxs, types.NewTx(depositTx))
	}

	return depositTxs, nil
}

func (c *preconfChecker) UpdateOptimismSyncStatus(newOptimismSyncStatus *preconf.OptimismSyncStatus) {
	defer preconf.LogIfSlow(time.Now(), "UpdateOptimismSyncStatus", "newOptimismSyncStatus", newOptimismSyncStatus)
	status, statusOk, header := func() (*preconf.OptimismSyncStatus, bool, *updateDepositTxsHeader) {
		c.mu.Lock()
		defer c.mu.Unlock()

		// Initialization
		if c.optimismSyncStatus == nil {
			return newOptimismSyncStatus, true, &updateDepositTxsHeader{newOptimismSyncStatus.UnsafeL2.L1Origin.Number, newOptimismSyncStatus.HeadL1.Number}
		}

		log.Debug("update optimism sync status",
			"current_l1.number", c.optimismSyncStatus.CurrentL1.Number,
			"head_l1.number", c.optimismSyncStatus.HeadL1.Number,
			"unsafe_l2.l1_origin.number", c.optimismSyncStatus.UnsafeL2.L1Origin.Number,
			"unsafe_l2.number", c.optimismSyncStatus.UnsafeL2.Number,
			"new_current_l1.number", newOptimismSyncStatus.CurrentL1.Number,
			"new_head_l1.number", newOptimismSyncStatus.HeadL1.Number,
			"new_unsafe_l2.l1_origin.number", newOptimismSyncStatus.UnsafeL2.L1Origin.Number,
			"new_unsafe_l2.number", newOptimismSyncStatus.UnsafeL2.Number,
		)

		// check optimism sync status
		if c.isSyncStatusOk(newOptimismSyncStatus) {
			// if l1 block changes, update depositTxs
			if c.optimismSyncStatus.UnsafeL2.L1Origin.Number != newOptimismSyncStatus.UnsafeL2.L1Origin.Number ||
				c.optimismSyncStatus.HeadL1.Number != newOptimismSyncStatus.HeadL1.Number {
				return newOptimismSyncStatus, true, &updateDepositTxsHeader{newOptimismSyncStatus.UnsafeL2.L1Origin.Number, newOptimismSyncStatus.HeadL1.Number}
			}
			return newOptimismSyncStatus, true, nil
		} else {
			log.Error("optimism sync status is not ok, l1 reorg?", "old", c.optimismSyncStatus, "new", newOptimismSyncStatus)
			return c.optimismSyncStatus, false, nil
		}
	}()

	// two step lock to reduce lock time
	if header != nil {
		if err := c.updateDepositTxs(header.currentL1, header.headL1); err != nil {
			log.Error("failed to update deposit txs", "err", err)
			c.mu.Lock()
			c.optimismSyncStatusOk = false
			c.mu.Unlock()
			return
		}
	}

	c.mu.Lock()
	c.optimismSyncStatus = status
	c.optimismSyncStatusOk = statusOk
	c.mu.Unlock()

	log.Debug("update optimism sync status", "status", status, "statusOk", statusOk)
}

// check optimism sync status
//
// head_l1.number normal growth
// unsafe_l2.number normal growth
// unsafe_l2.l1_origin.number normal growth
func (c *preconfChecker) isSyncStatusOk(newStatus *preconf.OptimismSyncStatus) bool {
	return newStatus.HeadL1.Number > 0 &&
		c.optimismSyncStatus.HeadL1.Number <= newStatus.HeadL1.Number &&
		c.optimismSyncStatus.UnsafeL2.Number <= newStatus.UnsafeL2.Number &&
		c.optimismSyncStatus.UnsafeL2.L1Origin.Number <= newStatus.UnsafeL2.L1Origin.Number
}

// update depositTxs
// We cannot use `newOptimismSyncStatus.CurrentL1.Number` because it may not have been successfully derived yet, which could cause us to
// miss the deposit transactions for this block. Therefore, we can only use `newOptimismSyncStatus.UnsafeL2.L1Origin.Number`
func (c *preconfChecker) updateDepositTxs(currentL1, headL1 uint64) error {
	defer preconf.LogIfSlow(time.Now(), "updateDepositTxs", "currentL1", currentL1, "headL1", headL1)
	start, end := currentL1+1, headL1-1
	depositTxs, err := c.GetDepositTxs(start, end)
	if err != nil {
		c.depositTxs = nil
		log.Error("failed to get deposit txs", "err", err, "start", start, "end", end)
		preconf.MetricsL1Deposit(false, 0)
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.depositTxs = depositTxs
	preconf.MetricsL1Deposit(true, len(depositTxs))
	log.Debug("update deposit txs", "current_l1.number", currentL1, "head_l1.number", headL1, "start", start, "end", end, "deposit_txs", len(depositTxs))
	return nil
}

func (c *preconfChecker) Preconf(tx *types.Transaction) (*types.Receipt, []byte, error) {
	defer preconf.MetricsPreconfExecuteCost(time.Now())

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.precheck(); err != nil {
		return nil, nil, fmt.Errorf("%w because of %w", ErrPreconfNotAvailable, err)
	}
	log.Trace("preconf", "tx", tx.Hash().Hex(), "nonce", tx.Nonce(), "env.header.Number", c.env.header.Number)

	// First check if the transaction is already in the env.receipts. If so, return it directly.
	// During unpause, unsealed preconf txs are loaded, and the transaction might already be included.
	// This also helps avoid "nonce too low" errors.
	// If a tx is rejected because of nonce too low, it is possible that it has already been included in a block.
	// In this case, check if there is a corresponding receipt in env, and return it if found.
	for _, receipt := range c.env.receipts {
		if receipt.TxHash == tx.Hash() {
			log.Trace("preconf tx already in block", "tx", tx.Hash().Hex())
			return receipt, nil, nil
		}
	}

	c.snapEnv = c.env
	c.snapTxHash = tx.Hash()
	c.env = c.env.copy(c.blockchain)
	// apply tx
	log.Trace("apply tx", "tx", tx.Hash().Hex(), "nonce", tx.Nonce())
	return c.applyTxWithResetEnv(c.env, tx)
}

func (c *preconfChecker) RevertTx(txHash common.Hash) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snapEnv == nil || c.snapTxHash != txHash {
		return fmt.Errorf("no snapshot env or tx hash not match")
	}
	c.env = c.snapEnv
	c.snapEnv = nil
	c.snapTxHash = common.Hash{}
	return nil
}

func (c *preconfChecker) PrecheckStatus() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.precheck(); err != nil {
		log.Warn("precheck status failed", "err", err)
		return fmt.Errorf("%w because of %w", ErrPreconfNotAvailable, err)
	}
	return nil
}

func (c *preconfChecker) precheck() error {
	if c.env == nil {
		return ErrEnvNil
	}

	if c.optimismSyncStatus == nil {
		return ErrOptimismSyncNil
	}

	if !c.optimismSyncStatusOk {
		return ErrOptimismSyncNotOk
	}

	// Not more than MantleToleranceDuration(default 12s) from the last L2Block.
	if time.Since(c.envUpdatedAt) > c.minerConfig.MantleToleranceDuration() {
		log.Warn("envTooOld", "env.header.Number", c.env.header.Number.Uint64(), "envUpdatedAt", c.envUpdatedAt, "time.Since(envUpdatedAt)", time.Since(c.envUpdatedAt), "tolerance", c.minerConfig.MantleToleranceDuration())
		return ErrEnvTooOld
	}

	// Not more than EthToleranceDuration(default 1m48s) from the last L1Block.
	headL1BlockTime := time.Unix(int64(c.optimismSyncStatus.HeadL1.Time), 0)
	if time.Since(headL1BlockTime) > c.minerConfig.EthToleranceDuration() {
		log.Warn("headL1BlockTooOld", "headL1Block.number", c.optimismSyncStatus.HeadL1.Number, "headL1BlockTime", headL1BlockTime, "time.Since(headL1BlockTime)", time.Since(headL1BlockTime), "tolerance", c.minerConfig.EthToleranceDuration())
		return ErrHeadL1BlockTooOld
	}

	// The distance between unsafe_l2.l1_origin.number and head_l1.number should not exceed EthToleranceBlock(default 6)
	if c.optimismSyncStatus.HeadL1.Number-c.optimismSyncStatus.UnsafeL2.L1Origin.Number > c.minerConfig.EthToleranceBlock() {
		log.Warn("currentL1NumberAndHeadL1NumberDistanceTooLarge", "unsafe_l2.l1_origin.number", c.optimismSyncStatus.UnsafeL2.L1Origin.Number, "headL1Number", c.optimismSyncStatus.HeadL1.Number, "tolerance", c.minerConfig.EthToleranceBlock())
		return ErrCurrentL1NumberAndHeadL1NumberDistanceTooLarge
	}

	// env block number should be greater than unsafe l2 block number
	envBlockNumber := c.env.header.Number.Uint64()
	unsafeL2BlockNumber := c.optimismSyncStatus.UnsafeL2.Number
	if envBlockNumber < unsafeL2BlockNumber {
		log.Warn("envBlockNumberLessThanUnsafeL2BlockNumber", "envBlockNumber", envBlockNumber, "unsafeL2BlockNumber", unsafeL2BlockNumber)
		return ErrEnvBlockNumberLessThanUnsafeL2BlockNumber
	}

	// The distance between envblock.number and unsafe_l2.number should not exceed 6.
	// Here's an explanation for why it's 6: a deposit transaction from L1 includes at least 1(l1.header.number-1 rather than l1.header.number-2) L1 block,
	// which corresponds to 6 L2 blocks. Therefore, we can have up to 6 blocks in advance to ensure that preconfirmations are not affected by deposit transactions.
	if envBlockNumber-unsafeL2BlockNumber > c.minerConfig.PreconfBufferBlock {
		// When there are a large number of preconfirmation transactions in the queue, it may cause the future 6 blocks to be
		// filled with preconfirmation transactions. At this point, stop new preconfirmation transactions from entering,
		// because there may be unblocked deposit transactions in future blocks, which cannot be predicted at this time.
		log.Warn("envBlockNumberAndUnsafeL2BlockNumberDistanceTooLarge", "envBlockNumber", c.env.header.Number.Uint64(), "unsafeL2BlockNumber", c.optimismSyncStatus.UnsafeL2.Number, "tolerance", c.minerConfig.EthToleranceBlock())
		return ErrEnvBlockNumberAndUnsafeL2BlockNumberDistanceTooLarge
	}

	return nil
}

// applyTxWithResetEnv applies a transaction. If the preconf block is full, it
// returns ErrPreconfBlockFull without advancing the virtual block number. Block
// number advances happen only in UnpausePreconf, enforcing single-block
// preconfirmation semantics.
func (c *preconfChecker) applyTxWithResetEnv(env *environment, tx *types.Transaction) (*types.Receipt, []byte, error) {
	defer preconf.LogIfSlow(time.Now(), "applyTxWithResetEnv", "tx", tx.Hash().Hex(), "nonce", tx.Nonce())
	receipt, returnData, err := c.applyTx(env, tx)
	if err != nil {
		if errors.Is(err, core.ErrGasLimitReached) || errors.Is(err, core.ErrBlockOversized) || errors.Is(err, core.ErrDAFootprintLimitReached) {
			log.Debug("preconf block full, rejecting tx", "tx", tx.Hash(), "err", err)
			return nil, nil, fmt.Errorf("%w: %w", ErrPreconfBlockFull, err)
		}
		return nil, nil, err
	}
	return receipt, returnData, nil
}

func (c *preconfChecker) applyTx(env *environment, tx *types.Transaction) (*types.Receipt, []byte, error) {
	// OP-Stack: DA footprint check for Jovian (must match commit path to avoid preconf rejection at seal time)
	chainConfig := env.evm.ChainConfig()
	if chainConfig.IsMantleArsia(env.header.Time) && !tx.IsDepositTx() && env.header.BlobGasUsed != nil {
		_, err := checkDAFootprint(env, tx.RollupCostData().EstimatedDASize().Uint64(), tx.Hash())
		if err != nil {
			log.Trace("Preconf applyTx checkDAFootprint failed", "err", err)
			return nil, nil, err
		}
	}

	if !env.txFitsSize(tx) {
		return nil, nil, fmt.Errorf("block size exceeds maximum in preconf: %w", core.ErrBlockOversized)
	}

	env.state.SetTxContext(tx.Hash(), env.tcount)
	var (
		snap = env.state.Snapshot()
		gp   = env.gasPool.Snapshot()
	)
	receipt, returnData, err := applyPreconfTransaction(env.evm, env.gasPool, env.state, env.header, tx, &env.header.GasUsed)
	if err != nil {
		env.state.RevertToSnapshot(snap)
		env.gasPool.Set(gp)
		return nil, nil, err
	}
	env.txs = append(env.txs, tx)
	env.receipts = append(env.receipts, receipt)
	env.tcount++
	env.size += tx.Size() // accumulate block byte-size during preconf admission, mirroring commitTransaction

	// OP-Stack: accumulate DA footprint for Jovian (must match commit path)
	if chainConfig.IsMantleArsia(env.header.Time) && !tx.IsDepositTx() && env.header.BlobGasUsed != nil {
		daFootprint := tx.RollupCostData().EstimatedDASize().Uint64() * uint64(env.daFootprintGasScalar)
		*env.header.BlobGasUsed += daFootprint
	}

	return receipt, returnData, nil
}

// core.ApplyTransaction(env.evm, env.gasPool, env.state, env.header, tx, &env.header.GasUsed)
func applyPreconfTransaction(evm *vm.EVM, gp *core.GasPool, statedb *state.StateDB, header *types.Header, tx *types.Transaction, usedGas *uint64) (receipt *types.Receipt, returnData []byte, err error) {
	// ApplyTransaction
	rules := evm.ChainConfig().Rules(header.Number, false, header.Time)
	msg, err := core.TransactionToMessage(tx, types.MakeSigner(evm.ChainConfig(), header.Number, header.Time), header.BaseFee, &rules)
	if err != nil {
		return nil, nil, err
	}
	tokenRatio := statedb.GetState(types.GasOracleAddr, types.TokenRatioSlot).Big()

	// ApplyTransactionWithEVM
	if hooks := evm.Config.Tracer; hooks != nil {
		if hooks.OnTxStart != nil {
			hooks.OnTxStart(evm.GetVMContext(), tx, msg.From)
		}
		if hooks.OnTxEnd != nil {
			defer func() { hooks.OnTxEnd(receipt, err) }()
		}
	}

	nonce := tx.Nonce()
	if msg.IsDepositTx && evm.ChainConfig().IsOptimismRegolith(evm.Context.Time) {
		nonce = statedb.GetNonce(msg.From)
	}

	// Apply the transaction to the current state (included in the env).
	result, err := core.ApplyMessage(evm, msg, gp)
	if err != nil {
		return nil, nil, err
	}
	if result.Failed() && result.Err != vm.ErrExecutionReverted {
		return nil, result.Return(), result.Err
	}
	blockNumber, blockHash, blockTime := header.Number, header.Hash(), header.Time
	// Update the state with pending changes.
	var root []byte
	if evm.ChainConfig().IsByzantium(blockNumber) {
		evm.StateDB.Finalise(true)
	} else {
		root = statedb.IntermediateRoot(evm.ChainConfig().IsEIP158(blockNumber)).Bytes()
	}
	*usedGas += result.UsedGas

	// Merge the tx-local access event into the "block-local" one, in order to collect
	// all values, so that the witness can be built.
	if statedb.Database().Type().Is(state.TypeUBT) {
		statedb.AccessEvents().Merge(evm.AccessEvents)
	}

	receipt = core.MakeReceipt(evm, result, statedb, blockNumber, blockHash, blockTime, tx, *usedGas, root, evm.ChainConfig(), nonce, tokenRatio)
	return receipt, result.Revert(), nil
}

func (c *preconfChecker) PausePreconf() chan<- []*types.Transaction {
	c.mu.Lock()

	// close old channel to avoid resource leak
	if c.unSealedPreconfTxsCh != nil {
		close(c.unSealedPreconfTxsCh)
	}
	c.unSealedPreconfTxsCh = make(chan []*types.Transaction, 1) // buffer 1 to avoid worker block

	c.lastPauseNow = time.Now()
	return c.unSealedPreconfTxsCh
}

// preconfL2BlockTime is the fixed Mantle L2 block interval in seconds. The admission
// env is block N+1, so its timestamp = sealed N's time + this interval. The value is not
// in params.ChainConfig (it lives in the op-node rollup config), so it is hardcoded here;
// if the L2 block time ever changes this must be updated.
const preconfL2BlockTime = 2

// nextBlockEnv constructs the preconf admission environment for the block after env,
// reusing env's post-execution state. It cannot use makeEnv (which derives from an
// already-committed parent; block N is not committed at this point), so it whitelists
// exactly the fields a fresh block needs. Anything not set here defaults to its zero
// value — the safe direction when upstream adds a field to environment.
//
// nextBlockEnv MUST NOT mutate its receiver: worker.go passes the just-sealed env by
// reference (no .copy()) and relies on this being read-only. The state is deep-copied
// internally and the child header is a fresh CopyHeader, so env is never touched.
func (env *environment) nextBlockEnv(chain core.ChainContext) *environment {
	chainConfig := env.evm.ChainConfig()
	header := types.CopyHeader(env.header)
	// CalcBaseFee reads the parent's Extra/GasUsed/BlobGasUsed under Arsia, so compute it
	// from the unmutated sealed header before zeroing GasUsed on the child.
	header.BaseFee = eip1559.CalcBaseFee(chainConfig, env.header)
	header.Number = new(big.Int).Add(env.header.Number, common.Big1)
	// The admission env represents block N+1; its timestamp = sealed N's time + the fixed
	// L2 block interval. Without this, CopyHeader carries N's time, so admission's TIMESTAMP
	// is one block (2s) early relative to where the tx actually lands. Set it before
	// MakeSigner below so the rebuilt signer is fork-correct for the child's time. Time is the
	// only header field both obtainable and correct here; ParentHash and the roots can't be,
	// since block N is not sealed yet when this env is built.
	header.Time = env.header.Time + preconfL2BlockTime
	header.GasUsed = 0 // P2: reset the CumulativeGasUsed accumulator carried in the header
	if header.BlobGasUsed != nil {
		zero := uint64(0)
		header.BlobGasUsed = &zero
	}

	state := env.state.Copy()
	return &environment{
		// signer is never read on the preconf admission path (applyPreconfTransaction
		// inlines MakeSigner per tx); rebuild it by block height only to keep it valid
		// (non-nil and fork-correct) should anyone read it later.
		signer:               types.MakeSigner(chainConfig, header.Number, header.Time),
		state:                state,
		size:                 uint64(header.Size()),
		gasPool:              core.NewGasPool(header.GasLimit),
		coinbase:             env.coinbase,
		daFootprintGasScalar: env.daFootprintGasScalar,
		header:               header,
		// author=nil preserves current preconf behavior (copy/UnpausePreconf both pass nil;
		// only makeEnv passes &coinbase); the EVM's COINBASE then resolves to header.Coinbase.
		evm: vm.NewEVM(core.NewEVMBlockContext(header, chain, nil, chainConfig, state), state, chainConfig, env.evm.Config),
		// tcount/txs/receipts/sidecars/blobs/witness intentionally left zero — new block.
	}
}

func (c *preconfChecker) UnpausePreconf(env *environment, preconfReady func()) {
	defer preconf.LogIfSlow(c.lastPauseNow, "UnpausePreconf", "env.header.Number", env.header.Number.Int64())
	defer preconf.MetricsPreconfMinerPauseCost(c.lastPauseNow)
	defer c.mu.Unlock()
	// Build the next block's preconf admission env from the just-sealed env. nextBlockEnv
	// whitelists exactly the fields a fresh block needs (Number+1, recalculated BaseFee,
	// GasUsed/size/tx accumulators reset) instead of copy()'s "carry everything then
	// hand-reset" blacklist, which silently leaked size (P1) and header.GasUsed (P2).
	c.env = env.nextBlockEnv(c.blockchain)
	c.envUpdatedAt = time.Now()
	log.Debug("unpause preconf", "env.header.Number", c.env.header.Number.Int64(), "env.gasPool", c.env.gasPool, "envUpdatedAt", c.envUpdatedAt)

	// Load deposit txs
	log.Trace("apply deposit txs", "deposit_txs", len(c.depositTxs))
	for _, tx := range c.depositTxs {
		if _, _, err := c.applyTx(c.env, tx); err != nil {
			log.Warn("failed to apply deposit tx", "err", err, "tx", tx.Hash().Hex())
			continue
		}
		log.Trace("applied deposit tx", "tx", tx.Hash().Hex(), "nonce", tx.Nonce())
	}

	// Load unsealed preconf txs
	var unsealedPreconfTxs []*types.Transaction
	select {
	case unsealedPreconfTxs = <-c.unSealedPreconfTxsCh:
	case <-time.After(1 * time.Second):
		log.Error("no received unsealed preconf txs to apply, please report this issue")
	}
	log.Trace("apply unsealed preconf txs", "count", len(unsealedPreconfTxs))
	for _, tx := range unsealedPreconfTxs {
		receipt, _, err := c.applyTxWithResetEnv(c.env, tx)
		if err != nil {
			log.Warn("failed to apply unsealed preconf tx", "err", err, "tx", tx.Hash().Hex())
			continue
		}
		log.Trace("applied unsealed preconf tx", "tx", tx.Hash().Hex(), "nonce", tx.Nonce(), "env.gasPool", c.env.gasPool.Gas(), "receipt.status", receipt.Status)
	}

	// Metrics (c.env is the new N+1 env; the input env is the sealed block N and no
	// longer mutated in place, so report block numbers from c.env).
	preconf.MetricsOpGethEnvBlockNumber(c.env.header.Number.Int64())

	// notify txpool that preconf is ready
	preconfReady()

	log.Info("ready to preconf", "env.header.Number", c.env.header.Number.Int64(), "env.gasPool", c.env.gasPool, "envUpdatedAt", c.envUpdatedAt, "deposit_txs", len(c.depositTxs), "unsealed_preconf_txs", len(unsealedPreconfTxs))
}
