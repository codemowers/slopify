import { MediaEngine, isVideo, currentEntry as current } from "./media.js";
import { fetchRetry, probeStream } from "./network.js";

// One durable queue per signed-in user. Tabs are outputs, alongside devices;
// opening another tab observes the queue without moving playback.

const $ = (id) => document.getElementById(id);
let audio = $("audio");
function onMedia(type, listener) { engine.on(type, listener); }
const engine = new MediaEngine({
  audio, video: $("video"),
  playback: () => player(me),
  audioProfile: () => quality(),
  streamURL, probeStream,
  metadata: (entry) => state.trackDetails.get(entry.path),
  enrich: async (entry) => {
    if (!isVideo(entry) || (entry.codec && entry.videoCodec)) return;
    try {
      const details = await api(`catalog/item?path=${encodeURIComponent(entry.path)}`);
      state.trackDetails.set(entry.path, details);
      entry.codec ||= details.codec;
      entry.videoCodec ||= details.videoCodec;
    } catch (_) { /* Try originals when metadata is unavailable. */ }
  },
  onElement: (element) => { audio = element; },
  onSource: () => {
    renderVideo();
    const entry = engine.loaded;
    if (entry && "mediaSession" in navigator) {
      navigator.mediaSession.metadata = new MediaMetadata({title: entry.title, artist: entry.artist || "", album: entry.album || ""});
    }
  },
  onBlocked: () => { $("blocked").hidden = false; report({blocked: true}); },
  onPlaying: () => { $("blocked").hidden = true; },
  onError: (error) => toast(error.message),
  onEvent: () => renderNerd(),
});
const systemTheme = matchMedia("(prefers-color-scheme: dark)");
function applyTheme(choice = localStorage.getItem("slopify-theme") || "system") {
  const theme = choice === "system" ? (systemTheme.matches ? "dark" : "light") : choice;
  document.documentElement.dataset.theme = theme;
  document.querySelector('meta[name="theme-color"]').content = theme === "light" ? "#f5f3f0" : "#090909";
  for (const button of document.querySelectorAll("[data-theme-choice]")) {
    button.setAttribute("aria-pressed", String(button.dataset.themeChoice === choice));
  }
}
applyTheme();
systemTheme.addEventListener("change", () => applyTheme());
for (const button of document.querySelectorAll("[data-theme-choice]")) {
  button.addEventListener("click", () => {
    localStorage.setItem("slopify-theme", button.dataset.themeChoice);
    applyTheme(button.dataset.themeChoice);
  });
}

const me = (() => {
  let id = sessionStorage.getItem("slopify-output");
  if (!id) {
    const bytes = crypto.getRandomValues(new Uint8Array(12));
    id = "web-" + [...bytes].map((b) => (b % 36).toString(36)).join("");
    sessionStorage.setItem("slopify-output", id);
  }
  return id;
})();

const state = {
  players: new Map(), // id -> {p, at}: player and when it arrived
  queueId: null,
  selected: "",
  outputIds: [],
  trackDetails: new Map(),
  playersReady: false,
  sel: new Set(), // selected playlist entry IDs
  lastClicked: null,
  tab: "artists",
  treeSel: null,
  drag: null, // tree node or playlist entry IDs being dragged
  queueKey: "",
  folderPath: ".",
};

// ---------------------------------------------------------------- helpers

function el(tag, props = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (k === "class") e.className = v;
    else if (k === "data") Object.assign(e.dataset, v);
    else e[k] = v;
  }
  e.append(...children.filter((c) => c !== null && c !== undefined && c !== false));
  return e;
}

const fmtTime = (s) => {
  if (!Number.isFinite(s) || s < 0) return "–:––";
  s = Math.floor(s);
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = String(s % 60).padStart(2, "0");
  return h ? `${h}:${String(m).padStart(2, "0")}:${sec}` : `${m}:${sec}`;
};

let toastTimer;
function toast(msg) {
  $("toast").textContent = msg;
  $("toast").hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => ($("toast").hidden = true), 3500);
}

async function signIn() {
  location.href = `/auth/login?return=${encodeURIComponent(location.pathname + location.hash)}`;
  throw new Error("signed out");
}

let accessToken = "", accessExpires = 0, tokenRequest = null;
async function getAccessToken(force = false) {
  if (!force && accessToken && Date.now() + 30000 < accessExpires) return accessToken;
  if (!tokenRequest) tokenRequest = (async () => {
    const res = await fetchRetry("/auth/token", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}", cache: "no-store" }, {retry: true});
    if (res.status === 401) return signIn();
    if (!res.ok) throw new Error("Cannot renew login");
    const token = await res.json();
    accessToken = token.accessToken;
    accessExpires = Date.parse(token.expiresAt);
    return accessToken;
  })().finally(() => { tokenRequest = null; });
  return tokenRequest;
}
async function authorizedFetch(path, options = {}) {
  let token = await getAccessToken();
  const request = () => fetchRetry(`/api/${path}`, { ...options, headers: { ...options.headers, Authorization: `Bearer ${token}` } });
  let res = await request();
  if (res.status === 401) { token = await getAccessToken(true); res = await request(); }
  if (res.status === 401) return signIn();
  return res;
}
async function api(path, options = {}) {
  const res = await authorizedFetch(path, options);
  if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);
  return res.json();
}
async function post(path, body) {
  const res = await authorizedFetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
}

