const root = document.querySelector('#app');
const nav = document.querySelector('#main-nav');
const nodePill = document.querySelector('#node-pill');
const headerQDAY = document.querySelector('#header-qday');
const headerBitcoin = document.querySelector('#header-bitcoin');
const headerSwaps = document.querySelector('#header-swaps');
const headerBitcoinUSD = document.querySelector('#header-btc-usd');
const headerQDAYUSD = document.querySelector('#header-qday-usd');
const exitButton = document.querySelector('#exit-app');
const modalBackdrop = document.querySelector('#modal-backdrop');
const modal = document.querySelector('#modal');
const toast = document.querySelector('#toast');

let state;
let orders = {items: [], page: 1, total: 0, totalPages: 0};
let marketTrades = {items: [], total: 0};
let negotiations = {pending: [], incoming: [], swaps: []};
let marketError = '';
let setupMode = 'new';
let toastTimer;
let refreshTimer;
let renderFingerprint = '';
let bitcoinUSD = 0;
let lastTrade = null;
let offerSide = 'buy';
let offerPriceCurrency = 'USD';
let pendingOffer = null;
let selectedOrderID = '';
let chartRange = 'ALL';
let destroyChart = () => {};
let applicationStopped = false;

const escapeHTML = value => String(value ?? '').replace(/[&<>'"]/g, character => ({
  '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;'
})[character]);

async function api(path, options = {}) {
  const headers = {Accept: 'application/json', ...(options.headers || {})};
  const response = await fetch(path, {...options, headers, cache: 'no-store'});
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `HTTP ${response.status}`);
  return body;
}

function post(path, value = {}) {
  return api(path, {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(value)});
}

function showToast(message) {
  toast.textContent = message;
  toast.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toast.classList.remove('show'), 1800);
}

function closeModal() {
  modal.textContent = '';
  modalBackdrop.classList.add('hidden');
}

function openModal(content) {
  modal.innerHTML = content;
  modalBackdrop.classList.remove('hidden');
  modal.querySelector('button, input')?.focus();
}

function commas(value) {
  const [whole, fraction] = String(value ?? '0').split('.');
  return `${whole.replace(/\B(?=(\d{3})+(?!\d))/g, ',')}${fraction ? `.${fraction}` : ''}`;
}

function decimalsFromUnit(unit) {
  const value = String(unit || '');
  return /^10*$/.test(value) ? value.length - 1 : null;
}

function formatUnits(atomic, unit, maximumPlaces = 8) {
  const decimals = decimalsFromUnit(unit);
  if (decimals === null) return commas(atomic);
  let digits = String(atomic || '0').padStart(decimals + 1, '0');
  const whole = digits.slice(0, -decimals) || '0';
  let fraction = decimals ? digits.slice(-decimals) : '';
  if (fraction.length > maximumPlaces) fraction = fraction.slice(0, maximumPlaces);
  fraction = fraction.replace(/0+$/, '');
  return `${commas(whole)}${fraction ? `.${fraction}` : ''}`;
}

function formatUnitsExact(atomic, unit) {
  const decimals = decimalsFromUnit(unit);
  if (decimals === null) return String(atomic ?? '0');
  const negative = String(atomic || '0').startsWith('-');
  let digits = String(atomic || '0').replace(/^-/, '').padStart(decimals + 1, '0');
  const whole = digits.slice(0, -decimals) || '0';
  const fraction = (decimals ? digits.slice(-decimals) : '').replace(/0+$/, '');
  return `${negative ? '-' : ''}${whole}${fraction ? `.${fraction}` : ''}`;
}

function amountText(amount, qdayUnit) {
  const unit = amount.asset === 'BTC' ? '100000000' : qdayUnit;
  return `${formatUnits(amount.atomic, unit, amount.asset === 'BTC' ? 8 : 4)} ${amount.asset}`;
}

function orderPrice(terms) {
  const give = BigInt(terms.give.atomic);
  const receive = BigInt(terms.receive.atomic);
  const qday = terms.give.asset === 'QDAY' ? give : receive;
  const sats = terms.give.asset === 'BTC' ? give : receive;
  if (qday === 0n) return '—';
  const scale = 10000000000000000n;
  const scaled = (sats * BigInt(terms.qdayUnitAtomic) * scale) / (qday * 100000000n);
  return formatUnits(String(scaled), String(scale), 16);
}

function orderPriceParts(terms) {
  const give = BigInt(terms.give.atomic);
  const receive = BigInt(terms.receive.atomic);
  const qday = terms.give.asset === 'QDAY' ? give : receive;
  const satoshis = terms.give.asset === 'BTC' ? give : receive;
  return {numerator: satoshis * BigInt(terms.qdayUnitAtomic), denominator: qday * 100000000n};
}

function compareOrderPrice(left, right) {
  const a = orderPriceParts(left.signed.order);
  const b = orderPriceParts(right.signed.order);
  const result = a.numerator * b.denominator - b.numerator * a.denominator;
  return result < 0n ? -1 : result > 0n ? 1 : 0;
}

function cleanDecimal(value, places = 12) {
  if (!Number.isFinite(value) || value <= 0) return '';
  return value.toFixed(places).replace(/0+$/, '').replace(/\.$/, '');
}

function signedDecimal(value, places = 2) {
  if (!Number.isFinite(value)) return '';
  const normalized = Math.abs(value) < 10 ** -places ? 0 : value;
  const magnitude = Math.abs(normalized).toFixed(places).replace(/0+$/, '').replace(/\.$/, '');
  return `${normalized > 0 ? '+' : normalized < 0 ? '-' : ''}${magnitude}`;
}

function dollars(value, bitcoin = false) {
  if (!Number.isFinite(value) || value <= 0) return '$—';
  const maximumFractionDigits = bitcoin ? 0 : value >= 1 ? 4 : value >= .01 ? 6 : 8;
  return new Intl.NumberFormat('en-US', {
    style: 'currency', currency: 'USD', minimumFractionDigits: 0, maximumFractionDigits
  }).format(value);
}

function refreshQDAYDollarPrice() {
  if (!lastTrade) {
    headerQDAYUSD.textContent = 'N/A';
    headerQDAYUSD.title = 'No matched trades yet';
    return;
  }
  if (!bitcoinUSD) {
    headerQDAYUSD.textContent = '—';
    return;
  }
  const scale = 1000000000000n;
  const numerator = BigInt(lastTrade.btcAtomic) * BigInt(lastTrade.qdayUnitAtomic) * scale;
  const denominator = BigInt(lastTrade.qdayAtomic) * 100000000n;
  const btcPerQDAY = Number(numerator / denominator) / Number(scale);
  headerQDAYUSD.textContent = dollars(btcPerQDAY * bitcoinUSD);
  headerQDAYUSD.title = `Last matched trade · ${new Date(lastTrade.matchedAt * 1000).toLocaleString()}`;
}

async function refreshPriceLoop() {
  if (applicationStopped) return;
  try {
    const result = await api('/api/v1/price');
    const numeric = Number(result.usd);
    if (Number.isFinite(numeric) && numeric > 0) {
      bitcoinUSD = numeric;
      headerBitcoinUSD.textContent = dollars(bitcoinUSD, true);
      headerBitcoinUSD.title = `${result.source} · ${new Date(result.updatedAt).toLocaleTimeString()}${result.stale ? ' · stale' : ''}`;
      refreshQDAYDollarPrice();
      updateOfferPreview();
    }
  } catch (_) {
    if (!bitcoinUSD) headerBitcoinUSD.textContent = '—';
  }
  if (!applicationStopped) setTimeout(refreshPriceLoop, 20000);
}

function short(value, left = 8, right = 6) {
  const text = String(value || '');
  return text.length > left + right + 1 ? `${text.slice(0, left)}…${text.slice(-right)}` : text;
}

function route() {
  return (location.hash.slice(1).split('?')[0] || 'market').replace(/[^a-z]/g, '');
}

function updateChrome() {
  nav.hidden = !state?.configured || !state?.unlocked;
  nav.querySelectorAll('a').forEach(link => link.classList.toggle('active', link.dataset.route === route()));
  nodePill.hidden = !state?.configured || !state?.unlocked;
  nodePill.classList.remove('waiting', 'error');
  const label = nodePill.querySelector('span');
  headerQDAY.textContent = state?.qday ? commas(state.qday.height) : '—';
  headerBitcoin.textContent = state?.bitcoin ? commas(state.bitcoin.headerHeight) : '—';
  headerSwaps.textContent = commas(state?.activeSwaps ?? 0);
  lastTrade = state?.relay?.stats?.lastTrade || null;
  refreshQDAYDollarPrice();
  if (!state?.configured) {
    nodePill.classList.add('waiting');
    label.textContent = 'SETUP';
  } else if (state.error) {
    nodePill.classList.add('error');
    label.textContent = 'NODE ERROR';
  } else if (!state.qday?.networkSynced) {
    nodePill.classList.add('waiting');
    label.textContent = state.qday ? `QDAY SYNC · BLOCK ${commas(state.qday.height)}` : 'QDAY STARTING';
  } else if (state.unlocked && !state.qday.synced) {
    nodePill.classList.add('waiting');
    label.textContent = `QDAY WALLET ${commas(state.qday.scanHeight)} / ${commas(state.qday.height)}`;
  } else if (!state.bitcoin?.headersSynced) {
    nodePill.classList.add('waiting');
    label.textContent = state.bitcoin ? `BITCOIN SYNC · HEADER ${commas(state.bitcoin.headerHeight)}` : 'BITCOIN STARTING';
  } else if (state.unlocked && !state.bitcoin.walletSynced) {
    nodePill.classList.add('waiting');
    label.textContent = `BITCOIN WALLET ${commas(state.bitcoin.walletHeight)} / ${commas(state.bitcoin.headerHeight)}`;
  } else {
    label.textContent = `PEERS: ${state.qday.connections} QDAY · ${state.bitcoin.peers} BITCOIN`;
  }
}

