"use strict";

// 主控台。整个页面没有任何内联脚本（CSP script-src 'self'），
// 所有请求都带会话 Cookie，改状态的请求额外带 X-WB-CSRF。

const state = {
  csrf: "",
  version: "",
  fingerprint: "",
  nodes: [],
  selected: new Set(),
  watchId: "",
  nodeTimer: 0,
  taskTimer: 0,
  lastTask: null,
};

const STATE_LABEL = {
  running: "运行中",
  done: "已完成",
  stopped: "已停止",
  degraded: "部分节点丢失",
};

const SHARD_LABEL = {
  assigned: "已派发",
  running: "运行中",
  done: "已完成",
  stopped: "已停止",
  lost: "已失联",
  error: "出错",
};

const $ = (id) => document.getElementById(id);

// ---------- 网络 ----------

async function api(path, options = {}) {
  const method = options.method || "GET";
  const init = { method, credentials: "same-origin", headers: {} };
  if (options.body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(options.body);
  }
  if (method !== "GET") {
    init.headers["X-WB-CSRF"] = state.csrf;
  }
  let res;
  try {
    res = await fetch(path, init);
  } catch (err) {
    throw new Error("无法连接主控：" + err.message);
  }
  let data = null;
  try {
    data = await res.json();
  } catch (err) {
    data = null;
  }
  if (res.status === 401 && path !== "/api/v1/login") {
    showLogin("登录已失效，请重新登录");
    throw new Error("登录已失效");
  }
  if (!res.ok) {
    throw new Error((data && data.error) || `请求失败（HTTP ${res.status}）`);
  }
  return data;
}

// ---------- 启动与登录 ----------

async function boot() {
  try {
    const s = await api("/api/v1/session");
    state.version = s.version;
    state.fingerprint = s.fingerprint;
    $("login-ver").textContent = "v" + s.version;
    if (!s.authenticated) {
      showLogin("");
      return;
    }
    state.csrf = s.csrf;
    await enter();
  } catch (err) {
    showLogin(err.message);
  }
}

function showLogin(message) {
  stopTimers();
  $("app").hidden = true;
  $("login").hidden = false;
  const err = $("login-err");
  err.textContent = message || "";
  const pw = $("password");
  pw.value = "";
  pw.focus();
}

async function enter() {
  $("login").hidden = true;
  $("app").hidden = false;
  $("ver").textContent = "v" + state.version;
  await refreshNodes();
  await refreshHistory();
  state.nodeTimer = setInterval(() => {
    refreshNodes().catch(() => {});
  }, 3000);
}

function stopTimers() {
  if (state.nodeTimer) clearInterval(state.nodeTimer);
  if (state.taskTimer) clearInterval(state.taskTimer);
  state.nodeTimer = 0;
  state.taskTimer = 0;
  state.watchId = "";
}

$("login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const err = $("login-err");
  err.textContent = "";
  try {
    const out = await api("/api/v1/login", {
      method: "POST",
      body: { password: $("password").value },
    });
    state.csrf = out.csrf;
    await enter();
  } catch (ex) {
    err.textContent = ex.message;
  }
});

$("logout").addEventListener("click", async () => {
  try {
    await api("/api/v1/logout", { method: "POST" });
  } catch (err) {
    // 会话已失效时也要把界面切回登录页
  }
  showLogin("");
});

// ---------- 节点 ----------

async function refreshNodes() {
  const out = await api("/api/v1/nodes");
  state.nodes = out.nodes || [];
  const ids = new Set(state.nodes.map((n) => n.id));
  for (const id of Array.from(state.selected)) {
    if (!ids.has(id)) state.selected.delete(id);
  }
  renderNodes();
}

function nodeSelectable(n) {
  return n.online && !n.revoked && !n.running;
}

