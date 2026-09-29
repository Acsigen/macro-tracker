document.addEventListener("change", (event) => {
  if (event.target.closest("[data-auto-submit]")) event.target.form.requestSubmit();
});

function baseChart(data) {
  const color = (name) => getComputedStyle(document.documentElement).getPropertyValue(`--${name}`).trim();
  return {
    animation: !matchMedia("(prefers-reduced-motion: reduce)").matches,
    animationDuration: 300,
    aria: { enabled: true },
    backgroundColor: "transparent",
    textStyle: { color: color("paper"), fontFamily: "Atkinson Hyperlegible" },
    tooltip: { trigger: "axis", backgroundColor: color("tooltip"), textStyle: { color: color("orbit") } },
    legend: { show: false },
    grid: { top: 16, right: 24, bottom: 16, left: 8, containLabel: true },
    xAxis: { type: "category", data: data.labels, axisLine: { lineStyle: { color: color("axis") } }, axisLabel: { color: color("muted"), hideOverlap: true, formatter: (value) => value.slice(5) } },
    yAxis: { type: "value", axisLine: { show: false }, splitLine: { lineStyle: { color: color("line") } }, axisLabel: { color: color("muted") } }
  };
}

function line(name, data, color, yAxisIndex = 0) {
  return { name, type: "line", data, yAxisIndex, symbol: "circle", symbolSize: 6, connectNulls: false, lineStyle: { width: 2, color }, itemStyle: { color } };
}

const metricSpecs = {
  carbohydrate: ["Carbohydrate", "carbohydrate", "cyan"],
  "total-sugar": ["Total sugar", "totalSugar", "sugar"],
  "free-sugar": ["Free sugar", "freeSugar", "sugar"],
  protein: ["Protein", "protein", "gold"],
  fat: ["Fat", "fat", "orange"],
  fiber: ["Fiber", "fiber", "paper"],
  salt: ["Salt", "salt", "axis"],
  weight: ["Weight kg", "weight", "cyan"],
  waist: ["Waist cm", "waist", "orange"],
  bmi: ["BMI", "bmi", "gold"]
};

function metricChart(kind, data, color) {
  const option = baseChart(data);
  if (kind === "sleep") {
    option.series = [{ name: "Sleep hours", type: "bar", data: data.sleep, itemStyle: { color: color("cyan"), borderRadius: [3, 3, 0, 0] } }];
    return option;
  }
  const [name, key, tone] = metricSpecs[kind];
  const series = line(name, data[key], color(tone));
  if (kind === "carbohydrate") series.markArea = { silent: true, itemStyle: { color: color("cyan-faint") }, data: [[{ name: "Carbohydrate band", yAxis: data.carbMin }, { yAxis: data.carbMax }]] };
  if (kind === "free-sugar") series.markLine = { silent: true, symbol: "none", data: [{ name: "Free sugar limit", yAxis: 25 }] };
  if (kind === "protein") series.markArea = { silent: true, itemStyle: { color: color("gold-faint") }, data: [[{ name: "Protein band", yAxis: data.proteinMin }, { yAxis: data.proteinMax }]] };
  if (kind === "fat") series.markArea = { silent: true, itemStyle: { color: color("orange-faint") }, data: [[{ name: "Fat band", yAxis: data.fatMin }, { yAxis: data.fatMax }]] };
  if (kind === "fiber") series.markLine = { silent: true, symbol: "none", data: [{ name: "Fiber minimum", yAxis: 25 }] };
  if (kind === "salt") series.markLine = { silent: true, symbol: "none", data: [{ name: "Salt maximum", yAxis: 5 }] };
  if (kind === "bmi") series.markLine = { silent: true, symbol: "none", data: [18.5, 25, 30].map((value) => ({ name: `BMI ${value}`, yAxis: value })) };
  if (kind === "waist" && data.waistTarget !== null) {
    series.markArea = { silent: true, itemStyle: { color: color("orange-faint") }, data: [[{ name: "Optimal waist range", yAxis: data.waistTarget - 4 }, { yAxis: data.waistTarget + 4 }]] };
    series.markLine = { silent: true, symbol: "none", data: [{ name: "Optimal waist", yAxis: data.waistTarget }] };
  }
  option.series = [series];
  return option;
}