function renderSetup() {
  root.innerHTML = `<section class="gate"><div class="gate-card">
    <div class="gate-head"><span class="eyebrow">LOCAL NONCUSTODIAL WALLET</span><h1>OPEN THE DOOR.</h1><p>One recovery phrase restores QDAY, Bitcoin and your swap identity.</p></div>
    <div class="gate-body">
      <div class="tabs"><button type="button" data-setup-mode="new" class="${setupMode === 'new' ? 'active' : ''}">CREATE WALLET</button><button type="button" data-setup-mode="import" class="${setupMode === 'import' ? 'active' : ''}">IMPORT 24 WORDS</button></div>
      <form id="setup-form">
        ${setupMode === 'import' ? `<label class="field"><span>Recovery phrase</span><textarea name="phrase" required autocomplete="off" spellcheck="false" placeholder="Enter all 24 words in order"></textarea></label>` : ''}
        <label class="field"><span>Wallet password</span><input name="password" type="password" minlength="12" required autocomplete="new-password"><small>At least 12 characters. This encrypts keys on this computer.</small></label>
        <label class="field"><span>Repeat password</span><input name="confirm" type="password" minlength="12" required autocomplete="new-password"></label>
        <p class="form-error" id="form-error" hidden></p>
        <button class="primary" type="submit">${setupMode === 'new' ? 'CREATE QDAY SWAP WALLET' : 'IMPORT WALLET'}</button>
      </form>
    </div>
  </div></section>`;
}

function renderUnlock() {
  root.innerHTML = `<section class="gate"><div class="gate-card">
    <div class="gate-head"><span class="eyebrow">WELCOME BACK</span><h1>UNLOCK.</h1><p>Your QDAY node keeps synchronizing while wallet keys stay encrypted.</p></div>
    <div class="gate-body"><form id="unlock-form">
      <label class="field"><span>Wallet password</span><input name="password" type="password" required autofocus autocomplete="current-password"></label>
      ${state.error ? `<p class="form-error">${escapeHTML(state.error)}</p>` : '<p class="form-error" id="form-error" hidden></p>'}
      <button class="primary" type="submit">UNLOCK WALLET</button>
    </form></div>
  </div></section>`;
}

function metric(label, value, note) {
  return `<div class="metric"><span>${escapeHTML(label)}</span><strong>${escapeHTML(value)}</strong><small>${escapeHTML(note)}</small></div>`;
}

function marketAmounts(terms) {
  return {
    qday: terms.give.asset === 'QDAY' ? terms.give : terms.receive,
    btc: terms.give.asset === 'BTC' ? terms.give : terms.receive
  };
}

function numericUnits(atomic, unit) {
  const value = Number(atomic);
  const scale = Number(unit);
  return Number.isFinite(value) && Number.isFinite(scale) && scale > 0 ? value / scale : 0;
}

function marketPriceNumber(value) {
  const qday = numericUnits(value.qdayAtomic, value.qdayUnitAtomic);
  const btc = numericUnits(value.btcAtomic, '100000000');
  return qday > 0 ? btc / qday : 0;
}

function orderBookRows(records, kind) {
  if (!records.length) return `<div class="book-empty">NO ${kind === 'ask' ? 'SELL' : 'BUY'} OFFERS</div>`;
  let cumulative = 0;
  const rows = records.map(record => {
    const terms = record.signed.order;
    const amounts = marketAmounts(terms);
    const quantity = numericUnits(amounts.qday.atomic, terms.qdayUnitAtomic);
    cumulative += quantity;
    return {record, terms, amounts, quantity, cumulative};
  });
  const maximum = Math.max(...rows.map(row => row.cumulative), 1);
  const displayed = kind === 'ask' ? [...rows].reverse() : rows;
  return displayed.map(row => {
    const mine = row.terms.makerPublicKey === state.identityPublicKey;
    const priceBTC = orderPrice(row.terms);
    const totalBTC = formatUnits(row.amounts.btc.atomic, '100000000', 8);
    const actionSide = kind === 'ask' ? 'buy' : 'sell';
    const depth = Math.max(10, Math.min(100, Math.ceil(row.cumulative / maximum * 10) * 10));
    return `<button type="button" class="book-row ${kind} depth-${depth} ${mine ? 'mine-row' : ''} ${selectedOrderID === row.record.signed.id ? 'selected' : ''}" data-book-order="${escapeHTML(row.record.signed.id)}" data-book-side="${actionSide}" ${mine ? 'disabled' : ''}>
      <span class="book-price">${escapeHTML(priceBTC)}</span><span>${escapeHTML(cleanDecimal(row.quantity, 8))}</span><span>${escapeHTML(totalBTC)}</span>
    </button>`;
  }).join('');
}

function orderBook() {
  const asks = orders.items.filter(record => record.signed.order.give.asset === 'QDAY').sort(compareOrderPrice).slice(0, 9);
  const bids = orders.items.filter(record => record.signed.order.give.asset === 'BTC').sort((left, right) => compareOrderPrice(right, left)).slice(0, 9);
  const bestAsk = asks.length ? Number(orderPrice(asks[0].signed.order)) : 0;
  const bestBid = bids.length ? Number(orderPrice(bids[0].signed.order)) : 0;
  const last = marketTrades.items.length ? marketPriceNumber(marketTrades.items[0]) : 0;
  const middle = last || (bestAsk && bestBid ? (bestAsk + bestBid) / 2 : bestAsk || bestBid);
  const middleUSD = middle && bitcoinUSD ? dollars(middle * bitcoinUSD) : '$—';
  return `<article class="market-panel order-book-panel">
    <header class="panel-head"><div><span>ORDER BOOK</span><strong>QDAY / BTC</strong></div><small>${orders.total} OPEN</small></header>
    <div class="book-columns"><span>PRICE BTC</span><span>AMOUNT QDAY</span><span>TOTAL BTC</span></div>
    <div class="book-side asks">${orderBookRows(asks, 'ask')}</div>
    <div class="book-middle"><strong>${middle ? escapeHTML(cleanDecimal(middle, 12)) : 'N/A'}</strong><span>${escapeHTML(middleUSD)} / QDAY</span></div>
    <div class="book-side bids">${orderBookRows(bids, 'bid')}</div>
    <p class="book-hint">Select an offer to fill the trade ticket.</p>
  </article>`;
}

function tradeTicket() {
  const buying = offerSide === 'buy';
  const selected = orders.items.find(record => record.signed.id === selectedOrderID);
  const compatible = selected && (buying ? selected.signed.order.give.asset === 'QDAY' : selected.signed.order.give.asset === 'BTC');
  const terms = compatible ? selected.signed.order : null;
  const amounts = terms ? marketAmounts(terms) : null;
  const quantity = amounts ? formatUnitsExact(amounts.qday.atomic, terms.qdayUnitAtomic) : '';
  const selectedPrice = terms ? orderPrice(terms).replaceAll(',', '') : '';
  const balance = buying ? `${state.bitcoinBalance?.confirmed?.btc ?? '0'} BTC` : `${state.balance?.spendable?.qday ?? '0'} QDAY`;
  return `<article class="market-panel ticket-panel">
    <div class="ticket-tabs" role="tablist"><button type="button" data-ticket-side="buy" class="${buying ? 'active' : ''}">BUY QDAY</button><button type="button" data-ticket-side="sell" class="${buying ? '' : 'active'}">SELL QDAY</button></div>
    <form id="create-offer-form" data-side="${offerSide}"${terms ? ` data-selected-order="${escapeHTML(selected.signed.id)}"` : ''}>
      <div class="ticket-balance"><span>AVAILABLE</span><strong>${escapeHTML(balance)}</strong></div>
      ${terms ? `<div class="selected-offer"><span>OPEN OFFER SELECTED</span><strong>${buying ? 'Buy' : 'Sell'} ${escapeHTML(quantity)} QDAY now</strong><button type="button" data-clear-order>USE MY OWN PRICE</button></div>` : ''}
      <label class="field light"><span>Limit price for 1 QDAY</span><div class="price-input"><input name="price" inputmode="decimal" autocomplete="off" maxlength="64" value="${escapeHTML(selectedPrice)}" placeholder="${offerPriceCurrency === 'USD' ? '1.00' : '0.00001'}" required ${terms ? 'readonly' : ''}><div class="currency-switch" aria-label="Price currency"><button type="button" data-price-currency="USD" class="${offerPriceCurrency === 'USD' ? 'active' : ''}" ${terms ? 'disabled' : ''}>USD</button><button type="button" data-price-currency="BTC" class="${offerPriceCurrency === 'BTC' ? 'active' : ''}" ${terms ? 'disabled' : ''}>BTC</button></div></div><small id="price-help">${offerPriceCurrency === 'USD' ? `Converted to BTC at review${bitcoinUSD ? ` using BTC/USD ${dollars(bitcoinUSD, true)}` : ''}.` : 'The selected signed offer is fixed in BTC.'}</small></label>
      <label class="field light"><span>Amount</span><div class="unit-input"><input name="quantity" inputmode="decimal" autocomplete="off" maxlength="64" value="${escapeHTML(quantity)}" placeholder="10" required ${terms ? 'readonly' : ''}><b>QDAY</b></div></label>
      <div class="ticket-total"><span>${buying ? 'YOU PAY' : 'YOU RECEIVE'}</span><strong id="summary-btc">— BTC</strong><small id="summary-usd">Enter price and amount</small></div>
      <input type="hidden" name="lifetimeMinutes" value="1440">
      <span id="summary-qday" hidden></span><span id="summary-price" hidden></span><span id="summary-price-btc" hidden></span>
      <p class="balance-warning" id="balance-warning" hidden></p><p class="form-error dark" id="form-error" hidden></p>
      <button class="primary ticket-submit ${buying ? 'buy' : 'sell'}" type="submit">${terms ? `REVIEW ${buying ? 'BUY' : 'SELL'}` : `PLACE ${buying ? 'BUY' : 'SELL'} ORDER`}</button>
      <p class="ticket-note">${terms ? 'The selected signed offer is fixed. Review it before any contract is created.' : 'Your signed offer moves no funds. You approve the atomic swap after another trader accepts.'}</p>
    </form>
  </article>`;
}

function recentTrades() {
  if (!marketTrades.items.length) return `<div class="empty compact-empty"><strong>NO MATCHES YET.</strong><p>The first matched order will establish the QDAY market price.</p></div>`;
  return `<div class="recent-trades">${marketTrades.items.slice(0, 12).map(trade => {
    const priceBTC = marketPriceNumber(trade);
    const qday = numericUnits(trade.qdayAtomic, trade.qdayUnitAtomic);
    return `<div><span class="${trade.side}">${escapeHTML(cleanDecimal(priceBTC, 12))}</span><span>${escapeHTML(cleanDecimal(qday, 8))}</span><time>${escapeHTML(new Date(trade.matchedAt * 1000).toLocaleTimeString([], {hour: '2-digit', minute: '2-digit', second: '2-digit'}))}</time></div>`;
  }).join('')}</div>`;
}

