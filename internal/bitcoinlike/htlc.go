package bitcoinlike

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

const AtomsPerCoin = int64(100_000_000)

// Contract is the complete public descriptor of one Bitcoin-family P2WSH swap
// output. Private keys and the hash preimage are deliberately absent.
type Contract struct {
	Amount          int64
	SecretHash      [32]byte
	RecipientPubKey [33]byte
	RefundPubKey    [33]byte
	RefundHeight    uint32
}

// Funding identifies the exact output committed to a contract.
type Funding struct {
	TxID   string
	Hash   chainhash.Hash
	Vout   uint32
	Amount int64
	Raw    string
}

// NewContract validates and copies a portable contract descriptor.
func NewContract(amount int64, secretHash [32]byte, recipientPubKey, refundPubKey []byte, refundHeight uint32) (Contract, error) {
	var c Contract
	if amount <= 0 {
		return c, errors.New("contract amount must be positive")
	} else if secretHash == ([32]byte{}) {
		return c, errors.New("contract secret hash is zero")
	} else if refundHeight == 0 {
		return c, errors.New("contract refund height is zero")
	} else if len(recipientPubKey) != len(c.RecipientPubKey) || len(refundPubKey) != len(c.RefundPubKey) {
		return c, errors.New("contract keys must be compressed 33-byte secp256k1 public keys")
	}
	if _, err := btcec.ParsePubKey(recipientPubKey); err != nil {
		return c, fmt.Errorf("invalid recipient key: %w", err)
	} else if _, err := btcec.ParsePubKey(refundPubKey); err != nil {
		return c, fmt.Errorf("invalid refund key: %w", err)
	}
	c.Amount = amount
	c.SecretHash = secretHash
	copy(c.RecipientPubKey[:], recipientPubKey)
	copy(c.RefundPubKey[:], refundPubKey)
	c.RefundHeight = refundHeight
	return c, nil
}

// WitnessScript returns the canonical SHA-256 claim / absolute-height refund
// script used by QDAY Swap.
func (c Contract) WitnessScript() ([]byte, error) {
	if _, err := NewContract(c.Amount, c.SecretHash, c.RecipientPubKey[:], c.RefundPubKey[:], c.RefundHeight); err != nil {
		return nil, err
	}
	return txscript.NewScriptBuilder().
		AddOp(txscript.OP_IF).
		AddOp(txscript.OP_SHA256).
		AddData(c.SecretHash[:]).
		AddOp(txscript.OP_EQUALVERIFY).
		AddData(c.RecipientPubKey[:]).
		AddOp(txscript.OP_CHECKSIG).
		AddOp(txscript.OP_ELSE).
		AddInt64(int64(c.RefundHeight)).
		AddOp(txscript.OP_CHECKLOCKTIMEVERIFY).
		AddOp(txscript.OP_DROP).
		AddData(c.RefundPubKey[:]).
		AddOp(txscript.OP_CHECKSIG).
		AddOp(txscript.OP_ENDIF).
		Script()
}

// PkScript returns the native SegWit v0 P2WSH output script.
func (c Contract) PkScript() ([]byte, error) {
	witnessScript, err := c.WitnessScript()
	if err != nil {
		return nil, err
	}
	witnessHash := sha256.Sum256(witnessScript)
	return txscript.NewScriptBuilder().AddOp(txscript.OP_0).AddData(witnessHash[:]).Script()
}

// FindFunding finds exactly one output matching the immutable amount and
// script. Ambiguous transactions are rejected so a session always names one
// outpoint.
func (c Contract) FindFunding(tx *wire.MsgTx) (uint32, error) {
	if tx == nil {
		return 0, errors.New("funding transaction is nil")
	}
	pkScript, err := c.PkScript()
	if err != nil {
		return 0, err
	}
	found := -1
	for i, output := range tx.TxOut {
		if output.Value != c.Amount || !bytes.Equal(output.PkScript, pkScript) {
			continue
		}
		if found >= 0 {
			return 0, errors.New("funding transaction contains duplicate contract outputs")
		}
		found = i
	}
	if found < 0 {
		return 0, errors.New("funding transaction does not contain the contract output")
	}
	return uint32(found), nil
}

// FundingFromRaw verifies a signed funding transaction and returns its exact
// outpoint.
func (c Contract) FundingFromRaw(raw string) (Funding, error) {
	tx, err := DecodeTransaction(raw)
	if err != nil {
		return Funding{}, err
	}
	vout, err := c.FindFunding(tx)
	if err != nil {
		return Funding{}, err
	}
	hash := tx.TxHash()
	return Funding{TxID: tx.TxID(), Hash: hash, Vout: vout, Amount: c.Amount, Raw: raw}, nil
}