async function playbackCommand(op, extra = {}, target = null) {
  // Opening a tab does not move playback. Explicit Play may join alongside
  // offline tabs, but persistent web sinks are never treated as abandoned.
  const queue = player();
  const startsPlayback = op === "play" || op === "replace" ||
    (op === "toggle" && queue?.state !== "playing") || extra.play;
  const abandonedBrowsers = state.outputIds.length > 0 && state.outputIds.every((id) => {
    const output = player(id);
    return output?.kind === "browser" && !output.connected;
  });
  if ((target === null || target === me) && startsPlayback && abandonedBrowsers) {
    if (!state.playersReady || !player(me)?.connected) throw new Error("This browser is reconnecting. Try Play again once connected.");
    await post("players/me/command", { op: "outputs", outputs: [...new Set([...state.outputIds, me])] });
    toast("Playing in this browser; the previous browser is offline.");
  }
  const endpoint = target === null || target === me ? "me" : target;
  return post(`players/${endpoint}/command`, { op, ...extra });
}

function cmd(op, extra = {}, target = null) {
  // Media controls in an inactive browser must not control another output.
  if (target === me && !state.outputIds.includes(me)) return Promise.resolve();
  return playbackCommand(op, extra, target).catch((e) => toast(e.message));
}

const toTrack = (i) => ({ path: i.path, title: i.title, artist: i.artist, album: i.album, duration: i.duration, codec: i.codec, videoCodec: i.videoCodec, kind: i.kind, size: i.size, bitrate: i.bitrate, sampleRate: i.sampleRate, channels: i.channels, bits: i.bits });

function browserName() {
  const ua = navigator.userAgent;
  const b = /Edg\//.test(ua) ? "Edge" : /Firefox\//.test(ua) ? "Firefox" : /Chrome\//.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : "Browser";
  const os = /Android/.test(ua) ? "Android" : /iPhone|iPad/.test(ua) ? "iOS" : /Mac OS/.test(ua) ? "macOS" : /Windows/.test(ua) ? "Windows" : /Ubuntu/i.test(ua) ? "Ubuntu" : "";
  return `${b}${os ? " on " + os : ""}`;
}

// ---------------------------------------------------------------- player state

const player = (id = state.queueId) => state.players.get(id)?.p;

// pos extrapolates a player's position from when its state arrived.
function pos(id) {
  if (id === state.queueId && state.outputIds.includes(me) && engine.loaded && player()?.current === engine.loaded.id) return enginePos();
  const rec = state.players.get(id);
  if (!rec) return 0;
  const { p, at } = rec;
  let t = p.position + (p.state === "playing" ? (performance.now() - at) / 1000 : 0);
  const d = current(p)?.duration;
  return d ? Math.min(t, d) : t;
}

function put(p) {
  state.players.set(p.id, { p, at: performance.now() });
  if (p.kind === "user") {
    state.queueId = p.id;
    state.selected = p.outputId || "";
    state.outputIds = p.outputIds || (p.outputId ? [p.outputId] : []);
  }
  if (p.id === me) reconcile(p);
}

function receivePlayers(type, data) {
  if (type === "snapshot") {
    state.players.clear();
    for (const p of JSON.parse(data)) put(p);
    state.playersReady = true;
    render();
    renderTree();
  } else if (type === "player") { put(JSON.parse(data).player); render(); }
  else if (type === "removed") { state.players.delete(JSON.parse(data).id); render(); }
}
// Fetch-based SSE sends the access token in Authorization, never in the URL.
async function connect() {
  let backoff = 1000;
  for (;;) {
    try {
      const res = await authorizedFetch(`players/events?player=${me}&name=${encodeURIComponent(browserName())}`);
      if (!res.ok || !res.body) throw new Error("Event stream unavailable");
      backoff = 1000;
      const reader = res.body.getReader(), decoder = new TextDecoder();
      let buffer = "", event = "", data = [];
      try {
        for (;;) {
          const chunk = await reader.read();
          if (chunk.done) break;
          buffer += decoder.decode(chunk.value, { stream: true });
          if (buffer.length > 64 * 1024 * 1024) throw new Error("Event too large");
          let newline;
          while ((newline = buffer.indexOf("\n")) >= 0) {
            const line = buffer.slice(0, newline).replace(/\r$/, ""); buffer = buffer.slice(newline + 1);
            if (!line) { if (data.length) receivePlayers(event, data.join("\n")); event = ""; data = []; }
            else if (line.startsWith("event:")) event = line.slice(6).trimStart();
            else if (line.startsWith("data:")) data.push(line.slice(5).trimStart());
          }
        }
      } finally { await reader.cancel().catch(() => {}); }
    } catch (error) { console.debug("Playback events reconnecting:", error.message); }
    state.playersReady = false;
    render();
    await new Promise((resolve) => setTimeout(resolve, backoff));
    backoff = Math.min(backoff * 2, 10000);
  }
}

// ---------------------------------------------------------------- this browser as an output

const quality = () => localStorage.getItem("slopify-quality") || "original";
const videoFormat = (entry) => engine.profile(entry);
const enginePos = () => engine.position();
const quiet = () => engine.quiet();
const load = (entry, at, profile, recovery = false) => engine.load(entry, at, profile, recovery);
const reconcile = (playback) => engine.sync(playback);

async function streamURL(entry, profile, start) {
  const query = new URLSearchParams({path: entry.path, format: profile});
  if (start) query.set("start", start.toFixed(3));
  if (entry.videoCodec === "hevc") query.set("videoCodec", "hevc");
  return (await api("stream?" + query)).url;
}

function report(extra = {}) {
  const p = player(me);
  if (!p || !engine.loaded || (engine.recovering && !extra.blocked)) return;
  post(`players/${me}/report`, {
    version: p.version, entry: engine.loaded.id, position: enginePos(),
    state: audio.paused ? "paused" : "playing", ...extra,
  }).catch(() => {});
}

