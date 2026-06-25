// nano-proxy admin dashboard — front-end bootstrap.
// Lightweight vanilla JS; no framework, no build step.

const api = {
  async get(url) {
    const res = await fetch(url, { credentials: "same-origin" });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    return res.json();
  },
  async post(url, body) {
    const res = await fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: body ? JSON.stringify(body) : null,
    });
    if (!res.ok) {
      const text = await res.text();
      throw new Error(text || `HTTP ${res.status}`);
    }
    return res.json();
  },
  async patch(url, body) {
    const res = await fetch(url, {
      method: "PATCH",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: body ? JSON.stringify(body) : null,
    });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    return res.json();
  },
  async put(url, body) {
    const res = await fetch(url, {
      method: "PUT",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: body ? JSON.stringify(body) : null,
    });
    if (!res.ok) {
      const text = await res.text();
      throw new Error(text || `HTTP ${res.status}`);
    }
    return res.json();
  },
  async del(url) {
    const res = await fetch(url, {
      method: "DELETE",
      credentials: "same-origin",
    });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    return res.json();
  },
};

// ───────── Formatting (mirrors Go-side helpers) ─────────

const fmtUSD = (v) => {
  if (v == null || v === 0) return "$0.00";
  if (Math.abs(v) < 0.01) return "$" + v.toFixed(6);
  return "$" + v.toFixed(2);
};
const fmtNum = (v) => (v ?? 0).toLocaleString("en-US");
const fmtPct = (v) => (v == null ? "0%" : (v * 100).toFixed(1) + "%");
const fmtTime = (ms) => {
  const d = new Date(ms);
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
};
const statusClass = (code) =>
  code >= 500 ? "badge-bad" : code >= 400 ? "badge-warn" : "badge-ok";

// ───────── Range state ─────────

const state = {
  range: "24h",
  customFrom: null,
  customTo: null,
};

// Convert range token to API params (server expects ?from=YYYY-MM-DD&to=…).
function rangeParams() {
  if (state.range === "custom" && state.customFrom && state.customTo) {
    return `from=${state.customFrom}&to=${state.customTo}`;
  }
  const now = new Date();
  const to = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1);
  const from = new Date(to);
  if (state.range === "24h") from.setDate(to.getDate() - 1);
  else if (state.range === "7d") from.setDate(to.getDate() - 7);
  else from.setDate(to.getDate() - 30);
  const fmt = (d) => d.toISOString().slice(0, 10);
  return `from=${fmt(from)}&to=${fmt(now)}`;
}

// ───────── Page detection ─────────

document.addEventListener("DOMContentLoaded", async () => {
  const path = location.pathname;
  if (path.endsWith("/login")) return initLogin();
  // everything else requires auth — the server has already redirected if not.
  initCommon();

  if (path === "/admin/" || path === "/admin") return initDashboard();
  if (path.startsWith("/admin/requests")) return initRequests();
  if (path.startsWith("/admin/keys")) return initKeys();
  if (path.startsWith("/admin/settings")) return initSettings();
});

function initCommon() {
  document.querySelectorAll('[data-action="logout"]').forEach((b) => {
    b.addEventListener("click", async (e) => {
      e.preventDefault();
      try {
        await api.post("/admin/api/logout");
      } catch {}
      location.href = "/admin/login";
    });
  });
}

// ───────── Login ─────────

function initLogin() {
  const form = document.getElementById("login-form");
  const err = document.getElementById("login-error");
  if (!form) return;
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    err.hidden = true;
    const fd = new FormData(form);
    try {
      await api.post("/admin/api/login", { token: fd.get("token") });
      location.href = "/admin/";
    } catch (e) {
      err.textContent = "Sign-in failed. Check the token and try again.";
      err.hidden = false;
    }
  });
}

// ───────── Dashboard ─────────

async function initDashboard() {
  bindRangePicker();
  bindSeriesTabs();
  await Promise.all([
    loadSummary(),
    loadTopKeys(),
    loadTopModels(),
    loadRecent(),
    loadTimeSeries(),
  ]);
}

function bindSeriesTabs() {
  document.querySelectorAll(".chart-card .tab").forEach((b) =>
    b.addEventListener("click", () => {
      document.querySelectorAll(".chart-card .tab").forEach((x) => x.classList.remove("is-active"));
      b.classList.add("is-active");
      state.series = b.dataset.series;
      renderMainChart();
    }));
  state.series = "cost";
}

let seriesCache = [];

async function loadTimeSeries() {
  try {
    seriesCache = await api.get(`/admin/api/stats/timeseries?${rangeParams()}`);
  } catch (e) {
    seriesCache = [];
    console.error("timeseries:", e);
  }
  renderMainChart();
  renderHeatmapChart();
}

