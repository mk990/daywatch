/* Daywatch interactive status chart (Chart.js).
 *
 * renderStatusChart(canvasId, data, opts)
 *   data: [{t, from, to, ok, warn, err, other, d}]  (d = avg duration in µs)
 *   opts.drillUrl: base URL that receives ?from=&to= when a bar is clicked
 *   opts.drillParams: extra query-string (already encoded) to preserve filters
 */
function renderStatusChart(canvasId, data, opts) {
  const el = document.getElementById(canvasId);
  if (!el || !data.length || typeof Chart === 'undefined') return;

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
    datasets.push({
      label: 'Avg duration',
      type: 'line',
      data: data.map((p) => p.d),
      borderColor: C.accent,
      backgroundColor: C.accent,
      yAxisID: 'y2',
      tension: 0.35,
      pointRadius: 0,
      pointHoverRadius: 4,
      borderWidth: 2,
      order: 1,
    });
  }

  const chart = new Chart(el, {
    type: 'bar',
    data: { labels: data.map((p) => p.t), datasets },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      animation: { duration: 250 },
      interaction: { mode: 'index', intersect: false },
      onHover: (e, active) => { e.native.target.style.cursor = active.length ? 'pointer' : 'default'; },
      onClick: (e, active) => {
        if (!opts.drillUrl) return;
        const pts = chart.getElementsAtEventForMode(e, 'index', { intersect: false }, true);
        if (!pts.length) return;
        const p = data[pts[0].index];
        if (p.ok + p.warn + p.err + p.other === 0) return;
        let url = opts.drillUrl + '?from=' + p.from + '&to=' + p.to;
        if (opts.drillParams) url += '&' + opts.drillParams;
        window.location.href = url;
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
            title: { display: true, text: 'avg duration', color: C.accent },
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
              if (item.dataset.yAxisID === 'y2') return ' avg duration: ' + fmtDur(item.parsed.y);
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