function setupPriceChart() {
  destroyChart();
  destroyChart = () => {};
  const canvas = document.querySelector('#price-chart');
  const empty = document.querySelector('#chart-empty');
  const tooltip = document.querySelector('#chart-tooltip');
  const lastOutput = document.querySelector('#chart-last');
  const changeOutput = document.querySelector('#chart-change');
  const highOutput = document.querySelector('#chart-high');
  const lowOutput = document.querySelector('#chart-low');
  const volumeOutput = document.querySelector('#chart-volume');
  if (!canvas || !empty || !tooltip) return;
  const rangeSeconds = chartRange === '24H' ? 86400 : chartRange === '7D' ? 604800 : 0;
  const cutoff = rangeSeconds ? Math.floor(Date.now() / 1000) - rangeSeconds : 0;
  const points = marketTrades.items
    .filter(trade => trade.matchedAt >= cutoff)
    .map(trade => ({
      time: Number(trade.matchedAt), price: marketPriceNumber(trade),
      volume: numericUnits(trade.qdayAtomic, trade.qdayUnitAtomic), side: trade.side
    }))
    .filter(point => Number.isFinite(point.time) && point.time > 0 && Number.isFinite(point.price) && point.price > 0)
    .sort((left, right) => left.time - right.time);
  empty.hidden = points.length > 0;
  canvas.hidden = !points.length;
  if (!points.length) {
    for (const output of [lastOutput, changeOutput, highOutput, lowOutput, volumeOutput]) {
      if (output) output.textContent = 'N/A';
    }
    if (changeOutput) changeOutput.className = '';
    return;
  }

  const firstPrice = points[0].price;
  const latestPrice = points.at(-1).price;
  const highPrice = Math.max(...points.map(point => point.price));
  const lowPrice = Math.min(...points.map(point => point.price));
  const totalVolume = points.reduce((total, point) => total + point.volume, 0);
  const change = firstPrice > 0 ? (latestPrice / firstPrice - 1) * 100 : 0;
  if (lastOutput) lastOutput.textContent = cleanDecimal(latestPrice, 16);
  if (changeOutput) {
    changeOutput.textContent = `${signedDecimal(change, 2)}%`;
    changeOutput.className = change > 0 ? 'up' : change < 0 ? 'down' : '';
  }
  if (highOutput) highOutput.textContent = cleanDecimal(highPrice, 16);
  if (lowOutput) lowOutput.textContent = cleanDecimal(lowPrice, 16);
  if (volumeOutput) volumeOutput.textContent = `${commas(cleanDecimal(totalVolume, 4))} QDAY`;

  const context = canvas.getContext('2d');
  const wrap = canvas.parentElement;
  let geometry = null;
  let hover = -1;
  const priceLabel = (value, step = 0) => {
    if (!Number.isFinite(value)) return '—';
    const places = step > 0
      ? Math.max(0, Math.min(16, Math.ceil(-Math.log10(step)) + 1))
      : value >= 1 ? 4 : value >= .001 ? 7 : 12;
    const label = value.toFixed(places).replace(/0+$/, '').replace(/\.$/, '');
    return label || '0';
  };
  const timeLabel = (timestamp, visibleSeconds) => {
    const date = new Date(timestamp * 1000);
    if (visibleSeconds <= 172800) return date.toLocaleTimeString([], {hour: '2-digit', minute: '2-digit'});
    if (visibleSeconds <= 31536000) return date.toLocaleDateString([], {month: 'short', day: 'numeric'});
    return date.toLocaleDateString([], {month: 'short', year: '2-digit'});
  };
  const niceStep = raw => {
    if (!Number.isFinite(raw) || raw <= 0) return 1;
    const power = 10 ** Math.floor(Math.log10(raw));
    const fraction = raw / power;
    const nice = fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 2.5 ? 2.5 : fraction <= 5 ? 5 : 10;
    return nice * power;
  };

  function draw() {
    const bounds = wrap.getBoundingClientRect();
    const width = Math.max(1, Math.floor(bounds.width));
    const height = Math.max(1, Math.floor(bounds.height));
    const ratio = Math.min(window.devicePixelRatio || 1, 2);
    const pixelWidth = Math.floor(width * ratio);
    const pixelHeight = Math.floor(height * ratio);
    if (canvas.width !== pixelWidth) canvas.width = pixelWidth;
    if (canvas.height !== pixelHeight) canvas.height = pixelHeight;
    context.setTransform(ratio, 0, 0, ratio, 0, 0);
    context.clearRect(0, 0, width, height);

    const volumeHeight = width < 520 ? 42 : 50;
    const observedMinimum = Math.min(...points.map(point => point.price));
    const observedMaximum = Math.max(...points.map(point => point.price));
    const observedSpread = observedMaximum - observedMinimum;
    const pricePad = observedSpread > 0 ? observedSpread * .1 : Math.max(observedMaximum * .04, 1e-16);
    const targetPriceTicks = height < 340 ? 4 : 5;
    const tickStep = niceStep(Math.max(observedSpread + pricePad * 2, 1e-16) / targetPriceTicks);
    let minimumPrice = Math.floor(Math.max(0, observedMinimum - pricePad) / tickStep) * tickStep;
    let maximumPrice = Math.ceil((observedMaximum + pricePad) / tickStep) * tickStep;
    if (maximumPrice <= minimumPrice) maximumPrice = minimumPrice + tickStep * targetPriceTicks;
    context.font = '11px ui-monospace, SFMono-Regular, Menlo, monospace';
    const axisWidth = Math.max(
      context.measureText(priceLabel(minimumPrice, tickStep)).width,
      context.measureText(priceLabel(maximumPrice, tickStep)).width,
      context.measureText(priceLabel(latestPrice)).width
    );
    const padding = {left: 14, right: Math.max(72, Math.min(width < 520 ? 102 : 124, Math.ceil(axisWidth) + 24)), top: 18, bottom: 35};
    const chartBottom = height - padding.bottom - volumeHeight - 12;
    const volumeTop = chartBottom + 18;
    let minimumTime;
    let maximumTime;
    if (rangeSeconds) {
      maximumTime = Math.max(Math.floor(Date.now() / 1000), points.at(-1).time);
      minimumTime = maximumTime - rangeSeconds;
    } else {
      minimumTime = points[0].time;
      maximumTime = points.at(-1).time;
      const dataSpan = maximumTime - minimumTime;
      const timePad = dataSpan > 0 ? Math.max(1, dataSpan * .025) : 1800;
      minimumTime -= timePad;
      maximumTime += timePad;
    }
    const plotWidth = width - padding.left - padding.right;
    const plotHeight = chartBottom - padding.top;
    const x = time => padding.left + (time - minimumTime) / (maximumTime - minimumTime) * plotWidth;
    const y = price => padding.top + (maximumPrice - price) / (maximumPrice - minimumPrice) * plotHeight;
    const maximumVolume = Math.max(...points.map(point => point.volume), 1);
    geometry = {width, height, padding, chartBottom, volumeTop, minimumTime, maximumTime, minimumPrice, maximumPrice, tickStep, x, y};

    context.font = '11px ui-monospace, SFMono-Regular, Menlo, monospace';
    context.textBaseline = 'middle';
    const tickCount = Math.max(2, Math.round((maximumPrice - minimumPrice) / tickStep));
    for (let index = 0; index <= tickCount; index++) {
      const lineY = padding.top + plotHeight * index / tickCount;
      context.strokeStyle = '#182019';
      context.lineWidth = 1;
      context.beginPath(); context.moveTo(padding.left, lineY); context.lineTo(width - padding.right, lineY); context.stroke();
      const value = maximumPrice - (maximumPrice - minimumPrice) * index / tickCount;
      context.fillStyle = '#657067';
      context.textAlign = 'left';
      context.fillText(priceLabel(value, tickStep), width - padding.right + 10, lineY);
    }
    const timeTickCount = width < 520 ? 3 : width < 850 ? 4 : 5;
    for (let index = 0; index <= timeTickCount; index++) {
      const lineX = padding.left + plotWidth * index / timeTickCount;
      context.strokeStyle = '#101510';
      context.beginPath(); context.moveTo(lineX, padding.top); context.lineTo(lineX, chartBottom); context.stroke();
      context.fillStyle = '#59615a';
      context.textAlign = index === 0 ? 'left' : index === timeTickCount ? 'right' : 'center';
      const time = minimumTime + (maximumTime - minimumTime) * index / timeTickCount;
      context.fillText(timeLabel(time, maximumTime - minimumTime), lineX, height - 12);
    }

    const barWidth = Math.max(2, Math.min(12, plotWidth / Math.max(points.length, 1) * .58));
    for (const point of points) {
      const barHeight = Math.max(1, point.volume / maximumVolume * volumeHeight);
      context.fillStyle = point.side === 'sell' ? 'rgba(255,118,84,.28)' : 'rgba(125,255,155,.25)';
      context.fillRect(x(point.time) - barWidth / 2, volumeTop + volumeHeight - barHeight, barWidth, barHeight);
    }

    const rising = latestPrice >= firstPrice;
    const trendColor = rising ? '#7dff9b' : '#ff896d';
    const gradient = context.createLinearGradient(0, padding.top, 0, chartBottom);
    gradient.addColorStop(0, rising ? 'rgba(125,255,155,.24)' : 'rgba(255,137,109,.2)');
    gradient.addColorStop(1, rising ? 'rgba(125,255,155,0)' : 'rgba(255,137,109,0)');
    context.beginPath();
    points.forEach((point, index) => index ? context.lineTo(x(point.time), y(point.price)) : context.moveTo(x(point.time), y(point.price)));
    context.lineTo(x(points.at(-1).time), chartBottom);
    context.lineTo(x(points[0].time), chartBottom);
    context.closePath(); context.fillStyle = gradient; context.fill();
    context.beginPath();
    points.forEach((point, index) => index ? context.lineTo(x(point.time), y(point.price)) : context.moveTo(x(point.time), y(point.price)));
    context.strokeStyle = trendColor; context.lineWidth = 1.6; context.stroke();

    const latest = points.at(-1);
    const latestY = y(latest.price);
    context.setLineDash([2, 4]); context.strokeStyle = rising ? 'rgba(125,255,155,.45)' : 'rgba(255,137,109,.45)'; context.lineWidth = 1;
    context.beginPath(); context.moveTo(padding.left, latestY); context.lineTo(width - padding.right, latestY); context.stroke();
    context.setLineDash([]);
    const latestText = priceLabel(latest.price);
    context.font = 'bold 11px ui-monospace, SFMono-Regular, Menlo, monospace';
    const latestWidth = Math.min(padding.right - 8, context.measureText(latestText).width + 12);
    context.fillStyle = trendColor; context.fillRect(width - padding.right + 4, latestY - 10, latestWidth, 20);
    context.fillStyle = '#021005'; context.textAlign = 'center'; context.fillText(latestText, width - padding.right + 4 + latestWidth / 2, latestY);
    context.font = '11px ui-monospace, SFMono-Regular, Menlo, monospace';

    if (points.length < 80) {
      context.fillStyle = trendColor;
      for (const point of points) { context.beginPath(); context.arc(x(point.time), y(point.price), 2.2, 0, Math.PI * 2); context.fill(); }
    }
    if (hover >= 0 && hover < points.length) {
      const point = points[hover];
      const pointX = x(point.time), pointY = y(point.price);
      context.setLineDash([3, 4]); context.strokeStyle = '#536057'; context.lineWidth = 1;
      context.beginPath(); context.moveTo(pointX, padding.top); context.lineTo(pointX, chartBottom); context.stroke();
      context.beginPath(); context.moveTo(padding.left, pointY); context.lineTo(width - padding.right, pointY); context.stroke();
      context.setLineDash([]); context.fillStyle = '#030403'; context.strokeStyle = trendColor;
      context.beginPath(); context.arc(pointX, pointY, 4, 0, Math.PI * 2); context.fill(); context.stroke();
      const hoverText = priceLabel(point.price);
      const hoverWidth = Math.min(padding.right - 8, context.measureText(hoverText).width + 12);
      context.fillStyle = '#667068'; context.fillRect(width - padding.right + 4, pointY - 10, hoverWidth, 20);
      context.fillStyle = '#f0f4f0'; context.textAlign = 'center'; context.fillText(hoverText, width - padding.right + 4 + hoverWidth / 2, pointY);
      const hoverTime = timeLabel(point.time, maximumTime - minimumTime);
      const hoverTimeWidth = context.measureText(hoverTime).width + 12;
      const hoverTimeX = Math.max(padding.left, Math.min(width - padding.right - hoverTimeWidth, pointX - hoverTimeWidth / 2));
      context.fillStyle = '#273029'; context.fillRect(hoverTimeX, height - padding.bottom + 2, hoverTimeWidth, 20);
      context.fillStyle = '#dce2dc'; context.fillText(hoverTime, hoverTimeX + hoverTimeWidth / 2, height - padding.bottom + 12);
    }
  }

  function pointer(event) {
    if (!geometry) return;
    const bounds = canvas.getBoundingClientRect();
    const pointerX = Math.max(geometry.padding.left, Math.min(geometry.width - geometry.padding.right, event.clientX - bounds.left));
    const pointerY = Math.max(geometry.padding.top, Math.min(geometry.chartBottom, event.clientY - bounds.top));
    const targetTime = geometry.minimumTime + (pointerX - geometry.padding.left) / (geometry.width - geometry.padding.left - geometry.padding.right) * (geometry.maximumTime - geometry.minimumTime);
    hover = points.reduce((best, point, index) => Math.abs(point.time - targetTime) < Math.abs(points[best].time - targetTime) ? index : best, 0);
    const point = points[hover];
    tooltip.hidden = false;
    tooltip.innerHTML = `<strong>${escapeHTML(priceLabel(point.price))} BTC</strong><span>${bitcoinUSD ? `${escapeHTML(dollars(point.price * bitcoinUSD))} / QDAY · ` : ''}${escapeHTML(cleanDecimal(point.volume, 8))} QDAY</span><time>${escapeHTML(new Date(point.time * 1000).toLocaleString())}</time>`;
    const tooltipWidth = tooltip.offsetWidth;
    const tooltipHeight = tooltip.offsetHeight;
    const left = pointerX + tooltipWidth + 24 > geometry.width ? pointerX - tooltipWidth - 14 : pointerX + 14;
    const top = pointerY + tooltipHeight + 24 > geometry.height ? pointerY - tooltipHeight - 14 : pointerY + 14;
    tooltip.style.left = `${Math.max(8, Math.min(geometry.width - tooltipWidth - 8, left))}px`;
    tooltip.style.top = `${Math.max(8, Math.min(geometry.height - tooltipHeight - 8, top))}px`;
    draw();
  }
  function leave() { hover = -1; tooltip.hidden = true; draw(); }
  canvas.addEventListener('pointermove', pointer);
  canvas.addEventListener('pointerdown', pointer);
  canvas.addEventListener('pointerleave', leave);
  const observer = new ResizeObserver(draw);
  observer.observe(wrap);
  draw();
  destroyChart = () => {
    observer.disconnect();
    canvas.removeEventListener('pointermove', pointer);
    canvas.removeEventListener('pointerdown', pointer);
    canvas.removeEventListener('pointerleave', leave);
  };
}

