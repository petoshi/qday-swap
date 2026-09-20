# Install QDAY Swap v1.0.0

Download the archive for your operating system from the
[v1.0.0 release](https://github.com/petoshi/qday-swap/releases/tag/v1.0.0) and
verify it against the release `SHA256SUMS` file. Extract the complete archive;
`qday-swap` and `qday-walletd` must stay together.

## Windows x86-64

Extract `QDAY-Swap-windows-amd64.zip`, then double-click `qday-swap.exe` or
`START QDAY SWAP.bat`.

## Linux x86-64 and ARM64

Extract the matching archive and run:

```sh
./start-qday-swap.sh
```

You can also start `./qday-swap` directly.

## macOS Intel and Apple Silicon

Extract `QDAY-Swap-macos-universal.zip`, then open `QDAY Swap.app`. The same
bundle contains native Intel and Apple Silicon executables.

## First start

The application opens its local interface in your browser. Create a new wallet
or import an existing 24-word QDAY recovery phrase, save the phrase offline and
allow both chain indicators to synchronize before funding a trade.

The application runs a validating QDAY node and a Bitcoin Neutrino light
client. The recovery phrase, private keys and signatures remain on your
computer. The public relay carries signed orders and encrypted protocol
messages; it cannot spend either asset.
