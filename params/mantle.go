package params

import (
	"math/big"
)

// Mantle chain config
var (
	MantleMainnetChainId = big.NewInt(5000)
	MantleSepoliaChainId = big.NewInt(5003)
	MantleHoodiChainId   = big.NewInt(50002)
	MantleLocalChainId   = big.NewInt(1337)
)

// MantleHoodiBlockTime is the Mantle Hoodi L2 block time in seconds. The Hoodi BVM upgrade writes its
// code in the first block that is active while the block one MantleHoodiBlockTime earlier is not.
// If Hoodi's block time changes before the fork activates, update this value or pick an activation
// time that equals a block timestamp. Must equal alloy-op-hardforks' MANTLE_HOODI_BLOCK_TIME.
const MantleHoodiBlockTime uint64 = 2

var (
	MantleMainnetUpgradeConfig = MantleUpgradeChainConfig{
		ChainID:                   MantleMainnetChainId,
		BaseFeeTime:               u64Ptr(0),
		BVMETHMintUpgradeTime:     u64Ptr(0),
		MetaTxV2UpgradeTime:       u64Ptr(0),
		MetaTxV3UpgradeTime:       u64Ptr(1_742_367_600),
		ProxyOwnerUpgradeTime:     u64Ptr(1_742_367_600),
		MantleEverestTime:         u64Ptr(1_742_367_600),
		MantleSkadiTime:           u64Ptr(1_756_278_000),
		MantleLimbTime:            u64Ptr(1_768_374_000),
		MantleArsiaTime:           u64Ptr(1_776_841_200),
		MantleHoodiBvmUpgradeTime: nil,
		MantleElysiumTime:         nil,
	}
	MantleSepoliaUpgradeConfig = MantleUpgradeChainConfig{
		ChainID:                   MantleSepoliaChainId,
		BaseFeeTime:               u64Ptr(1_704_891_600),
		BVMETHMintUpgradeTime:     u64Ptr(1_720_594_800),
		MetaTxV2UpgradeTime:       u64Ptr(1_720_594_800),
		MetaTxV3UpgradeTime:       u64Ptr(1_720_594_800),
		ProxyOwnerUpgradeTime:     nil,
		MantleEverestTime:         u64Ptr(1_737_010_800),
		MantleSkadiTime:           u64Ptr(1_752_649_200),
		MantleLimbTime:            u64Ptr(1_764_745_200),
		MantleArsiaTime:           u64Ptr(1_774_422_000),
		MantleHoodiBvmUpgradeTime: nil,
		MantleElysiumTime:         nil,
	}
	// MantleHoodiUpgradeConfig carries Mantle Hoodi's fork times. Hoodi activated every fork up to
	// Arsia at genesis. MantleHoodiBvmUpgradeTime must equal op-node's
	// rollup.BundledMantleHoodiBVMUpgradeTime and alloy-op-hardforks'
	// MANTLE_HOODI_BVM_UPGRADE_TIMESTAMP; it stays nil until scheduled.
	MantleHoodiUpgradeConfig = MantleUpgradeChainConfig{
		ChainID:                   MantleHoodiChainId,
		BaseFeeTime:               u64Ptr(0),
		BVMETHMintUpgradeTime:     u64Ptr(0),
		MetaTxV2UpgradeTime:       u64Ptr(0),
		MetaTxV3UpgradeTime:       u64Ptr(0),
		ProxyOwnerUpgradeTime:     nil,
		MantleEverestTime:         u64Ptr(0),
		MantleSkadiTime:           u64Ptr(0),
		MantleLimbTime:            u64Ptr(0),
		MantleArsiaTime:           u64Ptr(0),
		MantleHoodiBvmUpgradeTime: nil,
		MantleElysiumTime:         nil,
	}

	// Starting from arsia, hardcoding upgrade timestamps of qa/dev/local networks are deprecated.
	// You should specify them either in deploy config json (will beactivated in genesis) or
	// through CLI override flags (will be activated in future).
	MantleLocalUpgradeConfig = MantleUpgradeChainConfig{
		ChainID:                   MantleLocalChainId,
		BaseFeeTime:               u64Ptr(0),
		BVMETHMintUpgradeTime:     u64Ptr(0),
		MetaTxV2UpgradeTime:       u64Ptr(0),
		MetaTxV3UpgradeTime:       u64Ptr(0),
		ProxyOwnerUpgradeTime:     nil,
		MantleEverestTime:         u64Ptr(0),
		MantleSkadiTime:           u64Ptr(0),
		MantleLimbTime:            u64Ptr(0),
		MantleArsiaTime:           u64Ptr(0),
		MantleHoodiBvmUpgradeTime: nil,
		MantleElysiumTime:         nil,
	}
	MantleDefaultUpgradeConfig = MantleUpgradeChainConfig{
		BaseFeeTime:               u64Ptr(0),
		BVMETHMintUpgradeTime:     u64Ptr(0),
		MetaTxV2UpgradeTime:       u64Ptr(0),
		MetaTxV3UpgradeTime:       u64Ptr(0),
		ProxyOwnerUpgradeTime:     nil,
		MantleEverestTime:         u64Ptr(0),
		MantleSkadiTime:           u64Ptr(0),
		MantleLimbTime:            u64Ptr(0),
		MantleArsiaTime:           u64Ptr(0),
		MantleHoodiBvmUpgradeTime: nil,
		MantleElysiumTime:         nil,
	}
)