onMedia("ended", () => report({ ended: true, state: "paused" }));
// Native video controls also need a fresh server stream when seeking.
onMedia("seeking", () => {
  if (!["video-aac-192", "video-copy"].includes(engine.format) || engine.loading || performance.now() < engine.quietUntil) return;
  const at = enginePos();
  cmd("seek", { position: at });
});
// Play and pause from the lock screen, headphones or the media keys change
// the output's state; changes this code made itself are not reported.
onMedia("pause", () => {
  // Chrome may queue pause just before its network error event. Let that
  // error arrive before treating this as an intentional user pause.
  if (performance.now() <= engine.quietUntil) return;
  setTimeout(() => {
    if (audio.paused && !audio.ended && !audio.error && !engine.recovering && navigator.onLine !== false && performance.now() > engine.quietUntil) report();
  }, 200);
});
onMedia("play", () => performance.now() > engine.quietUntil && report());
setInterval(() => !audio.paused && report(), 5000);

$("unblock").addEventListener("click", () => {
  $("blocked").hidden = true;
  engine.blocked = false;
  audio.play().catch(() => {});
  cmd("play", {}, me);
});

if ("mediaSession" in navigator) {
  navigator.mediaSession.setActionHandler("play", () => cmd("play", {}, me));
  navigator.mediaSession.setActionHandler("pause", () => cmd("pause", {}, me));
  navigator.mediaSession.setActionHandler("previoustrack", () => cmd("prev", {}, me));
  navigator.mediaSession.setActionHandler("nexttrack", () => cmd("next", {}, me));
  navigator.mediaSession.setActionHandler("seekto", (d) => cmd("seek", { position: d.seekTime }, me));
}

// ---------------------------------------------------------------- virtual player and playlist

function render() {
  // Every selected output must remain visible, including offline secondary tabs.
  const outputs = visibleOutputs();
  renderDevices(outputs);

  const p = player();
  if (!p) return;
  const e = current(p);
  $("play").textContent = p.state === "playing" ? "⏸" : "▶";
  $("play").setAttribute("aria-label", p.state === "playing" ? "Pause" : "Play");
  $("connected-output").textContent = outputSummary();
  $("now-title").textContent = e ? `${e.title}${e.artist ? " — " + e.artist : ""}` : "Nothing playing";
  document.title = e && p.state === "playing" ? `${e.title} · Slopify` : "Slopify";
  $("hero-title").textContent = e?.title || "Nothing playing";
  $("hero-artist").textContent = e ? [e.artist, e.album].filter(Boolean).join(" · ") : "Find something you love in your library.";
  renderVideo();
  renderPlaylist(p);
  tick();
}

function renderPlaylist(p) {
  const queue = p.queue || [];
  const key = p.id + ":" + queue.map((e) => e.id).join(",");
  const tbody = $("playlist").tBodies[0];
  if (key !== state.queueKey) {
    state.queueKey = key;
    const ids = new Set(queue.map((e) => e.id));
    for (const id of state.sel) if (!ids.has(id)) state.sel.delete(id);
    tbody.replaceChildren(...queue.map((e, i) => el("tr", { draggable: true, data: { id: e.id } },
      el("td", { class: "c-num" }, String(i + 1)),
      el("td", { title: codecTooltip(e), class: "queue-title" },
        el("button", { class: "queue-play", ariaLabel: `Play ${e.title}`, title: "Play now", onclick: (ev) => { ev.stopPropagation(); cmd("play", { id: e.id }); } }, "▶"),
        el("span", {}, `${isVideo(e) ? "▣ " : ""}${e.title}`)),
      el("td", {}, e.artist || ""), el("td", {}, e.album || ""),
      el("td", { class: "c-len" }, e.duration ? fmtTime(e.duration) : ""),
      el("td", { class: "c-actions" }, el("button", { class: "queue-remove", ariaLabel: `Remove ${e.title} from queue`, title: "Remove from queue", onclick: (ev) => { ev.stopPropagation(); cmd("remove", { ids: [e.id] }); } }, "×")))));
    const total = queue.reduce((s, e) => s + (e.duration || 0), 0);
    $("status").textContent = `${queue.length} queued item${queue.length === 1 ? "" : "s"} · ${fmtTime(total)}`;
  }
  $("pl-empty").hidden = queue.length > 0;
  $("clear").disabled = queue.length === 0;
  for (const tr of tbody.rows) {
    const id = Number(tr.dataset.id);
    tr.classList.toggle("current", id === p.current);
    tr.classList.toggle("selected", state.sel.has(id));
  }
}

function tick() {
  const p = player();
  const e = current(p);
  const t = pos(state.queueId);
  $("pos").textContent = fmtTime(e ? t : 0);
  $("dur").textContent = fmtTime(e?.duration);
  const seek = $("seek");
  seek.max = Math.floor(e?.duration || 0);
  seek.disabled = !e?.duration;
  if (!seek.matches(":active")) seek.value = Math.floor(t);
  renderNerd();
}
setInterval(tick, 500);

async function setOutput(id, enabled) {
  if (!state.playersReady) return toast("Reconnecting to playback…");
  const existing = state.outputIds.filter((output) => state.players.has(output));
  const ids = enabled ? [...new Set([...existing, id])] : existing.filter((x) => x !== id);
  try { await post("players/me/command", { op: "outputs", outputs: ids }); }
  catch (e) { toast(e.message); render(); }
}
async function attachOutput(id) { await setOutput(id, true); }

function visibleOutputs() {
  const outputs = [...state.players.values()].map((r) => r.p)
    .filter((p) => p.kind !== "user" && (p.kind !== "browser" || p.connected || p.id === me || state.outputIds.includes(p.id)));
  for (const id of state.outputIds) {
    if (!outputs.some((p) => p.id === id)) outputs.push({id, kind: "unknown", name: "Unavailable output", adopted: true, connected: false});
  }
  return outputs.sort((a, b) => (a.id === me ? -1 : b.id === me ? 1 : a.kind.localeCompare(b.kind) || a.name.localeCompare(b.name)));
}

