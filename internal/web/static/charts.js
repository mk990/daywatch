/* Daywatch interactive status chart (Chart.js).
 *
 * Charts are declared in markup so they can be (re)initialized after
 * live-reload swaps:
 *   <canvas data-chart="some-id"></canvas>
 *   <script type="application/json" data-chart-for="some-id">{"data":[...],"opts":{...}}</script>
 *
 * data: [{t, from, to, ok, warn, err, other, d}]  (d = avg duration in µs)
 * opts.drillUrl: base URL that receives ?from=&to= when a bar is clicked
 * opts.drillParams: extra query-string (already encoded) to preserve filters
 */
function initCharts(root) {
  if (typeof Chart === 'undefined') return;
  (root || document).querySelectorAll('script[data-chart-for]').forEach((tag) => {
    const el = document.querySelector('canvas[data-chart="' + tag.dataset.chartFor + '"]');
    if (!el) return;
    let payload;
    try { payload = JSON.parse(tag.textContent); } catch { return; }
    const existing = Chart.getChart(el);
    if (existing) existing.destroy();
    renderStatusChart(el, payload.data || [], payload.opts || {});
  });
}

document.addEventListener('DOMContentLoaded', function () { initCharts(); });

function renderStatusChart(el, data, opts) {
  if (!el || !data.length || typeof Chart === 'undefined') return;

  function drillTo(from, to) {
    if (!opts.drillUrl) return;
    let url = opts.drillUrl + '?from=' + from + '&to=' + to;
    if (opts.drillParams) url += '&' + opts.drillParams;
    window.location.href = url;
  }

  // Drag-to-zoom: drag across the chart to select a time span, release to
  // zoom into it. A short drag (<8px) falls through to the click handler.
  const sel = { start: null, now: null, justDragged: false };

  const dragZoomPlugin = {
    id: 'dwDragZoom',
    afterEvent(chart, args) {
      const e = args.event;
      if (!opts.drillUrl) return;
      if (e.type === 'mousedown') {
        const area = chart.chartArea;
        if (e.y >= area.top && e.y <= area.bottom) {
          sel.start = Math.min(Math.max(e.x, area.left), area.right);
          sel.now = sel.start;
          window.dwChartDragging = true; // pauses live-reload swaps
        }
      } else if (e.type === 'mousemove' && sel.start !== null) {
        sel.now = Math.min(Math.max(e.x, chart.chartArea.left), chart.chartArea.right);
        chart.draw();
      } else if (e.type === 'mouseup' && sel.start !== null) {
        const a = Math.min(sel.start, sel.now);
        const b = Math.max(sel.start, sel.now);
        sel.start = sel.now = null;
        window.dwChartDragging = false;
        chart.draw();
        if (b - a < 8) return; // treat as a plain click
        sel.justDragged = true;
        const x = chart.scales.x;
        let i0 = Math.round(x.getValueForPixel(a));
        let i1 = Math.round(x.getValueForPixel(b));
        i0 = Math.min(Math.max(i0, 0), data.length - 1);
        i1 = Math.min(Math.max(i1, 0), data.length - 1);
        if (i1 < i0) { const t = i0; i0 = i1; i1 = t; }
        drillTo(data[i0].from, data[i1].to);
      } else if (e.type === 'mouseout' && sel.start !== null) {
        sel.start = sel.now = null;
        window.dwChartDragging = false;
        chart.draw();
      }
    },
    afterDraw(chart) {
      if (sel.start === null || sel.now === null || sel.start === sel.now) return;
      const ctx = chart.ctx;
      const area = chart.chartArea;
      const a = Math.min(sel.start, sel.now);
      const b = Math.max(sel.start, sel.now);
      const accent = (getComputedStyle(document.documentElement).getPropertyValue('--accent') || '#e8a33d').trim();
      ctx.save();
      ctx.fillStyle = accent + '26'; // ~15% alpha
      ctx.fillRect(a, area.top, b - a, area.bottom - area.top);
      ctx.strokeStyle = accent;
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(a, area.top); ctx.lineTo(a, area.bottom);
      ctx.moveTo(b, area.top); ctx.lineTo(b, area.bottom);
      ctx.stroke();
      ctx.restore();
    },
  };

  const css = getComputedStyle(document.documentElement);
  const color = (name, fallback) => (css.getPropertyValue(name) || fallback).trim();
  const C = {
    ok: color('--ok', '#3dbb72'),
    warn: color('--warn', '#d9a534'),
    err: color('--err', '#e05252'),
    other: '#5a6378',
    accent: color('--accent', '#e8a33d'),
    grid: 'rgba(138, 146, 166, .12)',
    text: color('--muted', '#8a92a6'),
  };

  const fmtDur = (us) => {
    if (us >= 1e6) return (us / 1e6).toFixed(2) + 's';
    if (us >= 1e3) return (us / 1e3).toFixed(1) + 'ms';
    return Math.round(us) + 'µs';
  };

  const hasDuration = data.some((p) => p.d > 0);

  const datasets = [
    { label: opts.okLabel || 'OK', data: data.map((p) => p.ok), backgroundColor: C.ok, stack: 's', order: 2 },
    { label: opts.warnLabel || 'Warning', data: data.map((p) => p.warn), backgroundColor: C.warn, stack: 's', order: 2 },
    { label: opts.errLabel || 'Error', data: data.map((p) => p.err), backgroundColor: C.err, stack: 's', order: 2 },
  ];
  if (data.some((p) => p.other > 0)) {
    datasets.push({ label: 'Other', data: data.map((p) => p.other), backgroundColor: C.other, stack: 's', order: 2 });
  }
  if (hasDuration) {
    const durLine = (label, key, dash, alpha) => ({
      label,
      type: 'line',
      data: data.map((p) => p[key]),
      borderColor: alpha ? C.accent + alpha : C.accent,
      backgroundColor: alpha ? C.accent + alpha : C.accent,
      yAxisID: 'y2',
      tension: 0.35,
      pointRadius: 0,
      pointHoverRadius: 4,
      borderWidth: dash ? 1.5 : 2,
      borderDash: dash,
      order: 1,
    });
    datasets.push(durLine('Avg duration', 'd'));
    if (data.some((p) => p.p95 > 0)) datasets.push(durLine('P95', 'p95', [6, 4], 'b3'));
    if (data.some((p) => p.p99 > 0)) datasets.push(durLine('P99', 'p99', [2, 4], '73'));
  }

  const chart = new Chart(el, {
    type: 'bar',
    plugins: [dragZoomPlugin],
    data: { labels: data.map((p) => p.t), datasets },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      animation: { duration: 250 },
      interaction: { mode: 'index', intersect: false },
      events: ['mousedown', 'mousemove', 'mouseup', 'mouseout', 'click', 'touchstart', 'touchmove', 'touchend'],
      onHover: (e, active) => {
        e.native.target.style.cursor = sel.start !== null ? 'col-resize' : (active.length ? 'pointer' : 'crosshair');
      },
      onClick: (e, active) => {
        if (sel.justDragged) { sel.justDragged = false; return; }
        if (!opts.drillUrl) return;
        const pts = chart.getElementsAtEventForMode(e, 'index', { intersect: false }, true);
        if (!pts.length) return;
        const p = data[pts[0].index];
        if (p.ok + p.warn + p.err + p.other === 0) return;
        drillTo(p.from, p.to);
      },
      scales: {
        x: {
          stacked: true,
          grid: { display: false },
          ticks: { color: C.text, maxTicksLimit: 12, maxRotation: 0 },
        },
        y: {
          stacked: true,
          beginAtZero: true,
          grid: { color: C.grid },
          ticks: { color: C.text, precision: 0 },
          title: { display: true, text: 'events', color: C.text },
        },
        ...(hasDuration && {
          y2: {
            position: 'right',
            beginAtZero: true,
            grid: { drawOnChartArea: false },
            ticks: { color: C.accent, callback: (v) => fmtDur(v) },
            title: { display: true, text: 'duration', color: C.accent },
          },
        }),
      },
      plugins: {
        legend: {
          labels: { color: C.text, boxWidth: 12, boxHeight: 12, usePointStyle: true, pointStyle: 'rectRounded' },
        },
        tooltip: {
          backgroundColor: '#1f2330',
          borderColor: 'rgba(138, 146, 166, .25)',
          borderWidth: 1,
          titleColor: '#d5d9e4',
          bodyColor: '#d5d9e4',
          padding: 10,
          callbacks: {
            label: (item) => {
              if (item.dataset.yAxisID === 'y2') return ' ' + item.dataset.label.toLowerCase() + ': ' + fmtDur(item.parsed.y);
              return ' ' + item.dataset.label + ': ' + item.parsed.y;
            },
            footer: () => (opts.drillUrl ? 'Click to inspect these records' : ''),
          },
        },
      },
    },
  });
  return chart;
}