func (c Contract) buildSpend(funding Funding, destinationScript []byte, fee int64, key *btcec.PrivateKey, secret *[32]byte) (string, error) {
	if funding.Amount != c.Amount {
		return "", errors.New("funding amount does not match contract")
	} else if funding.TxID != "" && funding.TxID != funding.Hash.String() {
		return "", errors.New("funding transaction ID does not match its hash")
	} else if len(destinationScript) == 0 {
		return "", errors.New("spend destination script is empty")
	} else if fee <= 0 || fee >= c.Amount {
		return "", errors.New("spend fee must be positive and smaller than the contract amount")
	} else if key == nil {
		return "", errors.New("spend key is nil")
	}
	claim := secret != nil
	expectedKey := c.RefundPubKey[:]
	if claim {
		if sha256.Sum256(secret[:]) != c.SecretHash {
			return "", errors.New("claim secret does not match the contract hash")
		}
		expectedKey = c.RecipientPubKey[:]
	}
	if !bytes.Equal(key.PubKey().SerializeCompressed(), expectedKey) {
		return "", errors.New("spend key does not match the selected contract branch")
	}

	witnessScript, err := c.WitnessScript()
	if err != nil {
		return "", err
	}
	pkScript, err := c.PkScript()
	if err != nil {
		return "", err
	}
	tx := wire.NewMsgTx(2)
	input := wire.NewTxIn(wire.NewOutPoint(&funding.Hash, funding.Vout), nil, nil)
	input.Sequence = 0xfffffffe
	tx.AddTxIn(input)
	tx.AddTxOut(wire.NewTxOut(c.Amount-fee, bytes.Clone(destinationScript)))
	if !claim {
		tx.LockTime = c.RefundHeight
	}
	fetcher := txscript.NewCannedPrevOutputFetcher(pkScript, c.Amount)
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)
	sig, err := txscript.RawTxInWitnessSignature(tx, sigHashes, 0, c.Amount, witnessScript, txscript.SigHashAll, key)
	if err != nil {
		return "", fmt.Errorf("sign contract spend: %w", err)
	}
	if claim {
		tx.TxIn[0].Witness = wire.TxWitness{sig, secret[:], []byte{1}, witnessScript}
	} else {
		tx.TxIn[0].Witness = wire.TxWitness{sig, nil, witnessScript}
	}
	return EncodeTransaction(tx)
}

// BuildClaim creates a fully signed transaction that reveals the preimage.
func (c Contract) BuildClaim(funding Funding, destinationScript []byte, fee int64, key *btcec.PrivateKey, secret [32]byte) (string, error) {
	return c.buildSpend(funding, destinationScript, fee, key, &secret)
}

// BuildClaimTemplate signs the claim branch while leaving the hash preimage
// empty. SegWit signatures commit to the transaction and witness script, but
// not to witness stack items, so the secret owner can safely complete and
// relay these exact terms later without holding the recipient's private key.
func (c Contract) BuildClaimTemplate(funding Funding, destinationScript []byte, fee int64, key *btcec.PrivateKey) (string, error) {
	if funding.Amount != c.Amount {
		return "", errors.New("funding amount does not match contract")
	} else if funding.TxID != "" && funding.TxID != funding.Hash.String() {
		return "", errors.New("funding transaction ID does not match its hash")
	} else if len(destinationScript) == 0 {
		return "", errors.New("spend destination script is empty")
	} else if fee <= 0 || fee >= c.Amount {
		return "", errors.New("spend fee must be positive and smaller than the contract amount")
	} else if key == nil || !bytes.Equal(key.PubKey().SerializeCompressed(), c.RecipientPubKey[:]) {
		return "", errors.New("spend key does not match the claim branch")
	}
	witnessScript, err := c.WitnessScript()
	if err != nil {
		return "", err
	}
	pkScript, err := c.PkScript()
	if err != nil {
		return "", err
	}
	tx := wire.NewMsgTx(2)
	input := wire.NewTxIn(wire.NewOutPoint(&funding.Hash, funding.Vout), nil, nil)
	input.Sequence = 0xfffffffe
	tx.AddTxIn(input)
	tx.AddTxOut(wire.NewTxOut(c.Amount-fee, bytes.Clone(destinationScript)))
	fetcher := txscript.NewCannedPrevOutputFetcher(pkScript, c.Amount)
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)
	sig, err := txscript.RawTxInWitnessSignature(tx, sigHashes, 0, c.Amount, witnessScript, txscript.SigHashAll, key)
	if err != nil {
		return "", fmt.Errorf("sign contract claim template: %w", err)
	}
	tx.TxIn[0].Witness = wire.TxWitness{sig, nil, []byte{1}, witnessScript}
	return EncodeTransaction(tx)
}

