const root = document.querySelector('#app');
const relayPill = document.querySelector('#relay-pill');
const relayPillBox = relayPill.parentElement;
const toast = document.querySelector('#toast');
const headerOpen = document.querySelector('#header-open');
const headerDay = document.querySelector('#header-day');
const headerTotal = document.querySelector('#header-total');
const headerBitcoinUSD = document.querySelector('#header-btc-usd');
const headerQDAYUSD = document.querySelector('#header-qday-usd');

let routeVersion = 0;
let refreshTimer;
let toastTimer;
let statusCache;
let directionFilter = '';
let viewFingerprint = '';
let bitcoinUSD = 0;
let lastTrade = null;

const escapeHTML = value => String(value ?? '').replace(/[&<>'"]/g, character => ({
  '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;'
})[character]);

function showToast(message) {
  toast.textContent = message;
  toast.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toast.classList.remove('show'), 1800);
}

async function api(path, options = {}) {
  const response = await fetch(path, {headers: {Accept: 'application/json'}, cache: 'no-store', ...options});
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `HTTP ${response.status}`);
  return body;
}

function commas(value) {
  return String(value ?? '0').replace(/\B(?=(\d{3})+(?!\d))/g, ',');
}

function short(value, left = 11, right = 9) {
  const text = String(value || '');
  return text.length > left + right + 1 ? `${text.slice(0, left)}…${text.slice(-right)}` : text;
}

function exactTime(seconds) {
  return new Date(Number(seconds) * 1000).toLocaleString(undefined, {dateStyle: 'medium', timeStyle: 'medium'});
}

function relativeFuture(seconds) {
  const remaining = Number(seconds) - Math.floor(Date.now() / 1000);
  if (remaining <= 0) return 'Expired';
  if (remaining < 60) return `${remaining}s`;
  if (remaining < 3600) return `${Math.ceil(remaining / 60)}m`;
  if (remaining < 86400) return `${Math.ceil(remaining / 3600)}h`;
  return `${Math.ceil(remaining / 86400)}d`;
}

function relativePast(seconds) {
  const elapsed = Math.max(0, Math.floor(Date.now() / 1000) - Number(seconds));
  if (elapsed < 60) return `${elapsed}s ago`;
  if (elapsed < 3600) return `${Math.floor(elapsed / 60)}m ago`;
  if (elapsed < 86400) return `${Math.floor(elapsed / 3600)}h ago`;
  return `${Math.floor(elapsed / 86400)}d ago`;
}

function decimalsFromUnit(unit) {
  const value = String(unit);
  if (!/^10*$/.test(value)) return null;
  return value.length - 1;
}

function formatUnits(atomic, unit, maximumPlaces = 8) {
  const decimals = decimalsFromUnit(unit);
  if (decimals === null) return commas(atomic);
  const negative = String(atomic).startsWith('-');
  let digits = negative ? String(atomic).slice(1) : String(atomic);
  digits = digits.padStart(decimals + 1, '0');
  const whole = digits.slice(0, -decimals) || '0';
  let fraction = decimals ? digits.slice(-decimals) : '';
  if (fraction.length > maximumPlaces) {
    const next = fraction[maximumPlaces];
    let scaled = BigInt(whole) * (10n ** BigInt(maximumPlaces)) + BigInt(fraction.slice(0, maximumPlaces) || '0');
    if (next >= '5') scaled += 1n;
    const scale = 10n ** BigInt(maximumPlaces);
    const roundedWhole = scaled / scale;
    fraction = String(scaled % scale).padStart(maximumPlaces, '0');
    const trimmed = fraction.replace(/0+$/, '');
    if (scaled === 0n && BigInt(digits) > 0n) return `<0.${'0'.repeat(Math.max(0, maximumPlaces - 1))}1`;
    return `${negative ? '-' : ''}${commas(roundedWhole)}${trimmed ? `.${trimmed}` : ''}`;
  }
  fraction = fraction.replace(/0+$/, '');
  return `${negative ? '-' : ''}${commas(whole)}${fraction ? `.${fraction}` : ''}`;
}

function displayAmount(amount, qdayUnit, details = false) {
  const unit = amount.asset === 'BTC' ? '100000000' : qdayUnit;
  const places = details ? decimalsFromUnit(unit) : amount.asset === 'BTC' ? 8 : 4;
  return `${escapeHTML(formatUnits(amount.atomic, unit, places))} <span class="unit">${escapeHTML(amount.asset)}</span>`;
}

