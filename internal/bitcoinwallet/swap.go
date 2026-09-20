package bitcoinwallet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/neutrino"
	"github.com/lightninglabs/neutrino/headerfs"
	"github.com/petoshi/qday-swap/internal/bitcoin"
)

const (
	// The fee rate is deliberately conservative for the first release. The
	// exact fee is still computed by btcwallet from the signed transaction
	// size, and can be made user-selectable without changing the protocol.
	defaultFundingFeeRate = btcutil.Amount(10_000) // 10 sat/vB
	waddrmgrNamespace     = "waddrmgr"
	wtxmgrNamespace       = "wtxmgr"
)

var ErrTransactionNotFound = errors.New("Bitcoin transaction not found")

const contractRescanSafetyBlocks = int32(6)

type contractWatch struct {
	startHeight int32
	done        chan struct{}
	err         error
}

// Transaction is the chain state needed by the swap engine. Raw is always the
// canonical signed wire transaction, including witness data.
type Transaction struct {
	ID            string
	Raw           string
	Confirmations int32
	Height        int32
}

func (c *Client) requireSwapWallet() error {
	if c == nil || c.wallet == nil || c.service == nil || c.lightClient == nil {
		return errors.New("Bitcoin wallet is unavailable")
	}
	return nil
}

// WatchContract imports the public P2WSH witness script, adds its address to
// the active Neutrino watch set, and scans from the height signed into the swap
// agreement. The short historical scan is required when one trader funds the
// contract while the other trader's application is offline.
func (c *Client) WatchContract(ctx context.Context, contract bitcoin.Contract, agreementHeight uint32) error {
	if err := c.requireSwapWallet(); err != nil {
		return err
	}
	witnessScript, err := contract.WitnessScript()
	if err != nil {
		return err
	}
	witnessHash := sha256.Sum256(witnessScript)
	contractAddress, err := address.NewAddressWitnessScriptHash(witnessHash[:], c.wallet.ChainParams())
	if err != nil {
		return fmt.Errorf("derive Bitcoin swap address: %w", err)
	}

	chainTip := c.wallet.SyncedTo()
	if chainTip.Height < 0 {
		return errors.New("Bitcoin wallet has not reached the genesis block")
	}
	startHeight := chainTip.Height
	if agreementHeight <= uint32(chainTip.Height) {
		startHeight = int32(agreementHeight)
	}
	if startHeight > contractRescanSafetyBlocks {
		startHeight -= contractRescanSafetyBlocks
	} else {
		startHeight = 0
	}
	watchID := hex.EncodeToString(witnessHash[:])
	c.watchMu.Lock()
	if existing := c.watches[watchID]; existing != nil && existing.startHeight <= startHeight {
		c.watchMu.Unlock()
		return waitForContractWatch(ctx, existing)
	}
	c.watchMu.Unlock()

	blockHash, err := c.lightClient.GetBlockHash(int64(startHeight))
	if err != nil {
		return fmt.Errorf("find Bitcoin swap rescan block %d: %w", startHeight, err)
	}
	blockHeader, err := c.lightClient.GetBlockHeader(blockHash)
	if err != nil {
		return fmt.Errorf("read Bitcoin swap rescan block %d: %w", startHeight, err)
	}
	stamp := waddrmgr.BlockStamp{Hash: *blockHash, Height: startHeight, Timestamp: blockHeader.Timestamp}
	err = walletdb.Update(c.wallet.Database(), func(tx walletdb.ReadWriteTx) error {
		namespace := tx.ReadWriteBucket([]byte(waddrmgrNamespace))
		if namespace == nil {
			return errors.New("Bitcoin address manager database is unavailable")
		}
		manager, err := c.wallet.Manager.FetchScopedKeyManager(waddrmgr.KeyScopeBIP0084)
		if err != nil {
			return err
		}
		_, err = manager.ImportWitnessScript(namespace, witnessScript, &stamp, 0, false)
		if waddrmgr.IsError(err, waddrmgr.ErrDuplicateAddress) {
			return nil
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("watch Bitcoin swap contract: %w", err)
	}
	if err := c.lightClient.NotifyReceived([]address.Address{contractAddress}); err != nil {
		return fmt.Errorf("watch Bitcoin swap address: %w", err)
	}

	state := &contractWatch{startHeight: startHeight, done: make(chan struct{})}
	c.watchMu.Lock()
	if c.watches == nil {
		c.watches = make(map[string]*contractWatch)
	}
	if existing := c.watches[watchID]; existing != nil && existing.startHeight <= startHeight {
		c.watchMu.Unlock()
		return waitForContractWatch(ctx, existing)
	}
	c.watches[watchID] = state
	c.watchMu.Unlock()

	go func() {
		state.err = c.rescanContract(ctx, contractAddress, contract, stamp)
		c.watchMu.Lock()
		if state.err != nil && c.watches[watchID] == state {
			delete(c.watches, watchID)
		}
		close(state.done)
		c.watchMu.Unlock()
	}()
	return waitForContractWatch(ctx, state)
}

func (c *Client) rescanContract(ctx context.Context, contractAddress address.Address, contract bitcoin.Contract, start waddrmgr.BlockStamp) error {
	end, err := c.service.BestBlock()
	if err != nil {
		return fmt.Errorf("read Bitcoin swap rescan tip: %w", err)
	}
	if end.Height <= start.Height {
		return nil
	}
	pkScript, err := contract.PkScript()
	if err != nil {
		return err
	}
	var callbackErr error
	quit := make(chan struct{})
	rescan := neutrino.NewRescan(
		&neutrino.RescanChainSource{ChainService: c.service},
		neutrino.NotificationHandlers(rpcclient.NotificationHandlers{
			OnFilteredBlockConnected: func(height int32, header *wire.BlockHeader, transactions []*btcutil.Tx) {
				if callbackErr != nil {
					return
				}
				block := wtxmgr.BlockMeta{
					Block: wtxmgr.Block{Hash: header.BlockHash(), Height: height},
					Time:  header.Timestamp,
				}
				for _, transaction := range transactions {
					if err := c.storeContractTransaction(transaction.MsgTx(), block, pkScript); err != nil {
						callbackErr = err
						return
					}
				}
			},
		}),
		neutrino.StartBlock(&headerfs.BlockStamp{Hash: start.Hash, Height: start.Height}),
		neutrino.EndBlock(end),
		neutrino.WatchAddrs(contractAddress),
		neutrino.QuitChan(quit),
	)
	result := rescan.Start()
	select {
	case err := <-result:
		if err != nil {
			return err
		}
		if callbackErr != nil {
			return callbackErr
		}
		return nil
	case <-ctx.Done():
		close(quit)
		<-result
		rescan.WaitForShutdown()
		return ctx.Err()
	}
}

func (c *Client) storeContractTransaction(transaction *wire.MsgTx, block wtxmgr.BlockMeta, contractScript []byte) error {
	record, err := wtxmgr.NewTxRecordFromMsgTx(transaction, block.Time)
	if err != nil {
		return err
	}
	return walletdb.Update(c.wallet.Database(), func(tx walletdb.ReadWriteTx) error {
		namespace := tx.ReadWriteBucket([]byte(wtxmgrNamespace))
		if namespace == nil {
			return errors.New("Bitcoin transaction manager database is unavailable")
		}
		exists, err := c.wallet.TxStore.InsertTxCheckIfExists(namespace, record, &block)
		if err != nil || exists {
			return err
		}
		for outputIndex, output := range record.MsgTx.TxOut {
			if !bytes.Equal(output.PkScript, contractScript) {
				continue
			}
			if err := c.wallet.TxStore.AddCredit(namespace, record, &block, uint32(outputIndex), false); err != nil {
				return err
			}
		}
		return nil
	})
}

func waitForContractWatch(ctx context.Context, state *contractWatch) error {
	select {
	case <-state.done:
		if state.err != nil {
			return fmt.Errorf("rescan Bitcoin swap contract from block %d: %w", state.startHeight, state.err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("rescan Bitcoin swap contract from block %d: %w", state.startHeight, ctx.Err())
	}
}

// PrepareFunding creates and signs the exact contract funding transaction.
// The caller must durably journal Funding.Raw before calling Broadcast.
func (c *Client) PrepareFunding(contract bitcoin.Contract) (bitcoin.Funding, error) {
	if err := c.requireSwapWallet(); err != nil {
		return bitcoin.Funding{}, err
	} else if c.wallet.Locked() {
		return bitcoin.Funding{}, errors.New("Bitcoin wallet is locked")
	}
	pkScript, err := contract.PkScript()
	if err != nil {
		return bitcoin.Funding{}, err
	}
	authored, err := c.wallet.CreateSimpleTx(
		&waddrmgr.KeyScopeBIP0084,
		waddrmgr.DefaultAccountNum,
		[]*wire.TxOut{wire.NewTxOut(contract.Amount, pkScript)},
		1,
		defaultFundingFeeRate,
		wallet.CoinSelectionLargest,
		false,
	)
	if err != nil {
		return bitcoin.Funding{}, fmt.Errorf("prepare Bitcoin swap funding: %w", err)
	}
	raw, err := bitcoin.EncodeTransaction(authored.Tx)
	if err != nil {
		return bitcoin.Funding{}, err
	}
	funding, err := contract.FundingFromRaw(raw)
	if err != nil {
		return bitcoin.Funding{}, fmt.Errorf("verify prepared Bitcoin funding: %w", err)
	}
	for _, input := range authored.Tx.TxIn {
		c.wallet.LockOutpoint(input.PreviousOutPoint)
	}
	return funding, nil
}

// ReserveFunding restores the in-memory btcwallet coin locks for a prepared
// transaction after an application restart. The durable swap journal remains
// the source of truth; btcwallet locks prevent a second prepared transaction
// from selecting the same inputs during this process lifetime.
func (c *Client) ReserveFunding(raw string) error {
	if err := c.requireSwapWallet(); err != nil {
		return err
	}
	tx, err := bitcoin.DecodeTransaction(raw)
	if err != nil {
		return err
	}
	for _, input := range tx.TxIn {
		c.wallet.LockOutpoint(input.PreviousOutPoint)
	}
	return nil
}

func (c *Client) ReleaseFunding(raw string) error {
	if err := c.requireSwapWallet(); err != nil {
		return err
	}
	tx, err := bitcoin.DecodeTransaction(raw)
	if err != nil {
		return err
	}
	for _, input := range tx.TxIn {
		c.wallet.UnlockOutpoint(input.PreviousOutPoint)
	}
	return nil
}

// Broadcast publishes a previously journaled signed transaction. Replaying an
// already known transaction is success, which makes process restarts safe.
func (c *Client) Broadcast(raw string) (Transaction, error) {
	if err := c.requireSwapWallet(); err != nil {
		return Transaction{}, err
	}
	tx, err := bitcoin.DecodeTransaction(raw)
	if err != nil {
		return Transaction{}, err
	}
	hash := tx.TxHash()
	if known, err := c.transaction(hash); err == nil {
		for _, input := range tx.TxIn {
			c.wallet.UnlockOutpoint(input.PreviousOutPoint)
		}
		return known, nil
	} else if !errors.Is(err, ErrTransactionNotFound) {
		return Transaction{}, err
	}
	if err := c.wallet.PublishTransaction(tx, "QDAY atomic swap"); err != nil {
		// A peer or a previous process may have published the same bytes while
		// this process was starting. Ask the wallet once more before failing.
		if known, lookupErr := c.transaction(hash); lookupErr == nil {
			return known, nil
		}
		return Transaction{}, fmt.Errorf("broadcast Bitcoin transaction %s: %w", hash.String(), err)
	}
	for _, input := range tx.TxIn {
		c.wallet.UnlockOutpoint(input.PreviousOutPoint)
	}
	return Transaction{ID: hash.String(), Raw: raw, Height: -1}, nil
}

func (c *Client) Transaction(txid string) (Transaction, error) {
	if err := c.requireSwapWallet(); err != nil {
		return Transaction{}, err
	}
	hash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return Transaction{}, fmt.Errorf("parse Bitcoin transaction ID: %w", err)
	}
	return c.transaction(*hash)
}

func (c *Client) transaction(hash chainhash.Hash) (Transaction, error) {
	result, err := c.wallet.GetTransaction(hash)
	if err != nil {
		if errors.Is(err, wallet.ErrNoTx) {
			return Transaction{}, ErrTransactionNotFound
		}
		return Transaction{}, fmt.Errorf("read Bitcoin transaction %s: %w", hash.String(), err)
	}
	raw := hex.EncodeToString(result.Summary.Transaction)
	return Transaction{
		ID: hash.String(), Raw: raw, Confirmations: result.Confirmations,
		Height: result.Height,
	}, nil
}

// FindSpend returns the claim or refund spending the exact contract outpoint.
func (c *Client) FindSpend(funding bitcoin.Funding) (Transaction, error) {
	if err := c.requireSwapWallet(); err != nil {
		return Transaction{}, err
	}
	cancel := make(chan struct{})
	result, err := c.wallet.GetTransactions(nil, nil, "", cancel)
	if err != nil {
		return Transaction{}, fmt.Errorf("scan Bitcoin wallet transactions: %w", err)
	}
	for _, block := range result.MinedTransactions {
		for _, summary := range block.Transactions {
			if spendsFunding(summary.Tx, funding) {
				return summaryTransaction(summary, block.Height, c.wallet.SyncedTo().Height), nil
			}
		}
	}
	for _, summary := range result.UnminedTransactions {
		if spendsFunding(summary.Tx, funding) {
			return summaryTransaction(summary, -1, c.wallet.SyncedTo().Height), nil
		}
	}
	return Transaction{}, ErrTransactionNotFound
}

func spendsFunding(tx *wire.MsgTx, funding bitcoin.Funding) bool {
	if tx == nil {
		return false
	}
	for _, input := range tx.TxIn {
		if input.PreviousOutPoint.Hash == funding.Hash && input.PreviousOutPoint.Index == funding.Vout {
			return true
		}
	}
	return false
}

func summaryTransaction(summary wallet.TransactionSummary, height, bestHeight int32) Transaction {
	confirmations := int32(0)
	if height >= 0 && bestHeight >= height {
		confirmations = bestHeight - height + 1
	}
	id := ""
	if summary.Hash != nil {
		id = summary.Hash.String()
	} else if summary.Tx != nil {
		id = summary.Tx.TxID()
	}
	return Transaction{
		ID: id, Raw: hex.EncodeToString(summary.Transaction),
		Confirmations: confirmations, Height: height,
	}
}

// DestinationScript derives a new local native SegWit receive script for a
// claim or refund transaction.
func (c *Client) DestinationScript() ([]byte, error) {
	if err := c.requireSwapWallet(); err != nil {
		return nil, err
	}
	addr, err := c.wallet.NewAddress(waddrmgr.DefaultAccountNum, waddrmgr.KeyScopeBIP0084)
	if err != nil {
		return nil, fmt.Errorf("derive Bitcoin swap destination: %w", err)
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, fmt.Errorf("build Bitcoin swap destination: %w", err)
	}
	return script, nil
}
