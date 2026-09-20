const root = document.querySelector('#app');
const relayPill = document.querySelector('#relay-pill');
const relayPillBox = relayPill.parentElement;
const headerMatches = document.querySelector('#header-matches');
const headerDay = document.querySelector('#header-day');
const headerVolume = document.querySelector('#header-volume');
const headerBitcoinUSD = document.querySelector('#header-btc-usd');
const headerQDAYUSD = document.querySelector('#header-qday-usd');

let routeVersion = 0;
let refreshTimer;
let statusCache;
let marketTrades = {items: [], total: 0};
let bitcoinUSD = 0;
let chartRange = 'ALL';
let destroyChart = () => {};

const escapeHTML = value => String(value ?? '').replace(/[&<>'"]/g, character => ({
  '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;'
})[character]);

async function api(path) {
  const response = await fetch(path, {headers: {Accept: 'application/json'}, cache: 'no-store'});
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `HTTP ${response.status}`);
  return body;
}

function commas(value) {
  const [whole, fraction] = String(value ?? '0').split('.');
  return `${whole.replace(/\B(?=(\d{3})+(?!\d))/g, ',')}${fraction ? `.${fraction}` : ''}`;
}

function cleanDecimal(value, places = 12) {
  const number = Number(value);
  if (!Number.isFinite(number)) return '0';
  if (number === 0) return '0';
  const magnitudePlaces = number < 1 ? Math.ceil(-Math.log10(Math.abs(number))) + 5 : 6;
  return number.toFixed(Math.max(0, Math.min(places, magnitudePlaces))).replace(/0+$/, '').replace(/\.$/, '');
}

function signedDecimal(value, places = 2) {
  const number = Number(value);
  if (!Number.isFinite(number)) return '0';
  return `${number > 0 ? '+' : ''}${number.toFixed(places)}`;
}

function dollars(value, bitcoin = false) {
  if (!Number.isFinite(value) || value <= 0) return '$—';
  const maximumFractionDigits = bitcoin ? 0 : value >= 1 ? 4 : value >= .01 ? 6 : 8;
  return new Intl.NumberFormat('en-US', {
    style: 'currency', currency: 'USD', minimumFractionDigits: 0, maximumFractionDigits
  }).format(value);
}

function numericUnits(atomic, unit) {
  const value = Number(atomic);
  const scale = Number(unit);
  return Number.isFinite(value) && Number.isFinite(scale) && scale > 0 ? value / scale : 0;
}

function marketPriceNumber(trade) {
  const qday = numericUnits(trade.qdayAtomic, trade.qdayUnitAtomic);
  const bitcoin = numericUnits(trade.btcAtomic, '100000000');
  return qday > 0 ? bitcoin / qday : 0;
}

function recentMarketStats() {
  const cutoff = Math.floor(Date.now() / 1000) - 86400;
  const recent = marketTrades.items.filter(trade => Number(trade.matchedAt) >= cutoff);
  return {
    count: recent.length,
    volume: recent.reduce((total, trade) => total + numericUnits(trade.qdayAtomic, trade.qdayUnitAtomic), 0)
  };
}

function updateHeader() {
  const recent = recentMarketStats();
  const lastTrade = marketTrades.items[0];
  headerMatches.textContent = commas(marketTrades.total);
  headerDay.textContent = commas(recent.count);
  headerVolume.textContent = recent.volume ? `${commas(cleanDecimal(recent.volume, 4))} QDAY` : '0 QDAY';
  if (!lastTrade) {
    headerQDAYUSD.textContent = 'N/A';
    headerQDAYUSD.title = 'No matched offers yet';
  } else if (!bitcoinUSD) {
    headerQDAYUSD.textContent = '—';
  } else {
    headerQDAYUSD.textContent = dollars(marketPriceNumber(lastTrade) * bitcoinUSD);
    headerQDAYUSD.title = `Last match · ${new Date(Number(lastTrade.matchedAt) * 1000).toLocaleString()}`;
  }
}