function formatRatio(numerator, denominator, places) {
  if (denominator === 0n) return '—';
  const scale = 10n ** BigInt(places);
  const scaled = (numerator * scale + denominator / 2n) / denominator;
  const whole = scaled / scale;
  const fraction = String(scaled % scale).padStart(places, '0').replace(/0+$/, '');
  return `${commas(whole)}${fraction ? `.${fraction}` : ''}`;
}

function price(order) {
  const give = BigInt(order.give.atomic);
  const receive = BigInt(order.receive.atomic);
  const qdayAtomic = order.give.asset === 'QDAY' ? give : receive;
  const satoshis = order.give.asset === 'BTC' ? give : receive;
  return formatRatio(satoshis * BigInt(order.qdayUnitAtomic), qdayAtomic * 100000000n, 16);
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
  const btcPerQDAY = formatRatio(
    BigInt(lastTrade.btcAtomic) * BigInt(lastTrade.qdayUnitAtomic),
    BigInt(lastTrade.qdayAtomic) * 100000000n,
    12
  );
  headerQDAYUSD.textContent = dollars(Number(btcPerQDAY) * bitcoinUSD);
  headerQDAYUSD.title = `Last matched trade · ${new Date(lastTrade.matchedAt * 1000).toLocaleString()}`;
}

function side(order) {
  return order.give.asset === 'QDAY' ? {className: 'sell', label: 'SELL QDAY'} : {className: 'buy', label: 'BUY QDAY'};
}

function statusBadge(status) {
  return `<span class="status ${escapeHTML(status)}">${escapeHTML(status.toUpperCase())}</span>`;
}

function copyButton(value) {
  return `<button class="copy-button" type="button" data-copy="${escapeHTML(value)}">COPY</button>`;
}

function orderRows(records, activity = false) {
  if (!records.length) return `<tr><td class="table-empty" colspan="${activity ? 8 : 7}">No ${activity ? 'orders have reached this relay yet' : 'open offers match this view'}.</td></tr>`;
  return records.map(record => {
    const terms = record.signed.order;
    const direction = side(terms);
    return `<tr data-href="/order/${escapeHTML(record.signed.id)}" tabindex="0">
      <td class="order-side" data-label="Side"><span class="side ${direction.className}">${direction.label}</span></td>
      ${activity ? `<td class="order-status" data-label="Status">${statusBadge(record.status)}</td>` : ''}
      <td class="order-id" data-label="Order"><a class="hash route-link" href="/order/${escapeHTML(record.signed.id)}" title="${escapeHTML(record.signed.id)}">${escapeHTML(record.signed.id)}</a></td>
      <td class="numeric" data-label="Price"><span class="price">${escapeHTML(price(terms))}</span> <span class="unit">BTC/QDAY</span></td>
      <td class="numeric" data-label="Maker gives"><span class="amount">${displayAmount(terms.give, terms.qdayUnitAtomic)}</span></td>
      <td class="numeric" data-label="Maker wants"><span class="amount">${displayAmount(terms.receive, terms.qdayUnitAtomic)}</span></td>
      <td class="order-expires" data-label="Expires" title="${escapeHTML(exactTime(terms.expiresAt))}">${escapeHTML(record.status === 'open' ? relativeFuture(terms.expiresAt) : relativePast(record.cancelledAt || terms.expiresAt))}</td>
      <td data-label="Proof"><span class="verified">SIGNED</span></td>
    </tr>`;
  }).join('');
}

function orderTable(records, activity = false) {
  return `<div class="table-scroll"><table class="order-table">
    <thead><tr><th>Side</th>${activity ? '<th>Status</th>' : ''}<th>Order ID</th><th class="numeric">Price</th><th class="numeric">Maker gives</th><th class="numeric">Maker wants</th><th>Expires</th><th>Proof</th></tr></thead>
    <tbody>${orderRows(records, activity)}</tbody>
  </table></div>`;
}

function pageNumbers(current, total) {
  if (total <= 1) return [];
  const pages = new Set([1, total, current - 1, current, current + 1]);
  return [...pages].filter(page => page >= 1 && page <= total).sort((a, b) => a - b);
}