function renderMarket() {
  const balance = state.balance?.spendable?.qday ?? '0';
  const pending = state.balance?.pendingIn?.qday ?? '0';
  const bitcoin = state.bitcoinBalance?.confirmed?.btc ?? '0';
  const bitcoinPending = state.bitcoinBalance?.pending?.btc ?? '0';
  const bitcoinImmature = state.bitcoinBalance?.immature?.btc ?? '0';
  const bitcoinBalanceNote = Number(bitcoinImmature) > 0
    ? `${commas(bitcoinPending)} pending · ${commas(bitcoinImmature)} immature`
    : `${commas(bitcoinPending)} pending`;
  root.innerHTML = `${state.error ? `<div class="alert"><strong>Local chain needs attention</strong>${escapeHTML(state.error)}</div>` : ''}
    ${state.relayError || marketError ? `<div class="alert"><strong>Order relay needs attention</strong>${escapeHTML(state.relayError || marketError)}</div>` : ''}
    <section class="pair-strip">
      <div class="pair-name"><span>SPOT ATOMIC SWAP</span><strong>QDAY / BTC</strong></div>
      <div><span>LAST MATCH</span><strong>${lastTrade ? escapeHTML(cleanDecimal(marketPriceNumber(lastTrade), 12)) : 'N/A'}</strong></div>
      <div><span>QDAY / USD</span><strong>${lastTrade && bitcoinUSD ? escapeHTML(dollars(marketPriceNumber(lastTrade) * bitcoinUSD)) : 'N/A'}</strong></div>
      <div><span>OPEN OFFERS</span><strong>${escapeHTML(state.relay?.stats?.open ?? orders.total ?? '0')}</strong></div>
      <div><span>QDAY BALANCE</span><strong>${escapeHTML(commas(balance))}</strong><small>${escapeHTML(commas(pending))} pending</small></div>
      <div><span>BTC BALANCE</span><strong>${escapeHTML(commas(bitcoin))}</strong><small>${escapeHTML(bitcoinBalanceNote)}</small></div>
    </section>
    <section class="exchange-grid">
      <article class="market-panel chart-panel"><header class="panel-head"><div><span>PRICE</span><strong>QDAY / BTC</strong></div><div class="chart-controls"><span class="chart-scale">AUTO SCALE</span><div class="chart-ranges"><button data-chart-range="24H" class="${chartRange === '24H' ? 'active' : ''}">24H</button><button data-chart-range="7D" class="${chartRange === '7D' ? 'active' : ''}">7D</button><button data-chart-range="ALL" class="${chartRange === 'ALL' ? 'active' : ''}">ALL</button></div></div></header><div class="chart-summary"><div><span>LAST</span><strong id="chart-last">N/A</strong></div><div><span>CHANGE</span><strong id="chart-change">N/A</strong></div><div><span>HIGH</span><strong id="chart-high">N/A</strong></div><div><span>LOW</span><strong id="chart-low">N/A</strong></div><div><span>VOLUME</span><strong id="chart-volume">N/A</strong></div></div><div class="chart-wrap"><canvas id="price-chart" aria-label="Matched QDAY Bitcoin price history"></canvas><div class="chart-empty" id="chart-empty" hidden>NO MATCHED TRADES YET</div><div class="chart-tooltip" id="chart-tooltip" hidden></div></div></article>
      ${orderBook()}
      ${tradeTicket()}
      <article class="market-panel trades-panel"><header class="panel-head"><div><span>RECENT MATCHES</span><strong>PRICE · AMOUNT · TIME</strong></div><small>${marketTrades.total} TOTAL</small></header>${recentTrades()}</article>
    </section>
    <section class="card my-orders"><div class="card-head"><h2>MY OPEN ORDERS</h2><span>${orders.items.filter(record => record.signed.order.makerPublicKey === state.identityPublicKey).length}</span></div>${orderList(orders.items.filter(record => record.signed.order.makerPublicKey === state.identityPublicKey))}</section>`;
  updateOfferPreview();
  setupPriceChart();
}

function orderList(records) {
  if (!records?.length) return `<div class="empty"><strong>NO OPEN OFFERS YET.</strong><p>Create the first signed offer or wait for another trader.</p></div>`;
  return `<div class="table-scroll"><table class="offers"><thead><tr><th>Side</th><th>Maker gives</th><th>Maker wants</th><th>Price</th><th>Expires</th><th></th></tr></thead><tbody>${records.map(record => {
    const terms = record.signed.order;
    const mine = terms.makerPublicKey === state.identityPublicKey;
    const side = terms.give.asset === 'QDAY' ? 'SELL QDAY' : 'BUY QDAY';
    const remaining = Math.max(0, terms.expiresAt - Math.floor(Date.now() / 1000));
    return `<tr><td><span class="side ${terms.give.asset === 'QDAY' ? 'sell' : 'buy'}">${side}</span>${mine ? '<small class="mine">MY OFFER</small>' : ''}</td><td>${escapeHTML(amountText(terms.give, terms.qdayUnitAtomic))}</td><td>${escapeHTML(amountText(terms.receive, terms.qdayUnitAtomic))}</td><td>${escapeHTML(orderPrice(terms))} BTC/QDAY</td><td>${Math.ceil(remaining / 60)}m</td><td class="row-action">${mine ? `<button class="text-button" data-cancel-order="${escapeHTML(record.signed.id)}">CANCEL</button>` : `<button class="secondary" data-accept-order="${escapeHTML(record.signed.id)}">ACCEPT</button>`}</td></tr>`;
  }).join('')}</tbody></table></div>`;
}

