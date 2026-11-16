// Retry reads during rolling/recreate deployments. Mutations are never replayed
// unless the caller explicitly opts in (the serialized auth-token renewal).
const transient = new Set([502, 503, 504]);
const pending = new Set();
function publish() {
  window.dispatchEvent(new CustomEvent("slopify:network", {detail: {retrying: pending.size > 0}}));
}
function wait(ms, signal) {
  return new Promise((resolve, reject) => {
    const done = () => { cleanup(); resolve(); };
    const aborted = () => { cleanup(); reject(signal.reason || new DOMException("Aborted", "AbortError")); };
    const timer = setTimeout(done, ms);
    function cleanup() {
      clearTimeout(timer);
      window.removeEventListener("online", done);
      signal?.removeEventListener("abort", aborted);
    }
    window.addEventListener("online", done, {once: true});
    signal?.addEventListener("abort", aborted, {once: true});
    if (signal?.aborted) aborted();
  });
}
export async function fetchRetry(url, options = {}, policy = {}) {
  const method = (options.method || "GET").toUpperCase();
  const retry = policy.retry ?? (method === "GET" || method === "HEAD");
  const key = Symbol();
  let delay = 1000;
  try {
    for (;;) {
      if (options.signal?.aborted) throw options.signal.reason || new DOMException("Aborted", "AbortError");
      const controller = new AbortController();
      const abort = () => controller.abort(options.signal.reason);
      options.signal?.addEventListener("abort", abort, {once: true});
      const timeout = setTimeout(() => controller.abort(), policy.timeout || 10000);
      let response;
      try {
        response = await fetch(url, {...options, signal: controller.signal});
        if (!retry || !transient.has(response.status)) return response;
        await response.body?.cancel().catch(() => {});
      } catch (error) {
        if (!retry || options.signal?.aborted || !["TypeError", "AbortError"].includes(error.name)) throw error;
      } finally {
        clearTimeout(timeout);
        options.signal?.removeEventListener("abort", abort);
      }
      if (!pending.has(key)) { pending.add(key); publish(); }
      await wait(delay, options.signal);
      delay = Math.min(delay * 2, 5000);
    }
  } finally {
    if (pending.delete(key)) publish();
  }
}

// A codec error can actually be a proxy outage or an expired signed URL.
export async function probeStream(url) {
  return (await fetchRetry(url, {method: "HEAD", credentials: "omit", cache: "no-store"}, {retry: false})).status;
}