function renderHeatmapChart() {
  const host = document.getElementById("chart-heatmap");
  if (!host || !window.npChart) return;
  if (!seriesCache.length) {
    window.npChart.setEmpty(host, "No data");
    return;
  }
  // Let's use requests for the heatmap
  window.npChart.renderHeatmap(host, {
    points: seriesCache.map((p) => ({ day: p.day, value: p.requests })),
  });
}

function renderMainChart() {
  const host = document.getElementById("chart-main");
  if (!host || !window.npChart) return;
  if (!seriesCache.length) {
    window.npChart.setEmpty(host, "No data in this range yet.");
    return;
  }
  const which = state.series || "cost";
  if (which === "cost") {
    window.npChart.renderLine(host, {
      points: seriesCache.map((p) => ({ day: p.day, value: p.cost_usd })),
      color: "#3b82f6",
      area: true,
    });
  } else if (which === "requests") {
    window.npChart.renderLine(host, {
      points: seriesCache.map((p) => ({ day: p.day, value: p.requests })),
      color: "#10b981",
      area: true,
    });
  } else {
    window.npChart.renderStacked(host, {
      points: seriesCache,
      series: [
        { key: "input_tokens",  label: "Input",  color: "#3b82f6" },
        { key: "output_tokens", label: "Output", color: "#10b981" },
        { key: "cached_tokens", label: "Cached", color: "#f59e0b" },
      ],
    });
  }
}

function bindRangePicker() {
  document.querySelectorAll(".range-btn").forEach((b) => {
    b.addEventListener("click", async () => {
      document.querySelectorAll(".range-btn").forEach((x) => x.classList.remove("is-active"));
      b.classList.add("is-active");
      state.range = b.dataset.range;
      if (state.range === "custom") {
        const from = prompt("From (YYYY-MM-DD)");
        const to = prompt("To (YYYY-MM-DD)");
        if (!from || !to) return;
        state.customFrom = from; state.customTo = to;
      }
      await Promise.all([loadSummary(), loadTopKeys(), loadTopModels(), loadRecent()]);
    });
  });
}

async function loadSummary() {
  try {
    const s = await api.get(`/admin/api/stats/summary?${rangeParams()}`);
    setKpi("cost", fmtUSD(s.cost_usd), `${fmtNum(s.requests)} requests`);
    setKpi("requests", fmtNum(s.requests),
      `${fmtNum(s.errors)} errors · ${fmtPct(s.errors / Math.max(s.requests, 1))}`);
    setKpi("tokens", `${fmtNum(s.input_tokens)} / ${fmtNum(s.output_tokens)}`,
      `${fmtNum(s.total_tokens)} total`);
    setKpi("cache", fmtPct(s.cache_hit_rate),
      `${fmtNum(s.cache_hits)} cache hits`);
  } catch (e) {
    console.error("summary:", e);
  }
}

function setKpi(key, value, foot) {
  const card = document.querySelector(`[data-kpi="${key}"]`);
  if (!card) return;
  card.querySelector(".kpi-value").textContent = value;
  card.querySelector("[data-kpi-foot]").textContent = foot || "";
}

async function loadTopKeys() {
  try {
    const rows = await api.get(`/admin/api/stats/breakdown?by=key&${rangeParams()}`);
    renderRank("rank-keys", rows);
  } catch (e) { console.error("top keys:", e); }
}
async function loadTopModels() {
  try {
    const rows = await api.get(`/admin/api/stats/breakdown?by=model&${rangeParams()}`);
    
    // Render donut chart
    const host = document.getElementById("chart-donut-models");
    if (host && window.npChart) {
      window.npChart.renderDonut(host, {
        data: rows.slice(0, 8).map(r => ({ label: r.model || "—", value: r.cost_usd }))
      });
    }
  } catch (e) { console.error("top models:", e); }
}

function renderRank(id, rows) {
  const el = document.getElementById(id);
  if (!el) return;
  if (!rows.length) {
    el.innerHTML = `<li class="rank-row"><span class="rank-name">No data</span><span class="rank-value">—</span></li>`;
    return;
  }
  el.innerHTML = rows.slice(0, 6).map((r) => `
    <li class="rank-row">
      <span class="rank-name">${escapeHtml(r.name || r.model || "—")}</span>
      <span class="rank-value">${fmtUSD(r.cost_usd)}</span>
    </li>
  `).join("");
}

