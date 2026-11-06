// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
	"golang.org/x/crypto/sha3"
)

var (
	BVM_ETH_ADDR     = common.HexToAddress("0xdEAddEaDdeadDEadDEADDEAddEADDEAddead1111")
	LEGACY_ERC20_MNT = common.HexToAddress("0xdEAddEaDdeadDEadDEADDEAddEADDEAddead0000")
)

// L2ProxyAdmin contract upgrade constants
var (
	// L2ProxyAdminAddress is the address of the L2ProxyAdmin contract
	// predeploy
	L2ProxyAdminAddress = common.HexToAddress("0x4200000000000000000000000000000000000018")

	// OwnerSlot refers to the storage slot in the L2ProxyAdmin contract that
	// holds the owner of the contract
	OwnerSlot = common.BigToHash(big.NewInt(0))

	// NewProxyAdminOwnerAddress is the address that the L2ProxyAdmin contract
	// will be transferred to
	NewProxyAdminOwnerAddress = common.HexToHash("0x09734bB3980906Bb217305EA6Bd34256feEAB105")
)

// ExecutionResult includes all output after executing given evm
// message no matter the execution itself is successful or not.
type ExecutionResult struct {
	UsedGas    uint64 // Total used gas, refunded gas is deducted
	MaxUsedGas uint64 // Maximum gas consumed during execution, excluding gas refunds.
	Err        error  // Any error encountered during the execution(listed in core/vm/errors.go)
	ReturnData []byte // Returned data from evm(function result or data supplied with revert opcode)
}

// Unwrap returns the internal evm error which allows us for further
// analysis outside.
func (result *ExecutionResult) Unwrap() error {
	return result.Err
}

// Failed returns the indicator whether the execution is successful or not
func (result *ExecutionResult) Failed() bool { return result.Err != nil }

// Return is a helper function to help caller distinguish between revert reason
// and function return. Return returns the data after execution if no error occurs.
func (result *ExecutionResult) Return() []byte {
	if result.Err != nil {
		return nil
	}
	return common.CopyBytes(result.ReturnData)
}

// Revert returns the concrete revert reason if the execution is aborted by `REVERT`
// opcode. Note the reason can be nil if no data supplied with revert opcode.
func (result *ExecutionResult) Revert() []byte {
	if result.Err != vm.ErrExecutionReverted {
		return nil
	}
	return common.CopyBytes(result.ReturnData)
}

// IntrinsicGas computes the 'intrinsic gas' for a message with the given data.
func IntrinsicGas(data []byte, accessList types.AccessList, authList []types.SetCodeAuthorization, isContractCreation, isHomestead, isEIP2028, isEIP3860, isAmsterdam bool) (vm.GasCosts, error) {
	// Set the starting gas for the raw transaction
	var gas uint64
	if isContractCreation && isHomestead {
		gas = params.TxGasContractCreation
	} else {
		gas = params.TxGas
	}
	dataLen := uint64(len(data))
	// Bump the required gas by the amount of transactional data
	if dataLen > 0 {
		// Zero and non-zero bytes are priced differently
		z := uint64(bytes.Count(data, []byte{0}))
		nz := dataLen - z

		// Make sure we don't exceed uint64 for all data combinations
		nonZeroGas := params.TxDataNonZeroGasFrontier
		if isEIP2028 {
			nonZeroGas = params.TxDataNonZeroGasEIP2028
		}
		if (math.MaxUint64-gas)/nonZeroGas < nz {
			return vm.GasCosts{}, ErrGasUintOverflow
		}
		gas += nz * nonZeroGas

		if (math.MaxUint64-gas)/params.TxDataZeroGas < z {
			return vm.GasCosts{}, ErrGasUintOverflow
		}
		gas += z * params.TxDataZeroGas

		if isContractCreation && isEIP3860 {
			lenWords := toWordSize(dataLen)
			if (math.MaxUint64-gas)/params.InitCodeWordGas < lenWords {
				return vm.GasCosts{}, ErrGasUintOverflow
			}
			gas += lenWords * params.InitCodeWordGas
		}
	}
	if accessList != nil {
		addresses := uint64(len(accessList))
		storageKeys := uint64(accessList.StorageKeys())
		if (math.MaxUint64-gas)/params.TxAccessListAddressGas < addresses {
			return vm.GasCosts{}, ErrGasUintOverflow
		}
		gas += addresses * params.TxAccessListAddressGas
		if (math.MaxUint64-gas)/params.TxAccessListStorageKeyGas < storageKeys {
			return vm.GasCosts{}, ErrGasUintOverflow
		}
		gas += storageKeys * params.TxAccessListStorageKeyGas

		// EIP-7981: access list data is charged in addition to the base charge.
		if isAmsterdam {
			const (
				addressCost    = common.AddressLength * params.TxCostFloorPerToken7976 * params.TxTokenPerNonZeroByte
				storageKeyCost = common.HashLength * params.TxCostFloorPerToken7976 * params.TxTokenPerNonZeroByte
			)
			if (math.MaxUint64-gas)/addressCost < addresses {
				return vm.GasCosts{}, ErrGasUintOverflow
			}
			gas += addresses * addressCost
			if (math.MaxUint64-gas)/storageKeyCost < storageKeys {
				return vm.GasCosts{}, ErrGasUintOverflow
			}
			gas += storageKeys * storageKeyCost
		}
	}
	if authList != nil {
		gas += uint64(len(authList)) * params.CallNewAccountGas
	}
	return vm.GasCosts{RegularGas: gas}, nil
}

// FloorDataGas computes the minimum gas required for a transaction based on its data tokens (EIP-7623).
func FloorDataGas(rules params.Rules, data []byte, accessList types.AccessList) (uint64, error) {
	var (
		tokens    uint64
		tokenCost uint64
	)
	if rules.IsAmsterdam {
		// EIP-7976 changes how calldata is priced.
		// From 10/40 to 64/64 for zero/non-zero bytes.
		tokenCost = params.TxCostFloorPerToken7976
		dataLen := uint64(len(data))
		if math.MaxUint64/params.TxTokenPerNonZeroByte < dataLen {
			return 0, ErrGasUintOverflow
		}
		tokens = dataLen * params.TxTokenPerNonZeroByte

		// EIP-7981 adds additional tokens for every entry in the accesslist
		const addressTokenCost = uint64(common.AddressLength) * params.TxTokenPerNonZeroByte
		addresses := uint64(len(accessList))
		if (math.MaxUint64-tokens)/addressTokenCost < addresses {
			return 0, ErrGasUintOverflow
		}
		tokens += addresses * addressTokenCost

		const storageKeyTokenCost = uint64(common.HashLength) * params.TxTokenPerNonZeroByte
		storageKeys := uint64(accessList.StorageKeys())
		if (math.MaxUint64-tokens)/storageKeyTokenCost < storageKeys {
			return 0, ErrGasUintOverflow
		}
		tokens += storageKeys * storageKeyTokenCost
	} else {
		var (
			z  = uint64(bytes.Count(data, []byte{0}))
			nz = uint64(len(data)) - z
		)
		// Pre-Amsterdam
		if math.MaxUint64/params.TxTokenPerNonZeroByte < nz {
			return 0, ErrGasUintOverflow
		}
		tokens = nz * params.TxTokenPerNonZeroByte
		if math.MaxUint64-tokens < z {
			return 0, ErrGasUintOverflow
		}
		tokens += z
		tokenCost = params.TxCostFloorPerToken
	}

	// Check for overflow
	if (math.MaxUint64-params.TxGas)/tokenCost < tokens {
		return 0, ErrGasUintOverflow
	}
	// Minimum gas required for a transaction based on its data tokens (EIP-7623).
	return params.TxGas + tokens*tokenCost, nil
}

// toWordSize returns the ceiled word size required for init code payment calculation.
func toWordSize(size uint64) uint64 {
	if size > math.MaxUint64-31 {
		return math.MaxUint64/32 + 1
	}

	return (size + 31) / 32
}