function outputSummary() {
  const selected = state.outputIds.map((id) => player(id));
  const active = selected.filter((p) => p?.available);
  const offline = selected.length - active.length;
  const name = active.length === 1 ? active[0].id === me ? "This browser" : active[0].kind === "web-sink" ? `${active[0].name} · ${sessionCount(active[0].receivers)}` : active[0].name :
    active.length ? `${active.length} outputs` : selected.length ? "No outputs online" : "Choose outputs";
  return name + (offline ? ` · ${offline} offline` : "");
}

function sessionCount(count = 0) { return `${count} session${count === 1 ? "" : "s"}`; }

function renderDevices(outputs) {
  if (state.drag?.output) return;
  $("devices").replaceChildren(...outputs.map((p) => {
    const checked = state.outputIds.includes(p.id);
    const checkbox = el("input", { type: "checkbox", checked, disabled: !state.playersReady || !p.adopted || (!p.available && !checked),
      ariaLabel: `Play on ${p.id === me ? "this browser" : p.name}`,
      onchange: (ev) => setOutput(p.id, ev.target.checked) });
    const card = el("div", { class: "device-card" + (checked ? " attached" : "") + (!p.available ? " offline" : ""), draggable: p.adopted && p.available },
      el("label", { class: "device-choice" }, checkbox,
        el("span", { class: "device-info" }, el("strong", {}, p.id === me ? "This browser" : p.name),
          el("span", {}, p.kind === "web-sink" ? `Web sink · ${sessionCount(p.receivers)}` : p.connected ? p.kind === "device" ? "Audio output" : "Audio and video" : checked ? "Offline · selected" : "Offline"))),
      !p.adopted ? el("button", { onclick: () => cmd("adopt", {}, p.id) }, "Adopt") : null,
      p.kind === "web-sink" ? el("div", {class: "shared-output-actions"},
        el("button", {title: "Create a new link and invalidate the previous one", onclick: () => newOutputLink(p.id)}, "New link"),
        el("button", {onclick: async () => {
          try { await post(`players/${p.id}/command`, {op: "forget"}); toast("Browser output link revoked."); }
          catch (error) { toast(error.message); }
        }}, "Revoke")) : null);
    card.addEventListener("dragstart", (ev) => {
      state.drag = { output: p.id };
      ev.dataTransfer.effectAllowed = "copy";
      ev.dataTransfer.setData("text/plain", p.name);
      $("menu-backdrop").hidden = true;
      $("settings-panel").style.pointerEvents = "none";
    });
    return card;
  }));
}

function showOutputLink(value) {
  $("output-link-url").value = new URL(value, location.origin).href;
  $("output-link-result").hidden = false;
  $("output-link-url").focus();
  $("output-link-url").select();
}
async function newOutputLink(id) {
  try {
    const response = await authorizedFetch(`players/${id}/link`, {method: "POST"});
    if (!response.ok) throw new Error(await response.text());
    showOutputLink((await response.json()).url);
  } catch (error) { toast(error.message); }
}
$("create-output-link").addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = event.currentTarget.querySelector("button");
  button.disabled = true;
  try {
    const response = await authorizedFetch("players/links", {method: "POST",
      headers: {"Content-Type": "application/json"}, body: JSON.stringify({name: $("output-link-name").value})});
    if (!response.ok) throw new Error(await response.text());
    showOutputLink((await response.json()).url);
    $("output-link-name").value = "";
  } catch (error) { toast(error.message); }
  finally { button.disabled = false; }
});
$("copy-output-link").addEventListener("click", async () => {
  try { await navigator.clipboard.writeText($("output-link-url").value); toast("Output link copied."); }
  catch (_) { $("output-link-url").focus(); $("output-link-url").select(); toast("Copy the selected link."); }
});

const virtualPlayer = $("playlist-pane");
virtualPlayer.addEventListener("dragover", (ev) => {
  if (!state.drag?.output) return;
  ev.preventDefault();
  ev.dataTransfer.dropEffect = "move";
  virtualPlayer.classList.add("device-over");
});
virtualPlayer.addEventListener("dragleave", (ev) => {
  if (!virtualPlayer.contains(ev.relatedTarget)) virtualPlayer.classList.remove("device-over");
});
virtualPlayer.addEventListener("drop", (ev) => {
  if (!state.drag?.output) return;
  ev.preventDefault();
  const id = state.drag.output;
  state.drag = null;
  virtualPlayer.classList.remove("device-over", "device-dragging");
  attachOutput(id);
});
document.addEventListener("dragend", () => { virtualPlayer.classList.remove("device-over", "device-dragging"); $("settings-panel").style.pointerEvents = ""; $("menu-backdrop").hidden = $("settings-panel").hidden; });

$("quality").value = quality();
$("quality").addEventListener("change", (ev) => {
  localStorage.setItem("slopify-quality", ev.target.value);
  const p = player(me);
  if (p && engine.loaded && !isVideo(engine.loaded)) {
    const at = enginePos();
    load(engine.loaded, at, quality()).catch((err) => toast(err.message));
  }
});
$("play").addEventListener("click", () => cmd("toggle"));
$("stop").addEventListener("click", () => cmd("stop"));
$("prev").addEventListener("click", () => cmd("prev"));
$("next").addEventListener("click", () => cmd("next"));
$("seek").addEventListener("change", (ev) => cmd("seek", { position: Number(ev.target.value) }));
$("clear").addEventListener("click", () => cmd("clear"));

function removeSelected() {
  if (state.sel.size) cmd("remove", { ids: [...state.sel] });
}

