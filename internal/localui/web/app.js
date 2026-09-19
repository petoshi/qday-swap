const root = document.querySelector('#app');
const nav = document.querySelector('#main-nav');
const nodePill = document.querySelector('#node-pill');
const modalBackdrop = document.querySelector('#modal-backdrop');
const modal = document.querySelector('#modal');
const toast = document.querySelector('#toast');

let state;
let setupMode = 'new';
let toastTimer;
let refreshTimer;

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
  if (!state?.configured) {
    nodePill.classList.add('waiting');
    label.textContent = 'SETUP';
  } else if (state.error) {
    nodePill.classList.add('error');
    label.textContent = 'NODE ERROR';
  } else if (!state.qday?.synced) {
    nodePill.classList.add('waiting');
    label.textContent = state.qday ? `SYNC ${commas(state.qday.scanHeight)} / ${commas(state.qday.height)}` : 'STARTING';
  } else {
    label.textContent = `${state.qday.connections} PEERS · ${commas(state.qday.height)}`;
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
  root.innerHTML = `${state.error ? `<div class="alert"><strong>QDAY node needs attention</strong>${escapeHTML(state.error)}</div>` : ''}
    <section class="hero">
      <div><span class="eyebrow">QDAY ↔ BITCOIN</span><h1>SWAP WITHOUT PERMISSION.</h1><p>Signed public offers. Bitcoin Native SegWit contracts. QDAY contracts protected by Ed25519 and SLH DSA. Your keys remain here.</p></div>
      <div class="wallet-card"><div><span>Spendable QDAY</span><strong>${escapeHTML(commas(balance))}</strong><small>exact balance from your local node</small></div><div><span>Pending</span><strong>${escapeHTML(commas(pending))}</strong><small>unconfirmed QDAY</small></div></div>
    </section>
    <section class="metrics">
      ${metric('QDAY height', state.qday ? commas(state.qday.height) : 'STARTING', state.qday?.synced ? 'fully synchronized' : 'synchronizing')}
      ${metric('QDAY peers', state.qday?.connections ?? '0', 'local full node')}
      ${metric('Open offers', '0', 'signed relay orders')}
      ${metric('Active swaps', '0', 'automatic claim or refund')}
    </section>
    <section class="card"><div class="card-head"><h2>Open QDAY ↔ BTC offers</h2><span>DEX RELAY</span></div><div class="empty"><strong>NO OPEN OFFERS YET.</strong><p>Create the first signed offer or wait for another trader. The relay can publish terms. It cannot touch funds.</p></div></section>`;
}

function renderEmpty(title, copy, action = '') {
  root.innerHTML = `<section class="page-head"><div><span class="eyebrow">QDAY SWAP</span><h1>${escapeHTML(title)}</h1><p>${escapeHTML(copy)}</p></div>${action}</section><section class="card"><div class="empty"><strong>NOTHING HERE YET.</strong><p>Completed and recoverable swaps will stay in the local journal.</p></div></section>`;
}

function renderSettings() {
  root.innerHTML = `<section class="page-head"><div><span class="eyebrow">LOCAL APPLICATION</span><h1>SETTINGS.</h1><p>Keys stay encrypted on this computer. The QDAY node keeps validating the chain.</p></div></section>
    ${state.error ? `<div class="alert"><strong>Local node error</strong>${escapeHTML(state.error)}</div>` : ''}
    <section class="settings-grid">
      <article class="card setting"><h2>QDAY RECEIVE.</h2><p>Generate the wallet address used to fund swaps and ordinary QDAY transfers.</p><button class="secondary" id="receive-qday">SHOW ADDRESS</button></article>
      <article class="card setting"><h2>RECOVERY PHRASE.</h2><p>Reveal the 24 words that restore every chain branch and swap identity.</p><button class="secondary" id="show-recovery">REVEAL 24 WORDS</button></article>
      <article class="card setting"><h2>PUBLIC MARKET.</h2><p>${escapeHTML(state.relayURL || 'Relay is not configured')}</p><a class="secondary" href="${escapeHTML(state.relayURL || '#')}" target="_blank" rel="noreferrer">OPEN DEX RELAY</a></article>
      <article class="card setting"><h2>LOCK WALLET.</h2><p>Clear private material from memory while the QDAY node continues syncing.</p><button class="danger" id="lock-wallet">LOCK NOW</button></article>
    </section>`;
}

function render() {
  clearTimeout(refreshTimer);
  updateChrome();
  if (!state.configured) renderSetup();
  else if (!state.unlocked) renderUnlock();
  else {
    switch (route()) {
      case 'create': renderEmpty('CREATE OFFER.', 'Choose what you give, what you receive and the exact price.'); break;
      case 'swaps': renderEmpty('ACTIVE SWAPS.', 'Funding, confirmations, claims and refunds appear here.'); break;
      case 'history': renderEmpty('HISTORY.', 'Every completed or refunded swap remains auditable here.'); break;
      case 'settings': renderSettings(); break;
      default: renderMarket();
    }
  }
  refreshTimer = setTimeout(refreshState, state.configured ? 5000 : 15000);
}

async function refreshState() {
  try {
    state = await api('/api/v1/state');
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
  if (event.target.closest('#show-recovery')) {
    if (!confirm('Reveal the recovery phrase on this screen?')) return;
    try { recoveryModal((await post('/api/v1/recovery')).phrase); } catch (error) { showToast(error.message); }
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
});

window.addEventListener('hashchange', () => state && render());
modalBackdrop.addEventListener('click', event => { if (event.target === modalBackdrop) closeModal(); });
refreshState();