async function loadPublicData() {
  const [statusResult, tradesResult, priceResult] = await Promise.allSettled([
    api('/api/v1/status'),
    api('/api/v1/trades?limit=2000'),
    api('/api/v1/price')
  ]);
  if (tradesResult.status === 'rejected') throw tradesResult.reason;
  marketTrades = tradesResult.value;
  if (statusResult.status === 'fulfilled') {
    statusCache = statusResult.value;
    relayPill.textContent = `${String(statusCache.network || 'MAINNET').toUpperCase()} LIVE`;
    relayPillBox.classList.remove('connecting', 'offline');
  } else {
    relayPill.textContent = 'RELAY LIVE';
    relayPillBox.classList.remove('connecting', 'offline');
  }
  if (priceResult.status === 'fulfilled') {
    const numeric = Number(priceResult.value.usd);
    if (Number.isFinite(numeric) && numeric > 0) {
      bitcoinUSD = numeric;
      headerBitcoinUSD.textContent = dollars(bitcoinUSD, true);
      headerBitcoinUSD.title = `${priceResult.value.source} · ${new Date(priceResult.value.updatedAt).toLocaleTimeString()}${priceResult.value.stale ? ' · stale' : ''}`;
    }
  } else if (!bitcoinUSD) {
    headerBitcoinUSD.textContent = '—';
  }
  updateHeader();
}

function markNavigation(routeName) {
  document.querySelectorAll('[data-nav]').forEach(link => link.classList.toggle('active', link.dataset.nav === routeName));
}

function renderProtocol() {
  root.innerHTML = `<div class="protocol-page"><section class="protocol-download protocol-download-home">
      <div class="download-intro"><span class="command">$ install qday-swap</span><h1>DOWNLOAD QDAY SWAP. FUCK KYC</h1><p>QDAY Swap is a noncustodial atomic swap protocol and desktop app for trading QDAY directly with assets on other blockchains. QDAY ↔ Bitcoin is the first market. More chains will be added to the same app and protocol. Create or accept an offer locally, and the swap settles directly on both native blockchains. No exchange account. No deposits. No custodian. This site explains the protocol and shows matched market activity.</p></div>
      <div class="download-grid">
        <a class="download-card" href="https://github.com/petoshi/qday-swap/releases/download/v1.0.0/QDAY-Swap-windows-amd64.zip">
          <svg class="os-icon" viewBox="0 0 64 64" aria-hidden="true"><path d="M7 11l22-3v22H7V11zm26-4l24-3v26H33V7zM7 34h22v22L7 53V34zm26 0h24v26l-24-3V34z"/></svg>
          <span><strong>WINDOWS</strong><small>X86 64 · ZIP</small></span><b>DOWNLOAD ↓</b>
        </a>
        <a class="download-card" href="https://github.com/petoshi/qday-swap/releases/download/v1.0.0/QDAY-Swap-linux-amd64.tar.gz">
          <svg class="os-icon linux-icon" viewBox="0 0 64 64" aria-hidden="true"><path class="tux-body" d="M32 4c-8 0-13 7-13 17 0 4-1 8-4 13-4 6-5 13-2 17 2 3 6 3 10 1 3 5 15 5 18 0 4 2 8 2 10-1 3-4 2-11-2-17-3-5-4-9-4-13C45 11 40 4 32 4z"/><ellipse class="tux-belly" cx="32" cy="39" rx="12" ry="15"/><ellipse class="tux-eye" cx="27" cy="18" rx="4" ry="5"/><ellipse class="tux-eye" cx="37" cy="18" rx="4" ry="5"/><circle class="tux-pupil" cx="28" cy="19" r="1.5"/><circle class="tux-pupil" cx="36" cy="19" r="1.5"/><path class="tux-beak" d="M26 23l6-4 6 4-6 5z"/><path class="tux-foot" d="M23 49c-7 1-11 5-9 8 2 2 9 1 14-2zm18 0c7 1 11 5 9 8-2 2-9 1-14-2z"/></svg>
          <span><strong>LINUX</strong><small>X86 64 · TAR.GZ</small></span><b>DOWNLOAD ↓</b>
        </a>
        <a class="download-card" href="https://github.com/petoshi/qday-swap/releases/download/v1.0.0/QDAY-Swap-linux-arm64.tar.gz">
          <svg class="os-icon linux-icon" viewBox="0 0 64 64" aria-hidden="true"><path class="tux-body" d="M32 4c-8 0-13 7-13 17 0 4-1 8-4 13-4 6-5 13-2 17 2 3 6 3 10 1 3 5 15 5 18 0 4 2 8 2 10-1 3-4 2-11-2-17-3-5-4-9-4-13C45 11 40 4 32 4z"/><ellipse class="tux-belly" cx="32" cy="39" rx="12" ry="15"/><ellipse class="tux-eye" cx="27" cy="18" rx="4" ry="5"/><ellipse class="tux-eye" cx="37" cy="18" rx="4" ry="5"/><circle class="tux-pupil" cx="28" cy="19" r="1.5"/><circle class="tux-pupil" cx="36" cy="19" r="1.5"/><path class="tux-beak" d="M26 23l6-4 6 4-6 5z"/><path class="tux-foot" d="M23 49c-7 1-11 5-9 8 2 2 9 1 14-2zm18 0c7 1 11 5 9 8-2 2-9 1-14-2z"/></svg>
          <span><strong>LINUX</strong><small>ARM64 · TAR.GZ</small></span><b>DOWNLOAD ↓</b>
        </a>
        <a class="download-card" href="https://github.com/petoshi/qday-swap/releases/download/v1.0.0/QDAY-Swap-macos-universal.zip">
          <svg class="os-icon apple-icon" viewBox="0 0 64 64" aria-hidden="true"><path d="M39 13c3-4 3-8 3-10-4 0-8 3-10 6-2 2-3 6-3 9 4 0 7-2 10-5zM49 35c0-8 7-12 7-12-4-6-10-7-13-7-6-1-11 4-14 4s-7-4-12-4C8 16 0 24 0 36c0 7 3 15 6 20 3 4 6 8 11 8 4 0 6-3 12-3s7 3 12 3 8-4 11-8c3-4 4-9 5-11-1 0-8-3-8-10z" transform="translate(4 -1) scale(.88)"/></svg>
          <span><strong>MACOS</strong><small>UNIVERSAL · ZIP</small></span><b>DOWNLOAD ↓</b>
        </a>
      </div>
    </section>
    <section class="quickstart">
      <header><span class="command">$ qday-swap quickstart</span><h2>THE SWAP RUNS LOCALLY.</h2></header>
      <div class="quickstart-grid">
        <article><b>01</b><h3>OPEN THE APP.</h3><p>Extract the complete archive and run QDAY Swap. Your browser opens the local interface. Create a wallet or restore a recovery phrase previously generated by QDAY Swap.</p></article>
        <article><b>02</b><h3>LET IT SYNC.</h3><p>The app starts a validating QDAY node and a Bitcoin light client. The status bar shows both chains. You do not need to download the full Bitcoin blockchain.</p></article>
        <article><b>03</b><h3>FUND YOUR WALLET.</h3><p>Open Wallets, copy your QDAY or Native SegWit Bitcoin receive address, and send the asset you want to trade. Both addresses belong to your local wallet. Your keys never leave your computer.</p></article>
        <article><b>04</b><h3>CREATE OR TAKE.</h3><p>Use Orders inside the local app to create an offer or accept one. Review the exact amounts and expiry locally before the swap starts.</p></article>
      </div>
    </section>
    <div class="protocol-art-space" aria-hidden="true"></div>
    <section class="page-header protocol-header"><h1>HYBRID POST QUANTUM ATOMIC SWAPS.</h1><p>QDAY and Bitcoin use different signature systems and share one SHA 256 hashlock.</p></section>
    <section class="protocol-grid">
      <article class="protocol-step"><b>01</b><h2>CREATE AND LEAVE.</h2><p>You can create an order, close your laptop, and leave. Someone can take the order while you are offline.</p></article>
      <article class="protocol-step"><b>02</b><h2>RETURN ONCE.</h2><p>When you return, QDAY Swap automatically selects the first valid acceptance and broadcasts the taker’s pre-signed funding transaction. The taker only needs to return once to finish the swap automatically.</p></article>
      <article class="protocol-step"><b>03</b><h2>CLAIM OR REFUND.</h2><p>If anything breaks or either side disappears, on-chain refunds protect both parties. No custody. No KYC. No need to be online at the same time.</p></article>
    </section>
    <div class="protocol-note"><strong>Hybrid means both sides are described honestly.</strong> QDAY spends require Ed25519 and SLH DSA. Bitcoin still uses secp256k1. The atomic protocol joins them without pretending Bitcoin is post quantum.</div></div>`;
}