type RunMode uint8

const (
	CommitMode RunMode = iota
	GasEstimationMode
	GasEstimationWithSkipCheckBalanceMode
	EthcallMode
)

// A Message contains the data derived from a single transaction that is relevant to state
// processing.
type Message struct {
	To                    *common.Address
	From                  common.Address
	Nonce                 uint64
	Value                 *uint256.Int
	GasLimit              uint64
	GasPrice              *uint256.Int
	GasFeeCap             *uint256.Int
	GasTipCap             *uint256.Int
	Data                  []byte
	AccessList            types.AccessList
	BlobGasFeeCap         *uint256.Int
	BlobHashes            []common.Hash
	SetCodeAuthorizations []types.SetCodeAuthorization

	// When SkipNonceChecks is true, the message nonce is not checked against the
	// account nonce in state.
	//
	// This field will be set to true for operations like RPC eth_call
	// or the state prefetching.
	SkipNonceChecks bool

	// When set, the message is not treated as a transaction, and certain
	// transaction-specific checks are skipped:
	//
	// - From is not verified to be an EOA
	// - GasLimit is not checked against the protocol defined tx gaslimit
	SkipTransactionChecks bool

	IsSystemTx     bool                 // IsSystemTx indicates the message, if also a deposit, does not emit gas usage.
	IsDepositTx    bool                 // IsDepositTx indicates the message is force-included and can persist a mint.
	Mint           *big.Int             // Mint is the amount to mint before EVM processing, or nil if there is no minting.
	ETHValue       *big.Int             // ETHValue is the amount to mint BVM_ETH before EVM processing, or nil if there is no minting.
	ETHTxValue     *big.Int             // ETHTxValue is the amount to be transferred to msg.To before EVM processing, and the transfer will be reverted if EVM failed
	MetaTxParams   *types.MetaTxParams  // MetaTxParams contains necessary parameter to sponsor gas fee for msg.From.
	RollupCostData types.RollupCostData // RollupCostData indicates the rollup cost of the message, 0 if not a rollup or no cost.

	// runMode
	RunMode RunMode
}

// TransactionToMessage converts a transaction into a Message.
func TransactionToMessage(tx *types.Transaction, s types.Signer, baseFee *big.Int, rules *params.Rules) (*Message, error) {
	if rules == nil {
		return nil, errors.New("param rules is nil pointer")
	}
	metaTxParams, err := types.DecodeAndVerifyMetaTxParams(tx, rules.IsMetaTxV2, rules.IsMetaTxV3, rules.IsMantleEverest)
	if err != nil {
		return nil, err
	}

	from, err := types.Sender(s, tx)
	if err != nil {
		return nil, err
	}
	gasPrice, overflow := uint256.FromBig(tx.GasPrice())
	if overflow {
		return nil, fmt.Errorf("%w: address %v, maxFeePerGas bit length: %d", ErrFeeCapVeryHigh,
			from.Hex(), tx.GasPrice().BitLen())
	}
	txGasFeeCap := tx.GasFeeCap()
	gasFeeCap, overflow := uint256.FromBig(txGasFeeCap)
	if overflow {
		return nil, fmt.Errorf("%w: address %v, maxFeePerGas bit length: %d", ErrFeeCapVeryHigh,
			from.Hex(), tx.GasFeeCap().BitLen())
	}
	txGasTipCap := tx.GasTipCap()
	gasTipCap, overflow := uint256.FromBig(txGasTipCap)
	if overflow {
		return nil, fmt.Errorf("%w: address %v, maxPriorityFeePerGas bit length: %d", ErrTipVeryHigh,
			from.Hex(), tx.GasTipCap().BitLen())
	}
	value, overflow := uint256.FromBig(tx.Value())
	if overflow {
		return nil, fmt.Errorf("value exceeds 256 bits: address %v", from.Hex())
	}
	blobGasFeeCap, overflow := uint256.FromBig(tx.BlobGasFeeCap())
	if overflow {
		return nil, fmt.Errorf("blobGasFeeCap exceeds 256 bits: address %v", from.Hex())
	}

	msg := &Message{
		From:                  from,
		Nonce:                 tx.Nonce(),
		GasLimit:              tx.Gas(),
		GasPrice:              gasPrice,
		GasFeeCap:             gasFeeCap,
		GasTipCap:             gasTipCap,
		To:                    tx.To(),
		Value:                 value,
		Data:                  tx.Data(),
		AccessList:            tx.AccessList(),
		SetCodeAuthorizations: tx.SetCodeAuthorizations(),
		SkipNonceChecks:       false,
		SkipTransactionChecks: false,
		BlobHashes:            tx.BlobHashes(),
		BlobGasFeeCap:         blobGasFeeCap,

		IsSystemTx:     tx.IsSystemTx(),
		IsDepositTx:    tx.IsDepositTx(),
		Mint:           tx.Mint(),
		RollupCostData: tx.RollupCostData(),
		ETHValue:       tx.ETHValue(),
		ETHTxValue:     tx.ETHTxValue(),
		MetaTxParams:   metaTxParams,
		RunMode:        CommitMode,
	}
	// If baseFee provided, set gasPrice to effectiveGasPrice.
	if baseFee != nil {
		effectiveGasPrice := new(big.Int).Add(baseFee, txGasTipCap)
		if effectiveGasPrice.Cmp(txGasFeeCap) > 0 {
			effectiveGasPrice = txGasFeeCap
		}
		// EffectiveGasPrice is already capped by txGasFeeCap, therefore
		// the overflow check is not required.
		msg.GasPrice = uint256.MustFromBig(effectiveGasPrice)
	}
	return msg, nil
}

// ApplyMessage computes the new state by applying the given message
// against the old state within the environment.
//
// ApplyMessage returns the bytes returned by any EVM execution (if it took place),
// the gas used (which includes gas refunds) and an error if it failed. An error always
// indicates a core error meaning that the message would always fail for that particular
// state and would never be accepted within a block.
func ApplyMessage(evm *vm.EVM, msg *Message, gp *GasPool) (*ExecutionResult, error) {
	// Do not panic if the gas pool is nil. This is allowed when executing
	// a single message via RPC invocation.
	if gp == nil {
		gp = NewGasPool(msg.GasLimit)
	}
	evm.SetTxContext(NewEVMTxContext(msg))
	return newStateTransition(evm, msg, gp).execute()
}

// stateTransition represents a state transition.
//
// == The State Transitioning Model
//
// A state transition is a change made when a transaction is applied to the current world
// state. The state transitioning model does all the necessary work to work out a valid new
// state root.
//
//  1. Nonce handling
//  2. Pre pay gas
//  3. Create a new state object if the recipient is nil
//  4. Value transfer
//
// == If contract creation ==
//
//	4a. Attempt to run transaction data
//	4b. If valid, use result as code for the new state object
//
// == end ==
//
//  5. Run Script section
//  6. Derive new state root
type stateTransition struct {
	gp            *GasPool
	msg           *Message
	initialBudget vm.GasBudget
	gasRemaining  vm.GasBudget
	state         vm.StateDB
	evm           *vm.EVM

	initialSponsorValue *uint256.Int
}

// newStateTransition initialises and returns a new state transition object.
func newStateTransition(evm *vm.EVM, msg *Message, gp *GasPool) *stateTransition {
	return &stateTransition{
		gp:                  gp,
		evm:                 evm,
		msg:                 msg,
		state:               evm.StateDB,
		initialSponsorValue: uint256.NewInt(0),
	}
}

// to returns the recipient of the message.
func (st *stateTransition) to() common.Address {
	if st.msg == nil || st.msg.To == nil /* contract creation */ {
		return common.Address{}
	}
	return *st.msg.To
}

