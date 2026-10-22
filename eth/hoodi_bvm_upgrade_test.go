package eth

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// stateAtTransaction rebuilds the pre-transaction state of a block for tracing. The Mantle Hoodi
// BVM upgrade is applied inside the activation block's first transaction, so the state before that
// transaction has no upgrade code and the state before every later transaction has it.
func TestHoodiBvmUpgradeStateAtTransaction(t *testing.T) {
	upgradeTime := uint64(20) // GenerateChain spaces blocks 10s apart: block 2 activates.
	config := *params.TestChainConfig
	config.ChainID = params.MantleHoodiChainId
	config.MantleHoodiBvmUpgradeTime = &upgradeTime

	accounts := newAccounts(1)
	gspec := &core.Genesis{
		Config: &config,
		Alloc:  types.GenesisAlloc{accounts[0].addr: {Balance: big.NewInt(params.Ether)}},
	}
	signer := types.LatestSigner(&config)
	to := accounts[0].addr
	engine := ethash.NewFaker()
	_, blocks, _ := core.GenerateChainWithGenesis(gspec, engine, 3, func(i int, b *core.BlockGen) {
		for nonce := uint64(2 * i); nonce < uint64(2*i+2); nonce++ {
			b.AddTx(types.MustSignNewTx(accounts[0].key, signer, &types.LegacyTx{
				Nonce: nonce, To: &to, Gas: params.TxGas, GasPrice: b.BaseFee(),
			}))
		}
	})
	db := rawdb.NewMemoryDatabase()
	chain, err := core.NewBlockChain(db, gspec, engine, &core.BlockChainConfig{
		TrieCleanLimit: 256, TrieDirtyLimit: 256, TrieTimeLimit: 5 * time.Minute, ArchiveMode: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer chain.Stop()
	if n, err := chain.InsertChain(blocks); err != nil {
		t.Fatalf("block %d: %v", n, err)
	}
	eth := &Ethereum{blockchain: chain, chainDb: db}

	for _, tc := range []struct {
		number uint64
		write  bool
	}{{1, false}, {2, true}} {
		block := chain.GetBlockByNumber(tc.number)
		if tc.number == 2 && block.Time() != upgradeTime {
			t.Fatalf("block 2 time = %d, want %d", block.Time(), upgradeTime)
		}
		for txIndex := range block.Transactions() {
			_, _, statedb, release, err := eth.stateAtTransaction(context.Background(), block, txIndex)
			if err != nil {
				t.Fatal(err)
			}
			got := statedb.GetCodeHash(core.MantleLegacyERC20MNTAddr) == core.MantleLegacyERC20MNTCodeHash
			release()
			if want := tc.write && txIndex > 0; got != want {
				t.Fatalf("block %d tx %d: BVM_MNT has the upgrade code = %v, want %v", tc.number, txIndex, got, !got)
			}
		}
	}
}
