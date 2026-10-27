// The usage page's script. The page reads whole without it; it switches the language and the theme, fills
// in the page's own host where the configuration does not name one, switches the runtime tabs, copies,
// converts image addresses and shows the live status from /status.json.
// It only writes text and DOM nodes: the page enforces Trusted Types with no policy at all, so assigning
// markup throws straight away.

const root = document.documentElement;
const page = JSON.parse(document.body.dataset.page || '{}');
const ups = page.upstreams || [];
const $ = (sel, el = document) => el.querySelector(sel);
const $$ = (sel, el = document) => [...el.querySelectorAll(sel)];
let lang = root.lang.startsWith('zh') ? 'zh' : 'en';

// msg is one of the script's messages in the language in use, with {name} filled in.
function msg(key, vars = {}) {
  let s = page.messages?.[lang]?.[key] ?? page.messages?.en?.[key] ?? key;
  for (const [k, v] of Object.entries(vars)) s = s.split(`{${k}}`).join(String(v));
  return s;
}

// el creates an element: attributes (class, text, and the rest as attributes) and children.
function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class') e.className = v;
    else if (k === 'text') e.textContent = v;
    else e.setAttribute(k, v === true ? '' : String(v));
  }
  for (const c of children) if (c !== null && c !== undefined && c !== false) e.append(c);
  return e;
}

// ---- Hosts: the single-domain entrance's host is the page's own when the page is on it

const here = location.host;
const hereName = location.hostname.toLowerCase();
const single = page.single || null;
const currentUp = ups.find((u) => u.domain && u.domain === hereName) || null;
let singleHost = null;
if (single) {
  if (!single.hosts || single.hosts.length === 0) singleHost = currentUp ? null : here;
  else singleHost = single.hosts.includes(hereName) ? here : single.hosts[0];
}
if (singleHost) for (const s of $$('[data-single]')) s.textContent = singleHost;
if (location.protocol === 'http:') {
  for (const s of $$('[data-scheme]')) s.textContent = 'http';
  for (const s of $$('[data-http]')) s.hidden = false;
}

// mirrorOf is where an upstream is pulled from: its own host, or the single-domain host and its prefix.
function mirrorOf(u) {
  if (u.domain) return u.domain;
  return (singleHost || '<host>') + (u.root ? '' : `/${u.name}`);
}

for (const card of $$('.ol-card[data-upstream]')) {
  if (currentUp && card.dataset.upstream === currentUp.name) {
    card.setAttribute('data-current', '');
    const badge = $('[data-this-site]', card);
    if (badge) badge.hidden = false;
  }
}

// ---- Copying

function writeClipboard(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
  return new Promise((resolve, reject) => {
    const ta = el('textarea', { class: 'ol-sr-only', readonly: true });
    ta.value = text;
    document.body.append(ta);
    ta.select();
    try {
      document.execCommand('copy') ? resolve() : reject(new Error('copy'));
    } catch (err) {
      reject(err);
    } finally {
      ta.remove();
    }
  });
}

function wireCopy(button) {
  let timer;
  button.addEventListener('click', () => {
    const target = document.getElementById(button.dataset.copy || '');
    if (!target) return;
    writeClipboard((target.innerText || target.textContent || '').trim()).then(() => {
      const label = button.getAttribute('aria-label') || '';
      if (!button.dataset.label) button.dataset.label = label;
      $('[data-copy-icon]', button).hidden = true;
      $('[data-copied-icon]', button).hidden = false;
      button.setAttribute('aria-label', msg('copied'));
      clearTimeout(timer);
      timer = setTimeout(() => {
        $('[data-copy-icon]', button).hidden = false;
        $('[data-copied-icon]', button).hidden = true;
        button.setAttribute('aria-label', button.dataset.label);
        delete button.dataset.label;
      }, 1500);
    }, () => {});
  });
}

function copyButton(id) {
  const b = $('#tpl-copy').content.firstElementChild.cloneNode(true);
  b.dataset.copy = id;
  swapAttrs(b);
  wireCopy(b);
  return b;
}