const tbody = $("playlist").tBodies[0];
tbody.addEventListener("click", (ev) => {
  const tr = ev.target.closest("tr");
  if (!tr) return;
  const id = Number(tr.dataset.id);
  const ids = [...tbody.rows].map((r) => Number(r.dataset.id));
  if (ev.shiftKey && state.lastClicked !== null) {
    const [a, b] = [ids.indexOf(state.lastClicked), ids.indexOf(id)].sort((x, y) => x - y);
    if (!ev.ctrlKey && !ev.metaKey) state.sel.clear();
    ids.slice(a, b + 1).forEach((x) => state.sel.add(x));
  } else if (ev.ctrlKey || ev.metaKey || matchMedia("(pointer: coarse)").matches) {
    state.sel.has(id) ? state.sel.delete(id) : state.sel.add(id);
    state.lastClicked = id;
  } else {
    state.sel = new Set([id]);
    state.lastClicked = id;
  }
  renderPlaylist(player());
});
tbody.addEventListener("dblclick", (ev) => {
  if (ev.target.closest("button,input")) return;
  const tr = ev.target.closest("tr");
  if (tr) cmd("play", { id: Number(tr.dataset.id) });
});
tbody.addEventListener("dragstart", (ev) => {
  if (ev.target.closest("button,input")) { ev.preventDefault(); return; }
  const id = Number(ev.target.closest("tr").dataset.id);
  if (!state.sel.has(id)) state.sel = new Set([id]);
  state.drag = { rows: [...state.sel] };
  ev.dataTransfer.effectAllowed = "move";
  ev.dataTransfer.setData("text/plain", "slopify");
});

const wrap = document.querySelector(".table-wrap");
function clearDrop() {
  for (const r of tbody.querySelectorAll(".drop-before")) r.classList.remove("drop-before");
  wrap.classList.remove("drop-end");
}
wrap.addEventListener("dragover", (ev) => {
  if (!state.drag || state.drag.output) return;
  ev.preventDefault();
  clearDrop();
  const tr = ev.target.closest("tbody tr");
  if (tr) tr.classList.add("drop-before");
  else wrap.classList.add("drop-end");
});
wrap.addEventListener("dragleave", (ev) => !wrap.contains(ev.relatedTarget) && clearDrop());
wrap.addEventListener("drop", async (ev) => {
  if (state.drag?.output) return;
  ev.preventDefault();
  clearDrop();
  const drag = state.drag;
  state.drag = null;
  const before = Number(ev.target.closest("tbody tr")?.dataset.id || 0);
  if (drag?.rows) cmd("move", { ids: drag.rows, before });
  else if (drag?.node) enqueue(drag.node, before ? "insert" : "append", { before });
});
document.addEventListener("dragend", () => { state.drag = null; clearDrop(); });

// ---------------------------------------------------------------- library tree

// A node knows how to list its children and collect its tracks.
function trackNode(item) {
  return {
    label: item.title, meta: item.duration ? fmtTime(item.duration) : "", item,
    badge: item.tagged ? null : "guessed",
    tracks: async () => [toTrack(item)],
  };
}

const MAX_ADD = 300;
async function folderTracks(path, node) {
  const result = await api(`catalog/tracks?path=${encodeURIComponent(path)}&limit=${MAX_ADD}`);
  if (node) node.truncated = result.truncated;
  return result.items.map(toTrack);
}

function navigateFolder(path) {
  state.folderPath = path;
  $("filter").value = "";
  renderTree();
}

function renderFolderNavigation() {
  $("folder-navigation").hidden = state.tab !== "folders";
  if (state.tab !== "folders") return;
  const crumbs = [el("button", { onclick: () => navigateFolder(".") }, "Music")];
  const parts = state.folderPath === "." ? [] : state.folderPath.split("/");
  parts.forEach((name, i) => {
    const path = parts.slice(0, i + 1).join("/");
    crumbs.push(el("span", { class: "muted" }, "›"), el("button", { onclick: () => navigateFolder(path) }, name));
  });
  $("folder-breadcrumbs").replaceChildren(...crumbs);
  $("queue-folder").disabled = !!$("filter").value.trim();
}
$("queue-folder").addEventListener("click", () => enqueue(folderNode({ path: state.folderPath, name: "Current folder" }), "append"));

function albumNode(a) {
  return {
    label: a.album, meta: a.year || "",
    children: async () => (await api(`catalog/browse?path=${encodeURIComponent(a.dir)}`)).items.filter((i) => i.kind === "audio").map(trackNode),
    tracks: async () => (await api(`catalog/browse?path=${encodeURIComponent(a.dir)}`)).items.filter((i) => i.kind === "audio").map(toTrack),
  };
}

function folderNode(d) {
  const node = { label: d.name, folder: true, meta: "Folder", path: d.path,
    open: () => navigateFolder(d.path),
    tracks: () => folderTracks(d.path, node) };
  return node;
}

async function rootNodes(signal) {
  const read = (path) => api(path, {signal});
  const q = $("filter").value.trim();
  if (q && state.tab === "folders") {
    const [dirs, files] = await Promise.all([
      read(`catalog/search?scope=folders&q=${encodeURIComponent(q)}`),
      read(`catalog/search?q=${encodeURIComponent(q)}`),
    ]);
    return [...dirs.map((d) => {
      const node = folderNode(d);
      node.meta = d.path;
      return node;
    }), ...files.map(trackNode)];
  }
  if (q) return (await read(`catalog/search?q=${encodeURIComponent(q)}`)).map((i) => {
    const n = trackNode(i);
    n.meta = [i.artist, i.album].filter(Boolean).join(" · ");
    return n;
  });
  switch (state.tab) {
    case "artists":
      return (await read("catalog/artists")).map((a) => ({
        label: a.name || "Unknown artist", meta: `${a.albums.length}`,
        children: async () => a.albums.map(albumNode),
        tracks: async () => {
          const tracks = [];
          for (const album of a.albums) {
            tracks.push(...await albumNode(album).tracks());
            if (tracks.length >= MAX_ADD) break;
          }
          return tracks;
        },
      }));
    case "folders": {
      const b = await read(`catalog/browse?path=${encodeURIComponent(state.folderPath)}`);
      return [...b.dirs.map(folderNode), ...b.items.map(trackNode)];
    }
    case "videos":
      return (await read("catalog/videos")).map((v) => ({ ...trackNode(v), meta: v.dir }));
  }
  return [];
}