// CalculateRollupCostDataFromMessage calculate RollupCostData from message.
func (st *stateTransition) CalculateRollupCostDataFromMessage() {
	tx := types.NewTx(&types.DynamicFeeTx{
		Nonce:     st.msg.Nonce,
		Value:     st.msg.Value.ToBig(),
		Gas:       st.msg.GasLimit,
		GasTipCap: st.msg.GasTipCap.ToBig(),
		GasFeeCap: st.msg.GasFeeCap.ToBig(),
		Data:      st.msg.Data,
	})

	st.msg.RollupCostData = tx.RollupCostData()

	// add a constant to cover sigs(V,R,S) and other data to make sure that the gasLimit from eth_estimateGas can cover L1 cost
	// just used for estimateGas and the actual L1 cost depends on users' tx when executing
	st.msg.RollupCostData.Ones += 80

	// add a constant to cover meta tx sigs(V,R,S)
	if st.msg.MetaTxParams != nil {
		st.msg.RollupCostData.Ones += 80
	}
}

func (st *stateTransition) buyGas() (*big.Int, error) {
	if !st.evm.ChainConfig().IsMetaTxV3(st.evm.Context.Time) {
		if err := st.applyMetaTransaction(); err != nil {
			return nil, err
		}
	}
	if st.msg.RunMode == GasEstimationMode || st.msg.RunMode == GasEstimationWithSkipCheckBalanceMode {
		st.CalculateRollupCostDataFromMessage()
	}

	mgval := new(uint256.Int).SetUint64(st.msg.GasLimit)
	_, overflow := mgval.MulOverflow(mgval, st.msg.GasPrice)
	if overflow {
		return nil, fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
	}

	var l1Cost *big.Int
	var operatorCost *uint256.Int
	isMantleArsia := st.evm.ChainConfig().IsMantleArsia(st.evm.Context.Time)
	if st.evm.Context.L1CostFunc != nil && st.msg.RunMode != EthcallMode && (!st.msg.SkipTransactionChecks || !isMantleArsia) {
		l1Cost = st.evm.Context.L1CostFunc(st.msg.RollupCostData, st.evm.Context.Time)
		// Before Arsia: Gas paid for l1 cost is not bought in advance but is included in gas used calculation
		//               which decides how much gas to refund.
		// After Arsia:  Gas paid for l1 cost is bought in advance and is excluded from gas used calculation.
		//               It means that pre-bought gas for l1 cost is not refunded.(But pre-bought gas for operator
		//               cost is calculated based on gas limit so it will be refunded according to gas used.)
		if isMantleArsia {
			if l1Cost != nil {
				if _, overflow := mgval.AddOverflow(mgval, uint256.MustFromBig(l1Cost)); overflow {
					return nil, fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
				}
			}
			if st.evm.Context.OperatorCostFunc != nil {
				operatorCost = st.evm.Context.OperatorCostFunc(st.msg.GasLimit, st.evm.Context.Time)
				if _, overflow := mgval.AddOverflow(mgval, operatorCost); overflow {
					return nil, fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
				}
			}
		}
	}

	balanceCheck := new(uint256.Int).Set(mgval)
	if st.msg.GasFeeCap != nil {
		balanceCheck.SetUint64(st.msg.GasLimit)
		if _, overflow := balanceCheck.MulOverflow(balanceCheck, st.msg.GasFeeCap); overflow {
			return nil, fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
		}

		// Before Arsia: Gas limit is a hard cap(xGasFeeCap) for total tx cost.
		// After Arsia:  Gas limit only limits the l2 gas used.
		if isMantleArsia {
			if l1Cost != nil {
				if _, overflow := balanceCheck.AddOverflow(balanceCheck, uint256.MustFromBig(l1Cost)); overflow {
					return nil, fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
				}
			}
			if operatorCost != nil {
				if _, overflow := balanceCheck.AddOverflow(balanceCheck, operatorCost); overflow {
					return nil, fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
				}
			}
		}
	}
	if st.msg.Value != nil {
		if _, overflow := balanceCheck.AddOverflow(balanceCheck, st.msg.Value); overflow {
			return nil, fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
		}
	}

	if st.evm.ChainConfig().IsCancun(st.evm.Context.BlockNumber, st.evm.Context.Time) {
		if blobGas := st.blobGasUsed(); blobGas > 0 {
			// Check that the user has enough funds to cover blobGasUsed * tx.BlobGasFeeCap
			blobBalanceCheck := new(uint256.Int).SetUint64(blobGas)
			if _, overflow := blobBalanceCheck.MulOverflow(blobBalanceCheck, st.msg.BlobGasFeeCap); overflow {
				return nil, fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
			}
			if _, overflow := balanceCheck.AddOverflow(balanceCheck, blobBalanceCheck); overflow {
				return nil, fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
			}
			// Pay for blobGasUsed * actual blob fee
			blobBaseFee, overflow := uint256.FromBig(st.evm.Context.BlobBaseFee)
			if overflow {
				return nil, fmt.Errorf("invalid blobBaseFee: %v", st.evm.Context.BlobBaseFee)
			}
			blobFee := new(uint256.Int).SetUint64(blobGas)

			// In practice, overflow checking is unnecessary, as blobBaseFee cannot exceed
			// BlobGasFeeCap. However, in eth_call it is still possible for users to specify
			// an excessively large blob base fee and bypass the blob base fee validation.
			_, overflow = blobFee.MulOverflow(blobFee, blobBaseFee)
			if overflow {
				return nil, fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
			}
			_, overflow = mgval.AddOverflow(mgval, blobFee)
			if overflow {
				return nil, fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
			}
		}
	}

	if st.msg.RunMode != GasEstimationWithSkipCheckBalanceMode && st.msg.RunMode != EthcallMode {
		if st.msg.MetaTxParams != nil {
			pureGasFeeValue := new(uint256.Int).Sub(balanceCheck, st.msg.Value)
			sponsorAmount, selfPayAmount := types.CalculateSponsorPercentAmount(st.msg.MetaTxParams, pureGasFeeValue)
			if have, want := st.state.GetBalance(st.msg.MetaTxParams.GasFeeSponsor), sponsorAmount; have.Cmp(want) < 0 {
				return nil, fmt.Errorf("%w: gas fee sponsor %v have %v want %v", ErrInsufficientFunds, st.msg.MetaTxParams.GasFeeSponsor.Hex(), have, want)
			}

			selfPayAmount = new(uint256.Int).Add(selfPayAmount, st.msg.Value)
			if have, want := st.state.GetBalance(st.msg.From), selfPayAmount; have.Cmp(want) < 0 {
				return nil, fmt.Errorf("%w: address %v have %v want %v", ErrInsufficientFunds, st.msg.From.Hex(), have, want)
			}

			if st.msg.MetaTxParams.GasFeeSponsor == st.msg.From {
				if have, want := st.state.GetBalance(st.msg.From), pureGasFeeValue; have.Cmp(want) < 0 {
					return nil, fmt.Errorf("%w: address %v have %v want %v", ErrInsufficientFunds, st.msg.From.Hex(), have, want)
				}
			}
		} else {
			if have, want := st.state.GetBalance(st.msg.From), balanceCheck; have.Cmp(want) < 0 {
				return nil, fmt.Errorf("%w: address %v have %v want %v", ErrInsufficientFunds, st.msg.From.Hex(), have, want)
			}
		}
	}

	if err := st.gp.SubGas(st.msg.GasLimit); err != nil {
		return nil, err
	}

	if st.evm.Config.Tracer != nil && st.evm.Config.Tracer.OnGasChange != nil {
		st.evm.Config.Tracer.OnGasChange(0, st.msg.GasLimit, tracing.GasChangeTxInitialBalance)
	}
	st.gasRemaining = vm.NewGasBudget(st.msg.GasLimit)
	st.initialBudget = st.gasRemaining.Copy()

	if st.msg.RunMode != GasEstimationWithSkipCheckBalanceMode && st.msg.RunMode != EthcallMode {
		if st.msg.MetaTxParams != nil {
			sponsorAmount, selfPayAmount := types.CalculateSponsorPercentAmount(st.msg.MetaTxParams, mgval)
			st.initialSponsorValue = sponsorAmount
			st.state.SubBalance(st.msg.MetaTxParams.GasFeeSponsor, sponsorAmount, tracing.BalanceDecreaseGasBuy)
			st.state.SubBalance(st.msg.From, selfPayAmount, tracing.BalanceDecreaseGasBuy)
		} else {
			st.state.SubBalance(st.msg.From, mgval, tracing.BalanceDecreaseGasBuy)
		}
	}

	return l1Cost, nil
}

