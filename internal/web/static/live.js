/* Daywatch live reload.
 *
 * Listens on /events (SSE). When new records are ingested the server emits
 * an "update" event; we refetch the current page, swap <main> content in
 * place, and re-initialize charts — no full page reload, no scroll jump.
 *
 * The 🔴 LIVE pill in the top bar toggles it (state kept in localStorage).
 */
(function () {
  const KEY = 'daywatch-live';
  let enabled = localStorage.getItem(KEY) !== 'off';
  let dirty = false;
  let timer = null;
  let refreshing = false;

  function pill() { return document.getElementById('live-pill'); }

  function paint() {
    const el = pill();
    if (!el) return;
    el.classList.toggle('on', enabled);
    el.title = enabled ? 'Live updates on — click to pause' : 'Live updates paused — click to resume';
    el.innerHTML = enabled ? '<span class="live-dot"></span> LIVE' : '⏸ PAUSED';
  }

  function userIsBusy() {
    if (window.dwChartDragging) return true;
    const a = document.activeElement;
    if (a && (a.tagName === 'INPUT' || a.tagName === 'TEXTAREA' || a.tagName === 'SELECT')) return true;
    const sel = window.getSelection();
    return !!(sel && sel.type === 'Range');
  }

  async function refresh() {
    if (refreshing) { dirty = true; return; }
    if (!enabled || document.hidden || userIsBusy()) { dirty = true; return; }
    dirty = false;
    refreshing = true;
    try {
      const res = await fetch(window.location.href, { headers: { 'X-Live-Reload': '1' } });
      if (res.status === 401) { window.location.href = '/login'; return; }
      if (!res.ok) return;
      const html = await res.text();
      const doc = new DOMParser().parseFromString(html, 'text/html');
      const fresh = doc.querySelector('main.content');
      const current = document.querySelector('main.content');
      if (!fresh || !current) return;
      const scrollY = window.scrollY;
      current.replaceChildren(...fresh.childNodes);
      window.scrollTo(0, scrollY);
      paint(); // the swapped topbar contains a new pill
      initCharts(current);
      wirePill();
    } catch {
      // transient fetch failure; next event will retry
    } finally {
      refreshing = false;
      if (dirty) schedule();
    }
  }

  function schedule() {
    if (timer) return;
    timer = setTimeout(() => { timer = null; refresh(); }, 800);
  }

  function wirePill() {
    const el = pill();
    if (!el || el.dataset.wired) return;
    el.dataset.wired = '1';
    el.addEventListener('click', () => {
      enabled = !enabled;
      localStorage.setItem(KEY, enabled ? 'on' : 'off');
      paint();
      if (enabled && dirty) schedule();
    });
  }

  document.addEventListener('visibilitychange', () => {
    if (!document.hidden && dirty) schedule();
  });

  document.addEventListener('DOMContentLoaded', () => {
    paint();
    wirePill();
    const es = new EventSource('/events');
    es.addEventListener('update', schedule);
  });
})();
