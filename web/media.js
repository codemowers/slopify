// Playback mechanics shared by authenticated controllers and read-only outputs.
// Stream authorization and queue commands belong to the caller, never here.
export const isVideo = (entry) => entry?.kind === "video" || /\.(mp4|m4v|mkv|webm|mov)$/i.test(entry?.path || "");
export const currentEntry = (state) => state?.queue?.find((entry) => entry.id === state.current) ||
  (state?.currentEntry && state.currentEntry.id === state.current ? state.currentEntry : null);
const key = (entry) => entry ? String(entry.id) + ":" + entry.path : "";
const audioTypes = {
  aac: 'audio/mp4; codecs="mp4a.40.2"', mp3: "audio/mpeg", flac: "audio/flac",
  opus: 'audio/ogg; codecs="opus"', vorbis: 'audio/ogg; codecs="vorbis"',
  ac3: 'audio/mp4; codecs="ac-3"', eac3: 'audio/mp4; codecs="ec-3"',
  alac: 'audio/mp4; codecs="alac"',
};
const diagnosticEvents = ["loadstart", "loadedmetadata", "loadeddata", "canplay", "playing", "waiting",
  "stalled", "suspend", "seeking", "seeked", "error"];

export class MediaEngine {
  constructor(options) {
    this.options = options;
    this.elements = [options.audio, options.video];
    this.element = options.audio;
    Object.assign(this, {
      loaded: null, format: null, offset: 0, applied: -1, quietUntil: 0,
      startedAt: 0, startupMs: null, events: [], error: "", blocked: false,
      retryTimer: null, retryDelay: 1000, recovering: false, lastPosition: 0,
      generation: 0, loading: null, pending: null, closed: false,
    });
    this.fallback = new Map();
    this.on("timeupdate", () => { if (!this.recovering) this.lastPosition = this.position(); });
    this.on("playing", () => {
      this.cancelRecovery();
      this.blocked = false;
      this.lastPosition = this.position();
      this.options.onPlaying?.();
    });
    this.on("error", () => this.mediaError());
    this.on("stalled", () => { if (this.element.readyState < 3) this.recover("stalled stream"); });
    if (options.readOnly) this.on("pause", () => setTimeout(() => {
      if (this.element.paused && !this.element.ended && !this.loading && !this.blocked &&
          performance.now() > this.quietUntil) this.play();
    }, 200));
    for (const type of diagnosticEvents) this.on(type, () => {
      const ms = Math.round(performance.now() - this.startedAt);
      this.events.push({type, ms});
      this.events = this.events.slice(-12);
      if (type === "playing" && this.startupMs === null) this.startupMs = ms;
      this.options.onEvent?.(type);
    });
    window.addEventListener("online", () => this.recover("network restored", true));
    navigator.connection?.addEventListener("change", () => {
      if (this.element.error || (this.desired()?.state === "playing" && this.element.readyState < 3)) {
        this.recover("network changed", true);
      }
    });
  }

  on(type, listener) {
    for (const element of this.elements) element.addEventListener(type, (event) => {
      if (element === this.element && !this.closed) listener(event);
    });
  }

  desired() { return this.options.playback(); }
  position() { return this.offset + (this.element.currentTime || 0); }
  quiet() { this.quietUntil = performance.now() + 800; }
  audioProfile() { return this.options.audioProfile?.() || "original"; }
  profile(entry) {
    if (!entry) return "original";
    if (isVideo(entry)) {
      const codec = entry.codec || this.options.metadata?.(entry)?.codec;
      const supported = !codec || codec === "none" || (audioTypes[codec] && this.elements[1].canPlayType(audioTypes[codec]));
      return this.fallback.get(entry.path) || (supported ? "original" : "video-aac-192");
    }
    const chosen = this.audioProfile();
    if (chosen !== "auto") return chosen;
    if (this.fallback.has(entry.path)) return this.fallback.get(entry.path);
    const codec = entry.codec;
    const supported = !codec || codec === "none" ||
      (codec.startsWith("pcm_") && this.elements[0].canPlayType("audio/wav")) ||
      (audioTypes[codec] && this.elements[0].canPlayType(audioTypes[codec]));
    return supported ? "original" : "mp3-192";
  }

  pause() { this.quiet(); this.element.pause(); }

  async play() {
    const desired = this.desired();
    if (this.closed || this.blocked || this.loading || desired?.state !== "playing" ||
        key(currentEntry(desired)) !== key(this.loaded)) return;
    const generation = this.generation;
    try {
      this.quiet();
      await this.element.play();
    } catch (error) {
      if (generation !== this.generation || this.closed) return;
      if (error.name === "NotAllowedError") {
        this.cancelRecovery();
        this.blocked = true;
        this.options.onBlocked?.();
      } else if (error.name !== "AbortError" && ![3, 4].includes(this.element.error?.code)) {
        this.recover("stream open");
      }
    }
  }