function renderNodes() {
  const body = $("nodes-table").querySelector("tbody");
  body.replaceChildren();
  $("nodes-empty").hidden = state.nodes.length > 0;

  for (const n of state.nodes) {
    const selectable = nodeSelectable(n);
    if (!selectable) state.selected.delete(n.id);

    const check = el("input", { type: "checkbox", checked: state.selected.has(n.id) });
    check.disabled = !selectable;
    check.addEventListener("change", () => {
      if (check.checked) state.selected.add(n.id);
      else state.selected.delete(n.id);
      updatePlan();
      updateStartButton();
    });

    const status = el("span");
    status.append(el("span", { class: "dot " + (n.revoked ? "revoked" : n.running ? "running" : n.online ? "online" : "offline") }));
    status.append(document.createTextNode(
      n.revoked ? "已吊销" : n.running ? "压测中" : n.online ? "在线" : "离线"));

    const name = el("td");
    name.append(document.createTextNode(n.name || n.id));
    name.append(el("span", { class: "sub mono", text: n.id }));

    const action = el("td", { class: "w-action" });
    if (!n.revoked) {
      const btn = el("button", { class: "ghost small", type: "button", text: "吊销" });
      btn.addEventListener("click", () => revokeNode(n));
      action.append(btn);
    }

    body.append(el("tr", {}, [
      el("td", { class: "w-check" }, [check]),
      name,
      el("td", {}, [status]),
      el("td", { class: "num", text: n.rps > 0 ? fmtRate(n.rps) : "-" }),
      el("td", { text: n.version || "-" }),
      el("td", { class: "num", text: n.online ? n.lastSeenAgoSec + "s" : "-" }),
      action,
    ]));
  }

  const online = state.nodes.filter((n) => n.online && !n.revoked).length;
  $("node-summary").textContent = `在线 ${online} / 共 ${state.nodes.length}`;
  updateSelectAllLabel();
  updatePlan();
  updateStartButton();
}

async function revokeNode(n) {
  if (!confirm(`吊销节点 ${n.name || n.id}？\n吊销后它会立即失去下发任务的资格，正在执行的任务会跑到结束。`)) {
    return;
  }
  try {
    await api(`/api/v1/nodes/${encodeURIComponent(n.id)}/revoke`, { method: "POST" });
    await refreshNodes();
  } catch (err) {
    alert(err.message);
  }
}

$("select-all").addEventListener("click", () => {
  const selectable = state.nodes.filter(nodeSelectable);
  const allSelected = selectable.length > 0 && selectable.every((n) => state.selected.has(n.id));
  state.selected.clear();
  if (!allSelected) {
    for (const n of selectable) state.selected.add(n.id);
  }
  renderNodes();
});

function updateSelectAllLabel() {
  const selectable = state.nodes.filter(nodeSelectable);
  const all = selectable.length > 0 && selectable.every((n) => state.selected.has(n.id));
  $("select-all").textContent = all ? "清空" : "全选";
}

function updateStartButton() {
  const btn = $("start-btn");
  btn.disabled = state.selected.size === 0;
  btn.title = state.selected.size === 0 ? "先勾选至少一台节点" : "";
}

function updatePlan() {
  const total = Number(document.querySelector('[name="totalConc"]').value) || 0;
  const count = state.selected.size;
  if (count === 0) {
    $("plan-hint").textContent = "先勾选节点";
    return;
  }
  let per = Math.ceil(total / count);
  if (!Number.isFinite(per) || per < 1) per = 1;
  if (per > 256) per = 256;
  const actual = per * count;
  const suffix = actual !== total ? `（实际 ${actual}，单节点上限 256）` : "";
  $("plan-hint").textContent = `${count} 台节点 → 每台 ${per} 并发${suffix}`;
}

document.querySelector('[name="totalConc"]').addEventListener("input", updatePlan);

// ---------- 任务 ----------

$("task-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = event.target;
  const err = $("task-err");
  err.textContent = "";

  let headers;
  try {
    headers = parseHeaders(form.headers.value);
  } catch (ex) {
    err.textContent = ex.message;
    return;
  }
  if (state.selected.size === 0) {
    err.textContent = "先勾选至少一台节点";
    return;
  }

  const body = {
    target: form.target.value.trim(),
    durationSec: Number(form.durationSec.value),
    totalConc: Number(form.totalConc.value),
    proto: form.proto.value,
    timeoutSec: Number(form.timeoutSec.value) || 0,
    noReuse: form.noReuse.checked,
    xforwardFor: form.xforwardFor.checked,
    proxySpoof: form.proxySpoof.checked,
    randomPost: form.randomPost.checked,
    randomSize: form.randomPost.checked ? 1024 : 0,
    family: form.ipv6.checked ? "ipv6" : "ipv4",
    headers,
    nodeIds: Array.from(state.selected),
  };

  $("start-btn").disabled = true;
  try {
    const task = await api("/api/v1/tasks", { method: "POST", body });
    watchTask(task.id);
    refreshNodes().catch(() => {});
    refreshHistory().catch(() => {});
  } catch (ex) {
    err.textContent = ex.message;
  } finally {
    updateStartButton();
  }
});