async function loadRecent() {
  try {
    const rows = await api.get(`/admin/api/requests?limit=8`);
    renderRequestsRows("recent-rows", rows, /*compact*/ true);
  } catch (e) { console.error("recent:", e); }
}

// ───────── Requests page ─────────

const reqState = { offset: 0, limit: 50 };

async function initRequests() {
  await loadFilterOptions();
  document.querySelectorAll("#f-key,#f-model,#f-status,#f-from,#f-to").forEach((el) =>
    el.addEventListener("change", () => { reqState.offset = 0; loadRequests(); }));
  document.querySelector('[data-action="reset"]').addEventListener("click", () => {
    document.getElementById("f-key").value = "";
    document.getElementById("f-model").value = "";
    document.getElementById("f-status").value = "";
    document.getElementById("f-from").value = "";
    document.getElementById("f-to").value = "";
    reqState.offset = 0;
    loadRequests();
  });
  document.querySelector('[data-action="export"]').addEventListener("click", () => {
    const params = new URLSearchParams(currentReqFilters());
    params.set("format", "csv");
    location.href = `/admin/api/requests/export?${params}`;
  });
  document.querySelector('[data-action="prev"]').addEventListener("click", () => {
    reqState.offset = Math.max(0, reqState.offset - reqState.limit);
    loadRequests();
  });
  document.querySelector('[data-action="next"]').addEventListener("click", () => {
    reqState.offset += reqState.limit;
    loadRequests();
  });
  loadRequests();
}

async function loadFilterOptions() {
  try {
    const opts = await api.get("/admin/api/filters");
    fillSelect("f-key", opts.keys, (k) => `${k.name} (${k.prefix})`, (k) => k.id);
    fillSelect("f-model", opts.models.map((m) => ({ name: m })), (m) => m.name, (m) => m.name);
  } catch (e) { console.error("filters:", e); }
}

function fillSelect(id, items, label, value) {
  const sel = document.getElementById(id);
  if (!sel) return;
  const current = sel.value;
  sel.innerHTML = `<option value="">All ${sel.options[0]?.text || ""}</option>` +
    items.map((it) => `<option value="${escapeAttr(value(it))}">${escapeHtml(label(it))}</option>`).join("");
  sel.value = current;
}

function currentReqFilters() {
  const params = { limit: reqState.limit, offset: reqState.offset };
  const k = document.getElementById("f-key")?.value;
  const m = document.getElementById("f-model")?.value;
  const s = document.getElementById("f-status")?.value;
  const from = document.getElementById("f-from")?.value;
  const to = document.getElementById("f-to")?.value;
  if (k) params.key = k;
  if (m) params.model = m;
  if (from) params.from = from;
  if (to) params.to = to;
  if (s === "err") params.status_min = 400;
  if (s === "ok") params.status_max = 299;
  if (s === "tool") params.has_tool_error = 1;
  return params;
}

async function loadRequests() {
  const params = new URLSearchParams(currentReqFilters());
  try {
    const res = await api.get(`/admin/api/requests?${params}`);
    renderRequestsRows("requests-rows", res.items, false);
    document.getElementById("pager-info").textContent =
      `${res.offset + 1}–${res.offset + res.items.length} of ${res.total}`;
  } catch (e) {
    console.error("requests:", e);
  }
}

function renderRequestsRows(id, rows, compact) {
  const tb = document.getElementById(id);
  if (!tb) return;
  if (!rows.length) {
    tb.innerHTML = `<tr><td colspan="${compact ? 7 : 10}" class="empty">No requests match these filters yet.</td></tr>`;
    return;
  }
  tb.innerHTML = rows.map((r) => {
    const st = statusClass(r.status_code);
    const cells = compact
      ? `<td class="col-time mono">${fmtTime(r.ts)}</td>
         <td>${escapeHtml(r.api_key_name)}</td>
         <td class="mono">${escapeHtml(r.model)}</td>
         <td class="col-num mono">${fmtNum(r.total_tokens)}</td>
         <td class="col-num mono">${fmtNum(r.cached_tokens)}</td>
         <td class="col-num mono">${fmtUSD(r.cost_usd)}</td>
         <td class="col-status"><span class="badge ${st}">${r.status_code}</span></td>`
      : `<td class="col-time mono">${fmtTime(r.ts)}</td>
         <td>${escapeHtml(r.api_key_name)}</td>
         <td class="mono">${escapeHtml(r.model)}</td>
         <td class="col-num mono">${fmtNum(r.prompt_tokens)}</td>
         <td class="col-num mono">${fmtNum(r.completion_tokens)}</td>
         <td class="col-num mono">${fmtNum(r.cached_tokens)}</td>
         <td class="col-num mono">${fmtUSD(r.cost_usd)}</td>
         <td>${r.tool_error ? `<span class="badge badge-warn" title="${escapeAttr(r.tool_error_msg || "")}">tool err</span>` : (r.has_tool_calls ? `<span class="badge">${r.tool_calls_count}</span>` : "—")}</td>
         <td class="col-status"><span class="badge ${st}">${r.status_code}</span></td>
         <td class="col-num mono">${r.latency_ms} ms</td>`;
    return `<tr>${cells}</tr>`;
  }).join("");
}

