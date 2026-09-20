package swap

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/petoshi/qday-swap/internal/order"
	"github.com/petoshi/qday-swap/internal/trade"
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

func TestAsyncAgreementUsesQueuedTakerPackageAndLongerTakerRefund(t *testing.T) {
	for _, makerGives := range []string{"QDAY", "BTC"} {
		now := time.Unix(1_800_000_000, 0)
		makerPublic, makerPrivate, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		_, takerPrivate, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		makerBitcoin, _ := btcec.NewPrivateKey()
		takerBitcoin, _ := btcec.NewPrivateKey()
		give := order.Amount{Asset: "BTC", Atomic: "250000"}
		receive := order.Amount{Asset: "QDAY", Atomic: order.QDAYDefendUnit}
		if makerGives == "QDAY" {
			give, receive = receive, give
		}
		payload, err := order.NewAsyncPayload(
			"mainnet", order.QDAYDefendUnit, give, receive, 24*time.Hour,
			makerPublic, [32]byte{1}, strings.Repeat("1", 64),
			order.QDAYKeys{Classical: strings.Repeat("2", 64), Reserve: strings.Repeat("3", 64), Address: "qday1maker"},
			hex.EncodeToString(makerBitcoin.PubKey().SerializeCompressed()), 10_000, 900_000, now,
		)
		if err != nil {
			t.Fatal(err)
		}
		signed, err := order.Sign(payload, makerPrivate, now)
		if err != nil {
			t.Fatal(err)
		}
		createdAt := now.Add(time.Hour)
		qdayRefund, bitcoinRefund, err := trade.AsyncRefundHeights(payload, createdAt.Unix(), 10_010, 900_002)
		if err != nil {
			t.Fatal(err)
		}
		fundingAsset := signed.Order.Receive.Asset
		funding := trade.FundingPackage{Asset: fundingAsset, TransactionID: strings.Repeat("7", 64)}
		if fundingAsset == "BTC" {
			funding.RawTransactions = []string{"010203"}
		} else {
			funding.RawTransactions = []string{"AQID"}
			funding.BasisHeight = 10_010
			funding.BasisID = strings.Repeat("8", 64)
		}
		acceptance, err := trade.NewAsyncAcceptance(
			signed, strings.Repeat("4", 64),
			order.QDAYKeys{Classical: strings.Repeat("5", 64), Reserve: strings.Repeat("6", 64), Address: "qday1taker"},
			hex.EncodeToString(takerBitcoin.PubKey().SerializeCompressed()), 10_010, 900_002,
			strings.Repeat("9", 64), qdayRefund, bitcoinRefund, funding,
			takerPrivate, [32]byte{2}, createdAt,
		)
		if err != nil {
			t.Fatal(err)
		}
		agreement, err := BuildAsyncAgreement(signed, acceptance)
		if err != nil {
			t.Fatal(err)
		} else if err := agreement.ValidateAsync(signed, acceptance); err != nil {
			t.Fatal(err)
		}
		remainingQDAY := agreement.QDAYRefundHeight - max(payload.MakerQDAYHeight, acceptance.Acceptance.TakerQDAYHeight)
		remainingBitcoin := agreement.BitcoinRefundHeight - max(payload.MakerBTCHeight, acceptance.Acceptance.TakerBTCHeight)
		if makerGives == "QDAY" && uint64(remainingBitcoin)*10 <= remainingQDAY {
			t.Fatal("taker Bitcoin leg did not receive the longer refund window")
		} else if makerGives == "BTC" && uint64(remainingBitcoin)*10 >= remainingQDAY {
			t.Fatal("taker QDAY leg did not receive the longer refund window")
		}
	}
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

	template := ClaimTemplate{
		Party: PartyMaker, Asset: "QDAY",
		FundingTransactionID: strings.Repeat("77", 32), TransactionID: strings.Repeat("88", 32),
		RawTransactions: []string{"AQID"}, BasisHeight: 120, BasisID: strings.Repeat("99", 32),
	}
	claimMessage := Message{Version: AsyncProtocolVersion, Kind: MessageClaimTemplate, TradeID: maker.TradeID, ClaimTemplate: &template}
	if err := claimMessage.ValidateBasic(); err != nil {
		t.Fatal(err)
	}
	claimMessage.Funding = &FundingNotice{Party: PartyMaker, Asset: "QDAY", TransactionID: strings.Repeat("aa", 32)}
	if err := claimMessage.ValidateBasic(); err == nil {
		t.Fatal("claim template mixed with a funding notice was accepted")
	}
}