function marketActivityRows() {
  if (!marketTrades.items.length) return '<tr><td class="market-empty" colspan="5">NO MATCHED ACTIVITY YET.</td></tr>';
  return marketTrades.items.slice(0, 20).map(trade => {
    const price = marketPriceNumber(trade);
    const amount = numericUnits(trade.qdayAtomic, trade.qdayUnitAtomic);
    const total = numericUnits(trade.btcAtomic, '100000000');
    const side = trade.side === 'sell' ? 'sell' : 'buy';
    return `<tr>
      <td data-label="Side"><span class="side ${side}">${side.toUpperCase()} QDAY</span></td>
      <td data-label="Price" class="numeric market-price">${escapeHTML(cleanDecimal(price, 16))} <span>BTC</span></td>
      <td data-label="Amount" class="numeric">${escapeHTML(commas(cleanDecimal(amount, 8)))} <span>QDAY</span></td>
      <td data-label="Total" class="numeric">${escapeHTML(cleanDecimal(total, 8))} <span>BTC</span></td>
      <td data-label="Matched" class="activity-time" title="${escapeHTML(new Date(Number(trade.matchedAt) * 1000).toLocaleString())}">${escapeHTML(new Date(Number(trade.matchedAt) * 1000).toLocaleString(undefined, {dateStyle: 'medium', timeStyle: 'short'}))}</td>
    </tr>`;
  }).join('');
}