// ───────── Keys page ─────────

async function initKeys() {
  await loadKeys();
  document.querySelector('[data-action="new-key"]').addEventListener("click", () => {
    document.getElementById("dlg-new-key-result").hidden = true;
    document.getElementById("form-new-key").hidden = false;
    document.getElementById("dlg-new-key").showModal();
  });
  document.querySelector('[data-action="cancel"]').addEventListener("click", () => {
    document.getElementById("dlg-new-key").close();
  });
  document.querySelector('[data-action="close"]').addEventListener("click", () => {
    document.getElementById("dlg-new-key").close();
  });
  document.querySelector('[data-action="cancel-limits"]')?.addEventListener("click", () => {
    document.getElementById("dlg-edit-limits").close();
  });
  
  document.getElementById("form-edit-limits")?.addEventListener("submit", async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    const id = fd.get("id");
    const body = {
      limit_interval: fd.get("limit_interval"),
      clear_budget: true,
      clear_input_tokens: true,
      clear_output_tokens: true,
      clear_total_tokens: true,
    };
    if (fd.get("budget")) { body.budget_usd = parseFloat(fd.get("budget")); body.clear_budget = false; }
    if (fd.get("limit_input_tokens")) { body.limit_input_tokens = parseInt(fd.get("limit_input_tokens")); body.clear_input_tokens = false; }
    if (fd.get("limit_output_tokens")) { body.limit_output_tokens = parseInt(fd.get("limit_output_tokens")); body.clear_output_tokens = false; }
    if (fd.get("limit_total_tokens")) { body.limit_total_tokens = parseInt(fd.get("limit_total_tokens")); body.clear_total_tokens = false; }
    
    try {
      await api.patch(`/admin/api/keys/${id}`, body);
      document.getElementById("dlg-edit-limits").close();
      loadKeys();
    } catch (err) {
      alert("Failed to update limits: " + err.message);
    }
  });

  document.getElementById("form-new-key").addEventListener("submit", async (e) => {
    e.preventDefault();
    const fd = new FormData(e.target);
    const body = { name: fd.get("name") };
    const b = fd.get("budget");
    if (b) body.budget_usd = parseFloat(b);
    try {
      const key = await api.post("/admin/api/keys", body);
      document.getElementById("form-new-key").hidden = true;
      document.getElementById("dlg-new-key-result").hidden = false;
      document.getElementById("dlg-key-value").textContent = key.raw_key;
      loadKeys();
    } catch (err) {
      alert("Failed to create key: " + err.message);
    }
  });
}

