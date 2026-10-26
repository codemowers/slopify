// Capability-only transport and read-only UI; playback is shared with app.js.
import { MediaEngine, isVideo, currentEntry } from "./media.js";
import { fetchRetry, probeStream } from "./network.js";

const $ = (id) => document.getElementById(id);
const match = /^#(link-[a-f0-9]{32})\.([A-Za-z0-9_-]{43})$/.exec(location.hash);
const id = match?.[1], token = match?.[2];
const receiver = crypto.randomUUID().replaceAll("-", "");
const requests = new AbortController();
let stopped = !match, state = null, receivedAt = 0;
const engine = new MediaEngine({
  audio: $("output-audio"), video: $("output-video"), readOnly: true,
  playback: () => state,
  probeStream,
  audioProfile: () => "auto",
  streamURL: async (entry, profile, start) => {
    const query = new URLSearchParams({entry: entry.id, format: profile, start: start.toFixed(3)});
    return (await (await request("stream?" + query)).json()).url;
  },
  onSource: (entry) => { $("output-video").hidden = !isVideo(entry); },
  onBlocked: () => {
    $("enable-output").hidden = false;
    $("connection").textContent = "Enable playback once to allow this browser to receive audio.";
    report({blocked: true});
  },
  onPlaying: () => {
    $("enable-output").hidden = true;
    $("connection").textContent = "Connected · read-only";
    report({blocked: false});
  },
  onError: (error) => { $("connection").textContent = error.message; },
});
const clock = (seconds = 0) => {
  seconds = Math.max(0, Math.floor(seconds));
  return [Math.floor(seconds / 3600), String(Math.floor(seconds / 60) % 60).padStart(2, "0"), String(seconds % 60).padStart(2, "0")].join(":");
};

function closeOutput(message) {
  stopped = true;
  requests.abort();
  engine.close();
  $("connection").textContent = message;
  $("enable-output").hidden = true;
}

async function request(path, options = {}) {
  const response = await fetchRetry(`/output-api/${id}/${path}`, {
    ...options, signal: requests.signal, credentials: "omit", cache: "no-store",
    headers: {...options.headers, Authorization: `Bearer ${token}`, "X-Slopify-Receiver": receiver},
  });
  if (response.status === 401) {
    closeOutput("This output link is invalid or has been revoked.");
    throw new Error("Output link closed");
  }
  if (!response.ok) throw new Error(`Output request failed (${response.status})`);
  return response;
}

function report(extra = {}) {
  if (!state || !engine.loaded || stopped || engine.loading || engine.recovering) return;
  request("report", {method: "POST", headers: {"Content-Type": "application/json"},
    body: JSON.stringify({version: state.version, entry: engine.loaded.id, position: engine.position(), ...extra}),
  }).catch(() => {});
}

function render(next) {
  const previous = state;
  state = next;
  receivedAt = performance.now();
  $("output-name").textContent = next.name;
  $("output-name").title = `${next.receivers} session${next.receivers === 1 ? "" : "s"} tuned in`;
  const entry = currentEntry(next);
  $("track-title").textContent = entry?.title || "Nothing playing";
  $("track-artist").textContent = [entry?.artist, entry?.album].filter(Boolean).join(" · ");
  document.title = `${next.name} · Slopify`;
  $("connection").textContent = engine.blocked ? "Enable playback in this browser." : "Connected · read-only";
  if (!previous || previous.current !== next.current || previous.queue.map(e => e.id).join() !== next.queue.map(e => e.id).join()) {
    $("output-queue").replaceChildren(...next.queue.map(e => {
      const row = document.createElement("li");
      row.textContent = [e.title, e.artist].filter(Boolean).join(" — ");
      row.classList.toggle("current", e.id === next.current);
      return row;
    }));
  }
  engine.sync(next);
}

$("enable-output").addEventListener("click", async () => {
  engine.blocked = false;
  $("enable-output").hidden = true;
  if (state?.state !== "playing") return;
  try {
    // Unlock directly from the gesture before requesting a fresh stream.
    await engine.element.play();
    const at = state.position + (performance.now() - receivedAt) / 1000;
    if (currentEntry(state) && Math.abs(engine.position() - at) > 2) await engine.load(currentEntry(state), at);
    report({blocked: false});
  } catch (error) {
    if (error.name === "NotAllowedError") { engine.blocked = true; $("enable-output").hidden = false; }
    else engine.recover("enable playback");
  }
});
$("output-video").addEventListener("click", async () => {
  try {
    if ($("output-video").requestFullscreen) await $("output-video").requestFullscreen();
    else $("output-video").webkitEnterFullscreen?.();
  } catch (_) {}
});
$("output-video").addEventListener("keydown", (event) => {
  if (event.key === "Enter") { event.preventDefault(); $("output-video").click(); }
});
engine.on("ended", () => report({ended: true}));
setInterval(() => {
  if (state) $("output-position").textContent = `${clock(engine.position())} / ${clock(currentEntry(state)?.duration)}`;
  if (!engine.element.paused && !engine.blocked) report();
}, 5000);

async function connect() {
  let delay = 1000;
  while (!stopped) {
    try {
      const response = await request("events");
      delay = 1000;
      const reader = response.body.getReader(), decoder = new TextDecoder();
      let buffer = "", event = "", data = [];
      try {
        while (!stopped) {
          const {value, done} = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, {stream: true});
          if (buffer.length > 64 * 1024 * 1024) throw new Error("Event too large");
          let end;
          while ((end = buffer.indexOf("\n")) >= 0) {
            const line = buffer.slice(0, end).replace(/\r$/, ""); buffer = buffer.slice(end + 1);
            if (!line) {
              if (event === "closed") { closeOutput("This output link was revoked or replaced."); break; }
              if (event === "player" && data.length) render(JSON.parse(data.join("\n")));
              event = ""; data = [];
            } else if (line.startsWith("event:")) event = line.slice(6).trimStart();
            else if (line.startsWith("data:")) data.push(line.slice(5).trimStart());
          }
        }
      } finally { await reader.cancel().catch(() => {}); }
    } catch (_) { if (!stopped) $("connection").textContent = "Reconnecting…"; }
    if (!stopped) { await new Promise(resolve => setTimeout(resolve, delay)); delay = Math.min(delay * 2, 10000); }
  }
}
window.addEventListener("slopify:network", (event) => {
  if (!stopped && event.detail.retrying) $("connection").textContent = "Reconnecting…";
});
if (stopped) $("connection").textContent = "Open the complete output link supplied by its owner.";
else connect();
