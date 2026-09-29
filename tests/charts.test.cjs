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

test('each metric chart has one series and keeps its reference marker', () => {
  const data = { labels: ['a', 'b', 'c'], carbohydrate: [0, 1, 2], totalSugar: [0, 5, 10], freeSugar: [0, 4, 8], protein: [0, 1, 2], fat: [null, 0, 1], fiber: [0, 1, 2], salt: [0, 1, 2], weight: [null, 80, 79], waist: [null, 90, 89], bmi: [null, 24, 23], sleep: [null, 8, 7], carbMin: 20, carbMax: 30, proteinMin: 10, proteinMax: 20, fatMin: 33.3, fatMax: 66.7, waistTarget: 88 };
  for (const kind of ['carbohydrate', 'total-sugar', 'free-sugar', 'protein', 'fat', 'fiber', 'salt', 'weight', 'waist', 'bmi', 'sleep']) {
    const h = harness(kind, data);
    h.handlers.get('DOMContentLoaded')();
    assert.equal(h.charts[0].option.series.length, 1, kind);
  }
  const fat = harness('fat', data);
  fat.handlers.get('DOMContentLoaded')();
  assert.deepEqual(Array.from(fat.charts[0].option.series[0].data), [null, 0, 1]);
  assert.equal(fat.charts[0].option.series[0].connectNulls, false);
  assert.equal(fat.charts[0].option.series[0].markArea.data[0][0].yAxis, 33.3);
  const sugar = harness('free-sugar', data);
  sugar.handlers.get('DOMContentLoaded')();
  assert.equal(sugar.charts[0].option.series[0].markLine.data[0].yAxis, 25);
});

test('nutrient donut has five slices and honest empty states', () => {
  const complete = harness('nutrient-ratio', { donut: { carbohydrate: 60, totalSugar: 10, protein: 12, fat: 7, fiber: 10 } });
  complete.handlers.get('DOMContentLoaded')();
  assert.equal(complete.charts[0].option.series.length, 1);
  assert.equal(complete.charts[0].option.series[0].type, 'pie');
  assert.equal(complete.charts[0].option.series[0].data.length, 5);
  const empty = harness('nutrient-ratio', { donut: { carbohydrate: 0, totalSugar: 0, protein: 0, fat: 0, fiber: 0 } });
  empty.handlers.get('DOMContentLoaded')();
  assert.equal(empty.charts[0].option.series.length, 0);
  assert.equal(empty.charts[0].option.title.text, 'No food logged today');
  const unknown = harness('nutrient-ratio', { donut: { carbohydrate: 60, totalSugar: 10, protein: 12, fat: null, fiber: 10 } });
  unknown.handlers.get('DOMContentLoaded')();
  assert.equal(unknown.charts[0].option.series.length, 0);
  assert.equal(unknown.charts[0].option.title.text, 'Fat data is incomplete');
});

test('theme changes release the observer for the disposed chart', () => {
  const h = harness('sleep', { labels: ['a'], sleep: [8] });
  h.handlers.get('DOMContentLoaded')();
  h.media.get('(prefers-color-scheme: light)').change();
  assert.equal(h.charts.length, 2);
  assert.equal(h.observers.filter((o) => o.active).length, 1, 'theme change retains the old ResizeObserver');
});

test('charts initialize after the bundled HTMX swap completes', () => {
  const h = harness('sleep', { labels: ['a'], sleep: [8] });
  const vendor = fs.readFileSync('assets/htmx.min.js', 'utf8');
  // The bundled v4 source emits this event, unlike the old v2 camelCase event.
  assert.ok(vendor.includes('"htmx:after:settle"'));
  h.handlers.get('htmx:after:settle')?.();
  assert.equal(h.charts.length, 1, 'chart inserted by HTMX was never initialized');
});

test('waist chart does not invent a personal range when height is absent', () => {
  const h = harness('waist', { labels: ['a'], waist: [90], waistTarget: null });
  h.handlers.get('DOMContentLoaded')();
  assert.equal(h.charts[0].option.series[0].markArea, undefined);
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
  const h = harness('sleep', { labels: ['2026-01-01'], sleep: [null] });
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
  const h = harness('sleep', { labels: ['a'], sleep: [8] });
  h.handlers.get('DOMContentLoaded')();
  h.element.isConnected = false;
  h.handlers.get('htmx:after:settle')();
  assert.equal(h.charts[0].disposed, true);
  assert.equal(h.observers[0].active, false);
  assert.equal(h.element.dataset.ready, undefined);
});

test('HTMX cleanup of a parent releases its descendant chart', () => {
  const h = harness('sleep', { labels: ['a'], sleep: [8] });
  h.handlers.get('DOMContentLoaded')();
  h.handlers.get('htmx:before:cleanup')({ target: { contains: element => element === h.element } });
  assert.equal(h.charts[0].disposed, true);
  assert.equal(h.observers[0].active, false);
});