function pagination(page, totalPages, basePath) {
  if (totalPages <= 1) return '';
  const pages = pageNumbers(page, totalPages);
  let previous = 0;
  const links = pages.map(number => {
    const gap = previous && number - previous > 1 ? '<span class="ellipsis">…</span>' : '';
    previous = number;
    return `${gap}${number === page ? `<span class="current">${number}</span>` : `<a class="route-link ${number !== 1 && number !== totalPages ? 'page-middle' : ''}" href="${basePath}?page=${number}">${number}</a>`}`;
  }).join('');
  return `<nav class="pagination" aria-label="Pagination">
    ${page > 1 ? `<a class="route-link" href="${basePath}?page=${page - 1}">← Previous</a>` : '<span class="disabled">← Previous</span>'}
    ${links}
    ${page < totalPages ? `<a class="route-link" href="${basePath}?page=${page + 1}">Next →</a>` : '<span class="disabled">Next →</span>'}
  </nav>`;
}

function metric(label, value, note) {
  return `<div class="metric"><span>${escapeHTML(label)}</span><strong>${escapeHTML(value)}</strong><small>${escapeHTML(note)}</small></div>`;
}

async function loadStatus() {
  try {
    statusCache = await api('/api/v1/status');
    relayPill.textContent = `${statusCache.network.toUpperCase()} LIVE`;
    relayPillBox.classList.remove('connecting', 'offline');
    headerOpen.textContent = commas(statusCache.stats.open);
    headerDay.textContent = commas(statusCache.stats.created24h);
    headerTotal.textContent = commas(statusCache.stats.total);
    lastTrade = statusCache.stats.lastTrade || null;
    refreshQDAYDollarPrice();
    return statusCache;
  } catch (error) {
    relayPill.textContent = 'RELAY OFFLINE';
    relayPillBox.classList.remove('connecting');
    relayPillBox.classList.add('offline');
    throw error;
  }
}

async function loadPrice() {
  try {
    const result = await api('/api/v1/price');
    const numeric = Number(result.usd);
    if (!Number.isFinite(numeric) || numeric <= 0) throw new Error('invalid BTC/USD price');
    bitcoinUSD = numeric;
    headerBitcoinUSD.textContent = dollars(bitcoinUSD, true);
    headerBitcoinUSD.title = `${result.source} · ${new Date(result.updatedAt).toLocaleTimeString()}${result.stale ? ' · stale' : ''}`;
    refreshQDAYDollarPrice();
    return result;
  } catch (_) {
    if (!bitcoinUSD) headerBitcoinUSD.textContent = '—';
    return null;
  }
}

async function refreshPriceLoop() {
  await loadPrice();
  setTimeout(refreshPriceLoop, 20000);
}

function marketHeader() {
  return `<section class="terminal-head">
    <div><span class="command">$ qday-swap orders --market QDAY/BTC</span>
      <h1>QDAY / BTC ORDER BOOK</h1>
    <p>Signed atomic swap offers. Keys and signing stay in the local app.</p>
    </div>
    <pre class="ascii-swap" aria-label="QDAY to Bitcoin atomic swap">+---------------------+       +---------------------+
| QDAY                |       | BITCOIN             |
| ED25519 + SLH-DSA   |&lt;-----&gt;| NATIVE SEGWIT HTLC |
+---------------------+ SHA256 +---------------------+</pre>
  </section>`;
}

async function renderMarket(token, silent = false) {
  const params = new URLSearchParams(location.search);
  const page = Math.max(1, Number(params.get('page') || 1));
  const query = new URLSearchParams({status: 'open', page: String(page), limit: '20'});
  if (directionFilter) query.set('giveAsset', directionFilter);
  const [status, orders] = await Promise.all([loadStatus(), api(`/api/v1/orders?${query}`)]);
  if (token !== routeVersion) return;
  const fingerprint = JSON.stringify({view: 'market', page, directionFilter, orders});
  if (silent && document.querySelector('#market-orders')) {
    if (fingerprint !== viewFingerprint) {
      document.querySelector('#market-orders tbody').innerHTML = orderRows(orders.items);
      document.querySelector('#market-pagination').innerHTML = pagination(orders.page, orders.totalPages, '/orders');
      document.querySelectorAll('[data-filter]').forEach(button => button.classList.toggle('active', button.dataset.filter === directionFilter));
      viewFingerprint = fingerprint;
    }
    const updated = document.querySelector('.toolbar .update');
    if (updated) updated.textContent = `UPDATED ${new Date().toLocaleTimeString()}`;
    return;
  }
  viewFingerprint = fingerprint;
  root.innerHTML = `${marketHeader()}
    <div class="toolbar"><strong>FILTER</strong>
      <button class="filter-button ${directionFilter === '' ? 'active' : ''}" data-filter="">ALL</button>
      <button class="filter-button ${directionFilter === 'BTC' ? 'active' : ''}" data-filter="BTC">BUY QDAY</button>
      <button class="filter-button ${directionFilter === 'QDAY' ? 'active' : ''}" data-filter="QDAY">SELL QDAY</button>
      <span class="update">UPDATED ${escapeHTML(new Date().toLocaleTimeString())}</span>
    </div>
    <section class="card" id="market-orders">${orderTable(orders.items)}</section>
    <div id="market-pagination">${pagination(orders.page, orders.totalPages, '/orders')}</div>`;
}

