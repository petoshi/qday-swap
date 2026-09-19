# QDAY Swap product plan

## What the user gets

The application has Market, Create offer, Active swaps, History and Settings.
A trade shows the asset pair, exact amounts, both network fees, confirmation
progress and the exact time or height at which each refund becomes available.

The normal path is:

1. Install QDAY Swap and save one 24-word recovery phrase.
2. Send ordinary QDAY or the external asset to the deposit address shown by the
   application.
3. Create or accept an offer.
4. Review exactly what leaves each wallet and approve funding once.
5. Watch both contracts settle automatically.

There are no contract descriptors, public keys, secrets, API tokens or raw
transactions in the normal interface. Coinbase and other custodial services
can be the source or destination of an ordinary transfer. They are not asked to
understand or sign an atomic swap contract.

## Wallet model

QDAY's 24 words encode 256 bits of entropy plus the standard BIP39 checksum.
The application uses that phrase as one recovery root:

- QDAY follows its existing direct entropy derivation for Ed25519 and SLH-DSA.
- Bitcoin-family and EVM wallets use separate standard BIP32 branches and
  registered coin types.
- Relay identity and per-swap material use explicit domain separation.

One phrase restores every supported wallet without reusing a private key across
chains. Seeds and signing keys stay on the user's computer and are encrypted by
the application password.

Bitcoin uses the standard BIP84 Native SegWit path
`m/84'/0'/account'/change/index`, producing `bc1q` receive addresses. A user
normally sends BTC from Coinbase, MetaMask or another wallet to the address
shown by QDAY Swap. Importing the shared 24 words into another compatible
wallet is an emergency recovery option because it exposes every branch derived
from that phrase to the imported application.

## Chain access

QDAY runs as an embedded full node because the chain is small and this also
strengthens the QDAY network. External chains use light clients and only store
what is needed to verify the wallet and active contracts. A Bitcoin user syncs
headers, compact filters and matching blocks, not the full Bitcoin blockchain.

For chains where a complete embedded verifier is not ready, the application may
offer a clearly marked compatibility backend using several independent RPC
servers. Compatibility mode never relies on one server and uses longer refund
windows. Strict mode requires verified proofs or the user's own node. A backend
failure may pause a swap, but it must never fabricate a confirmation, secret or
safe refund condition.

## Protocol

Every trade has a QDAY side and one external-chain side.

1. The initiator creates a random 32-byte secret and shares only its SHA-256
   hash.
2. The side with the longer refund window locks first.
3. After the required confirmations, the second side locks with a shorter
   refund window.
4. The first claim reveals the secret on one chain.
5. The other party verifies and extracts that secret, then claims the second
   contract.
6. If either party stops, each application waits for its verified timeout and
   refunds automatically.

Both clients derive and verify every contract from signed immutable trade
terms. The relay cannot change an amount, address, key, hash or timeout.

## Components

### Local application

- local browser interface
- built-in QDAY full node and wallet
- external-chain light clients and optional user-owned node adapters
- encrypted application identity and per-chain wallet keys
- durable trade journal written before every broadcast
- restart, mempool, reorganization and rebroadcast recovery
- automatic claim and refund worker

### Public order relay

- stores signed, short-lived offers and signed cancellations
- exposes a small versioned API to installed applications
- serves a public read-only order explorer at `dex.pqday.com`
- relays negotiation messages between local applications in the next phase
- stores no wallet seed or signing key
- cannot alter, spend, redirect or complete a swap
- may delay, hide or delete messages, so clients always retain unilateral refund
  paths

### Order book

The installed application and `dex.pqday.com` read the same signed relay order
book. The website is an explorer: it can inspect terms and open a selected order
in the local application. It never asks for a seed and never runs a swap inside
a browser tab. A later mesh can distribute the same signed records between
several relays and installed applications.

## Safety policy

- All amounts are exact integers in the smallest native unit. Decimal floating
  point is never used for money.
- Confirmation and timeout policy is defined per chain and per trade size.
- The second contract expires first. The first leaves a separate safety window
  for observing the secret and claiming after a restart or reorganization.
- Funding needs explicit approval. Claims and refunds are automatic.
- An offer can be cancelled only before either funding transaction exists.
- Every message is bound to protocol version, networks, assets, offer ID, trade
  ID and an immutable terms hash.
- Mainnet beta starts with configurable low value limits and removes them only
  after real recovery drills on each adapter.

## Delivery order

1. Generalize the tested Litecoin P2WSH code into a Bitcoin-family contract
   engine without weakening the existing tests.
2. Add the Bitcoin adapter and validating light client.
3. Implement the persistent two-party state machine and encrypted key store.
4. Run QDAY/BTC and QDAY/LTC regtest scenarios through that state machine,
   including restarts, stalled peers and reorganizations.
5. Add the single public signed-order relay and web order explorer.
6. Add relayed acceptance, the local UI and one-click automatic recovery.
7. Complete two small QDAY/BTC mainnet swaps between independent machines.
8. Add peer-to-peer order discovery when real usage justifies it.
9. Add EVM and XRP adapters, then DOGE, LTC and BCH markets.

No public release is ready until a fresh install can complete and refund a
trade without a command line, a foreign full-chain download or a manual API
call.

## BasicSwap

BasicSwap remains a later integration target because it already has a
distributed order book. Its standard HTLC engine manipulates Bitcoin-style raw
transactions and its messaging layer requires Particl. QDAY has a different
transaction model and native swap API, so correct support needs a custom
protocol adapter rather than a coin configuration entry.
