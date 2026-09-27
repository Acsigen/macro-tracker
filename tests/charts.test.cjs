const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

// Exercise application callbacks without a DOM package or vendored chart internals.
function harness(kind, data) {
  const handlers = new Map();
  const element = { isConnected: true, dataset: { kind, chart: JSON.stringify(data) }, getAttribute: () => 'Health history. Missing measurements are shown as gaps.' };
  const observers = [];
  const charts = [];
  const media = new Map();
  const context = {
    document: {
      documentElement: {},
      addEventListener: (event, fn) => handlers.set(event, fn),
      querySelectorAll: (selector) => !element.isConnected || selector.includes('[data-ready]') && !element.dataset.ready ? [] : [element],
    },
    getComputedStyle: () => ({ getPropertyValue: () => '#123456' }),
    matchMedia: (query) => {
      if (!media.has(query)) media.set(query, { matches: false, addEventListener: (_, fn) => { media.get(query).change = fn; } });
      return media.get(query);
    },
    ResizeObserver: class {
      constructor(fn) { this.fn = fn; this.active = false; observers.push(this); }
      observe() { this.active = true; }
      disconnect() { this.active = false; }
    },
    echarts: {
      init: () => {
        const chart = { disposed: false, setOption: (option) => { chart.option = option; }, resize: () => assert.equal(chart.disposed, false, 'resize called on disposed chart') };
        charts.push(chart);
        return chart;
      },
      dispose: () => { charts.at(-1).disposed = true; },
    },
  };
  context.window = context;
  vm.runInNewContext(fs.readFileSync('assets/app.js', 'utf8'), context);
  return { handlers, element, observers, charts, media };
}

test('chart preserves zero versus unknown and repeated settle does not initialize twice', () => {
  const h = harness('nutrition', { labels: ['a', 'b', 'c'], carbohydrate: [0, 1, 2], protein: [0, 1, 2], fiber: [0, 1, 2], salt: [0, 1, 2], freeSugar: [null, 0, 1] });
  h.handlers.get('DOMContentLoaded')();
  h.handlers.get('htmx:after:settle')();
  assert.equal(h.charts.length, 1);
  const series = h.charts[0].option.series.find((s) => s.name === 'Free sugar');
  assert.deepEqual(Array.from(series.data), [null, 0, 1]);
  assert.equal(series.connectNulls, false);
});

test('theme changes release the observer for the disposed chart', () => {
  const h = harness('sleep', { labels: ['a'], hours: [8] });
  h.handlers.get('DOMContentLoaded')();
  h.media.get('(prefers-color-scheme: light)').change();
  assert.equal(h.charts.length, 2);
  assert.equal(h.observers.filter((o) => o.active).length, 1, 'theme change retains the old ResizeObserver');
});

test('charts initialize after the bundled HTMX swap completes', () => {
  const h = harness('sleep', { labels: ['a'], hours: [8] });
  const vendor = fs.readFileSync('assets/htmx.min.js', 'utf8');
  // The bundled v4 source emits this event, unlike the old v2 camelCase event.
  assert.ok(vendor.includes('"htmx:after:settle"'));
  h.handlers.get('htmx:after:settle')?.();
  assert.equal(h.charts.length, 1, 'chart inserted by HTMX was never initialized');
});

test('body chart does not invent personal ranges when height is absent', () => {
  const h = harness('body', { labels: ['a'], weight: [80], bmi: [null], waist: [90], waistTarget: null });
  h.handlers.get('DOMContentLoaded')();
  const series = h.charts[0].option.series;
  assert.equal(series.find((s) => s.name === 'Waist cm').markArea, undefined);
  assert.equal(series.find((s) => s.name === 'BMI').data[0], null);
});

test('navigation inherits boost under the bundled HTMX defaults', () => {
  const html = fs.readFileSync('templates/pages.html', 'utf8');
  const bodyTag = html.match(/<body\b([^>]*)>/)[1];
  const attrs = new Map(Array.from(bodyTag.matchAll(/([\w:-]+)="([^"]*)"/g), m => [m[1], m[2]]));
  const body = {
    getAttribute: (name) => attrs.get(name) ?? null,
    hasAttribute: (name) => attrs.has(name),
    closest: (selector) => {
      const names = Array.from(selector.replaceAll('\\', '').matchAll(/\[([^\]]+)\]/g), m => m[1]);
      return names.some(name => attrs.has(name)) ? body : null;
    },
  };
  const link = { getAttribute: () => null, hasAttribute: () => false, parentNode: body };
  const context = {
    window: { location: { origin: 'http://localhost' } },
    document: { readyState: 'loading', addEventListener() {}, querySelector: () => null, adoptedStyleSheets: [] },
    XPathEvaluator: class { createExpression() { return {}; } },
    CSSStyleSheet: class { replaceSync() {} },
    CSS: { escape: (s) => s.replaceAll(':', '\\:') },
  };
  vm.runInNewContext(fs.readFileSync('assets/htmx.min.js', 'utf8'), context);
  let api;
  context.htmx.registerExtension('validation', { init(value) { api = value; } });
  assert.equal(api.attributeValue(link, 'hx-boost'), 'true', 'body boost does not reach navigation links');
});

test('missing sleep data is not announced as NaN', () => {
  const h = harness('sleep', { labels: ['2026-01-01'], hours: [null] });
  h.handlers.get('DOMContentLoaded')();
  // Use the actual bundled renderer and accessibility generator in SSR mode.
  const echarts = require('../assets/echarts.min.js');
  const attrs = {};
  const chart = echarts.init({ setAttribute: (key, value) => { attrs[key] = value; } }, null,
    { renderer: 'svg', ssr: true, width: 500, height: 300 });
  try {
    chart.setOption(h.charts[0].option);
    assert.ok(attrs['aria-label'], 'missing accessible chart description');
    assert.ok(!attrs['aria-label'].includes('NaN'), attrs['aria-label']);
  } finally {
    chart.dispose();
  }
});

test('chart removed by a swap releases its observer and instance', () => {
  const h = harness('sleep', { labels: ['a'], hours: [8] });
  h.handlers.get('DOMContentLoaded')();
  h.element.isConnected = false;
  h.handlers.get('htmx:after:settle')();
  assert.equal(h.charts[0].disposed, true);
  assert.equal(h.observers[0].active, false);
  assert.equal(h.element.dataset.ready, undefined);
});

test('HTMX cleanup of a parent releases its descendant chart', () => {
  const h = harness('sleep', { labels: ['a'], hours: [8] });
  h.handlers.get('DOMContentLoaded')();
  h.handlers.get('htmx:before:cleanup')({ target: { contains: element => element === h.element } });
  assert.equal(h.charts[0].disposed, true);
  assert.equal(h.observers[0].active, false);
});