async function renderActivity(token, silent = false) {
  const params = new URLSearchParams(location.search);
  const page = Math.max(1, Number(params.get('page') || 1));
  const [orders] = await Promise.all([
    api(`/api/v1/orders?status=all&page=${page}&limit=20`),
    loadStatus().catch(() => null)
  ]);
  if (token !== routeVersion) return;
  const fingerprint = JSON.stringify({view: 'activity', page, orders});
  if (silent && document.querySelector('#activity-orders')) {
    if (fingerprint !== viewFingerprint) {
      document.querySelector('#activity-orders tbody').innerHTML = orderRows(orders.items, true);
      document.querySelector('#activity-pagination').innerHTML = pagination(orders.page, orders.totalPages, '/activity');
      const total = document.querySelector('#activity-total');
      if (total) total.textContent = `${commas(orders.total)} orders`;
      viewFingerprint = fingerprint;
    }
    return;
  }
  viewFingerprint = fingerprint;
  root.innerHTML = `<section class="page-header"><nav class="breadcrumbs"><a class="route-link" href="/orders">Order book</a><i>/</i><span>Activity</span></nav><h1>ORDER ACTIVITY.</h1><p>Open, cancelled and expired signed offers. Newest first.</p></section>
    <section class="card" id="activity-orders"><div class="card-header"><h2>Relay history</h2><span class="card-meta" id="activity-total">${commas(orders.total)} orders</span></div>${orderTable(orders.items, true)}</section>
    <div id="activity-pagination">${pagination(orders.page, orders.totalPages, '/activity')}</div>`;
}

function hexBytes(value) {
  if (!/^[0-9a-f]+$/.test(value) || value.length % 2) throw new Error('invalid hex');
  return Uint8Array.from(value.match(/../g), byte => parseInt(byte, 16));
}

function concatBytes(parts) {
  const length = parts.reduce((total, part) => total + part.length, 0);
  const result = new Uint8Array(length);
  let offset = 0;
  for (const part of parts) { result.set(part, offset); offset += part.length; }
  return result;
}

function u16(value) {
  const bytes = new Uint8Array(2);
  new DataView(bytes.buffer).setUint16(0, Number(value), false);
  return bytes;
}

function i64(value) {
  const bytes = new Uint8Array(8);
  new DataView(bytes.buffer).setBigUint64(0, BigInt(value), false);
  return bytes;
}

function textField(value) {
  const text = new TextEncoder().encode(value);
  const length = new Uint8Array(4);
  new DataView(length.buffer).setUint32(0, text.length, false);
  return concatBytes([length, text]);
}

function canonicalOrder(order) {
  return concatBytes([
    new TextEncoder().encode('QDAY_SWAP_ORDER_V1'), u16(order.version),
    textField(order.network), textField(order.market), textField(order.qdayUnitAtomic),
    textField(order.makerPublicKey), textField(order.makerMessageKey), textField(order.give.asset), textField(order.give.atomic),
    textField(order.receive.asset), textField(order.receive.atomic), i64(order.createdAt),
    i64(order.expiresAt), textField(order.nonce)
  ]);
}

function toHex(bytes) {
  return [...new Uint8Array(bytes)].map(byte => byte.toString(16).padStart(2, '0')).join('');
}