function nodeRow(node) {
  const li = el("li");
  const twisty = el("span", { class: "twisty" }, node.children ? "▸" : "");
  const row = el("div", { class: "row" + (node.folder ? " folder-row" : ""), draggable: !!node.tracks },
    twisty,
    node.folder ? el("button", { class: "folder-link", title: node.path, onclick: () => navigateFolder(node.path) }, "▱ " + node.label) : el("span", { class: "label", title: node.item ? codecTooltip(node.item) : node.label }, node.label),
    node.badge ? el("span", { class: "badge", title: "No tags; guessed from the file name" }, node.badge) : null,
    el("span", { class: "meta" }, node.meta || ""),
    el("button", { class: "item-add", title: "Append to end of queue", ariaLabel: "Append " + node.label + " to end of queue",
      onclick: (ev) => { ev.stopPropagation(); enqueue(node, "append"); } }, "+"),
    !node.folder ? el("button", { class: "item-play", title: "Play now", ariaLabel: "Play " + node.label,
      onclick: (ev) => { ev.stopPropagation(); enqueue(node, "next-up", { play: true }); } }, "▶") : null);
  let open = false;
  let sub;
  async function toggle() {
    if (!node.children) return;
    open = !open;
    twisty.textContent = open ? "▾" : "▸";
    if (open) {
      sub = el("ul");
      li.append(sub);
      sub.append(...(await node.children()).map(nodeRow));
    } else sub?.remove();
  }
  twisty.addEventListener("click", (ev) => { ev.stopPropagation(); toggle(); });
  row.addEventListener("click", () => selectNode(node, row));
  row.addEventListener("dblclick", (ev) => { if (!ev.target.closest("button,input")) activate(node); });
  row.addEventListener("dragstart", (ev) => {
    state.drag = { node };
    ev.dataTransfer.effectAllowed = "copy";
    ev.dataTransfer.setData("text/plain", node.label);
  });
  node.toggle = toggle;
  li.append(row);
  return li;
}

function selectNode(node, row) {
  for (const r of $("tree").querySelectorAll(".row.selected")) r.classList.remove("selected");
  row.classList.add("selected");
  state.treeSel = node;
  updateLibraryActions();
}

function updateLibraryActions() {
  const results = !!state.treeSel?.searchResults;
  $("add").textContent = results ? "Add results to queue" : "Add to queue";
  $("add-next").textContent = results ? "Play results next" : "Play next";
  $("replace").textContent = results ? "Play results now" : "Play now";
  for (const id of ["add", "add-next", "replace"]) {
    $(id).disabled = !state.treeSel?.tracks;
    $(id).title = results ? "Up to 300 unique tracks from these search results" : state.treeSel ? state.treeSel.label : "Select an item or search your collection";
  }
}

function searchResultsNode(nodes) {
  const result = { searchResults: true, label: "Search results", tracks: async () => {
    result.truncated = false;
    const tracks = [], seen = new Set();
    for (const node of nodes) {
      for (const track of await node.tracks()) {
        if (seen.has(track.path)) continue;
        seen.add(track.path);
        tracks.push(track);
        if (tracks.length > MAX_ADD) { result.truncated = true; return tracks; }
      }
      if (node.truncated) result.truncated = true;
    }
    return tracks;
  }};
  return result;
}

function activate(node) {
  if (node.folder) return node.open();
  if (node.tracks) enqueue(node, "next-up", { play: true });
}

async function enqueue(node, op, extra = {}) {
  try {
    const all = await node.tracks();
    const tracks = all.slice(0, MAX_ADD);
    if (!tracks.length) return toast("No tracks there.");
    await playbackCommand(op, { tracks, ...extra });
    setMenu(false);
    const capped = node.truncated || all.length > MAX_ADD;
    const placement = op === "append" ? " to end of queue" : op === "next-up" && !extra.play ? " to play next" : "";
    toast(`${extra.play || op === "replace" ? "Playing" : "Added"} ${tracks.length} tracks${placement}${capped ? " · limited to first 300" : ""}`);
  } catch (e) { toast(e.message); }
}

let treeToken = 0, libraryRequest;
async function renderTree() {
  const token = ++treeToken;
  libraryRequest?.abort();
  libraryRequest = new AbortController();
  const signal = libraryRequest.signal;
  state.treeSel = null;
  updateLibraryActions();
  renderFolderNavigation();
  $("filter").placeholder = state.tab === "folders" ? "Find a folder…" : "Search everything…";
  const tree = $("tree");
  tree.replaceChildren(el("li", { class: "hint" }, "Loading…"));
  try {
    const nodes = await rootNodes(signal);
    if (token !== treeToken) return;
    tree.replaceChildren(...(nodes.length ? nodes.map(nodeRow) : [el("li", { class: "hint" }, "Nothing here.")]));
    if ($("filter").value.trim() && nodes.length) state.treeSel = searchResultsNode(nodes);
    updateLibraryActions();
  } catch (e) {
    if (token !== treeToken || signal.aborted) return;
    tree.replaceChildren(el("li", { class: "hint" }, `Could not load: ${e.message}`));
  }
}

