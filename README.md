# QDAY SWAP

`qday-swap` is a user-facing, non-custodial atomic swap application for QDAY.

The first public market will be QDAY/BTC. Litecoin remains the reference
integration because the complete QDAY/LTC protocol has already been exercised
on regtest, including claims, refunds, restarts and reorganizations. The engine
is chain-neutral so new assets are adapters rather than separate applications.

Planned adapter order:

1. Bitcoin
2. USDC and ETH on an EVM network
3. XRP
4. Dogecoin
5. Litecoin and Bitcoin Cash

The product has one local interface, one recovery phrase, signed public offers,
exact integer accounting and automatic claim or refund. The first public order
book runs at `dex.pqday.com`. It stores signed offers and cancellations and has
a read-only web explorer. It never receives a wallet seed, signing key or spend
authority. A peer-to-peer discovery mesh can replace this first relay when the
market is large enough without changing the signed order format.

QDAY uses its native SHA-256 hashlock policy with Ed25519 and SLH-DSA. Each
external adapter locks its asset against the same 32-byte secret using that
chain's native script, escrow or contract mechanism.

## No foreign full-chain downloads

The application runs its own fully validating QDAY node. It does not require
ordinary users to download Bitcoin, Litecoin, Dogecoin, XRP or EVM blockchains.
External adapters use a validating light client where the chain supports one:

- Bitcoin uses proof-of-work headers, compact filters and Merkle proofs.
- Other Bitcoin-family chains use headers and inclusion proofs, with multiple
  peers or proof-serving backends when compact filters are unavailable.
- Ethereum uses a consensus light client plus verified execution proofs. A
  multiple-RPC compatibility mode must be labelled as weaker than verification.
- XRP uses multiple independent validated-ledger sources until a suitable
  embedded verifier is available.

A single unverified explorer or RPC response is never enough to advance a swap
state. Users may optionally connect their own full node for any supported asset.

The repository is under active local development and has no public release yet.
See [the product plan](docs/PRODUCT.md) and [chain adapter plan](docs/CHAINS.md).

## DEX relay development server

The relay API and embedded order explorer can be started locally with:

```sh
go run ./cmd/qday-swap-relay \
  -listen 127.0.0.1:8080 \
  -data ./data/orders.db \
  -network mainnet \
  -public-url https://dex.pqday.com
```

Open `http://127.0.0.1:8080`. See [the relay protocol and API](docs/RELAY.md)
for the canonical signed payload and endpoints.
