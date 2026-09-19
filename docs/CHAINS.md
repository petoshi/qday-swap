# Chain adapter plan

## Selection rule

An asset is useful only when it has real demand, a safe unilateral refund path,
wallets users can operate and a chain backend that can detect claims and
reorganizations without downloading the full ledger. Market activity alone is
not enough.

## Priority

### Bitcoin

Bitcoin is the first public pair. It has the strongest market demand and the
existing Litecoin P2WSH contract uses the same SHA-256 and absolute-height
building blocks. The production backend will verify proof-of-work headers,
compact filters and Merkle inclusion proofs. A Bitcoin Core RPC adapter remains
available for advanced users.

### USDC and ETH

One audited EVM HTLC adapter can support native ETH and an allowlist of exact
ERC20 contracts. USDC supplies a stable quote asset, while browser wallets can
sign locally through WalletConnect. The contract uses SHA-256 so the same QDAY
secret closes both sides. Network and token contract addresses are immutable
trade terms. Fee-on-transfer, rebasing, upgradeable or unknown tokens are
rejected.

The first network must be chosen only after measuring fees, light-client
support and finality. Cheap RPC access is not a substitute for verified state.

### XRP

XRPL Escrow natively supports PREIMAGE-SHA-256 plus expiration and cancellation.
That makes XRP a strong direct adapter candidate without deploying a custom
contract. QDAY height locks and XRPL time locks require conservative independent
safety windows. The first version will cross-check independent validated-ledger
sources; support is not considered strict until the client can verify enough
ledger evidence locally or use the user's own rippled node.

### Dogecoin

DOGE has active demand and useful community overlap. It supports P2SH, SHA-256
and CHECKLOCKTIMEVERIFY, but the adapter uses legacy script rather than the
Bitcoin and Litecoin SegWit contract. It therefore gets separate transaction,
fee, malleability and reorganization tests.

### Litecoin and Bitcoin Cash

Litecoin stays enabled as the reference and low-cost test market. Bitcoin Cash
gets a separate legacy/P2SH policy and CashAddr handling. Both follow the first
public adapters rather than defining the product.

## Later research

Zcash can support a transparent-address HTLC, but a transparent-only swap can
be confused with shielded ZEC support and needs explicit privacy wording.
Monero requires a different adaptor-signature protocol rather than the current
SHA-256 HTLC engine. Solana needs a dedicated audited program and a different
verification backend. None should delay BTC, a stable quote asset or XRP.

## Common adapter contract

Every adapter must provide:

- deterministic wallet derivation and ordinary receive addresses
- exact smallest-unit balances and fee estimates
- canonical contract construction from signed trade terms
- prepare, journal, sign and broadcast ordering
- independent observation of funding, confirmations and reorganizations
- claim and refund construction
- strict secret extraction from the exact expected contract spend
- idempotent rebroadcast and restart recovery
- a declared verification mode: strict light client, own node or compatibility
  RPC quorum

An adapter cannot move a trade forward from explorer HTML, one unauthenticated
RPC answer or a relay message.