function parseHeaders(text) {
  const out = [];
  for (const raw of String(text || "").split(/\r?\n/)) {
    const line = raw.trim();
    if (!line) continue;
    const i = line.indexOf(":");
    if (i <= 0) throw new Error(`请求头要写成 Name: Value，这一行无法解析：${line}`);
    out.push({ name: line.slice(0, i).trim(), value: line.slice(i + 1).trim() });
  }
  return out;
}

function watchTask(id) {
  if (state.taskTimer) clearInterval(state.taskTimer);
  state.watchId = id;
  $("live-panel").hidden = false;
  const tick = async () => {
    if (state.watchId !== id) return;
    try {
      const task = await api(`/api/v1/tasks/${encodeURIComponent(id)}`);
      renderTask(task);
      if (task.state !== "running") {
        clearInterval(state.taskTimer);
        state.taskTimer = 0;
        refreshNodes().catch(() => {});
        refreshHistory().catch(() => {});
      }
    } catch (err) {
      clearInterval(state.taskTimer);
      state.taskTimer = 0;
      $("live-note").textContent = err.message;
    }
  };
  tick();
  state.taskTimer = setInterval(tick, 1000);
}

function renderTask(task) {
  state.lastTask = task;

  const chip = $("live-state");
  chip.textContent = STATE_LABEL[task.state] || task.state;
  chip.className = "chip " + task.state;

  const bits = [task.target, task.proto, `每节点 ${task.perNodeConc} 并发`, `${task.durationSec} 秒`];
  if (task.noReuse) bits.push("每次新建连接");
  if (task.proxySpoof) bits.push("伪造代理头");
  $("live-target").textContent = bits.join(" · ");

  const done = task.state !== "running";
  const elapsed = Math.round(task.elapsedMs / 1000);
  const remain = Math.max(0, Math.round(task.remainingMs / 1000));
  renderCards([
    { k: "总 RPS", v: fmtRate(task.totals.rps) },
    { k: "请求数", v: fmtInt(task.totals.requests) },
    { k: "失败", v: fmtInt(task.totals.failed) },
    { k: "流量", v: fmtBytes(task.totals.bytes) },
    { k: "峰值 RPS", v: fmtRate(task.totals.peakRps) },
    { k: done ? "耗时" : "剩余", v: done ? `${elapsed}s` : `${remain}s` },
  ]);

  drawChart(task.series || []);

  const body = $("live-nodes").querySelector("tbody");
  body.replaceChildren();
  for (const n of task.nodes || []) {
    const status = el("span");
    status.append(el("span", { class: "dot " + dotFor(n.state) }));
    status.append(document.createTextNode(SHARD_LABEL[n.state] || n.state));
    body.append(el("tr", {}, [
      el("td", {}, [document.createTextNode(n.name || n.nodeId),
        el("span", { class: "sub mono", text: n.nodeId })]),
      el("td", {}, [status]),
      el("td", { class: "num", text: String(n.conc) }),
      el("td", { class: "num", text: n.rps > 0 ? fmtRate(n.rps) : "-" }),
      el("td", { class: "num", text: fmtInt(n.requests) }),
      el("td", { class: "num", text: fmtInt(n.failed) }),
      el("td", { class: "num", text: fmtBytes(n.bytes) }),
    ]));
  }

  const notes = [];
  if (task.stoppedBy) notes.push("由 " + task.stoppedBy + " 停止");
  const errors = (task.nodes || []).filter((n) => n.lastError);
  for (const n of errors.slice(0, 3)) {
    notes.push(`${n.name || n.nodeId}: ${n.lastError}`);
  }
  $("live-note").textContent = notes.join("　");

  const stop = $("stop-btn");
  stop.hidden = task.state !== "running";
  stop.onclick = async () => {
    stop.disabled = true;
    try {
      await api(`/api/v1/tasks/${encodeURIComponent(task.id)}/stop`, { method: "POST" });
    } catch (err) {
      $("live-note").textContent = err.message;
    } finally {
      stop.disabled = false;
    }
  };
}