  async load(entry, at = 0, chosen, recovery = false) {
    if (this.closed || !entry) return;
    const generation = ++this.generation;
    this.loading = generation;
    this.pending = {key: key(entry), at, profile: chosen || this.profile(entry)};
    clearTimeout(this.retryTimer);
    this.retryTimer = null;
    try {
      await this.options.enrich?.(entry);
      if (generation !== this.generation || this.closed) return;
      const target = this.elements[isVideo(entry) ? 1 : 0];
      if (target !== this.element) {
        this.pause();
        this.element.removeAttribute("src");
        this.element.onloadedmetadata = null;
        this.element.load();
        this.element = target;
        this.options.onElement?.(target);
      }
      const profile = isVideo(entry) ? this.profile(entry) : chosen || this.profile(entry);
      this.pending.profile = profile;
      if (!recovery) this.cancelRecovery();
      this.lastPosition = at;
      this.quiet();
      this.startedAt = performance.now();
      this.startupMs = null;
      this.events = [];
      this.error = "";
      const start = profile === "original" ? 0 : at;
      const url = await this.options.streamURL(entry, profile, start);
      if (generation !== this.generation || this.closed) return;
      this.loaded = entry;
      this.format = profile;
      this.offset = start;
      this.element.onloadedmetadata = profile === "original" && at > 0 ? () => {
        if (generation === this.generation) { this.quiet(); this.element.currentTime = at; }
      } : null;
      this.element.src = url;
      this.options.onSource?.(entry);
    } catch (error) {
      if (generation === this.generation && !this.closed) {
        this.error = error.message;
        this.options.onError?.(error);
        this.recover("signing request");
      }
    } finally {
      if (generation === this.generation) {
        this.loading = null;
        this.pending = null;
        this.play();
      }
    }
  }

  seek(at) {
    this.lastPosition = at;
    if (this.format === "original" && !this.element.error && !this.recovering) {
      this.quiet();
      this.element.currentTime = at;
    } else {
      this.load(currentEntry(this.desired()) || this.loaded, at);
    }
  }

  sync(state) {
    if (this.closed) return;
    const entry = currentEntry(state);
    if (!entry || state.state === "stopped") {
      this.generation++;
      this.loading = null;
      this.pending = null;
      this.cancelRecovery();
      this.pause();
      this.loaded = null;
      this.offset = 0;
      this.lastPosition = 0;
      this.element.onloadedmetadata = null;
      this.element.removeAttribute("src");
      this.element.load();
      this.options.onSource?.(null);
      this.applied = state?.version ?? -1;
      return;
    }
    if (state.state !== "playing") this.cancelRecovery();
    const target = state.position || 0, profile = this.profile(entry);
    if (this.loading) {
      if (this.pending.key !== key(entry) || this.pending.profile !== profile ||
          (this.applied !== state.version && Math.abs(this.pending.at - target) > 2)) {
        this.load(entry, target);
      }
    } else if (key(this.loaded) !== key(entry) || this.format !== profile || !this.element.getAttribute("src")) {
      this.load(entry, target);
    } else if (this.applied !== state.version && Math.abs(this.position() - target) > 2) {
      this.seek(target);
    }
    this.applied = state.version;
    if (state.state === "playing") this.play();
    else this.pause();
  }

  cancelRecovery() {
    clearTimeout(this.retryTimer);
    this.retryTimer = null;
    this.recovering = false;
    this.retryDelay = 1000;
  }

  recover(reason, immediate = false) {
    const desired = this.desired(), entry = currentEntry(desired);
    if (this.closed || this.blocked || desired?.state !== "playing" || !entry) return;
    if (this.loaded && key(entry) !== key(this.loaded) && !this.loading) return;
    if (!this.recovering) this.lastPosition = this.loaded ? Math.max(this.lastPosition, this.position()) : desired.position || 0;
    this.recovering = true;
    if (this.retryTimer && !immediate) return;
    clearTimeout(this.retryTimer);
    this.retryTimer = setTimeout(async () => {
      this.retryTimer = null;
      const latest = this.desired(), track = currentEntry(latest);
      if (latest?.state !== "playing" || key(track) !== key(entry)) { this.cancelRecovery(); return; }
      if (navigator.onLine === false) { this.recover("offline"); return; }
      const delay = this.retryDelay;
      await this.load(track, this.lastPosition, undefined, true);
      this.events.push({type: "reconnecting: " + reason, ms: 0});
      if (this.recovering) this.retryDelay = Math.min(delay * 2, 10000);
    }, immediate ? 0 : this.retryDelay);
  }

  async mediaError() {
    const generation = this.generation;
    const error = this.element.error;
    this.error = error ? "Code " + error.code + ": " + error.message : "Unknown media error";
    if (error?.code === 2 || /network|ERR_NETWORK_CHANGED/i.test(error?.message || "")) {
      this.recover("network error");
    } else if ([3, 4].includes(error?.code) && this.loaded) {
      if (this.options.probeStream) {
        try {
          const status = await this.options.probeStream(this.element.currentSrc || this.element.src);
          if (generation !== this.generation || this.closed) return;
          if ([403, 502, 503, 504].includes(status)) { this.recover("stream service restarting"); return; }
        } catch (_) {
          if (generation === this.generation && !this.closed) this.recover("stream service unavailable");
          return;
        }
      }
      if (this.format === "original" && (isVideo(this.loaded) || this.audioProfile() === "auto")) {
        this.fallback.set(this.loaded.path, isVideo(this.loaded) ? "video-copy" : "mp3-192");
        this.load(this.loaded, this.lastPosition);
      } else {
        this.cancelRecovery();
        this.options.onError?.(new Error("This browser cannot decode the stream. Video is never transcoded; try the original in mpv or VLC."));
      }
    }
  }

  close() {
    this.closed = true;
    this.generation++;
    this.loading = null;
    this.pending = null;
    this.cancelRecovery();
    for (const element of this.elements) {
      element.pause();
      element.onloadedmetadata = null;
      element.removeAttribute("src");
      element.load();
    }
  }
}
