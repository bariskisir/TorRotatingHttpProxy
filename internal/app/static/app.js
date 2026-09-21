'use strict';
const $ = id => document.getElementById(id);
const number = value => new Intl.NumberFormat().format(value || 0);
const time = value => !value || value.startsWith('0001') ? '—' : new Date(value).toLocaleTimeString();
const put = (id, value) => { $(id).textContent = value; };
function element(tag, text, className) { const el = document.createElement(tag); if (text !== undefined) el.textContent = text; if (className) el.className = className; return el; }
let page = 1, ipRequest = 0, historyPending = false, lastHistoryRefresh = 0, lastRevision = -1, testInitialized = false;

function renderStatus(s) {
  put('ready', number(s.states.Ready)); put('busy', number(s.states.Busy));
  put('replenishing', number(s.instances.length - (s.states.Ready || 0) - (s.states.Busy || 0)));
  put('used', number(s.used_ips)); put('known', number(s.known_ips)); put('instance-count', number(s.instances.length));
  put('policy', s.unique_ip ? 'Unique IPs enforced' : 'IP reuse allowed');
  put('intro-sub', s.unique_ip ? 'Verified circuits. Request-by-request rotation.' : 'Verified circuits. Same addresses, round-robin.');
  put('footer-rotation', s.unique_ip ? 'HTTPS rotates when the tunnel closes.' : 'Reuse mode: circuits stay pinned until they die.');
  put('restart-policy', s.unique_ip ? `Fresh restart after ${s.max_used_ip_retries} consecutive used IPs` : 'Rotation stays enabled after every request');
  put('request-summary', `${number(s.completed)} completed · ${number(s.failed)} failed · ${number(s.rejected)} unavailable`);
  if (lastRevision !== s.revision) {
    const rows = s.instances.map(i => {
      const row = element('tr'); row.append(element('td', `tor-${String(i.id).padStart(2, '0')}`, 'mono'));
      const status = element('td'); status.append(element('span', i.state, `badge ${i.state.toLowerCase()}`)); row.append(status);
      row.append(element('td', i.ip || '—', 'mono'));
      const progress = element('td'); const progressWrap = element('div', undefined, 'bootstrap'); const bar = element('progress'); bar.max = 100; bar.value = i.bootstrap; bar.setAttribute('aria-label', `Bootstrap ${i.bootstrap}%`); progressWrap.append(bar, element('span', `${i.bootstrap}%`)); progress.append(progressWrap); row.append(progress);
      row.append(element('td', number(i.requests)), element('td', time(i.last_check)));
      const issue = element('td', i.last_error || '—', 'issue'); issue.title = i.last_error; row.append(issue); return row;
    });
    $('instances').replaceChildren(...rows);
    $('events').replaceChildren(...s.events.slice().reverse().map(event => {
      const li = element('li'); const detail = element('span'); detail.append(element('b', `tor-${event.instance}`), document.createTextNode(event.message)); li.append(element('time', time(event.time)), detail); return li;
    }));
    lastRevision = s.revision;
  }
  renderTest(s.test);
  if (Date.now() - lastHistoryRefresh > 2500) refreshHistory();
}

function renderTest(t) {
  if (!t) return;
  if (!testInitialized) { $('test-url').value = t.url; $('test-rps').value = t.target_rps; testInitialized = true; }
  const active = t.state === 'running' || t.state === 'stopping';
  put('test-state', t.state[0].toUpperCase() + t.state.slice(1)); $('test-state').className = `badge ${active ? 'busy' : ''}`;
  $('test-start').disabled = active; $('test-stop').disabled = t.state !== 'running';
  $('test-url').disabled = active; $('test-rps').disabled = active;
  if (active) { $('test-url').value = t.url; $('test-rps').value = t.target_rps; }
  put('test-success', number(t.success)); put('test-failed', number(t.failed)); put('test-503', number(t.unavailable));
  put('test-average', t.success + t.failed ? `${t.average_ms.toFixed(0)} ms` : '—');
  put('test-success-average', t.success ? `${t.success_average_ms.toFixed(0)} ms` : '—');
  put('test-actual', number(t.actual_rps));
  put('test-details', `${number(t.sent)} sent · ${number(t.in_flight)} in flight · ${number(t.canceled)} canceled · ${number(t.skipped)} skipped`);
  put('test-error', t.last_error ? `Last failure: ${t.last_error}` : '');
}

async function refreshHistory(force = false) {
  if (historyPending && !force) return;
  historyPending = true; lastHistoryRefresh = Date.now(); const request = ++ipRequest;
  try {
    const query = new URLSearchParams({page, page_size: 10, search: $('ip-search').value, used: $('ip-filter').value});
    const response = await fetch(`/api/ips?${query}`); if (!response.ok) throw new Error('IP history is temporarily unavailable.');
    const data = await response.json(); if (request !== ipRequest) return;
    if (page > 1 && !data.items.length) { page = Math.max(1, Math.ceil(data.total / 10)); historyPending = false; return refreshHistory(true); }
    const rows = data.items.map(ip => { const row = element('tr'); row.append(element('td', ip.ip, 'mono')); const usage = element('td'); usage.append(element('span', ip.used ? 'Used' : 'Not used', `badge ${ip.used ? '' : 'ready'}`)); row.append(usage); return row; });
    if (!rows.length) { const row = element('tr'); const cell = element('td', 'No matching addresses.', 'empty'); cell.colSpan = 2; row.append(cell); rows.push(row); }
    $('ip-history').replaceChildren(...rows); put('page-label', `${number(data.total)} addresses · Page ${page} of ${Math.max(1, Math.ceil(data.total / 10))}`);
    $('previous').disabled = page === 1; $('next').disabled = page * 10 >= data.total;
  } catch (err) { if (request === ipRequest) put('page-label', err.message); }
  finally { if (request === ipRequest) historyPending = false; }
}

const events = new EventSource('/api/events');
events.addEventListener('status', event => { try { renderStatus(JSON.parse(event.data)); put('connection', 'Live'); $('connection').className = 'badge live'; } catch { put('connection', 'Update error'); } });
events.onerror = () => { put('connection', 'Reconnecting'); $('connection').className = 'badge connecting'; };
$('previous').onclick = () => { page--; refreshHistory(true); };
$('next').onclick = () => { page++; refreshHistory(true); };
let searchTimer;
$('ip-search').oninput = () => { clearTimeout(searchTimer); searchTimer = setTimeout(() => { page = 1; refreshHistory(true); }, 200); };
$('ip-filter').onchange = () => { page = 1; refreshHistory(true); };
async function testAction(action, body) {
  put('test-feedback', '');
  try {
    const response = await fetch(`/api/test/${action}`, {method: 'POST', headers: {'Content-Type': 'application/json'}, body: body ? JSON.stringify(body) : undefined});
    if (!response.ok) throw new Error((await response.text()).trim()); renderTest(await response.json());
  } catch (err) { put('test-feedback', err.message); }
}
$('test-form').onsubmit = event => { event.preventDefault(); testAction('start', {url: $('test-url').value, rps: Number($('test-rps').value)}); };
$('test-stop').onclick = () => testAction('stop');