for (const b of $$('button[data-copy]')) if (b.dataset.copy) wireCopy(b);

// ---- Language

// swapAttrs puts the Chinese or English text into the attributes that have both.
function swapAttrs(e) {
  for (const a of ['aria-label', 'placeholder', 'title']) {
    const zh = e.getAttribute(`data-zh-${a}`);
    if (zh === null) continue;
    if (!e.hasAttribute(`data-en-${a}`)) e.setAttribute(`data-en-${a}`, e.getAttribute(a) || '');
    e.setAttribute(a, lang === 'zh' ? zh : e.getAttribute(`data-en-${a}`));
  }
}

function setLang(l, save) {
  lang = l;
  root.lang = l === 'zh' ? 'zh-CN' : 'en';
  for (const e of $$('[data-zh-aria-label], [data-zh-placeholder], [data-zh-title]')) swapAttrs(e);
  for (const a of $$('#lang-menu [data-lang]')) {
    if (a.dataset.lang === l) a.setAttribute('aria-current', 'true');
    else a.removeAttribute('aria-current');
  }
  if (save) {
    const secure = location.protocol === 'https:' ? '; Secure' : '';
    document.cookie = `relay-lang=${l}; Path=/; Max-Age=31536000; SameSite=Lax${secure}`;
    const q = new URLSearchParams(location.search);
    if (q.has('lang')) {
      q.delete('lang');
      const s = q.toString();
      history.replaceState(null, '', location.pathname + (s ? `?${s}` : '') + location.hash);
    }
  }
  renderConvert();
  renderStatus();
}

for (const a of $$('#lang-menu [data-lang]')) {
  a.addEventListener('click', (ev) => {
    ev.preventDefault();
    setLang(a.dataset.lang === 'zh' ? 'zh' : 'en', true);
    $('#lang-menu').hidePopover?.();
  });
}

// ---- Theme: auto, light or dark, kept in this browser

function setTheme(t, save) {
  if (t === 'light' || t === 'dark') root.dataset.theme = t;
  else delete root.dataset.theme;
  for (const b of $$('button[data-set-theme]')) b.setAttribute('aria-pressed', String(b.dataset.setTheme === t));
  if (save) {
    try {
      localStorage.setItem('relay-theme', t);
    } catch {
      // storage blocked
    }
  }
}

for (const b of $$('button[data-set-theme]')) b.addEventListener('click', () => setTheme(b.dataset.setTheme, true));
setTheme(root.dataset.theme || 'auto', false);

// ---- Tabs: the ARIA tabs pattern, with the arrow keys

const tablist = $('[role="tablist"]');
if (tablist) {
  const tabs = $$('[role="tab"]', tablist);
  const select = (tab, focus) => {
    for (const t of tabs) {
      const on = t === tab;
      t.setAttribute('aria-selected', String(on));
      t.tabIndex = on ? 0 : -1;
      const panel = document.getElementById(t.getAttribute('aria-controls'));
      if (panel) panel.hidden = !on;
    }
    if (focus) tab.focus();
  };
  for (const t of tabs) {
    t.addEventListener('click', () => select(t, false));
    t.addEventListener('keydown', (ev) => {
      const i = tabs.indexOf(t);
      const to = { ArrowRight: i + 1, ArrowLeft: i - 1, Home: 0, End: tabs.length - 1 }[ev.key];
      if (to === undefined) return;
      ev.preventDefault();
      select(tabs[(to + tabs.length) % tabs.length], true);
    });
  }
  select(tabs.find((t) => t.getAttribute('aria-selected') === 'true') || tabs[0], false);
}

// ---- Section links follow the reader

