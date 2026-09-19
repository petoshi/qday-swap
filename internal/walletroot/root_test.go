package walletroot

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
)

const zeroPhrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art"

func TestQDAYPhraseCompatibility(t *testing.T) {
	root := Root{}
	phrase, err := root.Phrase()
	if err != nil {
		t.Fatal(err)
	} else if phrase != zeroPhrase {
		t.Fatalf("zero entropy phrase = %q", phrase)
	}
	decoded, err := ParsePhrase(phrase)
	if err != nil {
		t.Fatal(err)
	} else if decoded != root || decoded.QDAYSeed() != [32]byte{} {
		t.Fatal("recovery phrase changed QDAY entropy")
	}
}

func TestChainAndApplicationDomainsAreSeparated(t *testing.T) {
	root := Root{1, 2, 3, 4}
	first, err := root.LitecoinKey(0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := root.LitecoinKey(0, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := root.LitecoinKey(0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Serialize(), second.Serialize()) {
		t.Fatal("two Litecoin child indices derived the same key")
	} else if !bytes.Equal(first.Serialize(), repeated.Serialize()) {
		t.Fatal("Litecoin derivation is not deterministic")
	}
	bitcoin, err := root.BitcoinKey(0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	dogecoin, err := root.DogecoinKey(0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	bitcoinCash, err := root.BitcoinCashKey(0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	evm, err := root.EVMKey(0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	keys := [][]byte{first.Serialize(), bitcoin.Serialize(), dogecoin.Serialize(), bitcoinCash.Serialize(), evm.Serialize()}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if bytes.Equal(keys[i], keys[j]) {
				t.Fatalf("chain wallet branches %d and %d overlap", i, j)
			}
		}
	}

	identity, err := root.DomainSeed("relay-identity", nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := root.DomainSeed("litecoin-session", []byte("trade-1"))
	if err != nil {
		t.Fatal(err)
	}
	if identity == session || bytes.Equal(identity[:], first.Serialize()) {
		t.Fatal("application derivation domains overlap")
	}
}

func TestRejectsInvalidPhraseAndPath(t *testing.T) {
	if _, err := ParsePhrase("abandon abandon"); err == nil {
		t.Fatal("short phrase accepted")
	}
	root := Root{9}
	if _, err := root.LitecoinKey(0, 2, 0); err == nil {
		t.Fatal("invalid change branch accepted")
	} else if _, err := root.BIPKey(hardened, 0, 0, 0, 0); err == nil {
		t.Fatal("invalid purpose accepted")
	}
	if _, err := root.DomainSeed("", nil); err == nil {
		t.Fatal("empty derivation domain accepted")
	}
}

func TestBitcoinAddressUsesNativeSegWit(t *testing.T) {
	root, err := ParsePhrase(zeroPhrase)
	if err != nil {
		t.Fatal(err)
	}
	first, err := root.BitcoinAddress(0, 0, 0, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := root.BitcoinAddress(0, 0, 0, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	change, err := root.BitcoinAddress(0, 1, 0, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	if first != repeated {
		t.Fatal("Bitcoin address derivation is not deterministic")
	} else if len(first) < 4 || first[:4] != "bc1q" {
		t.Fatalf("Bitcoin address %q is not native SegWit P2WPKH", first)
	} else if change == first {
		t.Fatal("receive and change branches derived the same address")
	}
	decoded, err := btcutil.DecodeAddress(first, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	} else if _, ok := decoded.(*btcutil.AddressWitnessPubKeyHash); !ok {
		t.Fatalf("Bitcoin address has type %T, expected native P2WPKH", decoded)
	}
	if _, err := root.BitcoinAddress(0, 0, 0, nil); err == nil {
		t.Fatal("nil Bitcoin network parameters accepted")
	}
}

func TestOrderIdentityIsRecoverableAndSeparated(t *testing.T) {
	root := Root{1, 2, 3, 4}
	first, err := root.OrderIdentity()
	if err != nil {
		t.Fatal(err)
	}
	second, err := root.OrderIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("order identity is not recoverable")
	}
	bitcoin, err := root.BitcoinKey(0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first[:32], bitcoin.Serialize()) || len(first.Public().(ed25519.PublicKey)) != ed25519.PublicKeySize {
		t.Fatal("order identity overlaps a chain key or has an invalid public key")
	}
}
