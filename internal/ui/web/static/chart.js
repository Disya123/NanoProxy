// Minimal SVG line chart — nano-proxy.
// Renders time-series data (one or more stacked series) as crisp SVG inside
// the host element. No external deps; designed for ~30 days of daily points.

(function () {
  const NS = "http://www.w3.org/2000/svg";

  function el(tag, attrs = {}, children = []) {
    const node = document.createElementNS(NS, tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (v === null || v === undefined) continue;
      node.setAttribute(k, String(v));
    }
    for (const c of children) {
      if (c) node.appendChild(c);
    }
    return node;
  }

  function clear(host) {
    while (host.firstChild) host.removeChild(host.firstChild);
    host.removeAttribute("data-empty");
  }

  function setEmpty(host, msg) {
    clear(host);
    host.setAttribute("data-empty", "1");
    host.textContent = msg || "No data";
  }

  // Compute nice y-axis ticks given [min, max].
  function niceTicks(min, max, count = 4) {
    if (max === min) {
      if (max === 0) return [0, 1];
      max = max * 1.2; min = min * 0.8;
    }
    const range = max - min;
    const step0 = range / count;
    const mag = Math.pow(10, Math.floor(Math.log10(step0)));
    const norm = step0 / mag;
    let step;
    if (norm < 1.5) step = 1 * mag;
    else if (norm < 3) step = 2 * mag;
    else if (norm < 7) step = 5 * mag;
    else step = 10 * mag;
    const tmin = Math.floor(min / step) * step;
    const tmax = Math.ceil(max / step) * step;
    const ticks = [];
    for (let v = tmin; v <= tmax + step / 2; v += step) ticks.push(v);
    return ticks;
  }

  function fmtAxisValue(v) {
    const abs = Math.abs(v);
    if (abs === 0) return "0";
    if (abs < 1) return v.toFixed(3);
    if (abs < 10) return v.toFixed(2);
    if (abs < 1000) return v.toFixed(0);
    if (abs < 1e6) return (v / 1e3).toFixed(1) + "k";
    return (v / 1e6).toFixed(1) + "M";
  }

  function fmtDayLabel(s) {
    // s = "YYYY-MM-DD"
    const parts = s.split("-");
    if (parts.length !== 3) return s;
    const months = ["Jan","Feb","Mar","Apr","May","Jun","Jul","Aug","Sep","Oct","Nov","Dec"];
    return months[parseInt(parts[1], 10) - 1] + " " + parseInt(parts[2], 10);
  }

  function buildPath(points) {
    if (!points.length) return "";
    let d = `M ${points[0][0].toFixed(2)} ${points[0][1].toFixed(2)}`;
    for (let i = 1; i < points.length; i++) {
      d += ` L ${points[i][0].toFixed(2)} ${points[i][1].toFixed(2)}`;
    }
    return d;
  }

  /**
   * Render a line/area chart.
   * host: HTMLElement to render into.
   * opts: {
   *   points: [{ day: "YYYY-MM-DD", value: number }, ...]
   *   area:   bool, fill under the line
   *   color:  CSS color for the line
   *   fill:   CSS color for the fill (default = color at 12% alpha)
   *   yLabel: optional formatter fn
   * }
   */
  function renderLine(host, opts) {
    if (!host) return;
    const data = (opts.points || []).filter((p) => p.value !== null && p.value !== undefined);
    if (data.length === 0) {
      setEmpty(host, "No data in this range yet.");
      return;
    }
    clear(host);

    const cssColor = opts.color || "#3b82f6";
    const cssFill  = opts.fill  || cssColor + "22"; // ~13% alpha
    const cssGrid  = "rgba(255,255,255,0.05)";
    const cssAxis  = "#71717a";
    const cssText  = "#a1a1aa";

    const rect = host.getBoundingClientRect();
    const W = Math.max(rect.width, 320);
    const H = Math.max(rect.height, 200);

    const padL = 44, padR = 14, padT = 12, padB = 24;
    const innerW = W - padL - padR;
    const innerH = H - padT - padB;

    const yMin = Math.min(0, ...data.map((p) => p.value));
    const yMax = Math.max(...data.map((p) => p.value));
    const ticks = niceTicks(yMin, yMax, 4);
    const y0 = ticks[0];
    const y1 = ticks[ticks.length - 1];
    const yRange = (y1 - y0) || 1;

    const n = data.length;
    const xStep = n > 1 ? innerW / (n - 1) : 0;
    const xOf = (i) => padL + i * xStep;
    const yOf = (v) => padT + innerH - ((v - y0) / yRange) * innerH;

    const svg = el("svg", {
      viewBox: `0 0 ${W} ${H}`,
      width: W,
      height: H,
      preserveAspectRatio: "none",
      role: "img",
    });

    // Grid + y-axis labels.
    for (const t of ticks) {
      const y = yOf(t);
      svg.appendChild(el("line", {
        x1: padL, x2: W - padR, y1: y, y2: y,
        stroke: cssGrid, "stroke-width": 1,
      }));
      svg.appendChild(el("text", {
        x: padL - 8, y: y + 3, "text-anchor": "end",
        "font-family": "var(--font-mono, monospace)",
        "font-size": 10, fill: cssText,
      }, [document.createTextNode(fmtAxisValue(t))]));
    }

    // X-axis labels: at most ~6 ticks.
    const xLabelEvery = Math.max(1, Math.floor(n / 6));
    for (let i = 0; i < n; i++) {
      if (i % xLabelEvery !== 0 && i !== n - 1) continue;
      const x = xOf(i);
      svg.appendChild(el("line", {
        x1: x, x2: x, y1: H - padB, y2: H - padB + 4,
        stroke: cssAxis, "stroke-width": 1,
      }));
      const txt = el("text", {
        x, y: H - padB + 16, "text-anchor": "middle",
        "font-family": "var(--font-mono, monospace)",
        "font-size": 10, fill: cssText,
      }, [document.createTextNode(fmtDayLabel(data[i].day))]);
      svg.appendChild(txt);
    }

    // Series line.
    const pts = data.map((p, i) => [xOf(i), yOf(p.value)]);
    if (opts.area !== false) {
      const fillPath = buildPath(pts) +
        ` L ${xOf(n - 1).toFixed(2)} ${(padT + innerH).toFixed(2)}` +
        ` L ${xOf(0).toFixed(2)} ${(padT + innerH).toFixed(2)} Z`;
      svg.appendChild(el("path", {
        d: fillPath, fill: cssFill, stroke: "none",
      }));
    }
    svg.appendChild(el("path", {
      d: buildPath(pts),
      fill: "none",
      stroke: cssColor,
      "stroke-width": 1.75,
      "stroke-linejoin": "round",
      "stroke-linecap": "round",
    }));

    // Data points (small dots).
    for (const [x, y] of pts) {
      svg.appendChild(el("circle", {
        cx: x, cy: y, r: 2,
        fill: cssColor, stroke: "var(--bg, #0a0b0d)", "stroke-width": 1.5,
      }));
    }

    host.appendChild(svg);
  }

  /**
   * Render a stacked area chart.
   * host: HTMLElement.
   * opts: {
   *   points: [{ day, input_tokens, output_tokens, cached_tokens }, ...]
   *   series: [{ key, label, color }]
   * }
   */
  function renderStacked(host, opts) {
    if (!host) return;
    const data = opts.points || [];
    if (data.length === 0) {
      setEmpty(host, "No data in this range yet.");
      return;
    }
    clear(host);

    const cssGrid = "rgba(255,255,255,0.05)";
    const cssAxis = "#71717a";
    const cssText = "#a1a1aa";

    const rect = host.getBoundingClientRect();
    const W = Math.max(rect.width, 320);
    const H = Math.max(rect.height, 200);
    const padL = 44, padR = 14, padT = 12, padB = 24;
    const innerW = W - padL - padR;
    const innerH = H - padT - padB;

    const n = data.length;
    const xOf = (i) => padL + (n > 1 ? (i * innerW) / (n - 1) : innerW / 2);
    const totals = data.map((p) =>
      (opts.series || []).reduce((s, sr) => s + (Number(p[sr.key]) || 0), 0));
    const yMax = Math.max(...totals, 1);
    const yOf = (v) => padT + innerH - (v / yMax) * innerH;

    const svg = el("svg", {
      viewBox: `0 0 ${W} ${H}`, width: W, height: H, role: "img",
    });

    // Y grid lines.
    const ticks = niceTicks(0, yMax, 4);
    for (const t of ticks) {
      const y = yOf(t);
      svg.appendChild(el("line", {
        x1: padL, x2: W - padR, y1: y, y2: y,
        stroke: cssGrid, "stroke-width": 1,
      }));
      svg.appendChild(el("text", {
        x: padL - 8, y: y + 3, "text-anchor": "end",
        "font-family": "var(--font-mono, monospace)",
        "font-size": 10, fill: cssText,
      }, [document.createTextNode(fmtAxisValue(t))]));
    }

    // X labels.
    const xLabelEvery = Math.max(1, Math.floor(n / 6));
    for (let i = 0; i < n; i++) {
      if (i % xLabelEvery !== 0 && i !== n - 1) continue;
      const x = xOf(i);
      svg.appendChild(el("line", {
        x1: x, x2: x, y1: H - padB, y2: H - padB + 4,
        stroke: cssAxis, "stroke-width": 1,
      }));
      svg.appendChild(el("text", {
        x, y: H - padB + 16, "text-anchor": "middle",
        "font-family": "var(--font-mono, monospace)",
        "font-size": 10, fill: cssText,
      }, [document.createTextNode(fmtDayLabel(data[i].day))]));
    }

    // Stacked areas.
    const cum = new Array(n).fill(0);
    for (const sr of (opts.series || [])) {
      const fill = sr.color + "33";
      const path = [];
      for (let i = 0; i < n; i++) {
        const v = Number(data[i][sr.key]) || 0;
        cum[i] += v;
      }
      // We compute forward then reverse.
      const fwd = data.map((_, i) => [xOf(i), yOf(cum[i])]);
      // Need cumulative from previous series; reset via subtract.
      const prev = new Array(n).fill(0);
      // Recompute from scratch — series order matters.
      // We rely on the caller providing series in stacking order (bottom → top).
      const bottom = cum.map((c, i) => c - (Number(data[i][sr.key]) || 0));
      const top = cum.slice();
      const fwdPath = fwd.map((p, i) => `${i === 0 ? "M" : "L"} ${p[0].toFixed(2)} ${p[1].toFixed(2)}`).join(" ");
      const revPath = bottom.map((_, i) => {
        const y = yOf(bottom[n - 1 - i]);
        return `L ${xOf(n - 1 - i).toFixed(2)} ${y.toFixed(2)}`;
      }).join(" ");
      svg.appendChild(el("path", {
        d: fwdPath + " " + revPath + " Z",
        fill, stroke: "none",
      }));
    }

    // Total line on top.
    const linePts = totals.map((t, i) => [xOf(i), yOf(t)]);
    svg.appendChild(el("path", {
      d: buildPath(linePts),
      fill: "none",
      stroke: "#e4e4e7",
      "stroke-width": 1.5,
      "stroke-linejoin": "round",
      "stroke-linecap": "round",
      "stroke-opacity": 0.85,
    }));

    host.appendChild(svg);
  }

  function renderDonut(host, opts) {
    if (!host) return;
    const data = opts.data || [];
    if (data.length === 0) {
      setEmpty(host, "No data");
      return;
    }
    clear(host);

    const W = 200;
    const H = 200;
    const cx = W / 2;
    const cy = H / 2;
    const r = 70;
    const strokeWidth = 30;
    
    let total = data.reduce((sum, d) => sum + (d.value || 0), 0);
    if (total === 0) total = 1;

    const svg = el("svg", {
      viewBox: `0 0 ${W} ${H}`, width: "100%", height: "100%", style: "max-height: 200px;", role: "img",
    });

    let cumulativePct = 0;
    const colors = ["#3b82f6", "#10b981", "#f59e0b", "#8b5cf6", "#ec4899", "#14b8a6", "#f43f5e", "#f97316"];
    
    data.forEach((d, i) => {
      const pct = (d.value || 0) / total;
      if (pct === 0) return;
      
      const dashArray = `${pct * 2 * Math.PI * r} ${2 * Math.PI * r}`;
      const dashOffset = -cumulativePct * 2 * Math.PI * r;
      
      svg.appendChild(el("circle", {
        cx: cx, cy: cy, r: r,
        fill: "transparent",
        stroke: d.color || colors[i % colors.length],
        "stroke-width": strokeWidth,
        "stroke-dasharray": dashArray,
        "stroke-dashoffset": dashOffset,
        "transform": `rotate(-90 ${cx} ${cy})`,
        "stroke-linecap": pct > 0.05 ? "round" : "butt"
      }, [
        el("title", {}, [document.createTextNode(`${d.label}: ${fmtAxisValue(d.value)}`)])
      ]));
      
      cumulativePct += pct;
    });
    
    host.appendChild(svg);
    
    // Add legend
    const legend = el("div", { style: "display: flex; flex-wrap: wrap; justify-content: center; gap: 8px; margin-top: 16px; font-size: 12px; color: var(--text-2);" });
    data.forEach((d, i) => {
      const item = el("div", { style: "display: flex; align-items: center; gap: 4px;" });
      item.appendChild(el("span", { style: `display: inline-block; width: 8px; height: 8px; border-radius: 50%; background-color: ${d.color || colors[i % colors.length]};` }));
      item.appendChild(el("span", {}, [document.createTextNode(d.label)]));
      legend.appendChild(item);
    });
    host.appendChild(legend);
  }

  function renderHeatmap(host, opts) {
    if (!host) return;
    const data = opts.points || [];
    if (data.length === 0) {
      setEmpty(host, "No data");
      return;
    }
    clear(host);

    const cellSize = 12;
    const cellGap = 3;
    const maxWeeks = 14; 
    
    const W = (cellSize + cellGap) * maxWeeks + 30; // 30px for Y-axis labels
    const H = (cellSize + cellGap) * 7 + 20; // 20px for X-axis labels
    
    const svg = el("svg", {
      viewBox: `0 0 ${W} ${H}`, width: "100%", height: "100%", style: "max-height: 180px;", role: "img",
    });

    const yLabels = ["Mon", "Wed", "Fri"];
    [0, 2, 4].forEach((y, i) => {
      svg.appendChild(el("text", {
        x: 0, y: (y + 1) * (cellSize + cellGap) + cellSize / 2 + 10,
        "font-family": "var(--font-mono, monospace)", "font-size": 10, fill: "#71717a",
      }, [document.createTextNode(yLabels[i])]));
    });

    const today = new Date();
    // Start maxWeeks ago
    const startDate = new Date(today);
    startDate.setDate(today.getDate() - maxWeeks * 7);
    
    let maxValue = 1;
    const map = new Map();
    data.forEach(d => {
      map.set(d.day, d.value);
      if (d.value > maxValue) maxValue = d.value;
    });

    const days = [];
    for (let i = 0; i < maxWeeks * 7; i++) {
      const d = new Date(startDate);
      d.setDate(startDate.getDate() + i);
      days.push(d);
    }
    
    // Group by week
    const weeks = [];
    let currentWeek = [];
    days.forEach(d => {
      currentWeek.push(d);
      if (d.getDay() === 0 || currentWeek.length === 7) { // Sunday
        weeks.push(currentWeek);
        currentWeek = [];
      }
    });
    if (currentWeek.length > 0) weeks.push(currentWeek);
    
    // Drop first week if partial to keep alignment, or just render it
    if (weeks.length > maxWeeks) weeks.shift();

    weeks.forEach((week, wIndex) => {
      const x = 30 + wIndex * (cellSize + cellGap);
      
      // X label for month changes
      if (week[0].getDate() <= 7) {
         const month = ["Jan","Feb","Mar","Apr","May","Jun","Jul","Aug","Sep","Oct","Nov","Dec"][week[0].getMonth()];
         svg.appendChild(el("text", {
           x: x, y: 10,
           "font-family": "var(--font-mono, monospace)", "font-size": 10, fill: "#71717a",
         }, [document.createTextNode(month)]));
      }

      week.forEach(d => {
        let dayOfWeek = d.getDay() - 1; // 0=Mon, 6=Sun
        if (dayOfWeek < 0) dayOfWeek = 6;
        
        const y = 20 + dayOfWeek * (cellSize + cellGap);
        const dayStr = d.toISOString().split('T')[0];
        const val = map.get(dayStr) || 0;
        
        // Compute color intensity (0.1 to 1.0)
        let opacity = 0.05;
        if (val > 0) {
           opacity = 0.2 + (val / maxValue) * 0.8;
        }
        
        svg.appendChild(el("rect", {
          x, y, width: cellSize, height: cellSize, rx: 2,
          fill: `rgba(16, 185, 129, ${opacity})`, // emerald-500 base
          stroke: "rgba(255,255,255,0.05)",
          "stroke-width": 1
        }, [
           el("title", {}, [document.createTextNode(`${dayStr}: ${val}`)])
        ]));
      });
    });

    host.appendChild(svg);
  }

  window.npChart = { renderLine, renderStacked, renderDonut, renderHeatmap, setEmpty };
})();