func (st *stateTransition) applyMetaTransaction() error {
	if st.msg.MetaTxParams == nil {
		return nil
	}
	if st.msg.MetaTxParams.ExpireHeight < st.evm.Context.BlockNumber.Uint64() {
		return types.ErrExpiredMetaTx
	}

	st.msg.Data = st.msg.MetaTxParams.Payload
	return nil
}

func (st *stateTransition) preCheck() (*big.Int, error) {
	if st.msg.IsDepositTx {
		// No fee fields to check, no nonce to check, and no need to check if EOA (L1 already verified it for us)
		// Gas is free, but no refunds!
		st.gasRemaining = vm.NewGasBudget(st.msg.GasLimit) // Add gas here in order to be able to execute calls.
		st.initialBudget = st.gasRemaining.Copy()
		// Don't touch the gas pool for system transactions
		if st.msg.IsSystemTx {
			if st.evm.ChainConfig().IsOptimismRegolith(st.evm.Context.Time) {
				return nil, fmt.Errorf("%w: address %v", ErrSystemTxNotSupported,
					st.msg.From.Hex())
			}
			return common.Big0, nil
		}
		if err := st.gp.SubGas(st.msg.GasLimit); err != nil {
			return nil, err
		}
		return common.Big0, nil // gas used by deposits may not be used by other txs
	}

	// Only check transactions that are not fake
	msg := st.msg
	if !msg.SkipNonceChecks {
		// Make sure this transaction's nonce is correct.
		stNonce := st.state.GetNonce(msg.From)
		if msgNonce := msg.Nonce; stNonce < msgNonce {
			return nil, fmt.Errorf("%w: address %v, tx: %d state: %d", ErrNonceTooHigh,
				msg.From.Hex(), msgNonce, stNonce)
		} else if stNonce > msgNonce {
			return nil, fmt.Errorf("%w: address %v, tx: %d state: %d", ErrNonceTooLow,
				msg.From.Hex(), msgNonce, stNonce)
		} else if stNonce+1 < stNonce {
			return nil, fmt.Errorf("%w: address %v, nonce: %d", ErrNonceMax,
				msg.From.Hex(), stNonce)
		}
	}
	isOsaka := st.evm.ChainConfig().IsOsaka(st.evm.Context.BlockNumber, st.evm.Context.Time)
	isAmsterdam := st.evm.ChainConfig().IsAmsterdam(st.evm.Context.BlockNumber, st.evm.Context.Time)
	if !msg.SkipTransactionChecks {
		// Verify tx gas limit does not exceed EIP-7825 cap.
		if !st.evm.ChainConfig().IsOptimism() && isOsaka && !isAmsterdam && msg.GasLimit > params.MaxTxGas {
			return nil, fmt.Errorf("%w (cap: %d, tx: %d)", ErrGasLimitTooHigh, params.MaxTxGas, msg.GasLimit)
		}
		// Make sure the sender is an EOA
		code := st.state.GetCode(msg.From)
		_, delegated := types.ParseDelegation(code)
		if len(code) > 0 && !delegated {
			return nil, fmt.Errorf("%w: address %v, len(code): %d", ErrSenderNoEOA, msg.From.Hex(), len(code))
		}
	}
	// Make sure that transaction gasFeeCap is greater than the baseFee (post london)
	if st.evm.ChainConfig().IsLondon(st.evm.Context.BlockNumber) {
		// Skip the checks if gas fields are zero and baseFee was explicitly disabled (eth_call)
		skipCheck := st.evm.Config.NoBaseFee && msg.GasFeeCap.BitLen() == 0 && msg.GasTipCap.BitLen() == 0
		if !skipCheck {
			if msg.GasFeeCap.Cmp(msg.GasTipCap) < 0 {
				return nil, fmt.Errorf("%w: address %v, maxPriorityFeePerGas: %s, maxFeePerGas: %s", ErrTipAboveFeeCap,
					msg.From.Hex(), msg.GasTipCap, msg.GasFeeCap)
			}
			// This will panic if baseFee is nil, but basefee presence is verified
			// as part of header validation.
			if msg.GasFeeCap.CmpBig(st.evm.Context.BaseFee) < 0 {
				return nil, fmt.Errorf("%w: address %v, maxFeePerGas: %s, baseFee: %s", ErrFeeCapTooLow,
					msg.From.Hex(), msg.GasFeeCap, st.evm.Context.BaseFee)
			}
		}
	}
	// Check the blob version validity
	if msg.BlobHashes != nil {
		// The to field of a blob tx type is mandatory, and a `BlobTx` transaction internally
		// has it as a non-nillable value, so any msg derived from blob transaction has it non-nil.
		// However, messages created through RPC (eth_call) don't have this restriction.
		if msg.To == nil {
			return nil, ErrBlobTxCreate
		}
		if len(msg.BlobHashes) == 0 {
			return nil, ErrMissingBlobHashes
		}
		if isOsaka && len(msg.BlobHashes) > params.BlobTxMaxBlobs {
			return nil, ErrTooManyBlobs
		}
		for i, hash := range msg.BlobHashes {
			if !kzg4844.IsValidVersionedHash(hash[:]) {
				return nil, fmt.Errorf("blob %d has invalid hash version", i)
			}
		}
	}
	// Check that the user is paying at least the current blob fee
	if st.evm.ChainConfig().IsCancun(st.evm.Context.BlockNumber, st.evm.Context.Time) {
		if st.blobGasUsed() > 0 {
			// Skip the checks if gas fields are zero and blobBaseFee was explicitly disabled (eth_call)
			skipCheck := st.evm.Config.NoBaseFee && msg.BlobGasFeeCap.BitLen() == 0
			if !skipCheck {
				// This will panic if blobBaseFee is nil, but blobBaseFee presence
				// is verified as part of header validation.
				if msg.BlobGasFeeCap.CmpBig(st.evm.Context.BlobBaseFee) < 0 {
					return nil, fmt.Errorf("%w: address %v blobGasFeeCap: %v, blobBaseFee: %v", ErrBlobFeeCapTooLow,
						msg.From.Hex(), msg.BlobGasFeeCap, st.evm.Context.BlobBaseFee)
				}
			}
		}
	}
	// Check that EIP-7702 authorization list signatures are well formed.
	if msg.SetCodeAuthorizations != nil {
		if msg.To == nil {
			return nil, fmt.Errorf("%w (sender %v)", ErrSetCodeTxCreate, msg.From)
		}
		if len(msg.SetCodeAuthorizations) == 0 {
			return nil, fmt.Errorf("%w (sender %v)", ErrEmptyAuthList, msg.From)
		}
	}
	return st.buyGas()
}