for (const b of document.querySelectorAll(".tabs button")) {
  b.addEventListener("click", () => {
    state.tab = b.dataset.tab;
    for (const x of document.querySelectorAll(".tabs button")) x.classList.toggle("active", x === b);
    $("filter").value = "";
    renderTree();
  });
}
let filterTimer;
$("filter").addEventListener("input", () => {
  clearTimeout(filterTimer);
  ++treeToken; // Invalidate pending searches as soon as the query changes.
  state.treeSel = null;
  updateLibraryActions();
  filterTimer = setTimeout(renderTree, 250);
});
$("add").addEventListener("click", () => state.treeSel && enqueue(state.treeSel, "append"));
$("add-next").addEventListener("click", () => state.treeSel && enqueue(state.treeSel, "next-up"));
$("replace").addEventListener("click", () => state.treeSel && enqueue(state.treeSel, "next-up", { play: true }));

// ---------------------------------------------------------------- video, keyboard, mobile

function renderVideo() {
  const entry = current(player());
  const available = state.outputIds.includes(me) && isVideo(entry) && engine.loaded?.id === entry?.id;
  $("video-dock").hidden = !available;
}

const mobileVideoLayout = matchMedia("(max-width: 760px)");
function positionVideo() {
  const dock = $("video-dock");
  const slot = $(mobileVideoLayout.matches ? "mobile-video-slot" : "desktop-video-slot");
  if (dock.parentElement === slot) return;
  // Preserve media state while changing the responsive layout.
  if (slot.moveBefore) slot.moveBefore(dock, null);
  else {
    const resume = !$("video").paused;
    quiet();
    slot.append(dock);
    if (resume) $("video").play().catch((err) => toast(err.message));
  }
}
mobileVideoLayout.addEventListener("change", positionVideo);
positionVideo();
async function fullscreenVideo() {
  const video = $("video");
  if (document.fullscreenElement || video.webkitDisplayingFullscreen) return;
  try {
    video.controls = true;
    if (video.requestFullscreen) await video.requestFullscreen();
    else if (video.webkitEnterFullscreen) video.webkitEnterFullscreen();
    else { video.controls = false; toast("Fullscreen is unavailable in this browser."); }
  } catch (err) { video.controls = false; toast(err.message); }
}
$("video").addEventListener("click", () => {
  if (!document.fullscreenElement && !$("video").webkitDisplayingFullscreen) fullscreenVideo();
});
$("video").addEventListener("keydown", (ev) => {
  if ((ev.key === "Enter" || ev.key === " ") && !document.fullscreenElement && !$("video").webkitDisplayingFullscreen) {
    ev.preventDefault(); ev.stopPropagation(); fullscreenVideo();
  }
});
document.addEventListener("fullscreenchange", () => { $("video").controls = document.fullscreenElement === $("video"); });
$("video").addEventListener("webkitbeginfullscreen", () => { $("video").controls = true; });
$("video").addEventListener("webkitendfullscreen", () => { $("video").controls = false; });
onMedia("seeked", () => { if (performance.now() > engine.quietUntil) report(); });

document.addEventListener("keydown", (ev) => {
  if (document.fullscreenElement || $("video").webkitDisplayingFullscreen) return;
  if (ev.key === "Escape") { setMenu(false); return; }
  if (ev.target.closest("input, select, textarea") || ev.ctrlKey || ev.metaKey || ev.altKey) return;
  const inTree = ev.target.closest("#tree");
  switch (ev.key) {
    case " ": ev.preventDefault(); cmd("toggle"); break;
    case "Delete": case "Backspace": removeSelected(); break;
    case "Enter":
      if (inTree && state.treeSel) activate(state.treeSel);
      else if (state.sel.size) cmd("play", { id: [...state.sel][0] });
      break;
    // Winamp and foobar2000 transport keys.
    case "z": cmd("prev"); break;
    case "x": cmd("play"); break;
    case "c": cmd("pause"); break;
    case "v": cmd("stop"); break;
    case "b": cmd("next"); break;
    default: return;
  }
});

function setMenu(open) {
  $("settings-panel").hidden = true;
  $("library").classList.toggle("menu-open", open);
  $("menu-toggle").setAttribute("aria-expanded", "false");
  $("library-toggle").setAttribute("aria-expanded", String(open));
  $("menu-backdrop").hidden = !open;
}
function setSettings(open) {
  setMenu(false);
  $("settings-panel").hidden = !open;
  $("settings-panel").style.pointerEvents = "";
  $("menu-toggle").setAttribute("aria-expanded", String(open));
  $("menu-backdrop").hidden = !open;
}
$("menu-toggle").addEventListener("click", () => setSettings($("settings-panel").hidden));
$("library-toggle").addEventListener("click", () => setMenu(!$("library").classList.contains("menu-open")));
$("menu-backdrop").addEventListener("click", () => setMenu(false));
$("menu-close").addEventListener("click", () => setMenu(false));
$("device-target").addEventListener("click", () => setSettings(true));

// ---------------------------------------------------------------- playback diagnostics