async function verifyInBrowser(signed) {
  const digest = await crypto.subtle.digest('SHA-256', canonicalOrder(signed.order));
  if (toHex(digest) !== signed.id) return false;
  const key = await crypto.subtle.importKey('raw', hexBytes(signed.order.makerPublicKey), {name: 'Ed25519'}, false, ['verify']);
  return crypto.subtle.verify({name: 'Ed25519'}, key, hexBytes(signed.signature), digest);
}

async function renderOrder(token, id) {
  const record = await api(`/api/v1/orders/${encodeURIComponent(id)}`);
  if (token !== routeVersion) return;
  const signed = record.signed;
  const terms = signed.order;
  const direction = side(terms);
  root.innerHTML = `<section class="page-header"><nav class="breadcrumbs"><a class="route-link" href="/orders">Order book</a><i>/</i><span>${escapeHTML(short(signed.id))}</span></nav><h1>${direction.label}.</h1><p>Created ${escapeHTML(exactTime(terms.createdAt))}. ${record.status === 'open' ? `Expires in ${escapeHTML(relativeFuture(terms.expiresAt))}.` : `Status: ${escapeHTML(record.status)}.`}</p></section>
    <section class="identifier"><div><span>Order ID</span><code>${escapeHTML(signed.id)}</code></div>${copyButton(signed.id)}</section>
    <div class="detail-grid">
      <section class="card"><div class="card-header"><h2>Immutable terms</h2>${statusBadge(record.status)}</div><div class="detail-list">
        <div><span>Maker gives</span><strong>${displayAmount(terms.give, terms.qdayUnitAtomic, true)}</strong></div>
        <div><span>Maker receives</span><strong>${displayAmount(terms.receive, terms.qdayUnitAtomic, true)}</strong></div>
        <div><span>Price</span><strong>${escapeHTML(price(terms))} BTC / QDAY</strong></div>
        <div><span>Market</span><strong>${escapeHTML(terms.market)}</strong></div>
        <div><span>Network</span><strong>${escapeHTML(terms.network.toUpperCase())}</strong></div>
        <div><span>Expires</span><strong>${escapeHTML(exactTime(terms.expiresAt))}</strong></div>
      </div></section>
      <section class="card"><div class="card-header"><h2>Cryptographic proof</h2><span class="card-meta">ED25519</span></div><div class="signature-box">
        <div class="signature-state" id="signature-state"><i></i><span>VERIFYING IN THIS BROWSER</span></div>
        <p>The signature binds the exact assets, atomic amounts, direction, network, QDAY denomination and expiry to this maker key.</p>
        <span class="unit">MAKER PUBLIC KEY</span><code>${escapeHTML(terms.makerPublicKey)}</code>
        <span class="unit">SIGNATURE</span><code>${escapeHTML(signed.signature)}</code>
      </div></section>
    </div>
    <section class="action-box"><h2>OPEN THIS ORDER IN QDAY SWAP.</h2><p>The local application checks the signature, amounts and both chains before asking you to approve funding.</p>${record.status === 'open' ? `<a class="button" href="qdayswap://order/${escapeHTML(signed.id)}">OPEN ORDER</a>` : ''}</section>`;
  const signatureState = document.querySelector('#signature-state');
  try {
    const verified = await verifyInBrowser(signed);
    if (token !== routeVersion) return;
    signatureState.classList.add(verified ? 'good' : 'bad');
    signatureState.querySelector('span').textContent = verified ? 'SIGNATURE VERIFIED IN THIS BROWSER' : 'SIGNATURE VERIFICATION FAILED';
  } catch (_) {
    signatureState.querySelector('span').textContent = 'SERVER VERIFIED · BROWSER ED25519 UNAVAILABLE';
  }
}