// execute will transition the state by applying the current message and
// returning the evm execution result with following fields.
//
//   - used gas: total gas used (including gas being refunded)
//   - returndata: the returned data from evm
//   - concrete execution error: various EVM errors which abort the execution, e.g.
//     ErrOutOfGas, ErrExecutionReverted
//
// However if any consensus issue encountered, return the error directly with
// nil evm execution result.
func (st *stateTransition) execute() (*ExecutionResult, error) {
	if mint := st.msg.Mint; mint != nil {
		mintU256, overflow := uint256.FromBig(mint)
		if overflow {
			return nil, fmt.Errorf("mint value exceeds uint256: %d", mintU256)
		}
		st.state.AddBalance(st.msg.From, mintU256, tracing.BalanceMint)
	}

	//Mint BVM_ETH
	rules := st.evm.ChainConfig().Rules(st.evm.Context.BlockNumber, st.evm.Context.Random != nil, st.evm.Context.Time)
	//add eth value
	if ethValue := st.msg.ETHValue; ethValue != nil && ethValue.Cmp(big.NewInt(0)) != 0 {
		st.mintBVMETH(ethValue, rules)
	}

	// Mantle Hoodi only: write the BVM predeploy code in the activation block. Every execution
	// path applies messages here, and every L2 block starts with the L1 info deposit, so the first
	// transaction of the activation block performs the write. Applied before the snapshot so that
	// a failed deposit does not revert it.
	ApplyMantleHoodiBvmUpgrade(st.evm.ChainConfig(), st.evm.Context.Time, st.state)

	snap := st.state.Snapshot()
	// Will be reverted if failed

	// Check if the owner of the L2ProxyAdmin contract needs to be upgraded
	if st.evm.ChainConfig().IsProxyOwnerUpgrade(st.evm.Context.Time) {
		st.evm.StateDB.SetState(L2ProxyAdminAddress, OwnerSlot, NewProxyAdminOwnerAddress)
	}

	result, err := st.innerExecute()
	// Failed deposits must still be included. Unless we cannot produce the block at all due to the gas limit.
	// On deposit failure, we rewind any state changes from after the minting, and increment the nonce.
	if err != nil && err != ErrGasLimitReached && st.msg.IsDepositTx {
		if st.evm.Config.Tracer != nil && st.evm.Config.Tracer.OnEnter != nil {
			st.evm.Config.Tracer.OnEnter(0, byte(vm.STOP), common.Address{}, common.Address{}, nil, 0, nil)
		}

		st.state.RevertToSnapshot(snap)
		// Even though we revert the state changes, always increment the nonce for the next deposit transaction
		st.state.SetNonce(st.msg.From, st.state.GetNonce(st.msg.From)+1, tracing.NonceChangeEoACall)
		// Record deposits as using all their gas (matches the gas pool)
		// System Transactions are special & are not recorded as using any gas (anywhere)
		// Regolith changes this behaviour so the actual gas used is reported.
		// In this case the tx is invalid so is recorded as using all gas.
		gasUsed := st.msg.GasLimit
		if st.msg.IsSystemTx && !st.evm.ChainConfig().IsRegolith(st.evm.Context.Time) {
			gasUsed = 0
		}
		if err := st.gp.ReturnGas(0, gasUsed); err != nil {
			return nil, fmt.Errorf("return gas for deposit tx: %w", err)
		}
		result = &ExecutionResult{
			UsedGas:    gasUsed,
			Err:        fmt.Errorf("failed deposit: %w", err),
			ReturnData: nil,
		}
		err = nil
	}
	return result, err
}

