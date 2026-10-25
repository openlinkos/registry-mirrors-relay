// Progressive enhancement for the admin console. Every page works without this script.
// It only writes text (textContent), never markup: pages enforce Trusted Types with no policy at all, so
// writing HTML throws straight away.

const pad = (n) => String(n).padStart(2, '0');

// Show times in the browser's time zone.
function localTimes(root) {
  for (const el of root.querySelectorAll('time[datetime]')) {
    const d = new Date(el.dateTime);
    if (Number.isNaN(d.getTime())) continue;
    el.textContent = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
    el.title = d.toString();
  }
}

// Copy buttons: configuration snippets, new tokens, new passwords. After copying, a tick and "Copied"
// are shown briefly.
function copyButtons() {
  if (!navigator.clipboard) return;
  for (const button of document.querySelectorAll('button[data-copy]')) {
    const target = document.getElementById(button.dataset.copy || '');
    if (!target) continue;
    const copyIcon = button.querySelector('[data-copy-icon]');
    const copiedIcon = button.querySelector('[data-copied-icon]');
    const label = button.querySelector('[data-copy-label]');
    const copied = button.dataset.copied || '';
    const before = { label: label ? label.textContent : '', aria: button.getAttribute('aria-label') };
    const show = (done) => {
      if (copyIcon) copyIcon.hidden = done;
      if (copiedIcon) copiedIcon.hidden = !done;
      if (label) label.textContent = done ? copied : before.label;
      if (before.aria !== null) {
        button.setAttribute('aria-label', done ? copied : before.aria);
        button.title = done ? copied : before.aria;
      }
    };
    let timer;
    button.hidden = false;
    button.addEventListener('click', () => {
      navigator.clipboard.writeText(target.textContent || '').then(
        () => {
          show(true);
          clearTimeout(timer);
          timer = setTimeout(() => show(false), 1500);
        },
        () => {},
      );
    });
  }
}

// Operations such as deletes ask for confirmation first: a form's data-confirm, or that of the button that
// submitted it (a reset next to a save). The server does not rely on this.
function confirmForms() {
  document.addEventListener('submit', (ev) => {
    const form = ev.target;
    if (!(form instanceof HTMLFormElement)) return;
    const text = ev.submitter?.dataset.confirm || form.dataset.confirm;
    if (text && !window.confirm(text)) ev.preventDefault();
  });
}

// The numbers on the overview page refresh every 15 seconds (not while the page is in the background).
function liveValues() {
  const els = document.querySelectorAll('[data-live]');
  if (els.length === 0) return;
  const tick = () => {
    if (document.hidden) return;
    fetch('/admin/live', { credentials: 'same-origin', headers: { Accept: 'application/json' } })
      .then((res) => (res.ok ? res.json() : Promise.reject(new Error(String(res.status)))))
      .then((data) => {
        for (const el of els) {
          const v = data[el.dataset.live];
          if (typeof v !== 'string' || v === el.textContent) continue;
          el.textContent = v;
          // A value that changed flashes once (the design system's .ol-flash)
          el.classList.remove('ol-flash');
          void el.offsetWidth;
          el.classList.add('ol-flash');
        }
      })
      .catch(() => {});
  };
  setInterval(tick, 15000);
}

// The flash is removed when it ends, so the next change can flash again.
document.addEventListener('animationend', (ev) => {
  if (ev.target instanceof Element && ev.animationName === 'ol-flash') ev.target.classList.remove('ol-flash');
});

localTimes(document);
copyButtons();
confirmForms();
liveValues();
