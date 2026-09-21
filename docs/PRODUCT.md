# QDAY Swap product plan

## What the user gets

The application has Orders, Swaps, History and Wallets. Creating and accepting
offers happens directly in the Orders screen. Wallets shows exact QDAY and
Bitcoin balances, receive addresses and ordinary sends from the same local
keys. A send can use a partial amount or MAX; the final address, recipient
amount, exact network fee and wallet total are reviewed before broadcast.
A trade shows the asset pair, exact amounts, both network fees, confirmation
progress and the exact time or height at which each refund becomes available.

The normal path is:

1. Install QDAY Swap and save one 24-word recovery phrase.
2. Send ordinary QDAY or the external asset to the deposit address shown by the
   application.
3. Review and sign an offer, or review and accept an existing one.
4. The application may be closed. Signed acceptances queue at the relay and the
   maker app selects the first valid one when it returns. Before selection, the
   taker may cancel a pending acceptance and immediately reuse the locally
   reserved funds after the relay acknowledges the signed cancellation.
5. The taker's acceptance already contains its exact signed first-leg funding.
   The maker can relay it, wait for confirmations, fund the second leg and leave
   a signed claim template before going offline again.
6. When the taker next opens the application, it can claim its side and complete
   the maker claim. If progress stops, each funded side retains its unilateral
   on-chain refund.

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

The Wallets screen exports the phrase only after the password is entered. It
can also replace both wallets from another 24-word phrase. Import is blocked
while a swap or acceptance is active, initializes both replacements before
touching the current data, and removes the replaced wallet data only after the
new QDAY and Bitcoin wallets open successfully.

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

1. The taker creates a random 32-byte secret and signs an acceptance containing
   its hash, both contract keys, refund heights and the exact first-leg funding
   transaction. The secret itself remains local.
2. The taker leg has the longer refund window. Either selected participant may
   relay that immutable transaction, so the taker does not need to remain online.
3. After the required confirmations, the maker signs a claim template for the
   first leg, sends it encrypted to the taker and funds the shorter second leg.
4. The taker adds the committed secret to the maker template, claims the maker
   leg and may relay both completed claims in one session.
5. The first claim reveals the same secret on-chain. Either application can
   verify it and replay any missing claim.
6. If progress stops, each application waits for its verified chain height and
   refunds its own funded contract automatically when next opened.

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
- serves protocol information and aggregate matched-market activity at
  `dex.pqday.com`; open offers are shown only in the installed application
- relays signed acceptance, atomic maker selection and end-to-end encrypted
  negotiation messages between local applications
- stores no wallet seed or signing key
- cannot alter, spend, redirect or complete a swap
- may delay, hide or delete messages, so clients always retain unilateral refund
  paths

### Order book and public market data

The installed application reads the signed relay order book and is the only UI
that lists or accepts open offers. `dex.pqday.com` publishes protocol information,
aggregate statistics, matched-price history and recent matched activity. It never
asks for a seed and never runs a swap inside a browser tab. A later mesh can
distribute the same signed records between several relays and installed
applications.

## Safety policy

- All amounts are exact integers in the smallest native unit. Decimal floating
  point is never used for money.
- Confirmation and timeout policy is defined per chain and per trade size.
- The second contract expires first. The first leaves a separate safety window
  for observing the secret and claiming after a restart or reorganization.
- Publishing or accepting exact signed terms authorizes automatic funding once
  the local application validates participant keys, amounts, exact funding bytes
  and refund heights. Claims and refunds are automatic too. The parties may run
  the application in separate sessions.
- Open offers reserve their amount in local accounting, preventing another
  offer or withdrawal from promising the same funds twice. They are not
  described as on-chain locked before a match.
- An offer can be cancelled only while the relay still reports it as open.
- A pending acceptance can be cancelled until the relay atomically selects it
  for a match. The client retries relay submission automatically after a
  timeout, and a signed cancellation tombstone prevents delayed delivery from
  reviving a cancelled acceptance.
- An acceptance alone does not reserve the public order. Several takers may
  queue; the relay closes the indivisible order only when the maker signs one
  match. The selected amount then belongs to that swap until claims or refunds
  finish.
- Every message is bound to protocol version, networks, assets, offer ID, trade
  ID and an immutable terms hash.
- Mainnet beta starts with configurable low value limits and removes them only
  after real recovery drills on each adapter.
- A Bitcoin leg must be at least 10,000 satoshis so ordinary claim and refund
  fees cannot consume the contract value.
- A client will not start local funding with fewer than 360 QDAY blocks or 36
  Bitcoin blocks left before its refund deadline. The unfunded swap then expires
  locally without moving money.

## Delivery order

1. Generalize the tested Litecoin P2WSH code into a Bitcoin-family contract
   engine without weakening the existing tests. Complete.
2. Add the Bitcoin adapter and validating Neutrino light client. Complete.
3. Implement the persistent two-party state machine and encrypted key store.
   Complete.
4. Exercise real QDAY and Bitcoin-family contracts across claims, refunds,
   restarts, stalled transactions, concurrent swaps and reorganizations.
   Complete in automated regtest coverage for QDAY/BTC and QDAY/LTC.
5. Add the single public signed-order relay and protocol/market website. Complete.
6. Add signed acceptance and the encrypted durable relay mailbox. Complete.
7. Connect the local UI to the persistent swap state machine, automatic maker
   matching and one-click recovery. Complete.
8. Complete two small QDAY/BTC mainnet swaps between independent machines.
9. Add peer-to-peer order discovery when real usage justifies it.
10. Add EVM and XRP adapters, then DOGE, LTC and BCH markets.

The v1.0.0 application provides the complete browser flow without a foreign
full-chain download or manual API call. Initial mainnet use remains deliberately
low value while independent machines exercise the public relay and recovery
paths.

## BasicSwap

BasicSwap remains a later integration target because it already has a
distributed order book. Its standard HTLC engine manipulates Bitcoin-style raw
transactions and its messaging layer requires Particl. QDAY has a different
transaction model and native swap API, so correct support needs a custom
protocol adapter rather than a coin configuration entry.