function availableOffers(side) {
  const records = orders.items
    .filter(record => side === 'buy' ? record.signed.order.give.asset === 'QDAY' : record.signed.order.give.asset === 'BTC')
    .sort((left, right) => compareOrderPrice(left, right) * (side === 'buy' ? 1 : -1));
  const title = side === 'buy' ? 'QDAY AVAILABLE TO BUY NOW' : 'BUYERS AVAILABLE NOW';
  const empty = side === 'buy'
    ? 'No sell offers are open. Set the price you want to pay below.'
    : 'No buy offers are open. Set the price you want to receive below.';
  if (!records.length) {
    return `<section class="card instant-offers"><div class="card-head"><h2>${title}</h2><span>0 MATCHES</span></div><div class="empty"><strong>NO MATCHING OFFERS.</strong><p>${empty}</p></div></section>`;
  }
  return `<section class="card instant-offers"><div class="card-head"><h2>${title}</h2><span>${records.length} READY</span></div><div class="table-scroll"><table class="offers trade-offers"><thead><tr><th>${side === 'buy' ? 'You buy' : 'You sell'}</th><th>${side === 'buy' ? 'You pay' : 'You receive'}</th><th>Price for 1 QDAY</th><th>Expires</th><th></th></tr></thead><tbody>${records.map(record => {
    const terms = record.signed.order;
    const qday = terms.give.asset === 'QDAY' ? terms.give : terms.receive;
    const btc = terms.give.asset === 'BTC' ? terms.give : terms.receive;
    const priceBTC = orderPrice(terms);
    const priceUSD = bitcoinUSD ? dollars(Number(priceBTC) * bitcoinUSD) : '$—';
    const totalUSD = bitcoinUSD ? dollars(Number(formatUnits(btc.atomic, '100000000', 8)) * bitcoinUSD) : '$—';
    const remaining = Math.max(0, terms.expiresAt - Math.floor(Date.now() / 1000));
    return `<tr><td><strong>${escapeHTML(amountText(qday, terms.qdayUnitAtomic))}</strong></td><td><strong>${escapeHTML(amountText(btc, terms.qdayUnitAtomic))}</strong><small class="fiat-line">≈ ${escapeHTML(totalUSD)}</small></td><td><span class="offer-price">${escapeHTML(priceBTC)} BTC</span><small class="fiat-line">≈ ${escapeHTML(priceUSD)}</small></td><td>${Math.ceil(remaining / 60)}m</td><td class="row-action"><button class="primary" data-accept-order="${escapeHTML(record.signed.id)}">${side === 'buy' ? 'BUY NOW' : 'SELL NOW'}</button></td></tr>`;
  }).join('')}</tbody></table></div></section>`;
}

function renderCreate() {
  const rawParameters = location.hash.includes('?') ? location.hash.slice(location.hash.indexOf('?') + 1) : '';
  const requestedSide = new URLSearchParams(rawParameters).get('side');
  if (requestedSide === 'buy' || requestedSide === 'sell') offerSide = requestedSide;
  const buying = offerSide === 'buy';
  const balance = buying ? `${state.bitcoinBalance?.confirmed?.btc ?? '0'} BTC` : `${state.balance?.spendable?.qday ?? '0'} QDAY`;
  root.innerHTML = `<section class="page-head trade-page-head"><div><span class="eyebrow">QDAY / BTC</span><h1>BUY OR SELL QDAY.</h1><p>Take an open offer now, or publish your own price and wait for another trader.</p></div></section>
    <section class="side-picker" aria-label="Choose trade direction">
      <button type="button" data-offer-side="buy" class="${buying ? 'active' : ''}"><b>BUY QDAY</b><span>Spend BTC and receive QDAY</span></button>
      <button type="button" data-offer-side="sell" class="${buying ? '' : 'active'}"><b>SELL QDAY</b><span>Send QDAY and receive BTC</span></button>
    </section>
    ${availableOffers(offerSide)}
    <section class="card offer-form-card">
      <div class="custom-offer-head"><span class="eyebrow">YOUR PRICE</span><h2>${buying ? 'CREATE A BUY OFFER.' : 'CREATE A SELL OFFER.'}</h2><p>${buying ? 'If the sell offers above are too expensive, set the price you want to pay.' : 'If the buy offers above are too low, set the price you want to receive.'}</p></div>
      <form id="create-offer-form" data-side="${offerSide}">
        <div class="offer-fields simple-offer-fields">
          <label class="field light"><span>QDAY amount</span><input name="quantity" inputmode="decimal" autocomplete="off" maxlength="64" placeholder="10" required><small>${buying ? 'How many QDAY you want to buy.' : 'How many QDAY you want to sell.'}</small></label>
          <label class="field light price-field"><span>Price for 1 QDAY</span><div class="price-input"><input name="price" inputmode="decimal" autocomplete="off" maxlength="64" placeholder="${offerPriceCurrency === 'USD' ? '1.00' : '0.00001'}" required><div class="currency-switch" aria-label="Price currency"><button type="button" data-price-currency="USD" class="${offerPriceCurrency === 'USD' ? 'active' : ''}">USD</button><button type="button" data-price-currency="BTC" class="${offerPriceCurrency === 'BTC' ? 'active' : ''}">BTC</button></div></div><small id="price-help">${offerPriceCurrency === 'USD' ? `Converted to BTC when you review${bitcoinUSD ? ` at ${dollars(bitcoinUSD, true)} per BTC` : ''}.` : 'The signed offer uses this BTC price directly.'}</small></label>
          <label class="field light"><span>Offer expires</span><select name="lifetimeMinutes"><option value="60">1 hour</option><option value="360">6 hours</option><option value="1440">24 hours</option><option value="10080">7 days</option></select><small>Unfilled offers can also be cancelled at any time.</small></label>
          <div class="available-balance"><span>AVAILABLE TO SPEND</span><strong>${escapeHTML(balance)}</strong><small>${buying ? 'Confirmed Bitcoin balance' : 'Spendable QDAY balance'}</small></div>
        </div>
        <div class="offer-summary" id="offer-summary">
          <div><span id="summary-qday-label">${buying ? 'YOU BUY' : 'YOU SELL'}</span><strong id="summary-qday">— QDAY</strong></div>
          <div><span id="summary-btc-label">${buying ? 'YOU PAY' : 'YOU RECEIVE'}</span><strong id="summary-btc">— BTC</strong><small id="summary-usd">Enter a price and amount</small></div>
          <div><span>LIMIT PRICE</span><strong id="summary-price">—</strong><small id="summary-price-btc">Final offer is fixed in BTC</small></div>
        </div>
        <p class="balance-warning" id="balance-warning" hidden></p>
        <div class="review-note"><strong>Nothing moves yet.</strong><span>Reviewing creates the exact BTC amount. Publishing signs the offer locally. Funds move only after another trader accepts and you approve the swap.</span></div>
        <p class="form-error dark" id="form-error" hidden></p><button class="primary review-offer" type="submit">REVIEW ${buying ? 'BUY' : 'SELL'} OFFER</button>
      </form>
    </section>`;
  updateOfferPreview();
}

function updateOfferPreview() {
  const form = document.querySelector('#create-offer-form');
  if (!form) return;
  const quantity = Number(form.elements.quantity.value);
  const enteredPrice = Number(form.elements.price.value);
  const buying = form.dataset.side === 'buy';
  const qdayOutput = document.querySelector('#summary-qday');
  const btcOutput = document.querySelector('#summary-btc');
  const usdOutput = document.querySelector('#summary-usd');
  const priceOutput = document.querySelector('#summary-price');
  const btcPriceOutput = document.querySelector('#summary-price-btc');
  const balanceWarning = document.querySelector('#balance-warning');
  const button = form.querySelector('[type=submit]');
  let priceBTC = 0;
  let priceUSD = 0;
  if (offerPriceCurrency === 'USD') {
    priceUSD = enteredPrice;
    priceBTC = bitcoinUSD ? enteredPrice / bitcoinUSD : 0;
  } else {
    priceBTC = enteredPrice;
    priceUSD = bitcoinUSD ? enteredPrice * bitcoinUSD : 0;
  }
  const selected = orders.items.find(record => record.signed.id === selectedOrderID);
  const selectedTerms = selected?.signed?.order;
  const selectedAmounts = selectedTerms ? marketAmounts(selectedTerms) : null;
  const selectedSide = selectedTerms?.give?.asset === 'QDAY' ? 'buy' : 'sell';
  const takingOrder = Boolean(selected && selectedSide === form.dataset.side && form.dataset.selectedOrder === selectedOrderID);
  const effectiveQuantity = takingOrder ? numericUnits(selectedAmounts.qday.atomic, selectedTerms.qdayUnitAtomic) : quantity;
  const effectiveTotalBTC = takingOrder ? numericUnits(selectedAmounts.btc.atomic, '100000000') : effectiveQuantity * priceBTC;
  const effectivePriceBTC = takingOrder ? marketPriceNumber({
    qdayAtomic: selectedAmounts.qday.atomic,
    btcAtomic: selectedAmounts.btc.atomic,
    qdayUnitAtomic: selectedTerms.qdayUnitAtomic
  }) : priceBTC;
  const valid = takingOrder || (Number.isFinite(quantity) && quantity > 0 && Number.isFinite(enteredPrice) && enteredPrice > 0 && priceBTC > 0);
  button.disabled = !valid;
  button.dataset.takeOrder = takingOrder ? selectedOrderID : '';
  button.textContent = takingOrder ? `REVIEW ${buying ? 'BUY' : 'SELL'}` : `PLACE ${buying ? 'BUY' : 'SELL'} ORDER`;
  balanceWarning.hidden = true;
  if (!valid) {
    qdayOutput.textContent = '— QDAY';
    btcOutput.textContent = '— BTC';
    usdOutput.textContent = offerPriceCurrency === 'USD' && !bitcoinUSD ? 'Waiting for BTC/USD price' : 'Enter a price and amount';
    priceOutput.textContent = '—';
    btcPriceOutput.textContent = 'Final offer is fixed in BTC';
    return;
  }
  const totalBTC = effectiveTotalBTC;
  const totalUSD = bitcoinUSD ? totalBTC * bitcoinUSD : 0;
  if (!takingOrder && totalBTC + 1e-12 < .0001) {
    button.disabled = true;
    balanceWarning.hidden = false;
    balanceWarning.textContent = 'Bitcoin contract total must be at least 0.0001 BTC.';
  }
  const available = Number(buying ? state.bitcoinBalance?.confirmed?.btc ?? 0 : state.balance?.spendable?.qday ?? 0);
  const required = buying ? totalBTC : effectiveQuantity;
  if (!Number.isFinite(available) || available < required) {
    button.disabled = true;
    balanceWarning.hidden = false;
    balanceWarning.textContent = buying
      ? `Not enough confirmed BTC. This offer needs ${cleanDecimal(totalBTC, 8)} BTC; your confirmed balance is ${state.bitcoinBalance?.confirmed?.btc ?? '0'} BTC.`
      : `Not enough spendable QDAY. This offer needs ${cleanDecimal(effectiveQuantity, 8)} QDAY; your spendable balance is ${state.balance?.spendable?.qday ?? '0'} QDAY.`;
  }
  qdayOutput.textContent = `${commas(cleanDecimal(effectiveQuantity, 8))} QDAY`;
  btcOutput.textContent = `${cleanDecimal(totalBTC, 8) || '< 0.00000001'} BTC`;
  usdOutput.textContent = totalUSD
    ? `≈ ${dollars(totalUSD)}${buying ? ' · plus Bitcoin funding fee' : ' · before Bitcoin claim fee'}`
    : buying ? 'Plus Bitcoin funding fee' : 'Bitcoin claim fee is deducted';
  priceOutput.textContent = takingOrder ? `${cleanDecimal(effectivePriceBTC, 16)} BTC / QDAY` : offerPriceCurrency === 'USD' ? `${dollars(priceUSD)} / QDAY` : `${cleanDecimal(effectivePriceBTC, 16)} BTC / QDAY`;
  btcPriceOutput.textContent = `${cleanDecimal(effectivePriceBTC, 16)} BTC per QDAY${takingOrder ? ' · exact signed amounts' : buying ? ' · maximum you pay' : ' · minimum you receive'}`;
}

