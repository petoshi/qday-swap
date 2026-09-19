package litecoin

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

func testContract(t *testing.T) (Contract, *btcec.PrivateKey, *btcec.PrivateKey, [32]byte) {
	t.Helper()
	recipient, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	refund, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	secret := sha256.Sum256([]byte("qday-swap-test-secret"))
	secretHash := sha256.Sum256(secret[:])
	contract, err := NewContract(2*LitoshisPerLitecoin, secretHash, recipient.PubKey().SerializeCompressed(), refund.PubKey().SerializeCompressed(), 500)
	if err != nil {
		t.Fatal(err)
	}
	return contract, recipient, refund, secret
}

func testFunding(t *testing.T, contract Contract) Funding {
	t.Helper()
	pkScript, err := contract.PkScript()
	if err != nil {
		t.Fatal(err)
	}
	tx := wire.NewMsgTx(2)
	// A zero-input transaction is ambiguous with the SegWit marker in the wire
	// format. Use a deterministic dummy input; only the resulting outpoint is
	// relevant to these contract-spend tests.
	previous := wire.OutPoint{Index: 7}
	tx.AddTxIn(wire.NewTxIn(&previous, nil, nil))
	tx.AddTxOut(wire.NewTxOut(contract.Amount, pkScript))
	raw, err := EncodeTransaction(tx)
	if err != nil {
		t.Fatal(err)
	}
	funding, err := contract.FundingFromRaw(raw)
	if err != nil {
		t.Fatal(err)
	}
	return funding
}

func executeSpend(t *testing.T, contract Contract, raw string) {
	t.Helper()
	tx, err := DecodeTransaction(raw)
	if err != nil {
		t.Fatal(err)
	}
	pkScript, err := contract.PkScript()
	if err != nil {
		t.Fatal(err)
	}
	fetcher := txscript.NewCannedPrevOutputFetcher(pkScript, contract.Amount)
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)
	engine, err := txscript.NewEngine(pkScript, tx, 0, txscript.StandardVerifyFlags, nil, sigHashes, contract.Amount, fetcher)
	if err != nil {
		t.Fatal(err)
	} else if err := engine.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestContractClaimAndRefund(t *testing.T) {
	contract, recipient, refund, secret := testContract(t)
	funding := testFunding(t, contract)
	destination := []byte{txscript.OP_TRUE}

	claim, err := contract.BuildClaim(funding, destination, 2_000, recipient, secret)
	if err != nil {
		t.Fatal(err)
	}
	executeSpend(t, contract, claim)
	revealed, err := contract.ExtractSecret(claim, funding)
	if err != nil {
		t.Fatal(err)
	} else if revealed != secret {
		t.Fatal("claim revealed another secret")
	}

	refundRaw, err := contract.BuildRefund(funding, destination, 2_000, refund)
	if err != nil {
		t.Fatal(err)
	}
	executeSpend(t, contract, refundRaw)
	if _, err := contract.ExtractSecret(refundRaw, funding); err == nil {
		t.Fatal("refund was accepted as a claim")
	}
}

func TestContractRejectsChangedTerms(t *testing.T) {
	contract, recipient, refund, secret := testContract(t)
	funding := testFunding(t, contract)
	destination := []byte{txscript.OP_TRUE}

	wrongSecret := secret
	wrongSecret[0] ^= 1
	if _, err := contract.BuildClaim(funding, destination, 2_000, recipient, wrongSecret); err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("wrong secret error = %v", err)
	}
	if _, err := contract.BuildClaim(funding, destination, 2_000, refund, secret); err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("wrong claim key error = %v", err)
	}
	if _, err := contract.BuildRefund(funding, destination, 2_000, recipient); err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("wrong refund key error = %v", err)
	}
	if _, err := contract.BuildClaim(funding, destination, contract.Amount, recipient, secret); err == nil || !strings.Contains(err.Error(), "fee") {
		t.Fatalf("oversized fee error = %v", err)
	}

	pkScript, err := contract.PkScript()
	if err != nil {
		t.Fatal(err)
	}
	duplicate := wire.NewMsgTx(2)
	previous := wire.OutPoint{Index: 9}
	duplicate.AddTxIn(wire.NewTxIn(&previous, nil, nil))
	duplicate.AddTxOut(wire.NewTxOut(contract.Amount, pkScript))
	duplicate.AddTxOut(wire.NewTxOut(contract.Amount, pkScript))
	raw, err := EncodeTransaction(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contract.FundingFromRaw(raw); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate funding error = %v", err)
	}
}
