# QDAY DEX relay

The first QDAY DEX market uses one public service at `dex.pqday.com`. The
service is an order relay and explorer, not an exchange wallet. Makers sign
offers locally. The relay verifies those signatures, stores the records and
serves them to takers. It has no key capable of moving QDAY or Bitcoin.

## Signed order v1

An order binds all economic terms needed to identify an offer:

- protocol version and network;
- canonical `QDAY-BTC` market;
- maker Ed25519 public key;
- exact give and receive amounts as unsigned atomic integers;
- the QDAY consensus unit used to interpret its atomic amount;
- creation and expiration Unix timestamps;
- a random 16-byte nonce.

The ID is SHA-256 over the canonical binary encoding of those fields. The
maker signs that 32-byte ID with Ed25519. JSON is only a transport encoding and
is never signed directly. Changing a field changes the ID and invalidates the
signature.

The QDAY unit is part of the signed terms because QDAY has two consensus
denominations. A local taker must compare it with the unit reported by its own
synced QDAY node before approving funding.

## API

All responses use JSON. List endpoints are newest first and use 20 records per
page by default.

```text
GET  /api/v1/status
GET  /api/v1/orders?status=open&page=1&limit=20
GET  /api/v1/orders/{orderID}
POST /api/v1/orders
POST /api/v1/orders/{orderID}/cancel
```

`POST /api/v1/orders` accepts the signed record:

```json
{
  "order": {
    "version": 1,
    "network": "mainnet",
    "market": "QDAY-BTC",
    "qdayUnitAtomic": "1000000000000000000000000",
    "makerPublicKey": "...",
    "give": {"asset": "QDAY", "atomic": "2500000000000000000000000"},
    "receive": {"asset": "BTC", "atomic": "150000"},
    "createdAt": 1800000000,
    "expiresAt": 1800003600,
    "nonce": "..."
  },
  "id": "...",
  "signature": "..."
}
```

Publishing the same ID again is idempotent. An order expires automatically at
its signed deadline. A maker may cancel it earlier by signing a separate
domain-separated cancellation containing the order ID, maker public key and
cancellation time. A relay never accepts an unsigned delete request.

The first server keeps at most 100 simultaneously open offers for one maker
identity. HTTP request bodies are capped at 64 KiB. Public deployment also
applies connection and write limits at the reverse proxy while keeping read-only
order pages available.

## Browser explorer

The embedded website lists open offers, complete relay history and individual
order proofs. On an order page it reconstructs the canonical payload, checks
the SHA-256 ID and verifies the Ed25519 signature again with browser WebCrypto.
The `Open in QDAY Swap` action passes only the public order ID to the installed
application.

## Remaining relay work

The next protocol layer adds signed acceptance and a durable message mailbox so
two installed applications behind NAT can negotiate one trade through this
server. Every message will bind its order ID, trade ID, sender, recipient and
sequence number. Contract construction, signing, chain observation, claim and
refund stay local. The later discovery mesh will transport the same signed
orders and messages.