func (st *stateTransition) innerExecute() (*ExecutionResult, error) {
	// First check this message satisfies all consensus rules before
	// applying the message. The rules include these clauses
	//
	// 1. the nonce of the message caller is correct
	// 2. caller has enough balance to cover transaction fee(gaslimit * gasprice)
	// 3. the amount of gas required is available in the block
	// 4. the purchased gas is enough to cover intrinsic usage
	// 5. there is no overflow when calculating intrinsic gas
	// 6. caller has enough balance to cover asset transfer for **topmost** call

	// for arsia, l1cost means l1cost + operator cost, coz we need to pay operator cost and l1 cost when executing tx
	// Check clauses 1-3, buy gas if everything is correct.
	//
	// A deposit must reserve its gas before the BVM_ETH transfer can fail: the
	// failed-deposit path charges the entire gas limit without reserving it again.
	l1Cost, err := st.preCheck()
	if err != nil {
		return nil, err
	}

	rules := st.evm.ChainConfig().Rules(st.evm.Context.BlockNumber, st.evm.Context.Random != nil, st.evm.Context.Time)
	if ethTxValue := st.msg.ETHTxValue; ethTxValue != nil && ethTxValue.Cmp(big.NewInt(0)) != 0 {
		err := st.transferBVMETH(ethTxValue, rules)
		if err != nil {
			return nil, err
		}
	}

	if st.msg.MetaTxParams != nil && rules.IsMantleEverest {
		return nil, types.ErrMetaTxDisabled
	}

	var (
		msg              = st.msg
		contractCreation = msg.To == nil
		floorDataGas     uint64
		tokenRatio       = st.state.GetState(types.GasOracleAddr, types.TokenRatioSlot).Big().Uint64()
	)
	// Check clauses 4-5, subtract intrinsic gas if everything is correct
	cost, err := IntrinsicGas(msg.Data, msg.AccessList, msg.SetCodeAuthorizations, contractCreation, rules.IsHomestead, rules.IsIstanbul, rules.IsShanghai, rules.IsAmsterdam)
	if err != nil {
		return nil, err
	}
	// after calculate intrinsic gas, apply meta tx data
	if rules.IsMetaTxV3 {
		if err := st.applyMetaTransaction(); err != nil {
			return nil, err
		}
	}
	if !st.msg.IsDepositTx && !st.msg.IsSystemTx && !rules.IsMantleArsia {
		cost.RegularGas = cost.RegularGas * tokenRatio
	}
	prior, sufficient := st.gasRemaining.Charge(cost)
	if !sufficient {
		return nil, fmt.Errorf("%w: have %d, want %d", ErrIntrinsicGas, st.gasRemaining.RegularGas, cost.RegularGas)
	}
	if t := st.evm.Config.Tracer; t != nil && t.OnGasChange != nil {
		t.OnGasChange(prior, st.gasRemaining.RegularGas, tracing.GasChangeTxIntrinsicGas)
	}
	// Gas limit suffices for the floor data cost (EIP-7623)
	if rules.IsPrague {
		floorDataGas, err = FloorDataGas(rules, msg.Data, msg.AccessList)
		if err != nil {
			return nil, err
		}
		if !st.msg.IsDepositTx && !st.msg.IsSystemTx && !rules.IsMantleArsia {
			floorDataGas = floorDataGas * tokenRatio
		}
		if msg.GasLimit < floorDataGas {
			return nil, fmt.Errorf("%w: have %d, want %d", ErrFloorDataGas, msg.GasLimit, floorDataGas)
		}
	}

	var l1Gas uint64
	if !st.msg.IsDepositTx && !st.msg.IsSystemTx && !rules.IsMantleArsia {
		// for non-Arsia, l1 gas is calculated based on gas price and l1 cost for total tx gas
		if st.msg.GasPrice.Cmp(common.U2560) > 0 && l1Cost != nil {
			l1Gas = new(uint256.Int).Div(uint256.MustFromBig(l1Cost), st.msg.GasPrice).Uint64()
		}
		if st.gasRemaining.RegularGas < l1Gas {
			return nil, fmt.Errorf("%w: have %d, want %d", ErrInsufficientGasForL1Cost, st.gasRemaining.RegularGas, l1Gas)
		}
		st.gasRemaining.RegularGas -= l1Gas
		if tokenRatio > 0 {
			st.gasRemaining.RegularGas = st.gasRemaining.RegularGas / tokenRatio
		}
	}

	if rules.IsEIP4762 {
		st.evm.AccessEvents.AddTxOrigin(msg.From)

		if targetAddr := msg.To; targetAddr != nil {
			st.evm.AccessEvents.AddTxDestination(*targetAddr, msg.Value.Sign() != 0, !st.state.Exist(*targetAddr))
		}
	}

	// Check clause 6
	value := msg.Value
	if value == nil {
		value = new(uint256.Int)
	}
	if !value.IsZero() && !st.evm.Context.CanTransfer(st.state, msg.From, value) {
		return nil, fmt.Errorf("%w: address %v", ErrInsufficientFundsForTransfer, msg.From.Hex())
	}

	// Check whether the init code size has been exceeded.
	if contractCreation {
		if err := vm.CheckMaxInitCodeSize(&rules, uint64(len(msg.Data))); err != nil {
			return nil, err
		}
	}

	// Execute the preparatory steps for state transition which includes:
	// - prepare accessList(post-berlin)
	// - reset transient storage(eip 1153)
	st.state.Prepare(rules, msg.From, st.evm.Context.Coinbase, msg.To, vm.ActivePrecompiles(rules), msg.AccessList)

	var (
		ret   []byte
		vmerr error // vm errors do not effect consensus and are therefore not assigned to err
	)
	if contractCreation {
		ret, _, st.gasRemaining, vmerr = st.evm.Create(msg.From, msg.Data, st.gasRemaining, value)
	} else {
		// Increment the nonce for the next transaction.
		st.state.SetNonce(msg.From, st.state.GetNonce(msg.From)+1, tracing.NonceChangeEoACall)

		// Apply EIP-7702 authorizations.
		if msg.SetCodeAuthorizations != nil {
			for _, auth := range msg.SetCodeAuthorizations {
				// Note errors are ignored, we simply skip invalid authorizations here.
				st.applyAuthorization(&auth)
			}
		}

		// Perform convenience warming of sender's delegation target. Although the
		// sender is already warmed in Prepare(..), it's possible a delegation to
		// the account was deployed during this transaction. To handle correctly,
		// simply wait until the final state of delegations is determined before
		// performing the resolution and warming.
		if addr, ok := types.ParseDelegation(st.state.GetCode(*msg.To)); ok {
			st.state.AddAddressToAccessList(addr)
		}

		// Execute the transaction's call.
		ret, st.gasRemaining, vmerr = st.evm.Call(msg.From, st.to(), msg.Data, st.gasRemaining, value)
	}

	// if deposit: skip refunds, skip tipping coinbase
	// Regolith changes this behaviour to report the actual gasUsed instead of always reporting all gas used.
	if st.msg.IsDepositTx && !rules.IsOptimismRegolith {
		// Record deposits as using all their gas (matches the gas pool)
		// System Transactions are special & are not recorded as using any gas (anywhere)
		gasUsed := st.msg.GasLimit
		if st.msg.IsSystemTx {
			gasUsed = 0
		}
		if err := st.gp.ReturnGas(0, gasUsed); err != nil {
			return nil, fmt.Errorf("return gas for pre-Regolith deposit tx: %w", err)
		}
		return &ExecutionResult{
			UsedGas:    gasUsed,
			Err:        vmerr,
			ReturnData: ret,
		}, nil
	}

	// Record the gas used excluding gas refunds. This value represents the actual
	// gas allowance required to complete execution.
	peakGasUsed := st.gasUsed()

	if !st.msg.IsDepositTx && !st.msg.IsSystemTx {
		if rules.IsMantleArsia {
			// Compute refund counter, capped to a refund quotient.
			st.gasRemaining.Refund(st.calcRefund())
		} else {
			peakGasUsed = st.initialBudget.RegularGas - st.gasRemaining.RegularGas*tokenRatio
			st.gasRemaining.RegularGas += st.calcRefundPreArsia(tokenRatio, rules.IsMantleSkadi)
			st.gasRemaining.RegularGas = st.gasRemaining.RegularGas * tokenRatio
		}
	}

	if rules.IsPrague {
		// After EIP-7623: Data-heavy transactions pay the floor gas.
		if used := st.gasUsed(); used < floorDataGas {
			prior, _ := st.gasRemaining.Charge(vm.GasCosts{RegularGas: floorDataGas - used})
			if t := st.evm.Config.Tracer; t != nil && t.OnGasChange != nil {
				t.OnGasChange(prior, st.gasRemaining.RegularGas, tracing.GasChangeTxDataFloor)
			}
		}
		if peakGasUsed < floorDataGas {
			peakGasUsed = floorDataGas
		}
	}

	if !st.msg.IsDepositTx && !st.msg.IsSystemTx {
		if rules.IsMantleArsia {
			// Return gas to the user
			st.returnGas()
		} else {
			st.returnGasPreArsia(rules.IsMetaTxV3)
		}
	}

	// Return gas to the gas pool
	if rules.IsAmsterdam {
		// Refund is excluded for returning
		err = st.gp.ReturnGas(st.initialBudget.RegularGas-peakGasUsed, st.gasUsed())
	} else {
		// Refund is included for returning
		err = st.gp.ReturnGas(st.gasRemaining.RegularGas, st.gasUsed())
	}
	if err != nil {
		return nil, err
	}

	// Note for deposit tx there is no ETH refunded for unused gas, but that's taken care of by the fact that gasPrice
	// is always 0 for deposit tx. So calling refundGas will ensure the gasUsed accounting is correct without actually
	// changing the sender's balance
	if st.msg.IsDepositTx && rules.IsOptimismRegolith {
		// Skip coinbase payments for deposit tx in Regolith
		return &ExecutionResult{
			UsedGas:    st.gasUsed(),
			MaxUsedGas: peakGasUsed,
			Err:        vmerr,
			ReturnData: ret,
		}, nil
	}

	effectiveTip := msg.GasPrice
	if rules.IsLondon {
		baseFee, overflow := uint256.FromBig(st.evm.Context.BaseFee)
		if overflow {
			return nil, fmt.Errorf("invalid baseFee: %v", st.evm.Context.BaseFee)
		}
		effectiveTip = new(uint256.Int).Sub(msg.GasPrice, baseFee)
	}

	if st.evm.Config.NoBaseFee && msg.GasFeeCap.Sign() == 0 && msg.GasTipCap.Sign() == 0 {
		// Skip fee payment when NoBaseFee is set and the fee fields
		// are 0. This avoids a negative effectiveTip being applied to
		// the coinbase when simulating calls.
	} else {
		fee := new(uint256.Int).SetUint64(st.gasUsed())
		fee.Mul(fee, effectiveTip)
		st.state.AddBalance(st.evm.Context.Coinbase, fee, tracing.BalanceIncreaseRewardTransactionFee)

		// add the coinbase to the witness iff the fee is greater than 0
		if rules.IsEIP4762 && fee.Sign() != 0 {
			st.evm.AccessEvents.AddAccount(st.evm.Context.Coinbase, true, math.MaxUint64)
		}
	}

	// Check that we are post bedrock to enable op-geth to be able to create pseudo pre-bedrock blocks (these are pre-bedrock, but don't follow l2 geth rules)
	// Note optimismConfig will not be nil if rules.IsOptimismBedrock is true
	if optimismConfig := st.evm.ChainConfig().Optimism; optimismConfig != nil && rules.IsOptimismBedrock {
		feeU256, overflow := uint256.FromBig(new(big.Int).Mul(new(big.Int).SetUint64(st.gasUsed()), st.evm.Context.BaseFee))
		if overflow {
			return nil, fmt.Errorf("base fee value exceeds uint256: %d", feeU256)
		}
		st.state.AddBalance(params.OptimismBaseFeeRecipient, feeU256, tracing.BalanceIncreaseRewardTransactionFee)
		if rules.IsMantleArsia {
			// L1 fee is accumulated in OptimismL1FeeRecipient only if Mantle Arsia is active.
			// Before Arsia, L1 fee is included in gas used calculation and is sent to OptimismBaseFeeRecipient.
			// Reuse l1Cost from preCheck to avoid redundant calculation
			if l1Cost != nil {
				amtU256, overflow := uint256.FromBig(l1Cost)
				if overflow {
					return nil, fmt.Errorf("optimism l1 cost overflows U256: %d", l1Cost)
				}
				st.state.AddBalance(params.OptimismL1FeeRecipient, amtU256, tracing.BalanceIncreaseRewardTransactionFee)
			}
			// Operator Fee refunds are only applied if Isthmus is active and the transaction is *not* a deposit.
			st.refundOperatorCost()
			if st.evm.Context.OperatorCostFunc != nil {
				operatorFeeCost := st.evm.Context.OperatorCostFunc(st.gasUsed(), st.evm.Context.Time)
				st.state.AddBalance(params.OptimismOperatorFeeRecipient, operatorFeeCost, tracing.BalanceIncreaseRewardTransactionFee)
			}
		}
	}

	if rules.IsAmsterdam {
		for _, log := range st.evm.StateDB.LogsForBurnAccounts() {
			st.evm.StateDB.AddLog(log)
		}
	}
	return &ExecutionResult{
		UsedGas:    st.gasUsed(),
		MaxUsedGas: peakGasUsed,
		Err:        vmerr,
		ReturnData: ret,
	}, nil
}

