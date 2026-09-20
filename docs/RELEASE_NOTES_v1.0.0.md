# QDAY Swap v1.0.0

QDAY Swap v1.0.0 is the first public release of the local noncustodial QDAY
atomic swap application. QDAY/BTC is the first live market. More markets can
use the same application and protocol.

![QDAY Swap trading interface](https://raw.githubusercontent.com/petoshi/qday-swap/v1.0.0/docs/assets/qday-swap.png)

## Highlights

- Trade QDAY and Bitcoin directly without an exchange account, custody or KYC.
- Create an order, leave, and return later. Both traders do not need to stay
  online at the same time.
- Follow every swap through a clear five stage on chain timeline with direct
  links to QDAY Explorer and mempool.space.
- See completed swaps and unread progress updates in the local notification
  center.
- Manage QDAY and Native SegWit Bitcoin balances, receive addresses and
  withdrawals from one Wallets screen.
- Restore QDAY, Bitcoin and the local swap identity from one 24 word recovery
  phrase.
- Use the live chart, order book and exact fixed rate order ticket from the
  local application.
- Run the same visual interface on Windows, Linux and macOS.

## Protocol and security

- QDAY transactions use Ed25519 and SLH DSA signatures.
- Bitcoin swaps use native HTLC transactions and an embedded Neutrino light
  client.
- Exact integer accounting is used for every asset amount.
- Swap messages are encrypted end to end. The public relay never receives a
  recovery phrase, signing key or general spend authority.
- Prepared transactions and timed on chain refunds protect both traders if one
  side disappears.
- The Windows package is scanned with Microsoft Defender. Every release archive
  is scanned with ClamAV before publication.
- This release bundles qday-walletd v0.3.1.

## Install

Download the archive for your operating system and extract the complete folder.
Keep `qday-swap` and `qday-walletd` together. Linux users can run
`start-qday-swap.sh`; Windows users can run `START QDAY SWAP.bat`; macOS users
can open `QDAY Swap.app`.

Verify the downloaded archive against `SHA256SUMS`. See the
[installation guide](https://github.com/petoshi/qday-swap/blob/v1.0.0/docs/INSTALL.md)
for platform specific instructions.