function setNerd(enabled) {
  localStorage.setItem("slopify-nerd", String(enabled));
  $("nerd-toggle").checked = enabled;
  $("nerd-panel").hidden = !enabled;
  document.querySelector(".panes").classList.toggle("with-nerd", enabled);
  renderNerd();
}
function localStreamBitrate(entry) {
  if (!state.outputIds.includes(me)) return null;
  if (isVideo(entry)) return videoFormat(entry) === "video-aac-192" ? "Video copied · AAC stereo · 192 kbps target" : "Original codecs · no transcoding";
  if (quality() === "original") return null;
  return quality() === "opus" ? "Opus · 128 kbps target" : "MP3 · 192 kbps target";
}
function renderNerd() {
  if ($("nerd-panel").hidden) return;
  const q = player(), e = current(q), output = player(state.selected);
  const details = e ? state.trackDetails.get(e.path) : null;
  if (e && !state.trackDetails.has(e.path)) {
    state.trackDetails.set(e.path, null);
    api(`catalog/item?path=${encodeURIComponent(e.path)}`).then((item) => { state.trackDetails.set(e.path, item); renderNerd(); }).catch(() => {});
  }
  const size = e?.size || details?.size;
  const duration = e?.duration || details?.duration;
  const measuredBitrate = e?.bitrate || details?.bitrate;
  const averageBitrate = size && duration ? Math.round(size * 8 / duration / 1000) : null;
  const streamBitrate = localStreamBitrate(e);
  const rec = state.players.get(state.queueId);
  const local = state.outputIds.includes(me);
  const rows = [
    ["Output", output ? output.kind === "web-sink" ? `${output.name} · web sink · ${sessionCount(output.receivers)}` : `${output.name} · ${output.kind} · ${output.connected ? "online" : "offline"}` : "None"],
    ["Queue / entry", `${q?.queue?.length || 0} tracks · entry ${q?.current || "none"} · revision ${q?.version || 0}`],
    ["State", `${q?.state || "connecting"}${q?.blocked ? " · audio permission required" : ""}`],
    ["File", e?.path || "—"],
    ["Audio codec", e?.codec || details?.codec || "Unknown"],
    ...(isVideo(e) ? [["Video codec", e?.videoCodec || details?.videoCodec || "Unknown"]] : []),
    ["Source bitrate", measuredBitrate ? `${Math.round(measuredBitrate / 1000)} kbps (ffprobe)` : averageBitrate ? `~${averageBitrate} kbps (whole-file average)` : "Unavailable in catalog metadata"],
    ["Output bitrate", streamBitrate || "Original · variable bitrate"],
    ["Selected outputs", `${state.outputIds.length}`],
    ["Duration / position", `${fmtTime(e?.duration)} / ${fmtTime(pos(state.queueId))}`],
    ["Stream", local ? isVideo(e) ? videoFormat(e) === "original" ? "Original · HTTP range seeking" : videoFormat(e) === "video-copy" ? "Original codecs · MP4 remux" : "Video copy + AAC · restart on seek (keyframe aligned)" : quality() === "original" ? "Original · HTTP range seeking" : `${quality()} · server transcoding` : "Original · decoded on device"],
    ["Last state update", rec ? `${((performance.now() - rec.at) / 1000).toFixed(1)} s ago` : "—"],
  ];
  if (local) {
    const ready = ["No data", "Metadata", "Current frame", "Future data", "Enough data"];
    const network = ["Empty", "Idle", "Loading", "No source"];
    let ahead = 0;
    const ranges = [];
    for (let i = 0; i < audio.buffered.length; i++) {
      const start = audio.buffered.start(i), end = audio.buffered.end(i);
      ranges.push(`${start.toFixed(1)}–${end.toFixed(1)} s`);
      if (audio.currentTime >= start && audio.currentTime <= end) ahead = end - audio.currentTime;
    }
    rows.push(["Browser buffer", `${ahead.toFixed(1)} s ahead`],
      ["Buffered ranges", ranges.join(", ") || "Empty"],
      ["Media / network", `${ready[audio.readyState] || "Unknown"} / ${network[audio.networkState] || "Unknown"}`],
      ["Time to first audio", engine.startupMs === null ? "Waiting for playback" : `${Math.round(engine.startupMs)} ms`],
      ["Recovery", engine.recovering ? `Reconnecting from ${fmtTime(engine.lastPosition)}` : "Not needed"],
      ["Media error", engine.error || "None"]);
  } else {
    rows.push(["Decoder / buffer", "Remote output; browser buffer measurements don't apply"],
      ["Device", [output?.model, output?.address].filter(Boolean).join(" · ") || "—"]);
  }
  $("nerd-info").replaceChildren(...rows.flatMap(([key, value]) => [el("dt", {}, key), el("dd", {}, value)]));
  $("nerd-events").hidden = !local;
  $("nerd-events").replaceChildren(...engine.events.map((event) => el("li", {}, `${event.ms} ms · ${event.type}`)));
}
$("nerd-toggle").addEventListener("change", (ev) => setNerd(ev.target.checked));
$("nerd-close").addEventListener("click", () => setNerd(false));
setNerd(localStorage.getItem("slopify-nerd") === "true");

// Proxy failures during a redeploy are temporary, not a lost login.
window.addEventListener("slopify:network", (event) => {
  $("reconnect-status").hidden = !event.detail.retrying;
});

// ---------------------------------------------------------------- start

api("me").then((u) => ($("user").textContent = u.name || u.email || u.sub)).catch((error) => toast(error.message));
renderTree();
connect();
if ("serviceWorker" in navigator) navigator.serviceWorker.register("/sw.js");

document.querySelectorAll("[data-playlist]").forEach((button) => button.addEventListener("click", async () => {
  const format = button.dataset.playlist;
  button.disabled = true;
  try {
    const res = await authorizedFetch(`players/me/playlist.${format}`);
    if (!res.ok) throw new Error(await res.text());
    const url = URL.createObjectURL(await res.blob());
    const link = document.createElement("a");
    link.href = url;
    link.download = `slopify.${format}`;
    document.body.append(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 60000);
  } catch (err) { toast(err.message); }
  finally { button.disabled = false; }
}));

function codecTooltip(entry) {
  const details = state.trackDetails.get(entry.path);
  return [entry.path, isVideo(entry) ? `Video: ${entry.videoCodec || details?.videoCodec || "not indexed yet"}` : null,
    `Audio: ${entry.codec || details?.codec || "not indexed yet"}`].filter(Boolean).join("\n");
}