const navLinks = $$('.ol-site-nav a[href^="#"]');
if ('IntersectionObserver' in window && navLinks.length) {
  const seen = new Map();
  const io = new IntersectionObserver(
    (entries) => {
      for (const e of entries) seen.set(e.target.id, e.isIntersecting);
      const first = navLinks.find((a) => seen.get(a.hash.slice(1)));
      for (const a of navLinks) {
        if (a === first) a.setAttribute('aria-current', 'true');
        else a.removeAttribute('aria-current');
      }
    },
    { rootMargin: '-30% 0px -60% 0px' },
  );
  for (const a of navLinks) {
    const s = document.getElementById(a.hash.slice(1));
    if (s) io.observe(s);
  }
}

// ---- Converting an image address (the rules of distribution/reference)

const OTHER_HUB_NAMES = ['registry.hub.docker.com', 'hub.docker.com'];
const EXAMPLES = ['nginx', 'nginx:1.27', 'bitnami/redis:7', 'ghcr.io/containerd/busybox:1.36', 'quay.io/coreos/etcd:v3.5.0',
  'registry.k8s.io/pause:3.10', 'gcr.io/distroless/static:nonroot', 'k8s.gcr.io/pause:3.2'];
const findUp = (name) => ups.find((u) => u.name === name) || null;
const registryList = () => ups.map((u) => u.label).join(msg('list'));