function offerReview(quote, lifetimeMinutes) {
  const buying = quote.side === 'buy';
  const entered = quote.priceCurrency === 'USD' ? `${dollars(Number(quote.enteredPrice))} per QDAY` : `${quote.enteredPrice} BTC per QDAY`;
  const reference = quote.usdTotal ? `${dollars(Number(quote.usdTotal))} at ${dollars(Number(quote.bitcoinUSD), true)} per BTC` : 'USD reference unavailable';
  openModal(`<div class="modal-head"><span class="eyebrow">FINAL CHECK</span><h2>${buying ? 'BUY' : 'SELL'} ${escapeHTML(quote.quantity)} QDAY.</h2></div><div class="modal-body">
    <div class="final-quote">
      <div><span>${buying ? 'YOU RECEIVE' : 'YOU SEND'}</span><strong>${escapeHTML(quote.quantity)} QDAY</strong></div>
      <div><span>${buying ? 'CONTRACT AMOUNT YOU FUND' : 'BITCOIN CONTRACT AMOUNT'}</span><strong>${escapeHTML(quote.btcAmount)} BTC</strong><small>≈ ${escapeHTML(reference)} · ${buying ? 'your wallet also pays the funding fee' : 'the claim fee is deducted when you receive it'}</small></div>
      <div><span>YOUR INPUT</span><strong>${escapeHTML(entered)}</strong><small>Exact signed rate: ${escapeHTML(quote.btcPerQDAY)} BTC per QDAY</small></div>
      <div><span>EXPIRES</span><strong>${escapeHTML(String(lifetimeMinutes < 60 ? `${lifetimeMinutes} minutes` : lifetimeMinutes === 60 ? '1 hour' : lifetimeMinutes === 1440 ? '24 hours' : lifetimeMinutes === 10080 ? '7 days' : `${lifetimeMinutes / 60} hours`))}</strong><small>You can cancel while it remains open.</small></div>
    </div>
    <p>This publishes a signed offer. It does not send funds. If another trader accepts, the app shows the exact contract before funding.</p>
    <div class="modal-actions"><button class="secondary" id="close-modal">BACK</button><button class="primary" id="publish-offer">SIGN AND PUBLISH</button></div>
  </div>`);
}

function acceptanceReview(record) {
  const terms = record.signed.order;
  const buying = terms.give.asset === 'QDAY';
  const qday = buying ? terms.give : terms.receive;
  const btc = buying ? terms.receive : terms.give;
  const totalUSD = bitcoinUSD ? dollars(Number(formatUnits(btc.atomic, '100000000', 8)) * bitcoinUSD) : '$—';
  const priceBTC = orderPrice(terms);
  openModal(`<div class="modal-head"><span class="eyebrow">TAKE OPEN OFFER</span><h2>${buying ? 'BUY' : 'SELL'} ${escapeHTML(formatUnits(qday.atomic, terms.qdayUnitAtomic, 8))} QDAY.</h2></div><div class="modal-body">
    <div class="final-quote">
      <div><span>${buying ? 'YOU RECEIVE' : 'YOU SEND'}</span><strong>${escapeHTML(amountText(qday, terms.qdayUnitAtomic))}</strong></div>
      <div><span>${buying ? 'YOU SEND' : 'YOU RECEIVE'}</span><strong>${escapeHTML(amountText(btc, terms.qdayUnitAtomic))}</strong><small>≈ ${escapeHTML(totalUSD)}</small></div>
      <div><span>PRICE</span><strong>${escapeHTML(priceBTC)} BTC per QDAY</strong><small>${bitcoinUSD ? `≈ ${escapeHTML(dollars(Number(priceBTC) * bitcoinUSD))} per QDAY` : 'USD reference unavailable'}</small></div>
      <div><span>OFFER EXPIRES</span><strong>${escapeHTML(new Date(terms.expiresAt * 1000).toLocaleString())}</strong><small>Amounts are fixed by the maker signature.</small></div>
    </div>
    <p>Accepting reserves this offer for negotiation. No funds move until the local app shows and you approve the exact contracts.</p>
    <div class="modal-actions"><button class="secondary" id="close-modal">BACK</button><button class="primary" id="confirm-accept-order" data-order-id="${escapeHTML(record.signed.id)}">${buying ? 'ACCEPT AND BUY' : 'ACCEPT AND SELL'}</button></div>
  </div>`);
}

function negotiationTerms(record) {
  const terms = record.order.order;
  return `${amountText(terms.give, terms.qdayUnitAtomic)} → ${amountText(terms.receive, terms.qdayUnitAtomic)}`;
}

function renderSwaps(historyOnly = false) {
  const completed = new Set(['complete', 'refunded', 'expired']);
  const swaps = negotiations.swaps.filter(swap => historyOnly ? completed.has(swap.phase) : !completed.has(swap.phase));
  if (historyOnly) {
    root.innerHTML = `<section class="page-head"><div><span class="eyebrow">LOCAL JOURNAL</span><h1>HISTORY.</h1><p>Completed and refunded swaps remain auditable on this computer.</p></div></section>${swapTable(swaps)}`;
    return;
  }
  root.innerHTML = `<section class="page-head"><div><span class="eyebrow">LOCAL JOURNAL</span><h1>ACTIVE SWAPS.</h1><p>Acceptances, matches and contract progress survive restarts.</p></div></section>
    ${negotiations.incoming.length ? `<section class="card"><div class="card-head"><h2>Match retry required</h2><span>${negotiations.incoming.length}</span></div><div class="negotiation-list">${negotiations.incoming.map(item => `<article><div><strong>${escapeHTML(negotiationTerms(item))}</strong><small>Automatic matching could not reach the relay.</small></div><button class="primary" data-match-acceptance="${escapeHTML(item.acceptance.id)}">RETRY MATCH</button></article>`).join('')}</div></section>` : ''}
    ${negotiations.pending.length ? `<section class="card"><div class="card-head"><h2>Waiting for maker</h2><span>${negotiations.pending.length}</span></div><div class="negotiation-list">${negotiations.pending.map(item => `<article><div><strong>${escapeHTML(negotiationTerms(item))}</strong><small>Acceptance expires ${escapeHTML(new Date(item.acceptance.acceptance.expiresAt * 1000).toLocaleString())}</small></div><span class="waiting-label">PENDING</span></article>`).join('')}</div></section>` : ''}
    ${swapTable(swaps)}`;
}

function swapTable(swaps) {
  if (!swaps.length) return `<section class="card"><div class="empty"><strong>NOTHING HERE YET.</strong><p>Matched swaps will appear here before either wallet broadcasts a contract.</p></div></section>`;
  return `<section class="card"><div class="card-head"><h2>Swaps</h2><span>${swaps.length}</span></div><div class="swap-list">${swaps.map(swap => {
    const view = swapView(swap);
    const canApprove = !swap.approved && swap.agreementJSON && ['terms_agreed', 'maker_funding', 'maker_funded'].includes(swap.phase);
    return `<article class="swap-row ${swap.lastError ? 'has-error' : ''}">
      <div class="swap-main"><span class="side ${view.side}">${view.label}</span><strong>${escapeHTML(view.receive)}</strong><small>You send ${escapeHTML(view.send)}</small></div>
      <div class="swap-progress"><b>${escapeHTML(swapStatus(swap))}</b><small>${escapeHTML(swapStatusNote(swap))}</small>${swap.lastError ? `<em>${escapeHTML(swap.lastError)}</em>` : ''}</div>
      <div class="swap-actions">${canApprove ? `<button class="primary" data-review-swap="${escapeHTML(swap.id)}">REVIEW AND START</button>` : ''}<button class="secondary" data-view-swap="${escapeHTML(swap.id)}">DETAILS</button></div>
    </article>`;
  }).join('')}</div></section>`;
}

function swapView(swap) {
  const terms = swap.order.order;
  const send = swap.role === 'maker' ? terms.give : terms.receive;
  const receive = swap.role === 'maker' ? terms.receive : terms.give;
  return {
    send: amountText(send, terms.qdayUnitAtomic),
    receive: amountText(receive, terms.qdayUnitAtomic),
    side: receive.asset === 'QDAY' ? 'buy' : 'sell',
    label: receive.asset === 'QDAY' ? 'BUY QDAY' : 'SELL QDAY'
  };
}

function swapStatus(swap) {
  if (swap.phase === 'terms_agreed' && !swap.approved) return 'Ready for your approval';
  const localMaker = swap.role === 'maker';
  return ({
    matched: 'Preparing secure swap',
    terms_proposed: 'Verifying exact terms',
    terms_agreed: 'Waiting for the other trader',
    maker_funding: localMaker ? 'Sending your deposit' : 'Waiting for maker deposit',
    maker_funded: localMaker ? 'Your deposit is secured' : (swap.approved ? 'Preparing your deposit' : 'Deposit received. Your approval needed'),
    taker_funding: localMaker ? 'Waiting for taker deposit' : 'Sending your deposit',
    taker_funded: 'Both deposits secured',
    maker_claiming: localMaker ? 'Claiming your coins' : 'Other trader is claiming',
    maker_claimed: localMaker ? 'Waiting for final claim' : 'Your coins are ready to claim',
    taker_claiming: localMaker ? 'Other trader is claiming' : 'Claiming your coins',
    complete: 'Swap complete',
    waiting_for_refund: 'Refund window reached',
    refunding: 'Returning your deposit',
    refunded: 'Deposit refunded',
    expired: 'Swap expired safely'
  })[swap.phase] || swap.phase.replaceAll('_', ' ');
}

