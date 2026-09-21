# QDAY DEX relay

The first QDAY DEX market uses one public service at `dex.pqday.com`. The
service is an order relay and explorer, not an exchange wallet. Makers sign
offers locally. The relay verifies those signatures, stores the records and
serves them to takers. It has no key capable of moving QDAY or Bitcoin.

## Signed order v2

An order binds all economic terms needed to identify an offer:

- protocol version and network;
- canonical `QDAY-BTC` market;
- maker Ed25519 public key;
- maker X25519 message key;
- exact give and receive amounts as unsigned atomic integers;
- the QDAY consensus unit used to interpret its atomic amount;
- creation and expiration Unix timestamps;
- a random 16-byte nonce;
- a unique maker session ID;
- the maker's QDAY hybrid contract keys and Bitcoin swap public key;
- the QDAY and Bitcoin heights observed when the order was signed.

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
GET  /api/v1/price
GET  /api/v1/trades?limit=200&since=0
GET  /api/v1/orders?status=open&page=1&limit=20
GET  /api/v1/orders/{orderID}
POST /api/v1/orders
POST /api/v1/orders/{orderID}/cancel
POST /api/v1/orders/{orderID}/accept
POST /api/v1/orders/{orderID}/acceptances/{acceptanceID}/cancel
POST /api/v1/orders/{orderID}/match
POST /api/v1/messages
POST /api/v1/mailbox/poll
```

`POST /api/v1/orders` accepts the signed record:

```json
{
  "order": {
    "version": 2,
    "network": "mainnet",
    "market": "QDAY-BTC",
    "qdayUnitAtomic": "1000000000000000000000000",
    "makerPublicKey": "...",
    "makerMessageKey": "...",
    "give": {"asset": "QDAY", "atomic": "2500000000000000000000000"},
    "receive": {"asset": "BTC", "atomic": "150000"},
    "createdAt": 1800000000,
    "expiresAt": 1800003600,
    "nonce": "...",
    "sessionID": "...",
    "makerQDAY": {"classical": "...", "reserve": "...", "address": "qday1..."},
    "makerBitcoinPublicKey": "...",
    "makerQDAYHeight": 12000,
    "makerBitcoinHeight": 900000
  },
  "id": "...",
  "signature": "..."
}
```

Publishing the same ID again is idempotent. An order expires automatically at
its signed deadline. A maker may cancel it earlier by signing a separate
domain-separated cancellation containing the order ID, maker public key and
cancellation time. A relay never accepts an unsigned delete request.

`GET /api/v1/price` returns the external BTC/USD reference price and the last
matched QDAY/BTC price. `GET /api/v1/trades` returns signed order matches. A
match proves that two identities committed to fixed terms; it is not a claim
that both chain settlements have completed.

The first server keeps at most 100 simultaneously open offers for one maker
identity. HTTP request bodies are capped at 6 MiB because a version 2
acceptance may carry a post-quantum signed QDAY funding transaction. Public deployment also
applies connection and write limits at the reverse proxy while keeping read-only
order pages available.

## Trade negotiation

A taker selects an order by signing an acceptance containing a random 32-byte
trade ID, its Ed25519 identity, X25519 message key, per-chain contract keys,
unique hashlock, absolute refund heights and its exact signed first-leg funding
package. The acceptance can remain valid until the signed order deadline and
cannot outlive the order. It does not close the public offer by itself, so one
offline taker cannot reserve the order against everyone else. Several
acceptances may queue. The maker application signs the first valid acceptance it
receives. Until that atomic selection, a taker may revoke its exact acceptance
with a separate domain-separated signature. The relay records cancellation
tombstones even when an earlier acceptance request has not arrived yet, so a
timed-out request cannot later revive a cancelled trade. Cancellation and match
run in one database transaction; exactly one of them can win. The match
binds the order ID, acceptance ID, trade ID and both identities. The store
changes the order from `open` to `matched` atomically, so two concurrent takers
cannot both acquire the same offer.

Acceptances appear only in the maker's authenticated mailbox. The chosen match
appears in the taker's mailbox. A mailbox poll contains a fresh timestamp,
random nonce and cursor and is signed by the recipient identity. The relay
cannot read another identity's mailbox by inventing a request.

After a match, either peer may submit strictly ordered encrypted messages. The
maker uses this mailbox for its funding notice and a claim template which is
fully signed except for the taker's committed hash preimage. Each
message binds the network, order, trade, sender, recipient, sequence, creation
time and expiry. The body is encrypted end to end with X25519 and NaCl box and
the complete envelope is signed with Ed25519. The relay can verify the sender
and route the ciphertext, but cannot read or replace contract data. Duplicate
submissions are idempotent. Reusing or skipping a sequence number is rejected.

Mailbox cursors and messages live in the same bbolt transaction as the signed
record. A successful API response therefore survives an immediate relay
restart. Clients persist the last processed cursor locally and may safely poll
again after a crash.

## Browser explorer

The embedded website lists open offers, complete relay history and individual
order proofs. On an order page it reconstructs the canonical payload, checks
the SHA-256 ID and verifies the Ed25519 signature again with browser WebCrypto.
The `Open in QDAY Swap` action passes only the public order ID to the installed
application.

## Installed application

Contract construction, chain observation, signing, claim and refund stay in the
installed application. Signed matches enter a durable local state machine;
publishing an exact offer or accepting one is the user's funding authorization.
The taker's exact first-leg funding is part of the signed acceptance. The maker
may relay it, fund the second leg and leave an encrypted claim template; the
taker may later complete both claims without the maker returning. Prepared
transactions, encrypted envelopes and mailbox cursors survive restarts, and the
client automatically retries an acceptance saved locally before a relay
timeout. A pending taker may cancel before maker selection; after the relay
acknowledges the signed cancellation, the client releases the prepared funding
reservation. If the relay is unavailable, the exact cancellation remains in the
local journal and retries every few seconds while the application is open, then
continues after any restart. Unilateral refund paths remain valid if neither
side returns in time. The later discovery mesh can transport the same records
without changing their canonical format.