function dotFor(shardState) {
  switch (shardState) {
    case "running":
      return "running";
    case "done":
      return "online";
    case "lost":
    case "error":
      return "revoked";
    default:
      return "offline";
  }
}

function renderCards(items) {
  const box = $("live-cards");
  box.replaceChildren();
  for (const item of items) {
    box.append(el("div", { class: "card" }, [
      el("div", { class: "k", text: item.k }),
      el("div", { class: "v", text: item.v }),
    ]));
  }
}

// ---------- 折线图 ----------

function drawChart(series) {
  const canvas = $("chart");
  const cssWidth = canvas.clientWidth || 800;
  const cssHeight = 150;
  const dpr = window.devicePixelRatio || 1;
  if (canvas.width !== Math.round(cssWidth * dpr) || canvas.height !== Math.round(cssHeight * dpr)) {
    canvas.width = Math.round(cssWidth * dpr);
    canvas.height = Math.round(cssHeight * dpr);
  }
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, cssWidth, cssHeight);

  const pad = { l: 52, r: 10, t: 12, b: 20 };
  const w = Math.max(10, cssWidth - pad.l - pad.r);
  const h = Math.max(10, cssHeight - pad.t - pad.b);

  let maxRps = 0;
  let maxT = 1;
  for (const p of series) {
    if (p.rps > maxRps) maxRps = p.rps;
    if (p.t > maxT) maxT = p.t;
  }
  if (maxRps <= 0) maxRps = 1;

  ctx.strokeStyle = "#232b38";
  ctx.fillStyle = "#8b95a7";
  ctx.font = "11px ui-monospace, Menlo, Consolas, monospace";
  ctx.lineWidth = 1;
  for (let i = 0; i <= 4; i++) {
    const y = pad.t + (h * i) / 4;
    ctx.beginPath();
    ctx.moveTo(pad.l, y);
    ctx.lineTo(pad.l + w, y);
    ctx.stroke();
    const value = maxRps * (1 - i / 4);
    ctx.fillText(fmtRate(value), 4, y + 3);
  }

  if (series.length === 0) {
    ctx.fillText("等待第一个采样点…", pad.l + 8, pad.t + h / 2);
    return;
  }

  const x = (t) => pad.l + (t / maxT) * w;
  const y = (v) => pad.t + h - (v / maxRps) * h;

  ctx.beginPath();
  series.forEach((p, i) => {
    const px = x(p.t);
    const py = y(p.rps);
    if (i === 0) ctx.moveTo(px, py);
    else ctx.lineTo(px, py);
  });
  ctx.strokeStyle = "#4da3ff";
  ctx.lineWidth = 1.6;
  ctx.lineJoin = "round";
  ctx.stroke();

  ctx.lineTo(x(series[series.length - 1].t), pad.t + h);
  ctx.lineTo(x(series[0].t), pad.t + h);
  ctx.closePath();
  const grad = ctx.createLinearGradient(0, pad.t, 0, pad.t + h);
  grad.addColorStop(0, "rgba(77,163,255,.28)");
  grad.addColorStop(1, "rgba(77,163,255,0)");
  ctx.fillStyle = grad;
  ctx.fill();

  ctx.fillStyle = "#8b95a7";
  ctx.fillText("0s", pad.l, cssHeight - 5);
  const label = Math.round(maxT / 1000) + "s";
  ctx.fillText(label, pad.l + w - ctx.measureText(label).width, cssHeight - 5);
}

window.addEventListener("resize", () => {
  if (state.lastTask) drawChart(state.lastTask.series || []);
});

// ---------- 历史 ----------

