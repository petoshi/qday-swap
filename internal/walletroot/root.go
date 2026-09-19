// Package walletroot owns the single recovery root used by the QDAY Swap
// application. Supported chains never reuse a private key: they interpret or
// derive from the same recoverable entropy in separate domains.
package walletroot

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/tyler-smith/go-bip39"
)

const hardened = hdkeychain.HardenedKeyStart

// Root is the 256-bit entropy encoded by the application's 24 words. QDAY
// consumes these bytes directly; other wallets derive isolated BIP32 branches.
type Root [32]byte

func New() (Root, error) {
	var root Root
	if _, err := rand.Read(root[:]); err != nil {
		return Root{}, fmt.Errorf("generate wallet entropy: %w", err)
	}
	return root, nil
}

func ParsePhrase(phrase string) (Root, error) {
	var root Root
	canonical := strings.Join(strings.Fields(strings.ToLower(phrase)), " ")
	if len(strings.Fields(canonical)) != 24 || !bip39.IsMnemonicValid(canonical) {
		return root, errors.New("recovery phrase must be 24 valid BIP39 words")
	}
	entropy, err := bip39.EntropyFromMnemonic(canonical)
	if err != nil {
		return root, fmt.Errorf("decode recovery phrase: %w", err)
	} else if len(entropy) != len(root) {
		return root, errors.New("recovery phrase does not contain 256 bits of entropy")
	}
	copy(root[:], entropy)
	clear(entropy)
	return root, nil
}

func (r Root) Phrase() (string, error) {
	return bip39.NewMnemonic(r[:])
}

// QDAYSeed returns the entropy format expected by QDAY's existing wallet.
func (r Root) QDAYSeed() [32]byte { return [32]byte(r) }

func (r Root) bip32Master() (*hdkeychain.ExtendedKey, error) {
	phrase, err := r.Phrase()
	if err != nil {
		return nil, err
	}
	seed := bip39.NewSeed(phrase, "")
	defer clear(seed)
	return hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
}

// BIPKey derives m/purpose'/coin_type'/account'/change/index. Purpose and coin
// type are explicit so every chain adapter gets an isolated standard wallet
// branch. Network-specific address encoding happens in the adapter; BIP32
// scalar derivation itself is network independent.
func (r Root) BIPKey(purpose, coinType, account, change, index uint32) (*btcec.PrivateKey, error) {
	if purpose >= hardened || coinType >= hardened || account >= hardened || change > 1 || index >= hardened {
		return nil, errors.New("invalid BIP derivation path")
	}
	key, err := r.bip32Master()
	if err != nil {
		return nil, err
	}
	defer key.Zero()
	path := []uint32{purpose + hardened, coinType + hardened, account + hardened, change, index}
	for _, child := range path {
		next, err := key.Derive(child)
		if err != nil {
			return nil, fmt.Errorf("derive BIP32 key path: %w", err)
		}
		key.Zero()
		key = next
	}
	private, err := key.ECPrivKey()
	if err != nil {
		return nil, fmt.Errorf("decode BIP32 private key: %w", err)
	}
	return private, nil
}

// BitcoinKey derives the native SegWit branch m/84'/0'/account'/change/index.
func (r Root) BitcoinKey(account, change, index uint32) (*btcec.PrivateKey, error) {
	return r.BIPKey(84, 0, account, change, index)
}

// BitcoinAddress returns the BIP84 native SegWit P2WPKH address for the
// selected wallet child. Mainnet addresses begin with bc1q; callers may pass
// TestNet3Params or RegressionNetParams for isolated tests.
func (r Root) BitcoinAddress(account, change, index uint32, network *chaincfg.Params) (string, error) {
	if network == nil {
		return "", errors.New("Bitcoin network parameters are required")
	}
	key, err := r.BitcoinKey(account, change, index)
	if err != nil {
		return "", err
	}
	program := btcutil.Hash160(key.PubKey().SerializeCompressed())
	address, err := btcutil.NewAddressWitnessPubKeyHash(program, network)
	if err != nil {
		return "", fmt.Errorf("encode native SegWit address: %w", err)
	}
	return address.EncodeAddress(), nil
}

// LitecoinKey derives the native SegWit branch m/84'/2'/account'/change/index.
func (r Root) LitecoinKey(account, change, index uint32) (*btcec.PrivateKey, error) {
	return r.BIPKey(84, 2, account, change, index)
}

// DogecoinKey derives the legacy branch m/44'/3'/account'/change/index.
func (r Root) DogecoinKey(account, change, index uint32) (*btcec.PrivateKey, error) {
	return r.BIPKey(44, 3, account, change, index)
}

// BitcoinCashKey derives the legacy branch m/44'/145'/account'/change/index.
func (r Root) BitcoinCashKey(account, change, index uint32) (*btcec.PrivateKey, error) {
	return r.BIPKey(44, 145, account, change, index)
}

// EVMKey derives m/44'/60'/account'/change/index for Ethereum-compatible
// networks. Address construction and transaction signing live in the EVM
// adapter.
func (r Root) EVMKey(account, change, index uint32) (*btcec.PrivateKey, error) {
	return r.BIPKey(44, 60, account, change, index)
}

// DomainSeed creates deterministic application-only material without
// overlapping either chain's wallet tree.
func (r Root) DomainSeed(domain string, context []byte) ([32]byte, error) {
	var result [32]byte
	if domain == "" || len(domain) > 128 {
		return result, errors.New("invalid wallet derivation domain")
	}
	mac := hmac.New(sha512.New, r[:])
	_, _ = mac.Write([]byte("QDAY/swap/root/v1/"))
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(context)
	sum := mac.Sum(nil)
	copy(result[:], sum[:32])
	clear(sum)
	return result, nil
}

// OrderIdentity derives the Ed25519 identity used to sign public offers and
// relay messages. It is separate from every QDAY, Bitcoin and swap-contract
// key while remaining recoverable from the same 24 words.
func (r Root) OrderIdentity() (ed25519.PrivateKey, error) {
	seed, err := r.DomainSeed("order-identity-ed25519-v1", nil)
	if err != nil {
		return nil, err
	}
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	clear(seed[:])
	return privateKey, nil
}