function swapStatusNote(swap) {
  if (swap.phase === 'terms_agreed' && !swap.approved) return 'Check the exact amounts once, then the app handles the rest.';
  if (swap.phase === 'maker_funded' && swap.role === 'taker' && !swap.approved) return 'The maker deposit is confirmed. Start when you are ready.';
  if (swap.phase === 'complete') return 'Both claims are confirmed.';
  if (swap.phase === 'refunded') return 'Your funded contract was returned to your wallet.';
  if (swap.phase === 'expired') return 'No local funds moved. The refund window became too short to start safely.';
  return 'The app continues automatically and survives restarts.';
}

function parseAgreement(swap) {
  try { return JSON.parse(swap.agreementJSON || ''); } catch (_) { return null; }
}

function swapDetailsModal(swap, approval = false) {
  const terms = swap.order.order;
  const view = swapView(swap);
  const agreement = parseAgreement(swap);
  const price = orderPrice(terms);
  const refund = agreement
    ? `<div><span>SAFETY REFUNDS</span><strong>QDAY block ${commas(agreement.qdayRefundHeight)}<br>Bitcoin block ${commas(agreement.bitcoinRefundHeight)}</strong><small>If the other trader disappears, your funded contract returns after its chain deadline.</small></div>`
    : `<div><span>SAFETY REFUNDS</span><strong>Preparing…</strong><small>Both absolute refund heights are verified before funding.</small></div>`;
  openModal(`<div class="modal-head"><span class="eyebrow">${approval ? 'FINAL CHECK' : 'ATOMIC SWAP'}</span><h2>${approval ? 'START THIS SWAP?' : escapeHTML(swapStatus(swap))}</h2></div><div class="modal-body">
    <div class="final-quote swap-review-grid">
      <div><span>YOU SEND</span><strong>${escapeHTML(view.send)}</strong><small>Plus that chain's normal network fee</small></div>
      <div><span>YOU RECEIVE</span><strong>${escapeHTML(view.receive)}</strong><small>Claim fee is deducted by the receiving chain</small></div>
      <div><span>FIXED RATE</span><strong>${escapeHTML(price)} BTC / QDAY</strong><small>The signed amounts cannot change.</small></div>
      ${refund}
    </div>
    <div class="swap-safety"><b>NONCUSTODIAL.</b><span>Your app signs locally. Neither the relay nor the other trader can take both deposits. Keep QDAY Swap running; if you close it, reopening resumes from the local journal.</span></div>
    ${swap.lastError ? `<p class="form-error dark">${escapeHTML(swap.lastError)}</p>` : ''}
    <div class="modal-actions"><button class="secondary" id="close-modal">CLOSE</button>${approval ? `<button class="primary" id="approve-swap" data-swap-id="${escapeHTML(swap.id)}">START SWAP</button>` : ''}</div>
  </div>`);
}

function renderEmpty(title, copy, action = '') {
  root.innerHTML = `<section class="page-head"><div><span class="eyebrow">QDAY SWAP</span><h1>${escapeHTML(title)}</h1><p>${escapeHTML(copy)}</p></div>${action}</section><section class="card"><div class="empty"><strong>NOTHING HERE YET.</strong><p>Completed and recoverable swaps will stay in the local journal.</p></div></section>`;
}

function renderSettings() {
  root.innerHTML = `<section class="page-head"><div><span class="eyebrow">LOCAL APPLICATION</span><h1>SETTINGS.</h1><p>Keys stay encrypted on this computer. The QDAY node keeps validating the chain.</p></div></section>
    ${state.error ? `<div class="alert"><strong>Local node error</strong>${escapeHTML(state.error)}</div>` : ''}
    <section class="settings-grid">
      <article class="card setting"><h2>QDAY RECEIVE.</h2><p>Generate the wallet address used to fund swaps and ordinary QDAY transfers.</p><button class="secondary" id="receive-qday">SHOW ADDRESS</button></article>
      <article class="card setting"><h2>BITCOIN RECEIVE.</h2><p>Show your standard Native SegWit address for funding BTC swaps.</p><button class="secondary" id="receive-bitcoin">SHOW ADDRESS</button></article>
      <article class="card setting"><h2>RECOVERY PHRASE.</h2><p>Reveal the 24 words that restore every chain branch and swap identity.</p><button class="secondary" id="show-recovery">REVEAL 24 WORDS</button></article>
      <article class="card setting"><h2>PUBLIC MARKET.</h2><p>${escapeHTML(state.relayURL || 'Relay is not configured')}</p><a class="secondary" href="${escapeHTML(state.relayURL || '#')}" target="_blank" rel="noreferrer">OPEN ORDER EXPLORER</a></article>
      <article class="card setting"><h2>LOCK WALLET.</h2><p>Clear private material from memory while the QDAY node continues syncing.</p><button class="danger" id="lock-wallet">LOCK NOW</button></article>
    </section>`;
}

function currentFingerprint() {
  const current = route();
  if (!state.configured) return 'setup';
  if (!state.unlocked) return 'locked';
  if (current === 'create') return 'create';
  if (current === 'settings') return JSON.stringify({current, error: state.error, relay: state.relayURL});
  if (current === 'swaps' || current === 'history') return JSON.stringify({current, negotiations, relayError: state.relayError});
  return JSON.stringify({
    current: 'market', orders, marketTrades, offerSide, selectedOrderID,
    balance: state.balance, bitcoinBalance: state.bitcoinBalance,
    relayStats: state.relay?.stats, error: state.error,
    relayError: state.relayError, marketError
  });
}

function render() {
  clearTimeout(refreshTimer);
  updateChrome();
  const fingerprint = currentFingerprint();
  if (fingerprint !== renderFingerprint) {
    destroyChart();
    destroyChart = () => {};
    renderFingerprint = fingerprint;
    if (!state.configured) renderSetup();
    else if (!state.unlocked) renderUnlock();
    else {
      switch (route()) {
        case 'create': renderCreate(); break;
        case 'swaps': renderSwaps(); break;
        case 'history': renderSwaps(true); break;
        case 'settings': renderSettings(); break;
        default: renderMarket();
      }
    }
  }
  if (state.configured && state.unlocked) refreshTimer = setTimeout(refreshState, 5000);
}

async function refreshState() {
  if (applicationStopped) return;
  try {
    state = await api('/api/v1/state');
    marketError = '';
    if (state.configured && state.unlocked) {
      const [orderResult, negotiationResult, tradeResult] = await Promise.allSettled([
        api('/api/v1/orders?page=1'), api('/api/v1/negotiations'), api('/api/v1/trades?limit=2000')
      ]);
      if (orderResult.status === 'fulfilled') orders = orderResult.value;
      else marketError = orderResult.reason.message;
      if (negotiationResult.status === 'fulfilled') negotiations = negotiationResult.value;
      else marketError = marketError || negotiationResult.reason.message;
      if (tradeResult.status === 'fulfilled') marketTrades = tradeResult.value;
      else marketError = marketError || tradeResult.reason.message;
    }
    render();
  } catch (error) {
    root.innerHTML = `<div class="alert"><strong>Local application unavailable</strong>${escapeHTML(error.message)}</div>`;
    nodePill.classList.add('error');
    nodePill.querySelector('span').textContent = 'DISCONNECTED';
  }
}

function showStopped() {
  applicationStopped = true;
  clearTimeout(refreshTimer);
  destroyChart();
  destroyChart = () => {};
  closeModal();
  nav.hidden = true;
  nodePill.classList.remove('waiting', 'error');
  nodePill.classList.add('error');
  nodePill.querySelector('span').textContent = 'STOPPED';
  exitButton.disabled = true;
  exitButton.querySelector('span').textContent = 'QDAY SWAP STOPPED';
  root.innerHTML = `<section class="gate"><div class="gate-card">
    <div class="gate-head"><span class="eyebrow">LOCAL SERVICES STOPPED</span><h1>OFFLINE.</h1><p>QDAY Swap, its QDAY node and Bitcoin light client have shut down cleanly.</p></div>
    <div class="gate-body"><p class="stopped-note">You can close this tab. Run QDAY Swap again when you want to trade.</p></div>
  </div></section>`;
  window.scrollTo({top: 0, behavior: 'instant'});
}

function recoveryModal(phrase, firstRun = false) {
  const words = phrase.trim().split(/\s+/);
  openModal(`<div class="modal-head"><span class="eyebrow">${firstRun ? 'WALLET CREATED' : 'RECOVERY'}</span><h2>SAVE ALL 24 WORDS.</h2></div><div class="modal-body"><p>Write them down in order and keep them offline. They restore QDAY, Bitcoin and the swap identity. The password cannot replace them.</p><div class="phrase">${words.map(word => `<span>${escapeHTML(word)}</span>`).join('')}</div><div class="modal-actions"><button class="secondary" id="copy-phrase">COPY</button><button class="primary" id="saved-phrase">I SAVED ALL 24 WORDS</button></div></div>`);
  document.querySelector('#copy-phrase').addEventListener('click', async () => {
    await navigator.clipboard.writeText(phrase);
    showToast('Recovery phrase copied');
  });
  document.querySelector('#saved-phrase').addEventListener('click', closeModal);
}