async function refreshHistory() {
  const out = await api("/api/v1/tasks");
  const rows = out.tasks || [];
  const body = $("history-table").querySelector("tbody");
  body.replaceChildren();
  $("history-empty").hidden = rows.length > 0;

  for (const t of rows) {
    const chip = el("span", { class: "chip " + t.state, text: STATE_LABEL[t.state] || t.state });
    const target = el("td");
    target.append(document.createTextNode(t.target));
    target.append(el("span", { class: "sub", text: new Date(t.startedAt).toLocaleString("zh-CN", { hour12: false }) }));

    const tr = el("tr", {}, [
      target,
      el("td", {}, [chip]),
      el("td", { class: "num", text: String((t.nodes || []).length) }),
      el("td", { class: "num", text: String(t.totalConc) }),
      el("td", { class: "num", text: fmtRate(t.totals.peakRps) }),
      el("td", { class: "num", text: fmtInt(t.totals.requests) }),
      el("td", { class: "num", text: fmtBytes(t.totals.bytes) }),
      el("td", { text: new Date(t.startedAt).toLocaleTimeString("zh-CN", { hour12: false }) }),
    ]);
    tr.addEventListener("click", () => {
      $("live-panel").hidden = false;
      if (t.state === "running") watchTask(t.id);
      else {
        if (state.taskTimer) clearInterval(state.taskTimer);
        state.taskTimer = 0;
        state.watchId = "";
        api(`/api/v1/tasks/${encodeURIComponent(t.id)}`).then(renderTask).catch(() => {});
      }
      $("live-panel").scrollIntoView({ behavior: "smooth", block: "start" });
    });
    body.append(tr);
  }

  // 刷新页面时自动接上正在跑的任务，而不是让人再点一次历史记录。
  const running = rows.find((t) => t.state === "running");
  if (running && !state.watchId) watchTask(running.id);
}

// ---------- 加入命令 ----------

$("join-btn").addEventListener("click", () => issueJoin(true));
$("join-url").addEventListener("change", () => issueJoin(false));

// issueJoin 只在需要时打开对话框：对话框已经打开时再 showModal 会抛异常，
// 而"改地址重新签发"正是对话框打开状态下的操作。
async function issueJoin(openDialog) {
  const dialog = $("join-dialog");
  const input = $("join-url");
  if (!input.value) input.value = location.origin;
  try {
    const out = await api("/api/v1/join", { method: "POST", body: { url: input.value } });
    $("join-cmd").textContent = out.command;
    $("join-exp").textContent = `指纹 ${out.fingerprint}　有效期至 ${new Date(out.expiresAt).toLocaleString("zh-CN", { hour12: false })}`;
  } catch (err) {
    $("join-cmd").textContent = err.message;
    $("join-exp").textContent = "";
  }
  if (openDialog && !dialog.open) dialog.showModal();
}

$("copy-join").addEventListener("click", async () => {
  const text = $("join-cmd").textContent;
  try {
    await navigator.clipboard.writeText(text);
    $("copy-join").textContent = "已复制";
  } catch (err) {
    const range = document.createRange();
    range.selectNodeContents($("join-cmd"));
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
    $("copy-join").textContent = "请按 Ctrl+C";
  }
  setTimeout(() => { $("copy-join").textContent = "复制命令"; }, 2000);
});

// ---------- 格式化与工具 ----------

const intFmt = new Intl.NumberFormat("en-US");

function fmtInt(n) {
  return intFmt.format(Number(n) || 0);
}

function fmtRate(n) {
  const v = Number(n) || 0;
  if (v >= 1000) return intFmt.format(Math.round(v)) + "/s";
  return v.toFixed(v >= 10 ? 0 : 1) + "/s";
}

function fmtBytes(n) {
  let v = Number(n) || 0;
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return (i === 0 ? String(Math.round(v)) : v.toFixed(v >= 100 ? 0 : 1)) + " " + units[i];
}

function el(tag, props = {}, children = []) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (key === "class") node.className = value;
    else if (key === "text") node.textContent = value;
    else if (key === "dataset") Object.assign(node.dataset, value);
    else if (key in node) node[key] = value;
    else node.setAttribute(key, value);
  }
  for (const child of [].concat(children)) {
    if (child !== null && child !== undefined) node.append(child);
  }
  return node;
}

boot();