type MantleUpgradeChainConfig struct {
	ChainID *big.Int `json:"chainId"` // chainId identifies the current chain and is used for replay protection

	BaseFeeTime               *uint64 `json:"baseFeeTime"`               // Mantle BaseFee switch time (nil = no fork, 0 = already on mantle baseFee)
	BVMETHMintUpgradeTime     *uint64 `json:"bvmETHMintUpgradeTime"`     // BVM_ETH mint upgrade switch time (nil = no fork, 0 = already on)
	MetaTxV2UpgradeTime       *uint64 `json:"metaTxV2UpgradeTime"`       // MetaTxV1UpgradeBlock identifies the current block height is using metaTx with MetaTxSignDataV2
	MetaTxV3UpgradeTime       *uint64 `json:"metaTxV3UpgradeTime"`       // MetaTxV3UpgradeBlock identifies the current block height is ensuring sponsor and sender are not the same
	ProxyOwnerUpgradeTime     *uint64 `json:"proxyOwnerUpgradeTime"`     // ProxyOwnerUpgradeBlock identifies the current block time is ensuring the L2ProxyAdmin contract owner is set to NewProxyAdminOwnerAddress
	MantleEverestTime         *uint64 `json:"mantleEverestTime"`         // MantleEverestTime identifies the current block time is ensuring eip-7212 & disable MetaTx
	MantleSkadiTime           *uint64 `json:"mantleSkadiTime"`           // MantleSkadiTime identifies the current block time is ensuring prague upgrade
	MantleLimbTime            *uint64 `json:"mantleLimbTime"`            // MantleLimbTime identifies the current block time is ensuring osaka upgrade
	MantleArsiaTime           *uint64 `json:"mantleArsiaTime"`           // MantleArsiaTime identifies the current block time is ensuring arsia upgrade
	MantleHoodiBvmUpgradeTime *uint64 `json:"mantleHoodiBvmUpgradeTime"` // MantleHoodiBvmUpgradeTime is the Mantle Hoodi BVM upgrade time (Mantle Hoodi only)
	MantleElysiumTime         *uint64 `json:"mantleElysiumTime"`         // MantleElysiumTime identifies the current block time is ensuring elysium upgrade
}

// GetUpgradeConfigForMantle returns a mantle upgrade config for Mainnet, Sepolia and Hoodi.
func GetUpgradeConfigForMantle(chainID *big.Int) *MantleUpgradeChainConfig {
	if chainID == nil {
		return nil
	}
	switch chainID.Int64() {
	case MantleMainnetChainId.Int64():
		return &MantleMainnetUpgradeConfig
	case MantleSepoliaChainId.Int64():
		return &MantleSepoliaUpgradeConfig
	case MantleHoodiChainId.Int64():
		return &MantleHoodiUpgradeConfig
	default:
		return nil
	}
}

func u64Ptr(v uint64) *uint64 {
	return &v
}