function renderProtocol() {
  root.innerHTML = `<div class="protocol-page"><section class="protocol-download protocol-download-home">
      <div class="download-intro"><span class="command">$ install qday-swap</span><h1>DOWNLOAD QDAY SWAP. FUCK KYC</h1><p>This explorer shows the public order book. The swap itself runs in the QDAY Swap application on your computer. Public binaries are in final testing. When downloads return, create your local wallet, save your seed phrase and deposit funds to trade. Then create an offer or accept one from this order book.</p></div>
      <div class="download-grid">
        <div class="download-card pending">
          <svg class="os-icon" viewBox="0 0 64 64" aria-hidden="true"><path d="M7 11l22-3v22H7V11zm26-4l24-3v26H33V7zM7 34h22v22L7 53V34zm26 0h24v26l-24-3V34z"/></svg>
          <span><strong>WINDOWS</strong><small>X86 64 · ZIP</small></span><b>TESTING</b>
        </div>
        <div class="download-card pending">
          <svg class="os-icon linux-icon" viewBox="0 0 64 64" aria-hidden="true"><path class="tux-body" d="M32 4c-8 0-13 7-13 17 0 4-1 8-4 13-4 6-5 13-2 17 2 3 6 3 10 1 3 5 15 5 18 0 4 2 8 2 10-1 3-4 2-11-2-17-3-5-4-9-4-13C45 11 40 4 32 4z"/><ellipse class="tux-belly" cx="32" cy="39" rx="12" ry="15"/><ellipse class="tux-eye" cx="27" cy="18" rx="4" ry="5"/><ellipse class="tux-eye" cx="37" cy="18" rx="4" ry="5"/><circle class="tux-pupil" cx="28" cy="19" r="1.5"/><circle class="tux-pupil" cx="36" cy="19" r="1.5"/><path class="tux-beak" d="M26 23l6-4 6 4-6 5z"/><path class="tux-foot" d="M23 49c-7 1-11 5-9 8 2 2 9 1 14-2zm18 0c7 1 11 5 9 8-2 2-9 1-14-2z"/></svg>
          <span><strong>LINUX</strong><small>X86 64 · TAR.GZ</small></span><b>TESTING</b>
        </div>
        <div class="download-card pending">
          <svg class="os-icon linux-icon" viewBox="0 0 64 64" aria-hidden="true"><path class="tux-body" d="M32 4c-8 0-13 7-13 17 0 4-1 8-4 13-4 6-5 13-2 17 2 3 6 3 10 1 3 5 15 5 18 0 4 2 8 2 10-1 3-4 2-11-2-17-3-5-4-9-4-13C45 11 40 4 32 4z"/><ellipse class="tux-belly" cx="32" cy="39" rx="12" ry="15"/><ellipse class="tux-eye" cx="27" cy="18" rx="4" ry="5"/><ellipse class="tux-eye" cx="37" cy="18" rx="4" ry="5"/><circle class="tux-pupil" cx="28" cy="19" r="1.5"/><circle class="tux-pupil" cx="36" cy="19" r="1.5"/><path class="tux-beak" d="M26 23l6-4 6 4-6 5z"/><path class="tux-foot" d="M23 49c-7 1-11 5-9 8 2 2 9 1 14-2zm18 0c7 1 11 5 9 8-2 2-9 1-14-2z"/></svg>
          <span><strong>LINUX</strong><small>ARM64 · TAR.GZ</small></span><b>TESTING</b>
        </div>
        <div class="download-card pending">
          <svg class="os-icon apple-icon" viewBox="0 0 64 64" aria-hidden="true"><path d="M39 13c3-4 3-8 3-10-4 0-8 3-10 6-2 2-3 6-3 9 4 0 7-2 10-5zM49 35c0-8 7-12 7-12-4-6-10-7-13-7-6-1-11 4-14 4s-7-4-12-4C8 16 0 24 0 36c0 7 3 15 6 20 3 4 6 8 11 8 4 0 6-3 12-3s7 3 12 3 8-4 11-8c3-4 4-9 5-11-1 0-8-3-8-10z" transform="translate(4 -1) scale(.88)"/></svg>
          <span><strong>MACOS</strong><small>UNIVERSAL · ZIP</small></span><b>TESTING</b>
        </div>
      </div>
    </section>
    <section class="quickstart">
      <header><span class="command">$ qday-swap quickstart</span><h2>THE SWAP RUNS LOCALLY.</h2></header>
      <div class="quickstart-grid">
        <article><b>01</b><h3>OPEN THE APP.</h3><p>Extract the complete archive and run QDAY Swap. Your browser opens the local interface. Create a new wallet or import your existing 24 word recovery phrase.</p></article>
        <article><b>02</b><h3>LET IT SYNC.</h3><p>The app starts a validating QDAY node and a Bitcoin light client. The status bar shows both chains. You do not need to download the full Bitcoin blockchain.</p></article>
        <article><b>03</b><h3>FUND YOUR WALLET.</h3><p>Open Settings, copy your QDAY or Native SegWit Bitcoin receive address, and send the asset you want to trade. Both addresses belong to your local wallet. You control the keys, and they never leave your computer.</p></article>
        <article><b>04</b><h3>CREATE OR TAKE.</h3><p>Create a signed offer in the app, or open an offer from this website. Review the exact amounts and expiry locally before approving the swap.</p></article>
      </div>
    </section>
    <div class="protocol-art-space" aria-hidden="true"></div>
    <section class="page-header protocol-header"><h1>HYBRID POST QUANTUM ATOMIC SWAPS.</h1><p>QDAY and Bitcoin use different signature systems and share one SHA 256 hashlock.</p></section>
    <section class="protocol-grid">
      <article class="protocol-step"><b>01</b><h2>SIGN AN OFFER.</h2><p>The local application signs exact QDAY and Bitcoin atomic amounts, direction, expiry and network. The relay cannot edit a byte without breaking the signature.</p></article>
      <article class="protocol-step"><b>02</b><h2>LOCK ON BOTH CHAINS.</h2><p>Two local applications agree on the immutable terms and create independent hashlocked contracts. Private keys never reach this server.</p></article>
      <article class="protocol-step"><b>03</b><h2>CLAIM OR REFUND.</h2><p>A claim reveals one shared secret and completes the other side. If either peer disappears, both users retain a unilateral timed refund path.</p></article>
    </section>
    <div class="protocol-note"><strong>Hybrid means both sides are described honestly.</strong> QDAY spends require Ed25519 and SLH DSA. Bitcoin still uses secp256k1. The atomic protocol joins them without pretending Bitcoin is post quantum.</div></div>`;
}