// validateAuthorization validates an EIP-7702 authorization against the state.
func (st *stateTransition) validateAuthorization(auth *types.SetCodeAuthorization) (authority common.Address, err error) {
	// Verify chain ID is null or equal to current chain ID.
	if !auth.ChainID.IsZero() && auth.ChainID.CmpBig(st.evm.ChainConfig().ChainID) != 0 {
		return authority, ErrAuthorizationWrongChainID
	}
	// Limit nonce to 2^64-1 per EIP-2681.
	if auth.Nonce+1 < auth.Nonce {
		return authority, ErrAuthorizationNonceOverflow
	}
	// Validate signature values and recover authority.
	authority, err = auth.Authority()
	if err != nil {
		return authority, fmt.Errorf("%w: %v", ErrAuthorizationInvalidSignature, err)
	}
	// Check the authority account
	//  1) doesn't have code or has exisiting delegation
	//  2) matches the auth's nonce
	//
	// Note it is added to the access list even if the authorization is invalid.
	st.state.AddAddressToAccessList(authority)
	code := st.state.GetCode(authority)
	if _, ok := types.ParseDelegation(code); len(code) != 0 && !ok {
		return authority, ErrAuthorizationDestinationHasCode
	}
	if have := st.state.GetNonce(authority); have != auth.Nonce {
		return authority, ErrAuthorizationNonceMismatch
	}
	return authority, nil
}

// applyAuthorization applies an EIP-7702 code delegation to the state.
func (st *stateTransition) applyAuthorization(auth *types.SetCodeAuthorization) error {
	authority, err := st.validateAuthorization(auth)
	if err != nil {
		return err
	}

	// If the account already exists in state, refund the new account cost
	// charged in the intrinsic calculation.
	if st.state.Exist(authority) {
		st.state.AddRefund(params.CallNewAccountGas - params.TxAuthTupleGas)
	}

	// Update nonce and account code.
	st.state.SetNonce(authority, auth.Nonce+1, tracing.NonceChangeAuthorization)
	if auth.Address == (common.Address{}) {
		// Delegation to zero address means clear.
		st.state.SetCode(authority, nil, tracing.CodeChangeAuthorizationClear)
		return nil
	}

	// Otherwise install delegation to auth.Address.
	st.state.SetCode(authority, types.AddressToDelegation(auth.Address), tracing.CodeChangeAuthorization)

	return nil
}

// calcRefund computes refund counter, capped to a refund quotient.
func (st *stateTransition) calcRefund() vm.GasBudget {
	var refund uint64
	if !st.evm.ChainConfig().IsLondon(st.evm.Context.BlockNumber) {
		// Before EIP-3529: refunds were capped to gasUsed / 2
		refund = st.gasUsed() / params.RefundQuotient
	} else {
		// After EIP-3529: refunds are capped to gasUsed / 5
		refund = st.gasUsed() / params.RefundQuotientEIP3529
	}
	if refund > st.state.GetRefund() {
		refund = st.state.GetRefund()
	}
	if st.evm.Config.Tracer != nil && st.evm.Config.Tracer.OnGasChange != nil && refund > 0 {
		st.evm.Config.Tracer.OnGasChange(st.gasRemaining.RegularGas, st.gasRemaining.RegularGas+refund, tracing.GasChangeTxRefunds)
	}
	return vm.NewGasBudget(refund)
}

// calcRefundPreArsia computes refund counter, capped to a refund quotient.
func (st *stateTransition) calcRefundPreArsia(tokenRatio uint64, isMantleSkadi bool) uint64 {
	var refund uint64
	gasUsed := st.gasUsed()
	if isMantleSkadi {
		// calc refund should remove token ratio in initialGas
		gasUsed = st.originalGasUsed(tokenRatio)
	}
	if !st.evm.ChainConfig().IsLondon(st.evm.Context.BlockNumber) {
		// Before EIP-3529: refunds were capped to gasUsed / 2
		refund = gasUsed / params.RefundQuotient
	} else {
		// After EIP-3529: refunds are capped to gasUsed / 5
		refund = gasUsed / params.RefundQuotientEIP3529
	}
	if refund > st.state.GetRefund() {
		refund = st.state.GetRefund()
	}
	if st.evm.Config.Tracer != nil && st.evm.Config.Tracer.OnGasChange != nil && refund > 0 {
		st.evm.Config.Tracer.OnGasChange(st.gasRemaining.RegularGas, st.gasRemaining.RegularGas+refund, tracing.GasChangeTxRefunds)
	}
	return refund
}

func (st *stateTransition) refundOperatorCost() {
	// Return ETH to transaction sender for operator cost overcharge.
	operatorCostGasLimit := st.evm.Context.OperatorCostFunc(st.msg.GasLimit, st.evm.Context.Time)
	operatorCostGasUsed := st.evm.Context.OperatorCostFunc(st.gasUsed(), st.evm.Context.Time)

	if operatorCostGasUsed.Cmp(operatorCostGasLimit) > 0 { // Sanity check.
		panic(fmt.Sprintf("operator cost gas used (%d) > operator cost gas limit (%d)", operatorCostGasUsed, operatorCostGasLimit))
	}

	st.state.AddBalance(st.msg.From, new(uint256.Int).Sub(operatorCostGasLimit, operatorCostGasUsed), tracing.BalanceIncreaseGasReturn)
}

// returnGas returns ETH for remaining gas,
// exchanged at the original rate.
func (st *stateTransition) returnGas() {
	remaining := uint256.NewInt(st.gasRemaining.RegularGas)
	remaining.Mul(remaining, st.msg.GasPrice)
	st.state.AddBalance(st.msg.From, remaining, tracing.BalanceIncreaseGasReturn)

	if st.evm.Config.Tracer != nil && st.evm.Config.Tracer.OnGasChange != nil && st.gasRemaining.RegularGas > 0 {
		st.evm.Config.Tracer.OnGasChange(st.gasRemaining.RegularGas, 0, tracing.GasChangeTxLeftOverReturned)
	}
}

// returnGasPreArsia returns ETH for remaining gas,
// exchanged at the original rate.
func (st *stateTransition) returnGasPreArsia(metaTxV3 bool) {
	if st.msg.RunMode == GasEstimationWithSkipCheckBalanceMode || st.msg.RunMode == EthcallMode {
		if st.evm.Config.Tracer != nil && st.evm.Config.Tracer.OnGasChange != nil && st.gasRemaining.RegularGas > 0 {
			st.evm.Config.Tracer.OnGasChange(st.gasRemaining.RegularGas, 0, tracing.GasChangeTxLeftOverReturned)
		}
		return
	}

	if st.msg.MetaTxParams != nil {
		remainingBigInt := new(uint256.Int).Mul(new(uint256.Int).SetUint64(st.gasRemaining.RegularGas), st.msg.GasPrice)
		sponsorRefundAmount, selfRefundAmount := types.CalculateSponsorPercentAmount(st.msg.MetaTxParams, remainingBigInt)
		st.state.AddBalance(st.msg.MetaTxParams.GasFeeSponsor, sponsorRefundAmount, tracing.BalanceIncreaseGasReturn)
		st.state.AddBalance(st.msg.From, selfRefundAmount, tracing.BalanceIncreaseGasReturn)
		if metaTxV3 {
			actualSponsorValue := new(uint256.Int).Sub(st.initialSponsorValue, sponsorRefundAmount)
			st.generateMetaTxSponsorEvent(st.msg.MetaTxParams.GasFeeSponsor, st.msg.From, actualSponsorValue.ToBig())
		}
	} else {
		remaining := uint256.NewInt(st.gasRemaining.RegularGas)
		remaining.Mul(remaining, st.msg.GasPrice)
		st.state.AddBalance(st.msg.From, remaining, tracing.BalanceIncreaseGasReturn)
	}

	if st.evm.Config.Tracer != nil && st.evm.Config.Tracer.OnGasChange != nil && st.gasRemaining.RegularGas > 0 {
		st.evm.Config.Tracer.OnGasChange(st.gasRemaining.RegularGas, 0, tracing.GasChangeTxLeftOverReturned)
	}
}

