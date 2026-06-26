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

  function htmlEl(tag, attrs = {}, children = []) {
    const node = document.createElement(tag);
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

  let ttEl = null;
  function getTooltip() {
    if (!ttEl) {
      ttEl = document.createElement("div");
      ttEl.style.position = "absolute";
      ttEl.style.pointerEvents = "none";
      ttEl.style.background = "rgba(10, 11, 13, 0.95)";
      ttEl.style.border = "1px solid rgba(255,255,255,0.1)";
      ttEl.style.color = "#fff";
      ttEl.style.padding = "4px 8px";
      ttEl.style.borderRadius = "4px";
      ttEl.style.fontSize = "12px";
      ttEl.style.fontFamily = "var(--font-mono, monospace)";
      ttEl.style.zIndex = "9999";
      ttEl.style.transform = "translate(-50%, -100%)";
      ttEl.style.marginTop = "-8px";
      ttEl.style.opacity = "0";
      ttEl.style.transition = "opacity 0.1s";
      ttEl.style.whiteSpace = "pre";
      document.body.appendChild(ttEl);
    }
    return ttEl;
  }

  function bindTooltip(node, textGetter) {
    node.addEventListener("mouseenter", (e) => {
      const tt = getTooltip();
      const val = typeof textGetter === 'function' ? textGetter() : textGetter;
      if (typeof val === 'object' && val.html) {
        tt.innerHTML = val.html;
        tt.style.whiteSpace = "normal";
      } else {
        tt.textContent = val;
        tt.style.whiteSpace = "pre";
      }
      tt.style.opacity = "1";
    });
    node.addEventListener("mousemove", (e) => {
      const tt = getTooltip();
      tt.style.left = e.pageX + "px";
      tt.style.top = e.pageY + "px";
    });
    node.addEventListener("mouseleave", () => {
      if (ttEl) ttEl.style.opacity = "0";
    });
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

    // Data points (small dots) + tooltips.
    for (let i = 0; i < n; i++) {
      const [x, y] = pts[i];
      svg.appendChild(el("circle", {
        cx: x, cy: y, r: 2,
        fill: cssColor, stroke: "var(--bg, #0a0b0d)", "stroke-width": 1.5,
      }));
      // Invisible hover rect
      const hoverW = n > 1 ? innerW / (n - 1) : innerW;
      const hoverRect = el("rect", {
        x: x - hoverW / 2, y: padT, width: hoverW, height: innerH,
        fill: "transparent", cursor: "pointer"
      });
      bindTooltip(hoverRect, `${data[i].day}: ${fmtAxisValue(data[i].value)}`);
      svg.appendChild(hoverRect);
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
      if (n === 1) {
        const topY = yOf(cum[0]);
        const botY = yOf(bottom[0]);
        svg.appendChild(el("rect", {
          x: xOf(0) - 8, y: topY, width: 16, height: Math.max(1, botY - topY),
          fill, stroke: "none",
        }));
      } else {
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
    }

    // Total line on top.
    if (n === 1) {
      svg.appendChild(el("circle", {
        cx: xOf(0), cy: yOf(totals[0]), r: 3,
        fill: "#e4e4e7", stroke: "var(--bg, #0a0b0d)", "stroke-width": 1.5,
      }));
    } else {
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
    }

    // Invisible hover rects for tooltips.
    for (let i = 0; i < n; i++) {
      const hoverW = n > 1 ? innerW / (n - 1) : innerW;
      const hoverRect = el("rect", {
        x: xOf(i) - hoverW / 2, y: padT, width: hoverW, height: innerH,
        fill: "transparent", cursor: "pointer"
      });
      const tps = [data[i].day];
      for (const sr of [...(opts.series || [])].reverse()) {
        const v = Number(data[i][sr.key]) || 0;
        tps.push(`${sr.label}: ${fmtAxisValue(v)}`);
      }
      tps.push(`Total: ${fmtAxisValue(totals[i])}`);
      bindTooltip(hoverRect, tps.join("\n"));
      svg.appendChild(hoverRect);
    }

    host.appendChild(svg);
  }

  function renderStackedBars(host, opts) {
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
    const H = Math.max(rect.height, 200) - 60; // Leave space for legend
    const padL = 44, padR = 14, padT = 12, padB = 24;
    const innerW = W - padL - padR;
    const innerH = H - padT - padB;

    const n = data.length;
    
    // For bars, xOf is the center of the bar
    const xStep = innerW / Math.max(1, n);
    const barW = Math.max(2, Math.min(xStep * 0.8, 40)); // Max width 40px
    const xOf = (i) => padL + i * xStep + xStep / 2;

    const totals = data.map((p) =>
      (opts.series || []).reduce((s, sr) => s + (Number(p[sr.key]) || 0), 0));
    const yMax = Math.max(...totals, 1);
    const yOf = (v) => padT + innerH - (v / yMax) * innerH;

    const wrapper = htmlEl("div", { style: "display: flex; flex-direction: column; width: 100%; height: 100%;" });
    const chartContainer = htmlEl("div", { style: "flex-grow: 1; min-height: 0; position: relative;" });

    const svg = el("svg", {
      viewBox: `0 0 ${W} ${H}`, width: "100%", height: "100%", role: "img", preserveAspectRatio: "none"
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

    // Stacked bars.
    for (let i = 0; i < n; i++) {
      let currentY = padT + innerH; // Start from bottom
      for (const sr of opts.series || []) {
        const v = Number(data[i][sr.key]) || 0;
        if (v === 0) continue;
        const h = (v / yMax) * innerH;
        currentY -= h;
        
        svg.appendChild(el("rect", {
          x: xOf(i) - barW / 2, y: currentY, width: barW, height: Math.max(0.5, h),
          fill: sr.color, stroke: "none",
        }));
      }
      
      // Invisible hover rect for tooltip
      const hoverRect = el("rect", {
        x: xOf(i) - xStep / 2, y: padT, width: xStep, height: innerH,
        fill: "transparent", cursor: "pointer"
      });
      
      let html = `<div style="font-weight:bold; margin-bottom:6px; color:#fff; display:flex; justify-content:space-between; gap:12px;"><span>${data[i].day}</span> <span>${fmtAxisValue(totals[i])} tokens</span></div>`;
      for (const sr of [...(opts.series || [])].reverse()) {
        const v = Number(data[i][sr.key]) || 0;
        if (v > 0) {
           html += `<div style="display:flex; justify-content:space-between; align-items:center; font-size:12px; margin-bottom:4px; gap:16px;">
             <div style="display:flex; align-items:center; gap:6px;">
               <div style="width:4px; height:12px; border-radius:2px; background-color:${sr.color};"></div>
               <span style="color:var(--text-2); font-family:var(--font-mono, monospace);">${sr.label}</span>
             </div>
             <span style="color:#fff;">${v.toLocaleString()}</span>
           </div>`;
        }
      }
      
      bindTooltip(hoverRect, { html });
      svg.appendChild(hoverRect);
    }
    
    chartContainer.appendChild(svg);
    wrapper.appendChild(chartContainer);
    
    // Legend
    const legend = htmlEl("div", { 
      style: "display: grid; grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); gap: 8px 16px; padding: 12px 16px; border-top: 1px solid rgba(255,255,255,0.05); margin-top: 8px; overflow-y: auto; max-height: 80px;" 
    });
    
    for (const sr of opts.series || []) {
      const item = htmlEl("div", { style: "display: flex; align-items: center; gap: 8px; font-size: 12px; color: var(--text-2);" });
      item.appendChild(htmlEl("span", { style: `display: inline-block; width: 10px; height: 10px; border-radius: 50%; background-color: ${sr.color}; flex-shrink: 0;` }));
      item.appendChild(htmlEl("span", { style: "font-family: var(--font-mono, monospace); white-space: nowrap; overflow: hidden; text-overflow: ellipsis;" }, [document.createTextNode(sr.label)]));
      legend.appendChild(item);
    }
    
    wrapper.appendChild(legend);
    host.appendChild(wrapper);
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
    
    let actualTotal = data.reduce((sum, d) => sum + (d.value || 0), 0);
    let total = actualTotal === 0 ? 1 : actualTotal;

    const wrapper = htmlEl("div", {
      style: "display: flex; align-items: center; gap: 32px; width: 100%; justify-content: flex-start; padding: 0 16px;"
    });

    const chartContainer = htmlEl("div", {
      style: "position: relative; width: 200px; height: 200px; flex-shrink: 0;"
    });

    const svg = el("svg", {
      viewBox: `0 0 ${W} ${H}`, width: "100%", height: "100%", role: "img",
    });

    let cumulativePct = 0;
    const colors = ["#3b82f6", "#10b981", "#f59e0b", "#8b5cf6", "#ec4899", "#14b8a6", "#f43f5e", "#f97316"];
    
    data.forEach((d, i) => {
      const pct = (d.value || 0) / total;
      if (pct === 0) return;
      
      const dashArray = `${pct * 2 * Math.PI * r} ${2 * Math.PI * r}`;
      const dashOffset = -cumulativePct * 2 * Math.PI * r;
      
      const color = d.color || colors[i % colors.length];
      const slice = el("circle", {
        cx: cx, cy: cy, r: r,
        fill: "transparent",
        stroke: color,
        "stroke-width": strokeWidth,
        "stroke-dasharray": dashArray,
        "stroke-dashoffset": dashOffset,
        "transform": `rotate(-90 ${cx} ${cy})`,
        "stroke-linecap": pct > 0.05 ? "round" : "butt",
        cursor: "pointer",
        style: "transition: stroke-width 0.1s"
      });
      
      slice.addEventListener("mouseenter", () => slice.setAttribute("stroke-width", strokeWidth + 4));
      slice.addEventListener("mouseleave", () => slice.setAttribute("stroke-width", strokeWidth));
      
      const pctStr = (pct * 100).toFixed(1) + "%";
      const tooltipHtml = `
<div style="display:flex; align-items:center; gap:6px; margin-bottom:4px;">
  <div style="width:4px; height:12px; border-radius:2px; background-color:${color};"></div>
  <span style="color:var(--text-2); font-size:12px;">${d.label}</span>
</div>
<div style="font-size:12px; display:flex; align-items:center; gap:8px;">
  <span style="font-weight:bold; color:#fff;">${fmtAxisValue(d.value)} tokens</span>
  <span style="color:var(--text-2);">${pctStr}</span>
</div>
      `;
      bindTooltip(slice, { html: tooltipHtml });
      
      svg.appendChild(slice);
      
      cumulativePct += pct;
    });
    
    chartContainer.appendChild(svg);
    
    const centerText = htmlEl("div", {
      style: "position: absolute; top: 50%; left: 50%; transform: translate(-50%, -50%); text-align: center; pointer-events: none;"
    });
    centerText.appendChild(htmlEl("div", { style: "font-size: 20px; font-weight: 600; color: #fff;" }, [document.createTextNode(fmtAxisValue(actualTotal))]));
    centerText.appendChild(htmlEl("div", { style: "font-size: 12px; color: var(--text-2);" }, [document.createTextNode("tokens")]));
    chartContainer.appendChild(centerText);
    
    wrapper.appendChild(chartContainer);
    
    const legend = htmlEl("div", { style: "display: flex; flex-direction: column; gap: 12px; flex-grow: 1; max-width: 500px; max-height: 220px; overflow-y: auto; padding-right: 8px;" });
    data.forEach((d, i) => {
      if ((d.value || 0) === 0) return;
      const pct = ((d.value || 0) / total * 100).toFixed(1) + "%";
      
      const item = htmlEl("div", { style: "display: flex; flex-direction: column; gap: 4px; border-bottom: 1px solid rgba(255,255,255,0.05); padding-bottom: 12px;" });
      
      const topRow = htmlEl("div", { style: "display: flex; justify-content: space-between; align-items: center; font-size: 13px;" });
      const leftCol = htmlEl("div", { style: "display: flex; align-items: center; gap: 8px; color: #fff;" });
      leftCol.appendChild(htmlEl("span", { style: `display: inline-block; width: 10px; height: 10px; border-radius: 50%; background-color: ${d.color || colors[i % colors.length]};` }));
      leftCol.appendChild(htmlEl("span", { style: "font-family: var(--font-mono, monospace); font-weight: 600; word-break: break-all;" }, [document.createTextNode(d.label)]));
      
      topRow.appendChild(leftCol);
      topRow.appendChild(htmlEl("span", { style: "color: var(--text-2);" }, [document.createTextNode(pct)]));
      
      item.appendChild(topRow);
      
      const botRow = htmlEl("div", { style: "padding-left: 18px; font-size: 12px; color: var(--text-2);" });
      botRow.appendChild(document.createTextNode(`${fmtAxisValue(d.value)} tokens`));
      
      item.appendChild(botRow);
      legend.appendChild(item);
    });
    
    wrapper.appendChild(legend);
    host.appendChild(wrapper);
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
    const maxWeeks = 26; 
    
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
      map.set(d.day, d);
      if (d.tokens > maxValue) maxValue = d.tokens;
    });

    const days = [];
    for (let i = 0; i <= maxWeeks * 7; i++) {
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
        const val = map.get(dayStr) || { requests: 0, tokens: 0 };
        
        // Compute color intensity (0.1 to 1.0)
        let opacity = 0.05;
        if (val.tokens > 0) {
           opacity = 0.2 + (val.tokens / maxValue) * 0.8;
        }
        
        const rect = el("rect", {
          x, y, width: cellSize, height: cellSize, rx: 2,
          fill: `rgba(16, 185, 129, ${opacity})`, // emerald-500 base
          stroke: "rgba(255,255,255,0.05)",
          "stroke-width": 1,
          cursor: "pointer",
          style: "transition: transform 0.1s; transform-origin: center;"
        });
        
        rect.addEventListener("mouseenter", () => {
           rect.setAttribute("stroke", "rgba(255,255,255,0.3)");
        });
        rect.addEventListener("mouseleave", () => {
           rect.setAttribute("stroke", "rgba(255,255,255,0.05)");
        });
        bindTooltip(rect, `${dayStr}\nTokens: ${fmtAxisValue(val.tokens)}\nRequests: ${fmtAxisValue(val.requests)}`);
        
        svg.appendChild(rect);
      });
    });

    host.appendChild(svg);
  }

  window.npChart = {
    renderLine,
    renderStacked,
    renderStackedBars,
    renderHeatmap,
    renderDonut,
    setEmpty,
  };
})();