function markNavigation(route) {
  document.querySelectorAll('[data-nav]').forEach(link => link.classList.toggle('active', link.dataset.nav === route));
}

async function route(silent = false) {
  clearTimeout(refreshTimer);
  const token = ++routeVersion;
  const pathname = location.pathname.replace(/\/+$/, '') || '/';
  if (!silent) {
    viewFingerprint = '';
    root.innerHTML = '<section class="loading-page"><p><b>$</b> loading signed orders<span class="terminal-cursor">_</span></p></section>';
  }
  try {
    if (pathname === '/orders') {
      markNavigation('market');
      await renderMarket(token, silent);
      refreshTimer = setTimeout(() => { if (location.pathname === '/orders') route(true); }, 20000);
    } else if (pathname === '/activity') {
      markNavigation('activity');
      await renderActivity(token, silent);
      refreshTimer = setTimeout(() => { if (location.pathname === '/activity') route(true); }, 20000);
    } else if (pathname === '/' || pathname === '/protocol') {
      markNavigation('protocol');
      renderProtocol();
      await loadStatus().catch(() => {});
    } else if (pathname.startsWith('/order/')) {
      markNavigation('');
      await Promise.all([
        renderOrder(token, decodeURIComponent(pathname.slice('/order/'.length))),
        loadStatus().catch(() => {})
      ]);
    } else {
      throw new Error('Page not found');
    }
  } catch (error) {
    if (token !== routeVersion) return;
    root.innerHTML = `<section class="error-card"><h1>NO MARKET DATA.</h1><p>${escapeHTML(error.message)}</p></section>`;
  }
}

document.addEventListener('click', event => {
  const routeLink = event.target.closest('a.route-link');
  if (routeLink && routeLink.origin === location.origin) {
    event.preventDefault();
    history.pushState({}, '', routeLink.href);
    route();
    return;
  }
  const row = event.target.closest('tr[data-href]');
  if (row && !event.target.closest('a, button')) {
    history.pushState({}, '', row.dataset.href);
    route();
    return;
  }
  const filter = event.target.closest('[data-filter]');
  if (filter) {
    directionFilter = filter.dataset.filter;
    // Filtering belongs to the order-book route. Reset pagination without
    // sending the browser to the protocol home page.
    history.replaceState({}, '', '/orders');
    route(true);
    return;
  }
  const copy = event.target.closest('[data-copy]');
  if (copy) navigator.clipboard.writeText(copy.dataset.copy).then(() => showToast('Copied'));
});

document.addEventListener('keydown', event => {
  if ((event.key === 'Enter' || event.key === ' ') && event.target.matches('tr[data-href]')) {
    event.preventDefault();
    history.pushState({}, '', event.target.dataset.href);
    route();
  }
});

window.addEventListener('popstate', () => route());
refreshPriceLoop();
route();