function parseRef(raw) {
  let s = raw.trim();
  if (!s) return { empty: true };
  if (/\s/.test(s)) {
    const parts = s.split(/\s+/); // a whole docker pull command pasted in
    s = parts[parts.length - 1];
  }
  s = s.replace(/^[a-z]+:\/\//i, '').replace(/\/+$/, '');
  const notes = [];
  let digest = '';
  const at = s.indexOf('@');
  if (at >= 0) {
    digest = s.slice(at + 1);
    s = s.slice(0, at);
    if (!/^[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[a-fA-F0-9]{32,}$/.test(digest)) return { error: msg('errDigest') };
  }
  let tag = '';
  const slash = s.lastIndexOf('/');
  const colon = s.lastIndexOf(':');
  if (colon > slash) {
    tag = s.slice(colon + 1);
    s = s.slice(0, colon);
    if (!/^[\w][\w.-]{0,127}$/.test(tag)) return { error: msg('errTag') };
  }
  let domain = 'docker.io';
  let path = s;
  const first = s.split('/')[0];
  if (s.includes('/') && (first.includes('.') || first.includes(':') || first === 'localhost')) {
    domain = first.toLowerCase();
    path = s.slice(first.length + 1);
  }
  let up = ups.find((u) => u.domain && u.domain === domain) || null;
  if (up) notes.push(msg('noteMirrorInput'));
  else if (singleHost && domain === singleHost.toLowerCase()) {
    notes.push(msg('noteMirrorInput'));
    const seg = path.split('/')[0];
    const named = ups.find((u) => u.name === seg || (u.aliases || []).includes(seg));
    if (named && path.includes('/')) {
      up = named;
      path = path.slice(seg.length + 1);
    } else {
      up = ups.find((u) => u.root) || null;
      if (!up) return { error: msg('errBad') };
    }
  }
  if (!up) {
    up = findUp(domain);
    if (!up) {
      up = ups.find((u) => (u.aliases || []).includes(domain)) || null;
      if (!up && OTHER_HUB_NAMES.includes(domain)) up = findUp('docker.io');
      if (up) notes.push(msg('noteAlias', { domain, label: up.label }));
    }
    if (!up && domain === 'k8s.gcr.io' && findUp('registry.k8s.io')) {
      up = findUp('registry.k8s.io');
      notes.push(msg('noteK8sGcr'));
    }
    if (!up && (/^[a-z]+\.gcr\.io$/.test(domain) || /\.pkg\.dev$/.test(domain)) && findUp('gcr.io')) {
      return { error: msg('errRegionalGcr', { domain }) };
    }
    if (!up) return { error: msg('errUnsupported', { domain, registries: registryList() }) };
  }
  if (!path) return { error: msg('errBad') };
  if (/[A-Z]/.test(path)) return { error: msg('errUpper') };
  if (!/^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:\/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$/.test(path)) return { error: msg('errBad') };
  if (up.library && !path.includes('/')) {
    path = `library/${path}`;
    notes.push(msg('noteLibrary'));
  }
  if (!tag && !digest) notes.push(msg('noteLatest'));
  return { up, path, tag, digest, notes };
}

const input = $('#convert-input');
const result = $('#convert-result');
let tool = 'docker';

function renderConvert() {
  if (!input || !result) return;
  const r = parseRef(input.value);
  if (r.empty) {
    result.replaceChildren();
    return;
  }
  if (r.error) {
    result.replaceChildren(el('div', { class: 'ol-alert ol-alert-warning', role: 'status' }, el('div', { class: 'ol-alert-body' }, el('p', { text: r.error }))));
    return;
  }
  const u = r.up;
  const suffix = (r.tag ? `:${r.tag}` : '') + (r.digest ? `@${r.digest}` : '');
  const mirror = `${mirrorOf(u)}/${r.path}${suffix}`;
  const original = `${u.name}/${r.path}${suffix}`;
  const short = u.library && r.path.startsWith('library/') ? r.path.slice(8) : u.name === 'docker.io' ? r.path : `${u.name}/${r.path}`;
  const local = tool === 'podman' ? `${u.name}/${r.path}` : short;
  const lines = [`${tool} pull ${mirror}`];
  if (!r.digest || r.tag) {
    lines.push(`${tool} tag ${mirror} ${local}:${r.tag || 'latest'}`);
    lines.push(`${tool} rmi ${mirror}`);
  }

  const grid = el('dl', { class: 'site-result-grid' },
    el('dt', { text: msg('outUpstream') }), el('dd', { class: 'ol-mono' }, original),
    el('dt', { text: msg('outMirror') }), el('dd', {}, el('span', { class: 'ol-mono', id: 'convert-mirror' }, mirror), copyButton('convert-mirror')));
  const notes = r.notes.map((n) => el('p', { class: 'ol-hint', text: n }));
  if (r.digest && !r.tag) notes.push(el('p', { class: 'ol-hint', text: msg('noteDigestOnly') }));

  const tools = el('fieldset', { class: 'ol-segmented' }, el('legend', { class: 'ol-sr-only', text: msg('tool') }));
  for (const x of ['docker', 'nerdctl', 'podman']) {
    const b = el('button', { type: 'button', 'aria-pressed': String(x === tool) }, el('span', { text: x }));
    b.addEventListener('click', () => {
      tool = x;
      renderConvert();
    });
    tools.append(b);
  }
  const codeBox = el('div', { class: 'ol-code' },
    el('div', { class: 'ol-code-label' }, msg('commands')),
    el('pre', {}, el('code', { id: 'convert-commands', text: lines.join('\n') })),
    copyButton('convert-commands'));

  let link = null;
  if (u.name === 'docker.io') {
    const name = r.path.startsWith('library/') ? `_/${r.path.slice(8)}` : `r/${r.path}`;
    link = el('a', { href: `https://hub.docker.com/${name}/tags`, target: '_blank', rel: 'noopener noreferrer', text: msg('hubLink') });
  } else if (u.name === 'quay.io') {
    link = el('a', { href: `https://quay.io/repository/${r.path}?tab=tags`, target: '_blank', rel: 'noopener noreferrer', text: msg('quayLink') });
  }
  result.replaceChildren(el('div', { class: 'ol-stack' }, grid, ...notes, tools, codeBox, link ? el('p', { class: 'ol-text-sm' }, link) : null));
}

if (input) {
  input.addEventListener('input', renderConvert);
  $('#convert-form').addEventListener('submit', (ev) => {
    ev.preventDefault();
    renderConvert();
  });
  const box = $('#convert-examples');
  for (const ex of EXAMPLES) {
    const r = parseRef(ex);
    if (r.error) continue;
    const b = el('button', { type: 'button', class: 'ol-chip ol-chip-mono' }, el('span', { class: 'ol-chip-label', text: ex }));
    b.addEventListener('click', () => {
      input.value = ex;
      renderConvert();
      input.focus();
    });
    box.append(b);
  }
  const pre = new URLSearchParams(location.search).get('image');
  if (pre) input.value = pre;
}

// ---- Status

let status = null;
const two = (n) => String(n).padStart(2, '0');
const pct = (v) => `${Math.round(v * 1000) / 10}%`;
const level = (v) => (v >= 0.99 ? 'ok' : v >= 0.9 ? 'degraded' : 'down');
const hourLabel = (start) => {
  const d = new Date(start * 1000);
  return `${two(d.getMonth() + 1)}-${two(d.getDate())} ${two(d.getHours())}:00`;
};
const timeLabel = (sec) => {
  const d = new Date(sec * 1000);
  return `${two(d.getMonth() + 1)}-${two(d.getDate())} ${two(d.getHours())}:${two(d.getMinutes())}`;
};

// dur writes seconds as the two largest units: "3 h 5 min".
function dur(sec) {
  const parts = [];
  let rest = Math.floor(sec);
  for (const [k, n] of [['d', 86400], ['h', 3600], ['m', 60], ['s', 1]]) {
    const v = Math.floor(rest / n);
    if (v > 0 || (k === 's' && !parts.length)) {
      parts.push(msg(`dur.${k}`, { n: v }));
      rest -= v * n;
    }
    if (parts.length === 2) break;
  }
  return parts.join(' ');
}

const TONE = { ok: 'success', degraded: 'warning', down: 'danger' };
const STATE_TONE = { ok: 'success', down: 'danger', open: 'danger' };

function stateText(u) {
  let st = msg(`state.${u.status}`);
  if (st === `state.${u.status}`) st = msg('state.unknown');
  if (u.reason) {
    const reason = /^http_\d+$/.test(u.reason) ? msg('reason.http', { code: u.reason.slice(5) }) : msg(`reason.${u.reason}`);
    st = msg('withReason', { state: st, reason: reason.startsWith('reason.') ? msg('reason.error') : reason });
  }
  return st;
}

function badge(text, tone) {
  return el('span', { class: `ol-badge${tone ? ` ol-badge-${tone}` : ''}` }, el('span', { class: 'ol-dot' }), text);
}

function strip(cells, label) {
  const s = el('div', { class: 'ol-uptime', role: 'img', 'aria-label': label });
  for (const c of cells) s.append(el('span', { class: c.level ? `ol-uptime-${c.level}` : null, title: c.title }));
  return s;
}

function renderStatus() {
  if (!status || !Array.isArray(status.upstreams)) return;
  for (const e of $$('[data-status-section], [data-status-link], [data-status-pill]')) e.hidden = false;
  const known = ['ok', 'degraded', 'down'].includes(status.status);
  const tone = known ? TONE[status.status] : '';
  for (const p of $$('[data-status-pill], [data-status-overall]')) {
    p.className = `ol-status${tone ? ` ol-status-${tone}` : ''}`;
    $('[data-status-text]', p).textContent = msg(`overall.${known ? status.status : 'unknown'}`);
  }

  const gen = Date.parse(status.generated) / 1000 || Date.now() / 1000;
  const started = Date.parse(status.started) / 1000 || gen;
  // Figures survive restarts when the operational database is writable; since is where they begin.
  const since = Date.parse(status.since) / 1000 || started;
  const checkedAt = Math.max(0, ...status.upstreams.map((u) => (u.checked ? Date.parse(u.checked) / 1000 : 0)));
  const meta = [];
  if (status.version) meta.push(msg('version', { v: /^\d/.test(status.version) ? `v${status.version}` : status.version }));
  meta.push(msg('uptime', { d: dur(Math.max(0, gen - started)) }));
  if (checkedAt) {
    const ago = Math.max(0, gen - checkedAt);
    meta.push(msg('checked', { ago: ago < 10 ? msg('justNow') : msg('ago', { d: dur(ago) }) }));
  }
  const metaEl = $('[data-status-meta]');
  if (metaEl) metaEl.textContent = meta.join(msg('sep'));

  // Upstreams in the page's order, others after them
  const rank = (name) => {
    const i = ups.findIndex((u) => u.name === name);
    return i < 0 ? ups.length : i;
  };
  const list = [...status.upstreams].sort((a, b) => rank(a.name) - rank(b.name) || a.name.localeCompare(b.name));
  const rows = [];
  for (const u of list) {
    const label = findUp(u.name)?.label || u.name;
    let ok = 0;
    let total = 0;
    const cells = (Array.isArray(u.hours) ? u.hours : []).map((h) => {
      ok += h.ok;
      total += h.total;
      const when = hourLabel(h.start);
      if (!h.total) return { title: msg('barNone', { time: when }) };
      return { level: level(h.ok / h.total), title: msg('barProbe', { time: when, pct: pct(h.ok / h.total), ok: h.ok, total: h.total }) };
    });
    const avail = total ? pct(ok / total) : '—';
    const latency = u.status === 'ok' && u.latency_ms ? msg('latency', { ms: u.latency_ms }) : '—';
    rows.push(el('tr', {},
      el('td', { class: 'ol-cell-title', text: label }),
      el('td', {}, badge(stateText(u), STATE_TONE[u.status])),
      el('td', { class: 'ol-num', text: latency }),
      el('td', {}, el('div', { class: 'site-strip' }, strip(cells, msg('strip', { name: label, pct: avail })))),
      el('td', { class: 'ol-num', text: avail })));

    // The upstream's card in the Mirrors section
    const card = $(`.ol-card[data-upstream="${CSS.escape(u.name)}"]`);
    const health = card && $('[data-health]', card);
    if (health) {
      health.className = `ol-card-meta site-health ol-tone-${STATE_TONE[u.status] || 'info'}`;
      $('[data-health-text]', health).textContent = u.status === 'ok' && u.latency_ms ? latency : stateText(u);
      health.hidden = false;
    }
  }

  // This site: the success rate per hour, the state from the latest hour with data
  const sh = Array.isArray(status.service?.hours) ? status.service.hours : [];
  const has = (h) => typeof h.value === 'number';
  const last = [...sh].reverse().find(has);
  const scells = sh.map((h) => {
    const when = hourLabel(h.start);
    return has(h) ? { level: level(h.value), title: msg('barService', { time: when, rate: pct(h.value) }) } : { title: msg('barServiceFew', { time: when }) };
  });
  rows.push(el('tr', {},
    el('td', { class: 'ol-cell-title', text: msg('service') }),
    el('td', {}, badge(last ? msg('serviceRate', { rate: pct(last.value) }) : msg('serviceFew'), last ? TONE[level(last.value)] : '')),
    el('td', { class: 'ol-num', text: '—' }),
    el('td', {}, el('div', { class: 'site-strip' }, strip(scells, msg('stripService')))),
    el('td', { class: 'ol-num', text: '—' })));
  $('[data-status-rows]')?.replaceChildren(...rows);

  const note = $('[data-status-note]');
  if (note) {
    let text = status.probe_interval > 0 ? msg('note', { interval: dur(status.probe_interval) }) : msg('noteNoProbe');
    // Say where the figures begin only while they cover less than the 24 hours shown.
    const windowStart = Math.floor(gen / 3600) * 3600 - 23 * 3600;
    if (since > windowStart) text += msg('space') + msg('since', { since: timeLabel(since) });
    note.textContent = text;
  }
}

function loadStatus() {
  if (!page.status || document.hidden) return;
  fetch('/status.json', { cache: 'no-store' })
    .then((r) => (r.ok ? r.json() : null))
    .then((j) => {
      status = j && typeof j === 'object' ? j : null;
      renderStatus();
    })
    .catch(() => {
      // status.json is optional
    });
}

setLang(lang, false);
loadStatus();
setInterval(loadStatus, 60000); // once a minute, not while the page is in the background
document.addEventListener('visibilitychange', () => {
  if (!document.hidden) loadStatus();
});