function nutrientRatio(data, color) {
  const common = {
    animation: !matchMedia("(prefers-reduced-motion: reduce)").matches,
    animationDuration: 300,
    aria: { enabled: true },
    backgroundColor: "transparent",
    textStyle: { color: color("paper"), fontFamily: "Atkinson Hyperlegible" },
    tooltip: { trigger: "item", backgroundColor: color("tooltip"), textStyle: { color: color("orbit") }, valueFormatter: (value) => `${Number(value).toFixed(1)} g` }
  };
  const values = data.donut;
  if (values.fat === null) return { ...common, title: { text: "Fat data is incomplete", subtext: "Add fat values to show this ratio.", left: "center", top: "middle", textStyle: { color: color("paper") }, subtextStyle: { color: color("muted") } }, series: [] };
  const slices = [
    { name: "Carbohydrate", value: values.carbohydrate, itemStyle: { color: color("cyan") } },
    { name: "Total sugar", value: values.totalSugar, itemStyle: { color: color("sugar") } },
    { name: "Protein", value: values.protein, itemStyle: { color: color("gold") } },
    { name: "Fat", value: values.fat, itemStyle: { color: color("orange") } },
    { name: "Fiber", value: values.fiber, itemStyle: { color: color("paper") } }
  ];
  if (slices.every((slice) => slice.value === 0)) return { ...common, title: { text: "No food logged today", left: "center", top: "middle", textStyle: { color: color("paper") } }, series: [] };
  return {
    ...common,
    legend: { type: "scroll", bottom: 0, textStyle: { color: color("paper") } },
    series: [{ name: "Consumed grams", type: "pie", radius: ["52%", "76%"], center: ["50%", "44%"], avoidLabelOverlap: true, label: { color: color("paper"), formatter: "{b}\n{c} g" }, labelLine: { lineStyle: { color: color("axis") } }, data: slices }]
  };
}

const chartObservers = new Map();

function disposeChart(element) {
  chartObservers.get(element)?.disconnect();
  chartObservers.delete(element);
  echarts.dispose(element);
  delete element.dataset.ready;
}

function renderCharts() {
  for (const element of chartObservers.keys()) {
    if (!element.isConnected) disposeChart(element);
  }
  document.querySelectorAll(".chart").forEach((element) => {
    if (element.dataset.ready || !window.echarts) return;
    const raw = element.dataset.chart || element.closest?.("[data-chart]")?.dataset.chart;
    if (!raw) return;
    const data = JSON.parse(raw);
    const color = (name) => getComputedStyle(document.documentElement).getPropertyValue(`--${name}`).trim();
    const option = element.dataset.kind === "nutrient-ratio" ? nutrientRatio(data, color) : metricChart(element.dataset.kind, data, color);
    option.aria.label = {
      description: element.getAttribute("aria-label") || "Health history. Missing measurements are shown as gaps."
    };
    option.series.forEach((series) => {
      if (series.markLine) series.markLine.label = { color: color("muted"), textBorderWidth: 0, position: "insideEndTop" };
      if (series.markArea) series.markArea.label = { color: color("muted"), textBorderWidth: 0, position: "insideTop" };
    });
    const chart = echarts.init(element);
    chart.setOption(option);
    const observer = new ResizeObserver(() => chart.resize());
    observer.observe(element);
    chartObservers.set(element, observer);
    element.dataset.ready = "true";
  });
}

document.addEventListener("DOMContentLoaded", renderCharts);
document.addEventListener("htmx:after:settle", renderCharts);
document.addEventListener("htmx:before:cleanup", (event) => {
  for (const element of chartObservers.keys()) {
    if (event.target === element || event.target.contains(element)) disposeChart(element);
  }
});
matchMedia("(prefers-color-scheme: light)").addEventListener("change", () => {
  for (const element of chartObservers.keys()) disposeChart(element);
  renderCharts();
});