async function loadKeys() {
  try {
    const keys = await api.get("/admin/api/keys");
    const tb = document.getElementById("keys-rows");
    if (!tb) return;
    if (!keys.length) {
      tb.innerHTML = `<tr><td colspan="7" class="empty">No keys yet. Create one above.</td></tr>`;
      return;
    }
    tb.innerHTML = keys.map((k) => `
      <tr>
        <td>${escapeHtml(k.name)}</td>
        <td class="mono">
           ${escapeHtml(k.key_prefix)}…
           ${k.raw_key ? `<button class="btn btn-ghost" style="padding: 2px 4px; font-size: 12px; margin-left: 8px;" data-action="copy" data-key="${escapeAttr(k.raw_key)}">Copy</button>` : ""}
        </td>
        <td><span class="badge ${k.enabled ? "badge-ok" : "badge-bad"}">${k.enabled ? "enabled" : "disabled"}</span></td>
        <td class="col-time mono">${fmtTime(k.created_at_ms ?? Date.parse(k.created_at))}</td>
        <td>${k.limit_interval || 'all_time'}</td>
        <td class="col-num mono">${k.budget_usd ? fmtUSD(k.budget_usd) : "—"}</td>
        <td class="col-num mono">${fmtUSD(k.spend_30d ?? 0)}</td>
        <td>
          <button class="btn btn-ghost" data-action="limits" data-key='${escapeAttr(JSON.stringify(k))}'>Limits</button>
          <button class="btn btn-ghost" data-action="toggle" data-id="${k.id}" data-enabled="${!k.enabled}">
            ${k.enabled ? "Disable" : "Enable"}
          </button>
          <button class="btn btn-ghost" data-action="delete" data-id="${k.id}">Delete</button>
        </td>
      </tr>
    `).join("");
    tb.querySelectorAll('[data-action="toggle"]').forEach((b) =>
      b.addEventListener("click", () => toggleKey(b.dataset.id, b.dataset.enabled === "true")));
    tb.querySelectorAll('[data-action="delete"]').forEach((b) =>
      b.addEventListener("click", () => deleteKey(b.dataset.id)));
    tb.querySelectorAll('[data-action="copy"]').forEach((b) =>
      b.addEventListener("click", () => {
        navigator.clipboard.writeText(b.dataset.key);
        const orig = b.textContent;
        b.textContent = "Copied!";
        setTimeout(() => b.textContent = orig, 2000);
      }));
    tb.querySelectorAll('[data-action="limits"]').forEach((b) =>
      b.addEventListener("click", () => {
        const k = JSON.parse(b.dataset.key);
        document.getElementById("edit-limits-id").value = k.id;
        document.getElementById("edit-limit-interval").value = k.limit_interval || "all_time";
        document.getElementById("edit-budget").value = k.budget_usd || "";
        document.getElementById("edit-limit-in").value = k.limit_input_tokens || "";
        document.getElementById("edit-limit-out").value = k.limit_output_tokens || "";
        document.getElementById("edit-limit-tot").value = k.limit_total_tokens || "";
        document.getElementById("dlg-edit-limits").showModal();
      }));
  } catch (e) { console.error("keys:", e); }
}

async function toggleKey(id, enable) {
  await api.patch(`/admin/api/keys/${id}`, { enabled: enable });
  loadKeys();
}
async function deleteKey(id) {
  if (!confirm("Delete this key? Its request history will also be removed.")) return;
  await api.del(`/admin/api/keys/${id}`);
  loadKeys();
}

// ───────── escaping ─────────

function escapeHtml(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[c]);
}
function escapeAttr(s) { return escapeHtml(s); }

// ───────── Settings page ─────────

async function initSettings() {
  await loadSettings();

  document.getElementById("form-upstream-key").addEventListener("submit", async (e) => {
    e.preventDefault();
    const input = document.getElementById("upstream-key-input");
    const value = input.value.trim();
    if (!value) return setFormStatus("Paste a key first.", "warn");
    setFormStatus("Saving…", "");
    try {
      await api.put("/admin/api/settings", { upstream_api_key: value });
      input.value = "";
      setFormStatus("Saved — proxy is using the new key for new requests.", "ok");
      await loadSettings();
    } catch (err) {
      setFormStatus(`Failed: ${err.message || err}`, "err");
    }
  });

  const reveal = document.getElementById("btn-reveal-key");
  reveal.addEventListener("click", () => {
    const input = document.getElementById("upstream-key-input");
    const isPassword = input.type === "password";
    input.type = isPassword ? "text" : "password";
    reveal.textContent = isPassword ? "Hide" : "Show";
  });

  document.getElementById("btn-clear-key").addEventListener("click", async () => {
    if (!confirm("Clear the upstream API key? The proxy will return 502 for all traffic until a new key is set.")) return;
    setFormStatus("Clearing…", "");
    try {
      await api.put("/admin/api/settings", { clear_upstream_api_key: true });
      setFormStatus("Cleared. Configure a new key to resume proxy traffic.", "warn");
      await loadSettings();
    } catch (err) {
      setFormStatus(`Failed: ${err.message || err}`, "err");
    }
  });
}

async function loadSettings() {
  try {
    const v = await api.get("/admin/api/settings");
    const status = document.getElementById("settings-status");
    const meta = document.getElementById("upstream-key-meta");
    if (v.upstream_key_set) {
      const last4 = v.upstream_key_last4 ? `…${v.upstream_key_last4}` : "";
      status.innerHTML = `<span class="badge badge-ok">configured</span>`;
      const updated = v.upstream_key_updated_ms ? fmtTime(v.upstream_key_updated_ms) : "";
      meta.textContent = `${last4}  ·  last updated ${updated}`;
    } else {
      status.innerHTML = `<span class="badge badge-warn">not set</span>`;
      meta.textContent = "proxy will return 502 until configured";
    }
  } catch (e) { console.error("settings:", e); }
}

function setFormStatus(text, kind) {
  const el = document.getElementById("form-status");
  if (!el) return;
  el.textContent = text;
  el.dataset.kind = kind || "";
}