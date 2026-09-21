# QDAY Swap v1.0.1

This patch fixes acceptances that could remain stuck in Pending when relay
delivery was interrupted and adds safe cancellation before the maker selects a
taker.

## Changes

- Save every signed acceptance before relay delivery and retry the same record
  automatically every five seconds while the app is open.
- Resume pending delivery and cancellation after closing and reopening the app.
- Show a clear relay unavailable state, a live retry countdown and a Retry Now
  button.
- Let the taker cancel a pending acceptance before maker selection.
- Keep funds reserved until the relay acknowledges the signed cancellation.
- Resolve cancellation and maker selection atomically at the relay so only one
  can win.
- Prevent a delayed acceptance request from reviving a cancelled trade.
- Prevent a provisional maker match from starting any on chain action before
  the relay accepts it.
- Preserve prepared funding when a match was committed before the acceptance
  deadline and the taker returns later.

This release bundles qday-walletd v0.3.1.
