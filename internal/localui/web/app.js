const root = document.querySelector('#app');
const nav = document.querySelector('#main-nav');
const nodePill = document.querySelector('#node-pill');
const headerQDAY = document.querySelector('#header-qday');
const headerBitcoin = document.querySelector('#header-bitcoin');
const headerSwaps = document.querySelector('#header-swaps');
const headerBitcoinUSD = document.querySelector('#header-btc-usd');
const headerQDAYUSD = document.querySelector('#header-qday-usd');
const modalBackdrop = document.querySelector('#modal-backdrop');
const modal = document.querySelector('#modal');
const toast = document.querySelector('#toast');

let state;
let orders = {items: [], page: 1, total: 0, totalPages: 0};
let negotiations = {pending: [], incoming: [], swaps: []};
let marketError = '';
let setupMode = 'new';
let toastTimer;
let refreshTimer;
let renderFingerprint = '';
let bitcoinUSD = 0;
let lastTrade = null;

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
  const scale = 100000000n;
  const scaled = (sats * BigInt(terms.qdayUnitAtomic) * scale) / (qday * 100000000n);
  return formatUnits(String(scaled), String(scale), 8);
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
  try {
    const result = await api('/api/v1/price');
    const numeric = Number(result.usd);
    if (Number.isFinite(numeric) && numeric > 0) {
      bitcoinUSD = numeric;
      headerBitcoinUSD.textContent = dollars(bitcoinUSD, true);
      headerBitcoinUSD.title = `${result.source} · ${new Date(result.updatedAt).toLocaleTimeString()}${result.stale ? ' · stale' : ''}`;
      refreshQDAYDollarPrice();
    }
  } catch (_) {
    if (!bitcoinUSD) headerBitcoinUSD.textContent = '—';
  }
  setTimeout(refreshPriceLoop, 20000);
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
  } else if (!state.qday?.synced) {
    nodePill.classList.add('waiting');
    label.textContent = state.qday ? `SYNC ${commas(state.qday.scanHeight)} / ${commas(state.qday.height)}` : 'STARTING';
  } else if (!state.bitcoin?.headersSynced || !state.bitcoin?.walletSynced) {
    nodePill.classList.add('waiting');
    label.textContent = state.bitcoin ? `BTC SYNC ${commas(state.bitcoin.walletHeight)} / ${commas(state.bitcoin.headerHeight)}` : 'BTC STARTING';
  } else {
    label.textContent = `${state.qday.connections} QDAY · ${state.bitcoin.peers} BTC`;
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

function renderMarket() {
  const balance = state.balance?.spendable?.qday ?? '0';
  const pending = state.balance?.pendingIn?.qday ?? '0';
  const bitcoin = state.bitcoinBalance?.confirmed?.btc ?? '0';
  const bitcoinPending = state.bitcoinBalance?.pending?.btc ?? '0';
  root.innerHTML = `${state.error ? `<div class="alert"><strong>Local chain needs attention</strong>${escapeHTML(state.error)}</div>` : ''}
    ${state.relayError || marketError ? `<div class="alert"><strong>DEX relay needs attention</strong>${escapeHTML(state.relayError || marketError)}</div>` : ''}
    <section class="terminal-head">
      <div><span class="command">$ qday-swap wallet --market QDAY/BTC</span><h1>LOCAL ORDER TERMINAL</h1><p>Wallets, keys and signatures stay on this computer.</p></div>
      <pre class="ascii-swap" aria-label="QDAY to Bitcoin atomic swap">+---------------------+       +---------------------+
| QDAY                |       | BITCOIN             |
| ED25519 + SLH-DSA   |&lt;-----&gt;| NATIVE SEGWIT HTLC |
+---------------------+ SHA256 +---------------------+</pre>
    </section>
    <section class="metrics">
      ${metric('Spendable QDAY', commas(balance), `${commas(pending)} pending`)}
      ${metric('Confirmed BTC', commas(bitcoin), `${commas(bitcoinPending)} pending`)}
      ${metric('Network peers', `${state.qday?.connections ?? 0} / ${state.bitcoin?.peers ?? 0}`, 'QDAY / Bitcoin')}
      ${metric('Open offers', state.relay?.stats?.open ?? orders.total ?? '0', 'signed orders')}
    </section>
    <section class="card"><div class="card-head"><h2>Open QDAY ↔ BTC offers</h2><span>${escapeHTML(orders.total)} SIGNED</span></div>${orderList(orders.items)}</section>`;
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

function renderCreate() {
  root.innerHTML = `<section class="page-head"><div><span class="eyebrow">SIGNED PUBLIC OFFER</span><h1>CREATE OFFER.</h1><p>Choose exact amounts. Another wallet can fill the whole offer once.</p></div></section>
    <section class="card offer-form-card"><form id="create-offer-form">
      <div class="offer-fields">
        <label class="field light"><span>You give</span><select name="giveAsset"><option value="QDAY">QDAY</option><option value="BTC">Bitcoin</option></select></label>
        <label class="field light"><span>Exact amount you give</span><input name="giveAmount" inputmode="decimal" placeholder="100" required></label>
        <label class="field light"><span>Exact amount you receive</span><input name="receiveAmount" inputmode="decimal" placeholder="0.001" required></label>
        <label class="field light"><span>Offer expires</span><select name="lifetimeMinutes"><option value="60">1 hour</option><option value="360">6 hours</option><option value="1440">24 hours</option><option value="10080">7 days</option></select></label>
      </div>
      <div class="review-note"><strong>One fill. Exact amounts.</strong><span>The offer is signed locally. Publishing it does not move funds. Contract funding still requires approval after a maker accepts a taker.</span></div>
      <p class="form-error dark" id="form-error" hidden></p><button class="primary" type="submit">SIGN AND PUBLISH OFFER</button>
    </form></section>`;
}

function negotiationTerms(record) {
  const terms = record.order.order;
  return `${amountText(terms.give, terms.qdayUnitAtomic)} → ${amountText(terms.receive, terms.qdayUnitAtomic)}`;
}

function renderSwaps(historyOnly = false) {
  const completed = new Set(['complete', 'refunded']);
  const swaps = negotiations.swaps.filter(swap => historyOnly ? completed.has(swap.phase) : !completed.has(swap.phase));
  if (historyOnly) {
    root.innerHTML = `<section class="page-head"><div><span class="eyebrow">LOCAL JOURNAL</span><h1>HISTORY.</h1><p>Completed and refunded swaps remain auditable on this computer.</p></div></section>${swapTable(swaps)}`;
    return;
  }
  root.innerHTML = `<section class="page-head"><div><span class="eyebrow">LOCAL JOURNAL</span><h1>ACTIVE SWAPS.</h1><p>Acceptances, matches and contract progress survive restarts.</p></div></section>
    ${negotiations.incoming.length ? `<section class="card"><div class="card-head"><h2>Waiting for your choice</h2><span>${negotiations.incoming.length}</span></div><div class="negotiation-list">${negotiations.incoming.map(item => `<article><div><strong>${escapeHTML(negotiationTerms(item))}</strong><small>Taker ${escapeHTML(short(item.acceptance.acceptance.takerPublicKey))}</small></div><button class="primary" data-match-acceptance="${escapeHTML(item.acceptance.id)}">MATCH TAKER</button></article>`).join('')}</div></section>` : ''}
    ${negotiations.pending.length ? `<section class="card"><div class="card-head"><h2>Waiting for maker</h2><span>${negotiations.pending.length}</span></div><div class="negotiation-list">${negotiations.pending.map(item => `<article><div><strong>${escapeHTML(negotiationTerms(item))}</strong><small>Acceptance expires ${escapeHTML(new Date(item.acceptance.acceptance.expiresAt * 1000).toLocaleString())}</small></div><span class="waiting-label">PENDING</span></article>`).join('')}</div></section>` : ''}
    ${swapTable(swaps)}`;
}

function swapTable(swaps) {
  if (!swaps.length) return `<section class="card"><div class="empty"><strong>NOTHING HERE YET.</strong><p>Matched swaps will appear here before either wallet broadcasts a contract.</p></div></section>`;
  return `<section class="card"><div class="card-head"><h2>Swaps</h2><span>${swaps.length}</span></div><div class="negotiation-list">${swaps.map(swap => `<article><div><strong>${escapeHTML(negotiationTerms(swap))}</strong><small>${escapeHTML(swap.role.toUpperCase())} · ${escapeHTML(swap.id)}</small></div><span class="phase">${escapeHTML(swap.phase.replaceAll('_', ' ').toUpperCase())}</span></article>`).join('')}</div></section>`;
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
      <article class="card setting"><h2>PUBLIC MARKET.</h2><p>${escapeHTML(state.relayURL || 'Relay is not configured')}</p><a class="secondary" href="${escapeHTML(state.relayURL || '#')}" target="_blank" rel="noreferrer">OPEN DEX RELAY</a></article>
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
  return JSON.stringify({current: 'market', orders, balance: state.balance, bitcoinBalance: state.bitcoinBalance, relay: state.relay, error: state.error, relayError: state.relayError, marketError});
}

function render() {
  clearTimeout(refreshTimer);
  updateChrome();
  const fingerprint = currentFingerprint();
  if (fingerprint !== renderFingerprint) {
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
  try {
    state = await api('/api/v1/state');
    marketError = '';
    if (state.configured && state.unlocked) {
      const [orderResult, negotiationResult] = await Promise.allSettled([
        api('/api/v1/orders?page=1'), api('/api/v1/negotiations')
      ]);
      if (orderResult.status === 'fulfilled') orders = orderResult.value;
      else marketError = orderResult.reason.message;
      if (negotiationResult.status === 'fulfilled') negotiations = negotiationResult.value;
      else marketError = marketError || negotiationResult.reason.message;
    }
    render();
  } catch (error) {
    root.innerHTML = `<div class="alert"><strong>Local application unavailable</strong>${escapeHTML(error.message)}</div>`;
    nodePill.classList.add('error');
    nodePill.querySelector('span').textContent = 'DISCONNECTED';
  }
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
    if (!confirm('Accept this exact signed offer? No funds move until contract funding is approved.')) return;
    accept.disabled = true;
    try { await post(`/api/v1/orders/${accept.dataset.acceptOrder}/accept`); location.hash = 'swaps'; renderFingerprint = ''; await refreshState(); } catch (error) { showToast(error.message); accept.disabled = false; }
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
    button.disabled = true; button.textContent = 'SIGNING AND PUBLISHING…';
    try {
      await post('/api/v1/orders', {
        giveAsset: form.elements.giveAsset.value,
        giveAmount: form.elements.giveAmount.value.trim(),
        receiveAmount: form.elements.receiveAmount.value.trim(),
        lifetimeMinutes: Number(form.elements.lifetimeMinutes.value)
      });
      form.reset(); location.hash = 'market'; renderFingerprint = ''; await refreshState(); showToast('Offer published');
    } catch (error) {
      errorBox.textContent = error.message; errorBox.hidden = false;
      button.disabled = false; button.textContent = 'SIGN AND PUBLISH OFFER';
    }
  }
});

window.addEventListener('hashchange', () => { renderFingerprint = ''; if (state) render(); });
modalBackdrop.addEventListener('click', event => { if (event.target === modalBackdrop) closeModal(); });
refreshPriceLoop();
refreshState();