function marketMarkup() {
  return `<section class="market-header"><span class="command">$ qday-swap market --matched</span><h1>QDAY / BTC MARKET.</h1><p>Live price history and the latest matched activity. Create and accept offers inside the local QDAY Swap application.</p></section>
    <section class="market-layout">
      <article class="market-panel chart-panel"><header class="panel-head"><div><span>PRICE</span><strong>QDAY / BTC</strong></div><div class="chart-controls"><span class="chart-scale">AUTO SCALE</span><div class="chart-ranges"><button type="button" data-chart-range="24H" class="${chartRange === '24H' ? 'active' : ''}">24H</button><button type="button" data-chart-range="7D" class="${chartRange === '7D' ? 'active' : ''}">7D</button><button type="button" data-chart-range="ALL" class="${chartRange === 'ALL' ? 'active' : ''}">ALL</button></div></div></header>
        <div class="chart-summary"><div><span>LAST</span><strong id="chart-last">N/A</strong></div><div><span>CHANGE</span><strong id="chart-change">N/A</strong></div><div><span>HIGH</span><strong id="chart-high">N/A</strong></div><div><span>LOW</span><strong id="chart-low">N/A</strong></div><div><span>VOLUME</span><strong id="chart-volume">N/A</strong></div></div>
        <div class="chart-wrap"><canvas id="price-chart" aria-label="Matched QDAY Bitcoin price history"></canvas><div class="chart-empty" id="chart-empty" hidden>NO MATCHED ACTIVITY YET</div><div class="chart-tooltip" id="chart-tooltip" hidden></div></div>
      </article>
      <article class="market-panel activity-panel"><header class="panel-head"><div><span>LATEST ACTIVITY</span><strong>MATCHED OFFERS</strong></div><small id="activity-total">0 TOTAL</small></header>
        <div class="activity-scroll"><table class="activity-table"><thead><tr><th>SIDE</th><th class="numeric">PRICE</th><th class="numeric">AMOUNT</th><th class="numeric">TOTAL</th><th>MATCHED</th></tr></thead><tbody id="activity-body"></tbody></table></div>
      </article>
    </section>
    <p class="market-disclosure">Public market data records signed offer matches. Final settlement remains verifiable on QDAY and Bitcoin inside each swap.</p>`;
}