// gasUsed returns the amount of gas used up by the state transition.
func (st *stateTransition) gasUsed() uint64 {
	return st.gasRemaining.Used(st.initialBudget)
}

func (st *stateTransition) originalGasUsed(tokenRatio uint64) uint64 {
	if tokenRatio > 0 {
		return st.initialBudget.RegularGas/tokenRatio - st.gasRemaining.RegularGas
	}
	return st.gasUsed()
}

// blobGasUsed returns the amount of blob gas used by the message.
func (st *stateTransition) blobGasUsed() uint64 {
	return uint64(len(st.msg.BlobHashes) * params.BlobTxBlobGasPerBlob)
}

func (st *stateTransition) mintBVMETH(ethValue *big.Int, rules params.Rules) {
	if !rules.IsMantleBVMETHMintUpgrade {
		var key common.Hash
		var ethRecipient common.Address
		if st.msg.To != nil {
			ethRecipient = *st.msg.To
		} else {
			ethRecipient = crypto.CreateAddress(st.msg.From, st.evm.StateDB.GetNonce(st.msg.From))
		}
		key = getBVMETHBalanceKey(ethRecipient)
		value := st.state.GetState(BVM_ETH_ADDR, key)
		bal := value.Big()
		bal = bal.Add(bal, ethValue)
		st.state.SetState(BVM_ETH_ADDR, key, common.BigToHash(bal))

		st.addBVMETHTotalSupply(ethValue)
		st.generateBVMETHMintEvent(ethRecipient, ethValue)
		return
	}
	key := getBVMETHBalanceKey(st.msg.From)
	value := st.state.GetState(BVM_ETH_ADDR, key)
	bal := value.Big()
	bal = bal.Add(bal, ethValue)
	st.state.SetState(BVM_ETH_ADDR, key, common.BigToHash(bal))

	st.addBVMETHTotalSupply(ethValue)
	st.generateBVMETHMintEvent(st.msg.From, ethValue)
}

func (st *stateTransition) addBVMETHTotalSupply(ethValue *big.Int) {
	key := getBVMETHTotalSupplyKey()
	value := st.state.GetState(BVM_ETH_ADDR, key)
	bal := value.Big()
	bal = bal.Add(bal, ethValue)
	st.state.SetState(BVM_ETH_ADDR, key, common.BigToHash(bal))
}

func (st *stateTransition) transferBVMETH(ethValue *big.Int, rules params.Rules) error {
	if !rules.IsMantleBVMETHMintUpgrade {
		return nil
	}
	var ethRecipient common.Address
	if st.msg.To != nil {
		ethRecipient = *st.msg.To
	} else {
		ethRecipient = crypto.CreateAddress(st.msg.From, st.evm.StateDB.GetNonce(st.msg.From))
	}
	if ethRecipient == st.msg.From {
		return nil
	}

	fromKey := getBVMETHBalanceKey(st.msg.From)
	toKey := getBVMETHBalanceKey(ethRecipient)

	fromBalanceValue := st.state.GetState(BVM_ETH_ADDR, fromKey)
	toBalanceValue := st.state.GetState(BVM_ETH_ADDR, toKey)

	fromBalance := fromBalanceValue.Big()
	toBalance := toBalanceValue.Big()

	if fromBalance.Cmp(ethValue) < 0 {
		return ErrEthTxValueTooLarge
	}

	fromBalance = new(big.Int).Sub(fromBalance, ethValue)
	toBalance = new(big.Int).Add(toBalance, ethValue)

	st.state.SetState(BVM_ETH_ADDR, fromKey, common.BigToHash(fromBalance))
	st.state.SetState(BVM_ETH_ADDR, toKey, common.BigToHash(toBalance))

	st.generateBVMETHTransferEvent(st.msg.From, ethRecipient, ethValue)
	return nil
}

func getBVMETHBalanceKey(addr common.Address) common.Hash {
	position := common.Big0
	hasher := sha3.NewLegacyKeccak256()
	hasher.Write(common.LeftPadBytes(addr.Bytes(), 32))
	hasher.Write(common.LeftPadBytes(position.Bytes(), 32))
	digest := hasher.Sum(nil)
	return common.BytesToHash(digest)
}

func getBVMETHTotalSupplyKey() common.Hash {
	return common.BytesToHash(common.Big2.Bytes())
}

func (st *stateTransition) generateBVMETHMintEvent(mintAddress common.Address, mintValue *big.Int) {
	// keccak("Mint(address,uint256)") = "0x0f6798a560793a54c3bcfe86a93cde1e73087d944c0ea20544137d4121396885"
	methodHash := common.HexToHash("0x0f6798a560793a54c3bcfe86a93cde1e73087d944c0ea20544137d4121396885")
	topics := make([]common.Hash, 2)
	topics[0] = methodHash
	topics[1] = mintAddress.Hash()
	//data means the mint amount in MINT EVENT.
	d := common.HexToHash(common.Bytes2Hex(mintValue.Bytes())).Bytes()
	st.evm.StateDB.AddLog(&types.Log{
		Address: BVM_ETH_ADDR,
		Topics:  topics,
		Data:    d,
		// This is a non-consensus field, but assigned here because
		// core/state doesn't know the current block number.
		BlockNumber: st.evm.Context.BlockNumber.Uint64(),
	})
}

func (st *stateTransition) generateBVMETHTransferEvent(from, to common.Address, amount *big.Int) {
	// keccak("Transfer(address,address,uint256)") = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	methodHash := common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	topics := make([]common.Hash, 3)
	topics[0] = methodHash
	topics[1] = from.Hash()
	topics[2] = to.Hash()
	//data means the transfer amount in Transfer EVENT.
	data := common.HexToHash(common.Bytes2Hex(amount.Bytes())).Bytes()
	st.evm.StateDB.AddLog(&types.Log{
		Address: BVM_ETH_ADDR,
		Topics:  topics,
		Data:    data,
		// This is a non-consensus field, but assigned here because
		// core/state doesn't know the current block number.
		BlockNumber: st.evm.Context.BlockNumber.Uint64(),
	})
}

func (st *stateTransition) generateMetaTxSponsorEvent(sponsor, txSender common.Address, actualSponsorAmount *big.Int) {
	// keccak("MetaTxSponsor(address,address,uint256)") = "0xe57e5af44e0b977ac446043abcb6b8958c04a1004c91d7342e2870761b21af95"
	methodHash := common.HexToHash("0xe57e5af44e0b977ac446043abcb6b8958c04a1004c91d7342e2870761b21af95")
	topics := make([]common.Hash, 3)
	topics[0] = methodHash
	topics[1] = sponsor.Hash()
	topics[2] = txSender.Hash()
	//data means the sponsor amount in MetaTxSponsor EVENT.
	d := common.HexToHash(common.Bytes2Hex(actualSponsorAmount.Bytes())).Bytes()
	st.evm.StateDB.AddLog(&types.Log{
		Address: LEGACY_ERC20_MNT,
		Topics:  topics,
		Data:    d,
		// This is a non-consensus field, but assigned here because
		// core/state doesn't know the current block number.
		BlockNumber: st.evm.Context.BlockNumber.Uint64(),
	})
}
