package swap

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/petoshi/qday-swap/internal/order"
)

func testAgreement(t *testing.T, makerGives string) (order.Signed, Hello, Hello, Agreement) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var messageKey [32]byte
	messageKey[0] = 1
	give := order.Amount{Asset: "BTC", Atomic: "250000000"}
	receive := order.Amount{Asset: "QDAY", Atomic: order.QDAYLegacyUnit}
	if makerGives == "QDAY" {
		give = order.Amount{Asset: "QDAY", Atomic: order.QDAYLegacyUnit}
		receive = order.Amount{Asset: "BTC", Atomic: "250000000"}
	}
	now := time.Unix(1_800_000_000, 0)
	payload, err := order.NewPayload("mainnet", order.QDAYLegacyUnit, give, receive, time.Hour, public, messageKey, now)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := order.Sign(payload, private, now)
	if err != nil {
		t.Fatal(err)
	}
	key1, _ := btcec.NewPrivateKey()
	key2, _ := btcec.NewPrivateKey()
	keys := QDAYKeys{Classical: strings.Repeat("11", 32), Reserve: strings.Repeat("22", 32), Address: "qday1ptest"}
	maker, err := NewHello("mainnet", signed.ID, strings.Repeat("33", 32), PartyMaker, keys, key1.PubKey().SerializeCompressed(), 100, 200)
	if err != nil {
		t.Fatal(err)
	}
	keys = QDAYKeys{Classical: strings.Repeat("44", 32), Reserve: strings.Repeat("55", 32), Address: "qday1ptaker"}
	taker, err := NewHello("mainnet", signed.ID, maker.TradeID, PartyTaker, keys, key2.PubKey().SerializeCompressed(), 105, 198)
	if err != nil {
		t.Fatal(err)
	}
	agreement, err := BuildAgreement(signed, maker.TradeID, strings.Repeat("66", 32), maker, taker)
	if err != nil {
		t.Fatal(err)
	}
	return signed, maker, taker, agreement
}

func TestAgreementBindsOrderParticipantsAndRefundWindows(t *testing.T) {
	for _, asset := range []string{"QDAY", "BTC"} {
		signed, maker, taker, agreement := testAgreement(t, asset)
		if err := agreement.Validate(signed, maker, taker); err != nil {
			t.Fatal(err)
		}
		if agreement.QDAYConfirmations != 3 || agreement.BitcoinConfirmations != 1 {
			t.Fatal("confirmation policy changed")
		}
		if asset == "QDAY" {
			if agreement.QDAYRefundHeight != 105+makerQDAYWindow || agreement.BitcoinRefundHeight != 200+takerBitcoinWindow {
				t.Fatal("wrong QDAY-first refund windows")
			}
		} else if agreement.QDAYRefundHeight != 105+takerQDAYWindow || agreement.BitcoinRefundHeight != 200+makerBitcoinWindow {
			t.Fatal("wrong BTC-first refund windows")
		}
		first, err := agreement.Hash()
		if err != nil {
			t.Fatal(err)
		}
		agreement.BitcoinAmountSatoshi++
		second, err := agreement.Hash()
		if err != nil {
			t.Fatal(err)
		}
		if first == second {
			t.Fatal("agreement hash ignored amount change")
		} else if err := agreement.Validate(signed, maker, taker); err == nil {
			t.Fatal("changed amount was accepted")
		}
	}
}

func TestMessageRejectsMixedPayloads(t *testing.T) {
	_, maker, _, agreement := testAgreement(t, "QDAY")
	message := Message{Version: ProtocolVersion, Kind: MessageHello, TradeID: maker.TradeID, Hello: &maker}
	if err := message.ValidateBasic(); err != nil {
		t.Fatal(err)
	}
	message.Agreement = &agreement
	if err := message.ValidateBasic(); err == nil {
		t.Fatal("mixed message payload was accepted")
	}
}
