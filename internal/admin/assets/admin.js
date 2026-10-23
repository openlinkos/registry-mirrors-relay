// 管理后台的渐进增强。没有这个脚本时每个页面都能用。
// 它只写文本（textContent），从不写标记：页面开了 Trusted Types 且没有任何策略，写 HTML 会直接抛错。

const pad = (n) => String(n).padStart(2, '0');

// 按浏览器的时区显示时间。
function localTimes(root) {
  for (const el of root.querySelectorAll('time[datetime]')) {
    const d = new Date(el.dateTime);
    if (Number.isNaN(d.getTime())) continue;
    el.textContent = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
    el.title = d.toString();
  }
}

// 复制按钮：配置片段、新令牌、新口令。复制后短暂显示对勾和"已复制"。
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

// 删除这类操作先确认一次。服务端不依赖它。
function confirmForms() {
  for (const form of document.querySelectorAll('form[data-confirm]')) {
    form.addEventListener('submit', (ev) => {
      if (!window.confirm(form.dataset.confirm || '')) ev.preventDefault();
    });
  }
}

// 概览页的数字每 15 秒刷新一次（页面在后台时不刷新）。
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
          if (typeof v === 'string') el.textContent = v;
        }
      })
      .catch(() => {});
  };
  setInterval(tick, 15000);
}

localTimes(document);
copyButtons();
confirmForms();
liveValues();