document.addEventListener('click', async event => {
  if (event.target.closest('#exit-app')) {
    if (applicationStopped || exitButton.disabled) return;
    exitButton.disabled = true;
    exitButton.querySelector('span').textContent = 'STOPPING…';
    try {
      await post('/api/v1/shutdown');
      showStopped();
    } catch (error) {
      exitButton.disabled = false;
      exitButton.querySelector('span').textContent = 'EXIT QDAY SWAP';
      showToast(error.message);
    }
    return;
  }
  if (event.target.closest('#close-modal')) {
    closeModal();
    return;
  }
  const bookOrder = event.target.closest('[data-book-order]');
  if (bookOrder) {
    selectedOrderID = bookOrder.dataset.bookOrder;
    offerSide = bookOrder.dataset.bookSide;
    offerPriceCurrency = 'BTC';
    renderFingerprint = '';
    render();
    document.querySelector('.ticket-panel')?.scrollIntoView({behavior: 'smooth', block: 'nearest'});
    return;
  }
  const ticketSide = event.target.closest('[data-ticket-side]');
  if (ticketSide) {
    offerSide = ticketSide.dataset.ticketSide;
    selectedOrderID = '';
    renderFingerprint = '';
    render();
    return;
  }
  if (event.target.closest('[data-clear-order]')) {
    selectedOrderID = '';
    renderFingerprint = '';
    render();
    return;
  }
  const range = event.target.closest('[data-chart-range]');
  if (range) {
    chartRange = range.dataset.chartRange;
    document.querySelectorAll('[data-chart-range]').forEach(button => button.classList.toggle('active', button.dataset.chartRange === chartRange));
    setupPriceChart();
    return;
  }
  const sideChoice = event.target.closest('[data-offer-side]');
  if (sideChoice) {
    offerSide = sideChoice.dataset.offerSide;
    location.hash = `create?side=${offerSide}`;
    renderFingerprint = '';
    render();
    return;
  }
  const currencyChoice = event.target.closest('[data-price-currency]');
  if (currencyChoice) {
    const nextCurrency = currencyChoice.dataset.priceCurrency;
    const form = document.querySelector('#create-offer-form');
    const input = form?.elements.price;
    const current = Number(input?.value);
    if (input && nextCurrency !== offerPriceCurrency && Number.isFinite(current) && current > 0 && bitcoinUSD > 0) {
      input.value = cleanDecimal(nextCurrency === 'BTC' ? current / bitcoinUSD : current * bitcoinUSD, nextCurrency === 'BTC' ? 16 : 8);
    }
    offerPriceCurrency = nextCurrency;
    document.querySelectorAll('[data-price-currency]').forEach(button => button.classList.toggle('active', button.dataset.priceCurrency === offerPriceCurrency));
    if (input) input.placeholder = offerPriceCurrency === 'USD' ? '1.00' : '0.00001';
    const help = document.querySelector('#price-help');
    if (help) help.textContent = offerPriceCurrency === 'USD' ? `Converted to BTC when you review${bitcoinUSD ? ` at ${dollars(bitcoinUSD, true)} per BTC` : ''}.` : 'The signed offer uses this BTC price directly.';
    updateOfferPreview();
    return;
  }
  const mode = event.target.closest('[data-setup-mode]');
  if (mode) {
    setupMode = mode.dataset.setupMode;
    renderSetup();
    return;
  }
  if (event.target.closest('#lock-wallet')) {
    try { state = await post('/api/v1/lock'); location.hash = 'market'; render(); } catch (error) { showToast(error.message); }
  }
  if (event.target.closest('#receive-qday')) {
    try {
      const address = await api('/api/v1/qday/address');
      openModal(`<div class="modal-head"><span class="eyebrow">QDAY WALLET</span><h2>RECEIVE QDAY.</h2></div><div class="modal-body"><p>Send ordinary QDAY transfers to this address. Wait for confirmations before opening a swap.</p><div class="address-box">${escapeHTML(address.address)}</div><div class="modal-actions"><button class="secondary" id="close-modal">CLOSE</button><button class="primary" id="copy-address">COPY ADDRESS</button></div></div>`);
      document.querySelector('#close-modal').addEventListener('click', closeModal);
      document.querySelector('#copy-address').addEventListener('click', async () => { await navigator.clipboard.writeText(address.address); showToast('Address copied'); });
    } catch (error) { showToast(error.message); }
  }
  if (event.target.closest('#receive-bitcoin')) {
    try {
      const result = await api('/api/v1/bitcoin/address');
      openModal(`<div class="modal-head"><span class="eyebrow">BITCOIN WALLET</span><h2>RECEIVE BTC.</h2></div><div class="modal-body"><p>Send BTC to this Native SegWit address. The local light client verifies confirmations without downloading the full Bitcoin chain.</p><div class="address-box">${escapeHTML(result.address)}</div><div class="modal-actions"><button class="secondary" id="close-modal">CLOSE</button><button class="primary" id="copy-address">COPY ADDRESS</button></div></div>`);
      document.querySelector('#close-modal').addEventListener('click', closeModal);
      document.querySelector('#copy-address').addEventListener('click', async () => { await navigator.clipboard.writeText(result.address); showToast('Bitcoin address copied'); });
    } catch (error) { showToast(error.message); }
  }
  if (event.target.closest('#show-recovery')) {
    if (!confirm('Reveal the recovery phrase on this screen?')) return;
    try { recoveryModal((await post('/api/v1/recovery')).phrase); } catch (error) { showToast(error.message); }
  }
  const accept = event.target.closest('[data-accept-order]');
  if (accept) {
    const record = orders.items.find(item => item.signed.id === accept.dataset.acceptOrder);
    if (!record) { showToast('Offer is no longer available'); return; }
    acceptanceReview(record);
    return;
  }
  const confirmAccept = event.target.closest('#confirm-accept-order');
  if (confirmAccept) {
    confirmAccept.disabled = true;
    confirmAccept.textContent = 'ACCEPTING…';
    try {
      await post(`/api/v1/orders/${confirmAccept.dataset.orderId}/accept`);
      closeModal(); location.hash = 'swaps'; renderFingerprint = ''; await refreshState(); showToast('Offer accepted');
    } catch (error) {
      showToast(error.message); confirmAccept.disabled = false; confirmAccept.textContent = 'TRY AGAIN';
    }
    return;
  }
  const publish = event.target.closest('#publish-offer');
  if (publish && pendingOffer) {
    publish.disabled = true;
    publish.textContent = 'SIGNING AND PUBLISHING…';
    try {
      const {quote, lifetimeMinutes} = pendingOffer;
      await post('/api/v1/orders', {
        giveAsset: quote.giveAsset, giveAmount: quote.giveAmount,
        receiveAmount: quote.receiveAmount, lifetimeMinutes
      });
      pendingOffer = null; closeModal(); location.hash = 'market'; renderFingerprint = ''; await refreshState(); showToast('Offer published');
    } catch (error) {
      showToast(error.message); publish.disabled = false; publish.textContent = 'TRY AGAIN';
    }
    return;
  }
  const cancel = event.target.closest('[data-cancel-order]');
  if (cancel) {
    if (!confirm('Cancel this offer?')) return;
    cancel.disabled = true;
    try { await post(`/api/v1/orders/${cancel.dataset.cancelOrder}/cancel`); renderFingerprint = ''; await refreshState(); } catch (error) { showToast(error.message); cancel.disabled = false; }
  }
  const match = event.target.closest('[data-match-acceptance]');
  if (match) {
    if (!confirm('Match this taker and close the public offer?')) return;
    match.disabled = true;
    try { await post(`/api/v1/acceptances/${match.dataset.matchAcceptance}/match`); renderFingerprint = ''; await refreshState(); } catch (error) { showToast(error.message); match.disabled = false; }
  }
  const reviewSwap = event.target.closest('[data-review-swap]');
  if (reviewSwap) {
    const swap = negotiations.swaps.find(item => item.id === reviewSwap.dataset.reviewSwap);
    if (!swap) { showToast('Swap is no longer available'); return; }
    swapDetailsModal(swap, true);
    return;
  }
  const viewSwap = event.target.closest('[data-view-swap]');
  if (viewSwap) {
    const swap = negotiations.swaps.find(item => item.id === viewSwap.dataset.viewSwap);
    if (!swap) { showToast('Swap is no longer available'); return; }
    swapDetailsModal(swap, false);
    return;
  }
  const approveSwap = event.target.closest('#approve-swap');
  if (approveSwap) {
    approveSwap.disabled = true;
    approveSwap.textContent = 'STARTING…';
    try {
      await post(`/api/v1/swaps/${approveSwap.dataset.swapId}/approve`);
      closeModal(); renderFingerprint = ''; await refreshState(); showToast('Swap started');
    } catch (error) {
      showToast(error.message); approveSwap.disabled = false; approveSwap.textContent = 'TRY AGAIN';
    }
    return;
  }
});

document.addEventListener('input', event => {
  if (event.target.closest('#create-offer-form')) updateOfferPreview();
});

document.addEventListener('submit', async event => {
  event.preventDefault();
  if (event.target.id === 'setup-form') {
    const form = event.target;
    const errorBox = form.querySelector('#form-error');
    const password = form.elements.password.value;
    if (password !== form.elements.confirm.value) {
      errorBox.textContent = 'Passwords do not match.'; errorBox.hidden = false; return;
    }
    const button = form.querySelector('[type=submit]');
    button.disabled = true; button.textContent = 'CREATING LOCAL WALLETS…';
    try {
      const result = await post('/api/v1/setup', {password, phrase: form.elements.phrase?.value.trim() || ''});
      form.reset(); state = result.state; location.hash = 'market'; render();
      if (result.phrase) recoveryModal(result.phrase, true);
    } catch (error) {
      errorBox.textContent = error.message; errorBox.hidden = false;
      button.disabled = false; button.textContent = setupMode === 'new' ? 'CREATE QDAY SWAP WALLET' : 'IMPORT WALLET';
    }
  }
  if (event.target.id === 'unlock-form') {
    const form = event.target;
    const button = form.querySelector('[type=submit]');
    button.disabled = true; button.textContent = 'UNLOCKING…';
    try {
      state = await post('/api/v1/unlock', {password: form.elements.password.value});
      form.reset(); render();
    } catch (error) {
      const errorBox = form.querySelector('#form-error') || form.querySelector('.form-error');
      errorBox.textContent = error.message; errorBox.hidden = false;
      button.disabled = false; button.textContent = 'UNLOCK WALLET';
    }
  }
  if (event.target.id === 'create-offer-form') {
    const form = event.target;
    const button = form.querySelector('[type=submit]');
    const errorBox = form.querySelector('#form-error');
    const originalLabel = button.textContent;
    const takeOrderID = button.dataset.takeOrder;
    if (takeOrderID) {
      const record = orders.items.find(item => item.signed.id === takeOrderID);
      if (!record) { showToast('Offer is no longer available'); return; }
      acceptanceReview(record);
      return;
    }
    button.disabled = true; button.textContent = 'CALCULATING EXACT AMOUNTS…';
    errorBox.hidden = true;
    try {
      const lifetimeMinutes = Number(form.elements.lifetimeMinutes.value);
      const quote = await post('/api/v1/offers/quote', {
        side: form.dataset.side,
        quantity: form.elements.quantity.value.trim(),
        price: form.elements.price.value.trim(),
        priceCurrency: offerPriceCurrency
      });
      pendingOffer = {quote, lifetimeMinutes};
      offerReview(quote, lifetimeMinutes);
      button.disabled = false; button.textContent = originalLabel;
    } catch (error) {
      errorBox.textContent = error.message; errorBox.hidden = false;
      button.disabled = false; button.textContent = originalLabel;
    }
  }
});

window.addEventListener('hashchange', () => { renderFingerprint = ''; if (state) render(); });
modalBackdrop.addEventListener('click', event => { if (event.target === modalBackdrop) closeModal(); });
refreshPriceLoop();
refreshState();
