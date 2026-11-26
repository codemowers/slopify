import Gio from 'gi://Gio';
import GLib from 'gi://GLib';
import Soup from 'gi://Soup?version=3.0';
import {privateIPv4, canTransport, transportActions, controlOrigin, volumeStep, outputPeer} from './model.js';

const config = `${GLib.get_user_config_dir()}/slopify-remote`;
export function readJSON(name, fallback = {}) {
    try { return JSON.parse(new TextDecoder().decode(GLib.file_get_contents(`${config}/${name}.json`)[1])); }
    catch (_) { return fallback; }
}
export function writeJSON(name, data) {
    GLib.mkdir_with_parents(config, 0o700);
    Gio.File.new_for_path(`${config}/${name}.json`).replace_contents(JSON.stringify(data), null, false,
        Gio.FileCreateFlags.PRIVATE | Gio.FileCreateFlags.REPLACE_DESTINATION, null);
}
export function call(bus, path, iface, method, args, cancellable = null) {
    return new Promise((resolve, reject) => bus.call('org.freedesktop.Avahi', path, iface, method,
        args, null, Gio.DBusCallFlags.NONE, 3000, cancellable,
        (bus, result) => { try { resolve(bus.call_finish(result).deepUnpack()); } catch (e) { reject(e); } }));
}
export class Client {
    constructor(changed, osd) {
        this.changed = changed; this.osd = osd;
        const settings = readJSON('settings');
        this.followOutput = settings.followOutput !== false;
        this.selected = this.followOutput ? '' : settings.selected || '';
        this.output = null;
        this.peers = new Map(); this.services = new Map(); this.pending = [];
        this.status = {}; this.transport = {}; this.error = ''; this.generation = 0;
        this.cancel = new Gio.Cancellable();
        this.http = new Soup.Session({timeout: 8, proxy_resolver: Gio.SimpleProxyResolver.new(null, [])});
        this.failures = 0; this.nextDiscovery = 0; this.discoveryEpoch = 0;
        this.watch = Gio.bus_watch_name(Gio.BusType.SYSTEM, 'org.freedesktop.Avahi', Gio.BusNameWatcherFlags.NONE,
            bus => { this.bus = bus; this.discover(); }, () => {
                this.discoveryEpoch++; this.discovering = false; this.browser = null;
                if (this.subscription) this.bus.signal_unsubscribe(this.subscription);
                this.subscription = 0; this.bus = null;
                this.services.clear(); this.refreshPeers();
            });
        this.timer = GLib.timeout_add_seconds(GLib.PRIORITY_DEFAULT, 1, () => {
            if ((!this.browser || this.selected && !this.peer || this.failures >= 3) &&
                GLib.get_monotonic_time() >= this.nextDiscovery) this.discover();
            this.pump(); return GLib.SOURCE_CONTINUE;
        });
    }
    get peer() { return this.peers.get(this.selected); }
    async discover() {
        if (this.dead || !this.bus || this.discovering) return;
        this.discovering = true; this.nextDiscovery = GLib.get_monotonic_time() + 5000000;
        const bus = this.bus, epoch = ++this.discoveryEpoch, early = [];
        let browser = null;
        if (this.subscription) bus.signal_unsubscribe(this.subscription);
        if (this.browser) call(bus, this.browser, 'org.freedesktop.Avahi.ServiceBrowser', 'Free', null).catch(() => {});
        this.browser = null; this.services.clear();
        const receive = (path, signal, args) => {
            if (this.dead || epoch !== this.discoveryEpoch || path !== browser) return;
            if (signal === 'Failure') { this.browser = null; this.error = 'Discovery disconnected; retrying…'; this.changed(); return; }
            if (signal !== 'ItemNew' && signal !== 'ItemRemove') return;
            const [iface, protocol, name, type, domain] = args.deepUnpack();
            const key = `${iface}:${protocol}:${name}:${domain}`;
            if (signal === 'ItemRemove') { this.services.delete(key); this.refreshPeers(); return; }
            if (this.services.size >= 64 && !this.services.has(key)) return;
            const ticket = {}; this.services.set(key, ticket);
            call(bus, '/', 'org.freedesktop.Avahi.Server', 'ResolveService',
                new GLib.Variant('(iisssiu)', [iface, protocol, name, type, domain, protocol, 0]), this.cancel)
                .then(result => {
                    if (this.dead || epoch !== this.discoveryEpoch || this.services.get(key) !== ticket) return;
                    const host = result[5].replace(/\.$/, '').toLowerCase(), ip = result[7], port = result[8];
                    if (!privateIPv4(ip) || !/^[a-z0-9][a-z0-9.-]*\.local$/.test(host)) return;
                    this.services.set(key, {id: `${name}.${domain}`, name, host, ip, origin: controlOrigin(`http://${ip}:${port}`),
                        url: `http://${host}${port === 80 ? '' : `:${port}`}/`});
                    this.refreshPeers();
                }).catch(() => {});
        };
        // Avahi may emit cached results before ServiceBrowserNew returns.
        this.subscription = bus.signal_subscribe('org.freedesktop.Avahi', 'org.freedesktop.Avahi.ServiceBrowser',
            null, null, null, Gio.DBusSignalFlags.NONE, (_b, _s, path, _i, signal, args) => {
                if (!browser) { if (early.length < 128) early.push([path, signal, args]); }
                else receive(path, signal, args);
            });
        try {
            [browser] = await call(bus, '/', 'org.freedesktop.Avahi.Server', 'ServiceBrowserNew',
                new GLib.Variant('(iissu)', [-1, 0, '_slopify._tcp', 'local', 0]), this.cancel);
            if (this.dead || epoch !== this.discoveryEpoch) {
                call(bus, browser, 'org.freedesktop.Avahi.ServiceBrowser', 'Free', null).catch(() => {}); return;
            }
            this.browser = browser;
            for (const event of early) receive(...event);
        } catch (_) {
            if (!this.dead && epoch === this.discoveryEpoch) { this.error = 'Discovery unavailable; retrying…'; this.changed(); }
        } finally { if (epoch === this.discoveryEpoch) this.discovering = false; }
    }
    refreshPeers() {
        if (this.dead) return;
        const previous = this.peer?.origin;
        this.peers = new Map([...this.services.values()].filter(p => p.id).map(p => [p.id, p]));
        if (this.followOutput) this.setTarget(outputPeer(this.output, this.peers.values()));
        if (previous !== this.peer?.origin) this.resetTarget();
        this.changed(); this.pump();
    }
    resetTarget() { this.generation++; this.pending = []; this.status = {}; this.transport = {}; this.error = ''; this.failures = 0; }
    setTarget(id) {
        if (id === this.selected) return;
        this.selected = id; this.resetTarget();
    }
    select(id) {
        this.followOutput = false;
        this.setTarget(id);
        writeJSON('settings', {selected: id, followOutput: false});
        this.changed(); this.pump();
    }
    follow() {
        this.followOutput = true;
        writeJSON('settings', {followOutput: true});
        this.setOutput(this.output);
    }
    setOutput(output) {
        this.output = output;
        if (!this.followOutput) return;
        this.setTarget(outputPeer(output, this.peers.values()));
        this.changed(); this.pump();
    }
    async request(origin, path, body) {
        origin = controlOrigin(origin);
        const message = Soup.Message.new(body === undefined ? 'GET' : 'POST', origin + path);
        message.set_flags(Soup.MessageFlags.NO_REDIRECT);
        if (body !== undefined) {
            message.request_headers.append('Origin', origin);
            message.request_headers.append('X-Slopify-Control', '1');
            message.set_request_body_from_bytes('application/json', new GLib.Bytes(new TextEncoder().encode(JSON.stringify(body))));
        }
        const cancel = new Gio.Cancellable();
        const parent = this.cancel.connect(() => cancel.cancel());
        let timer = GLib.timeout_add(GLib.PRIORITY_DEFAULT, 8000, () => { timer = 0; cancel.cancel(); return GLib.SOURCE_REMOVE; });
        let stream;
        try {
            stream = await new Promise((resolve, reject) => this.http.send_async(message, GLib.PRIORITY_DEFAULT, cancel,
                (session, result) => { try { resolve(session.send_finish(result)); } catch (e) { reject(e); } }));
            if (message.status_code < 200 || message.status_code >= 300) throw Error(`Player returned HTTP ${message.status_code}`);
            let total = 0; const chunks = [];
            for (;;) {
                const bytes = await new Promise((resolve, reject) => stream.read_bytes_async(16384, GLib.PRIORITY_DEFAULT, cancel,
                    (input, result) => { try { resolve(input.read_bytes_finish(result)); } catch (e) { reject(e); } }));
                if (!bytes.get_size()) break;
                total += bytes.get_size(); if (total > 1024 * 1024) throw Error('Player response too large');
                chunks.push(bytes.get_data());
            }
            const buffer = new Uint8Array(total); let offset = 0;
            for (const chunk of chunks) { buffer.set(chunk, offset); offset += chunk.length; }
            const text = new TextDecoder().decode(buffer); return text ? JSON.parse(text) : {};
        } finally { stream?.close(null); if (timer) GLib.source_remove(timer); this.cancel.disconnect(parent); }
    }
    command(action, value) {
        if (!this.peer || transportActions.has(action) && !canTransport(this.status, this.transport)) return;
        if (action === 'volume') this.pending = this.pending.filter(j => j.action !== 'volume');
        if (this.pending.length < 16) this.pending.push({action, value, generation: this.generation, at: GLib.get_monotonic_time()});
        this.pump();
    }
    async pump() {
        if (this.dead || this.busy || !this.peer) return;
        this.busy = true;
        const origin = this.peer.origin, generation = this.generation;
        const job = this.pending.shift();
        const current = () => !this.dead && generation === this.generation;
        try {
            let status = await this.request(origin, '/api/status');
            let transport = await this.request(origin, '/api/playback');
            if (!current()) return;
            if (job && job.generation === generation && GLib.get_monotonic_time() - job.at < 2500000) {
                const {action, value} = job, v = status.volume;
                if (transportActions.has(action)) {
                    if (!canTransport(status, transport)) throw Error('Select Slopify for playback controls');
                    await this.request(origin, '/api/playback', {op: action, version: transport.version, entry: transport.entry});
                } else if (action === 'source') {
                    await this.request(origin, '/api/sources', {revision: status.sourceControl.revision,
                        ...(value === null ? {unpin: true} : {selected: value})});
                } else if (v) {
                    let body;
                    if (action === 'mute') body = {muted: !v.muted};
                    else if (action === 'volume' && value === 0 && v.muteAvailable) body = {muted: true};
                    else body = {level: Math.max(v.min, Math.min(v.max, action === 'volume' ?
                        Math.round(v.min + value * (v.max - v.min)) : volumeStep(v, action === 'volume-up' ? 1 : -1)))};
                    let volume = await this.request(origin, '/api/volume', body);
                    if (!current()) return;
                    if (v.switchMuted && (action === 'volume-up' || action === 'volume' && value > 0))
                        volume = await this.request(origin, '/api/volume', {muted: false});
                    if (current()) this.osd(volume);
                }
                if (!current()) return;
                status = await this.request(origin, '/api/status'); transport = await this.request(origin, '/api/playback');
            }
            if (current()) { this.status = status; this.transport = transport; this.error = ''; this.failures = 0; }
        } catch (e) {
            if (current()) { this.failures++; this.error = e.message.startsWith('Player returned') ? e.message : 'Connection lost; reconnecting…'; this.status = {}; this.transport = {}; }
        } finally {
            this.busy = false;
            if (!this.dead) { this.changed(); if (this.pending.length) this.pump(); }
        }
    }
    destroy() {
        this.dead = true; this.cancel.cancel(); this.http.abort();
        GLib.source_remove(this.timer); Gio.bus_unwatch_name(this.watch);
        if (this.subscription) this.bus.signal_unsubscribe(this.subscription);
        if (this.browser) call(this.bus, this.browser, 'org.freedesktop.Avahi.ServiceBrowser', 'Free', null).catch(() => {});
    }
}
