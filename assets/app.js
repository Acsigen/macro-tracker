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
    legend: { type: "scroll", top: 0, textStyle: { color: color("paper") }, pageTextStyle: { color: color("muted") }, pageIconColor: color("cyan") },
    grid: { top: 48, right: 32, bottom: 16, left: 8, containLabel: true },
    xAxis: { type: "category", data: data.labels, axisLine: { lineStyle: { color: color("axis") } }, axisLabel: { color: color("muted"), hideOverlap: true, formatter: (value) => value.slice(5) } },
    yAxis: { type: "value", axisLine: { show: false }, splitLine: { lineStyle: { color: color("line") } }, axisLabel: { color: color("muted") } }
  };
}

function line(name, data, color, yAxisIndex = 0) {
  return { name, type: "line", data, yAxisIndex, symbol: "circle", symbolSize: 6, connectNulls: false, lineStyle: { width: 2, color }, itemStyle: { color } };
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
    const data = JSON.parse(element.dataset.chart);
    const option = baseChart(data);
    option.aria.label = {
      description: element.getAttribute("aria-label") || "Health history. Missing measurements are shown as gaps."
    };
    const color = (name) => getComputedStyle(document.documentElement).getPropertyValue(`--${name}`).trim();
    if (element.dataset.kind === "nutrition") {
      const carbohydrate = line("Carbohydrate", data.carbohydrate, color("cyan"));
      carbohydrate.markArea = { silent: true, itemStyle: { color: color("cyan-faint") }, data: [[{ name: "Carbohydrate band", yAxis: data.carbMin }, { yAxis: data.carbMax }]] };
      const freeSugar = line("Free sugar", data.freeSugar, color("sugar"));
      freeSugar.markLine = { silent: true, symbol: "none", data: [{ name: "Free sugar limit", yAxis: 25 }] };
      const protein = line("Protein", data.protein, color("gold"));
      protein.markArea = { silent: true, itemStyle: { color: color("gold-faint") }, data: [[{ name: "Protein band", yAxis: data.proteinMin }, { yAxis: data.proteinMax }]] };
      const fat = line("Fat", data.fat, color("orange"));
      fat.markArea = { silent: true, itemStyle: { color: color("orange-faint") }, data: [[{ name: "Fat band", yAxis: data.fatMin }, { yAxis: data.fatMax }]] };
      const fiber = line("Fiber", data.fiber, color("paper"));
      fiber.markLine = { silent: true, symbol: "none", data: [{ name: "Fiber minimum", yAxis: 25 }] };
      const salt = line("Salt", data.salt, color("axis"));
      salt.markLine = { silent: true, symbol: "none", data: [{ name: "Salt maximum", yAxis: 5 }] };
      option.series = [carbohydrate, freeSugar, protein, fat, fiber, salt];
    } else if (element.dataset.kind === "body") {
      option.yAxis = [option.yAxis, { type: "value", position: "right", splitLine: { show: false }, axisLabel: { color: color("muted") } }];
      const bmi = line("BMI", data.bmi, color("gold"));
      bmi.markLine = { silent: true, symbol: "none", data: [18.5, 25, 30].map((value) => ({ name: `BMI ${value}`, yAxis: value })) };
      const waist = line("Waist cm", data.waist, color("orange"), 1);
      if (data.waistTarget !== null) {
        waist.markArea = { silent: true, itemStyle: { color: color("orange-faint") }, data: [[{ name: "Optimal waist range", yAxis: data.waistTarget - 4 }, { yAxis: data.waistTarget + 4 }]] };
        waist.markLine = { silent: true, symbol: "none", data: [{ name: "Optimal waist", yAxis: data.waistTarget }] };
      }
      option.series = [line("Weight kg", data.weight, color("cyan")), bmi, waist];
    } else {
      option.series = [{ name: "Sleep hours", type: "bar", data: data.hours, itemStyle: { color: color("cyan"), borderRadius: [3, 3, 0, 0] } }];
    }
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