function updateMarketView() {
  const body = document.querySelector('#activity-body');
  const total = document.querySelector('#activity-total');
  if (body) body.innerHTML = marketActivityRows();
  if (total) total.textContent = `${commas(marketTrades.total)} TOTAL`;
  setupPriceChart();
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
    .filter(trade => Number(trade.matchedAt) >= cutoff)
    .map(trade => ({
      time: Number(trade.matchedAt), price: marketPriceNumber(trade),
      volume: numericUnits(trade.qdayAtomic, trade.qdayUnitAtomic), side: trade.side
    }))
    .filter(point => Number.isFinite(point.time) && point.time > 0 && Number.isFinite(point.price) && point.price > 0)
    .sort((left, right) => left.time - right.time);

  empty.hidden = points.length > 0;
  canvas.hidden = !points.length;
  if (!points.length) {
    for (const output of [lastOutput, changeOutput, highOutput, lowOutput, volumeOutput]) if (output) output.textContent = 'N/A';
    if (changeOutput) changeOutput.className = '';
    return;
  }

  const firstPrice = points[0].price;
  const latestPrice = points.at(-1).price;
  const highPrice = Math.max(...points.map(point => point.price));
  const lowPrice = Math.min(...points.map(point => point.price));
  const totalVolume = points.reduce((total, point) => total + point.volume, 0);
  const change = firstPrice > 0 ? (latestPrice / firstPrice - 1) * 100 : 0;
  lastOutput.textContent = cleanDecimal(latestPrice, 16);
  changeOutput.textContent = `${signedDecimal(change, 2)}%`;
  changeOutput.className = change > 0 ? 'up' : change < 0 ? 'down' : '';
  highOutput.textContent = cleanDecimal(highPrice, 16);
  lowOutput.textContent = cleanDecimal(lowPrice, 16);
  volumeOutput.textContent = `${commas(cleanDecimal(totalVolume, 4))} QDAY`;

  const context = canvas.getContext('2d');
  const wrap = canvas.parentElement;
  let geometry = null;
  let hover = -1;
  const priceLabel = (value, step = 0) => {
    if (!Number.isFinite(value)) return '—';
    const places = step > 0 ? Math.max(0, Math.min(16, Math.ceil(-Math.log10(step)) + 1)) : value >= 1 ? 4 : value >= .001 ? 7 : 12;
    return value.toFixed(places).replace(/0+$/, '').replace(/\.$/, '') || '0';
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
    return (fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 2.5 ? 2.5 : fraction <= 5 ? 5 : 10) * power;
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

    const volumeHeight = width < 520 ? 42 : 54;
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
    const axisWidth = Math.max(context.measureText(priceLabel(minimumPrice, tickStep)).width, context.measureText(priceLabel(maximumPrice, tickStep)).width, context.measureText(priceLabel(latestPrice)).width);
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
    geometry = {width, height, padding, chartBottom, minimumTime, maximumTime, x, y};

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
      context.fillText(timeLabel(minimumTime + (maximumTime - minimumTime) * index / timeTickCount, maximumTime - minimumTime), lineX, height - 12);
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
    context.setLineDash([2, 4]); context.strokeStyle = rising ? 'rgba(125,255,155,.45)' : 'rgba(255,137,109,.45)';
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
      context.setLineDash([3, 4]); context.strokeStyle = '#536057';
      context.beginPath(); context.moveTo(pointX, padding.top); context.lineTo(pointX, chartBottom); context.stroke();
      context.beginPath(); context.moveTo(padding.left, pointY); context.lineTo(width - padding.right, pointY); context.stroke();
      context.setLineDash([]); context.fillStyle = '#030403'; context.strokeStyle = trendColor;
      context.beginPath(); context.arc(pointX, pointY, 4, 0, Math.PI * 2); context.fill(); context.stroke();
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
    tooltip.innerHTML = `<strong>${escapeHTML(cleanDecimal(point.price, 16))} BTC</strong><span>${bitcoinUSD ? `${escapeHTML(dollars(point.price * bitcoinUSD))} / QDAY · ` : ''}${escapeHTML(cleanDecimal(point.volume, 8))} QDAY</span><time>${escapeHTML(new Date(point.time * 1000).toLocaleString())}</time>`;
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

async function renderMarket(token, silent = false) {
  await loadPublicData();
  if (token !== routeVersion) return;
  if (!silent || !document.querySelector('.market-layout')) root.innerHTML = marketMarkup();
  updateMarketView();
}

function normalizeLegacyRoute(pathname) {
  if (pathname === '/orders' || pathname === '/activity' || pathname.startsWith('/order/')) {
    history.replaceState({}, '', '/market');
    return '/market';
  }
  return pathname;
}

async function route(silent = false) {
  clearTimeout(refreshTimer);
  const token = ++routeVersion;
  let pathname = location.pathname.replace(/\/+$/, '') || '/';
  pathname = normalizeLegacyRoute(pathname);
  if (!silent) {
    destroyChart();
    destroyChart = () => {};
    root.innerHTML = '<section class="loading-page"><p><b>$</b> loading market data<span class="terminal-cursor">_</span></p></section>';
  }
  try {
    if (pathname === '/' || pathname === '/protocol') {
      markNavigation('protocol');
      if (!silent) renderProtocol();
      await loadPublicData();
    } else if (pathname === '/market') {
      markNavigation('market');
      await renderMarket(token, silent);
    } else {
      throw new Error('Page not found');
    }
  } catch (error) {
    if (token !== routeVersion) return;
    relayPill.textContent = 'RELAY OFFLINE';
    relayPillBox.classList.remove('connecting');
    relayPillBox.classList.add('offline');
    if (!silent) root.innerHTML = `<section class="error-card"><h1>MARKET DATA UNAVAILABLE.</h1><p>${escapeHTML(error.message)}</p></section>`;
  }
  if (token === routeVersion) refreshTimer = setTimeout(() => route(true), 20000);
}

document.addEventListener('click', event => {
  const routeLink = event.target.closest('a.route-link');
  if (routeLink && routeLink.origin === location.origin) {
    event.preventDefault();
    history.pushState({}, '', routeLink.href);
    route();
    return;
  }
  const range = event.target.closest('[data-chart-range]');
  if (range) {
    chartRange = range.dataset.chartRange;
    document.querySelectorAll('[data-chart-range]').forEach(button => button.classList.toggle('active', button.dataset.chartRange === chartRange));
    setupPriceChart();
  }
});

window.addEventListener('popstate', () => route());
route();