// CompleteClaimTemplate verifies the exact contract outpoint and canonical
// claim witness, inserts the matching secret, and executes the script before
// returning broadcast-ready bytes.
func (c Contract) CompleteClaimTemplate(raw string, funding Funding, secret [32]byte) (string, error) {
	if sha256.Sum256(secret[:]) != c.SecretHash {
		return "", errors.New("claim secret does not match the contract hash")
	}
	tx, err := DecodeTransaction(raw)
	if err != nil {
		return "", err
	} else if len(tx.TxIn) != 1 || len(tx.TxOut) != 1 {
		return "", errors.New("claim template must contain exactly one input and one output")
	}
	input := tx.TxIn[0]
	if input.PreviousOutPoint.Hash != funding.Hash || input.PreviousOutPoint.Index != funding.Vout {
		return "", errors.New("claim template spends another outpoint")
	} else if tx.LockTime != 0 || input.Sequence == 0xffffffff {
		return "", errors.New("claim template lock fields are invalid")
	} else if tx.TxOut[0].Value <= 0 || tx.TxOut[0].Value >= c.Amount {
		return "", errors.New("claim template output amount is invalid")
	}
	witnessScript, err := c.WitnessScript()
	if err != nil {
		return "", err
	}
	witness := input.Witness
	if len(witness) != 4 || len(witness[0]) == 0 || len(witness[1]) != 0 || len(witness[2]) == 0 || !bytes.Equal(witness[3], witnessScript) {
		return "", errors.New("claim template does not contain the canonical incomplete witness")
	}
	input.Witness[1] = bytes.Clone(secret[:])
	pkScript, err := c.PkScript()
	if err != nil {
		return "", err
	}
	fetcher := txscript.NewCannedPrevOutputFetcher(pkScript, c.Amount)
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)
	engine, err := txscript.NewEngine(pkScript, tx, 0, txscript.StandardVerifyFlags, nil, sigHashes, c.Amount, fetcher)
	if err != nil {
		return "", fmt.Errorf("verify completed claim template: %w", err)
	} else if err := engine.Execute(); err != nil {
		return "", fmt.Errorf("verify completed claim template: %w", err)
	}
	return EncodeTransaction(tx)
}

// BuildRefund creates a fully signed transaction that becomes valid at the
// contract's absolute refund height.
func (c Contract) BuildRefund(funding Funding, destinationScript []byte, fee int64, key *btcec.PrivateKey) (string, error) {
	return c.buildSpend(funding, destinationScript, fee, key, nil)
}

// ExtractSecret accepts only the canonical claim witness and verifies both the
// funding outpoint and the SHA-256 hash before returning the public preimage.
func (c Contract) ExtractSecret(raw string, funding Funding) ([32]byte, error) {
	var secret [32]byte
	tx, err := DecodeTransaction(raw)
	if err != nil {
		return secret, err
	} else if len(tx.TxIn) != 1 {
		return secret, errors.New("claim must contain exactly one input")
	}
	previous := tx.TxIn[0].PreviousOutPoint
	if previous.Hash != funding.Hash || previous.Index != funding.Vout {
		return secret, errors.New("claim spends another outpoint")
	}
	witness := tx.TxIn[0].Witness
	if len(witness) != 4 || len(witness[1]) != len(secret) || len(witness[2]) == 0 {
		return secret, errors.New("claim does not contain the canonical HTLC witness")
	}
	witnessScript, err := c.WitnessScript()
	if err != nil {
		return secret, err
	} else if !bytes.Equal(witness[3], witnessScript) {
		return secret, errors.New("claim witness script does not match the contract")
	}
	copy(secret[:], witness[1])
	if sha256.Sum256(secret[:]) != c.SecretHash {
		return [32]byte{}, errors.New("claim reveals the wrong secret")
	}
	return secret, nil
}

// IsRefund recognizes the canonical refund branch of this exact contract.
// A confirmed transaction that passes this check can only have spent through
// the absolute-height refund path; signature validity is enforced by Bitcoin
// consensus before confirmation.
func (c Contract) IsRefund(raw string, funding Funding) (bool, error) {
	tx, err := DecodeTransaction(raw)
	if err != nil {
		return false, err
	} else if len(tx.TxIn) != 1 {
		return false, errors.New("refund must contain exactly one input")
	}
	input := tx.TxIn[0]
	if input.PreviousOutPoint.Hash != funding.Hash || input.PreviousOutPoint.Index != funding.Vout {
		return false, errors.New("refund spends another outpoint")
	} else if tx.LockTime < c.RefundHeight || input.Sequence == 0xffffffff {
		return false, errors.New("refund locktime is invalid")
	}
	witnessScript, err := c.WitnessScript()
	if err != nil {
		return false, err
	}
	witness := input.Witness
	if len(witness) != 3 || len(witness[0]) == 0 || len(witness[1]) != 0 || !bytes.Equal(witness[2], witnessScript) {
		return false, errors.New("refund does not contain the canonical HTLC witness")
	}
	return true, nil
}
