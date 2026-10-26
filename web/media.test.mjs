import assert from "node:assert/strict";
import {readFile} from "node:fs/promises";
import test from "node:test";

// Load the browser ES module without adding Node/package metadata to the app.
const source = await readFile(new URL("./media.js", import.meta.url), "utf8");
const {MediaEngine, currentEntry} = await import("data:text/javascript;base64," + Buffer.from(source).toString("base64"));
globalThis.window = {addEventListener() {}};
Object.defineProperty(globalThis, "navigator", {value: {onLine: true}, configurable: true});

class Media extends EventTarget {
  paused = true;
  currentTime = 0;
  readyState = 4;
  allowed = true;
  _src = "";
  _metadata = false;
  get src() { return this._src; }
  set src(value) { this._src = value; this._metadata = false; this.currentTime = 0; this.paused = true; this.error = null; }
  getAttribute(name) { return name === "src" ? this._src || null : null; }
  removeAttribute(name) { if (name === "src") this._src = ""; }
  load() { this.currentTime = 0; this.paused = true; }
  pause() { if (!this.paused) { this.paused = true; this.emit("pause"); } }
  canPlayType(type) { return /mp4a|mpeg|flac|opus|vorbis|wav/.test(type) ? "probably" : ""; }
  emit(type) { this["on" + type]?.(); this.dispatchEvent(new Event(type)); }
  async play() {
    if (this._src && !this._metadata) { this._metadata = true; this.emit("loadedmetadata"); }
    if (!this.allowed) throw new DOMException("gesture required", "NotAllowedError");
    if (this.paused) { this.paused = false; this.emit("play"); this.emit("playing"); }
  }
}
const tick = () => new Promise(resolve => setImmediate(resolve));
const entry = (id, extra = {}) => ({id, path: id + ".mp3", kind: "audio", codec: "mp3", duration: 120, ...extra});
const playback = (track, extra = {}) => ({state: "playing", version: track.id, position: 0, current: track.id, queue: [track], ...extra});
function fixture(t, options = {}) {
  let state;
  const calls = [], audio = new Media(), video = new Media();
  const engine = new MediaEngine({
    audio, video, playback: () => state,
    streamURL: async (track, profile, start) => { calls.push({track, profile, start}); return "/stream/" + track.id; },
    ...options,
  });
  t.after(() => engine.close());
  return {engine, audio, video, calls, setState(next) { state = next; engine.sync(next); }};
}

test("profiles preserve supported originals and never transcode video", (t) => {
  const {engine} = fixture(t, {audioProfile: () => "auto"});
  assert.equal(currentEntry(undefined), null);
  assert.equal(engine.profile(entry(1)), "original");
  assert.equal(engine.profile(entry(1, {codec: "pcm_s24le"})), "original");
  assert.equal(engine.profile(entry(2, {path: "movie.mkv", codec: "aac", kind: "video"})), "original");
  assert.equal(engine.profile(entry(3, {path: "movie.mkv", codec: "dts", kind: "video"})), "video-aac-192");
});

test("stale stream responses cannot replace a newer track", async (t) => {
  const pending = [];
  const {engine, audio, setState} = fixture(t, {streamURL: (track) => new Promise(resolve => pending.push({track, resolve}))});
  setState(playback(entry(1)));
  await tick();
  setState(playback(entry(2)));
  await tick();
  assert.equal(pending.length, 2);
  pending[1].resolve("/new");
  await tick();
  pending[0].resolve("/old");
  await tick();
  assert.equal(engine.loaded.id, 2);
  assert.equal(audio.src, "/new");
});

test("seek received during loading replaces the pending request", async (t) => {
  const pending = [], track = entry(1);
  const {engine, audio, setState} = fixture(t, {streamURL: () => new Promise(resolve => pending.push(resolve))});
  setState(playback(track)); await tick();
  setState(playback(track, {version: 2, position: 42})); await tick();
  pending[0]("/old"); pending[1]("/new"); await tick();
  assert.equal(audio.currentTime, 42);
  assert.equal(engine.loaded.id, track.id);
});

test("video switches element and falls back to codec-copy remux", async (t) => {
  const {engine, video, calls, setState} = fixture(t);
  setState(playback(entry(1, {kind: "video", path: "movie.mkv", codec: "aac"})));
  await tick();
  assert.equal(engine.element, video);
  assert.equal(calls[0].profile, "original");
  video.error = {code: 4, message: "container unsupported"};
  video.emit("error"); await tick();
  assert.equal(calls[1].profile, "video-copy");
});

test("original seeks use currentTime; transcodes reopen at the offset", async (t) => {
  const original = fixture(t);
  original.setState(playback(entry(1))); await tick();
  original.engine.seek(42);
  assert.equal(original.audio.currentTime, 42);
  assert.equal(original.calls.length, 1);
  const transcode = fixture(t, {audioProfile: () => "opus"});
  transcode.setState(playback(entry(2))); await tick();
  transcode.engine.seek(42); await tick();
  assert.equal(transcode.calls[1].profile, "opus");
  assert.equal(transcode.calls[1].start, 42);
  assert.equal(transcode.engine.position(), 42);
});

test("network recovery resumes at the last played position", async (t) => {
  const {engine, audio, calls, setState} = fixture(t);
  setState(playback(entry(1), {position: 42})); await tick();
  audio.currentTime = 47; audio.emit("timeupdate");
  audio.error = {code: 2, message: "ERR_NETWORK_CHANGED"};
  audio.emit("error");
  assert(engine.recovering);
  engine.recover("network changed", true);
  await new Promise(resolve => setTimeout(resolve, 10));
  assert.equal(calls.length, 2);
  assert.equal(engine.position(), 47);
});

test("autoplay prompts once and read-only state does not become a command", async (t) => {
  let prompts = 0;
  const f = fixture(t, {readOnly: true, onBlocked: () => { prompts++; }});
  f.audio.allowed = false;
  const next = playback(entry(1));
  f.setState(next); await tick();
  f.setState(next); await tick();
  assert.equal(prompts, 1);
  assert.equal(f.engine.blocked, true);
  assert.equal(next.state, "playing");
  f.engine.blocked = false;
  f.audio.allowed = true;
  await f.engine.play();
  assert.equal(f.audio.paused, false);
});

test("revocation and stop invalidate pending loads and clear playback", async (t) => {
  let resolve;
  const f = fixture(t, {streamURL: () => new Promise(done => { resolve = done; })});
  f.setState(playback(entry(1))); await tick();
  f.engine.close(); resolve("/revoked"); await tick();
  assert.equal(f.audio.getAttribute("src"), null);
  const live = fixture(t, {audioProfile: () => "opus"});
  const track = entry(2);
  live.setState(playback(track, {position: 42})); await tick();
  live.setState(playback(track, {state: "stopped", version: 3}));
  assert.equal(live.engine.position(), 0);
  assert.equal(live.audio.getAttribute("src"), null);
  live.setState(playback(track, {version: 4})); await tick();
  assert.equal(live.calls[1].start, 0);
});
