import Gio from 'gi://Gio';
import GLib from 'gi://GLib';
import {canTransport, playingInfo, volumeFraction} from './model.js';

const root = 'org.mpris.MediaPlayer2', player = `${root}.Player`, path = '/org/mpris/MediaPlayer2';
const types = {PlaybackStatus: 's', Metadata: 'a{sv}', Volume: 'd', Rate: 'd', MinimumRate: 'd', MaximumRate: 'd',
    Position: 'x', CanGoNext: 'b', CanGoPrevious: 'b', CanPlay: 'b', CanPause: 'b', CanSeek: 'b', CanControl: 'b'};
export class Mpris {
    constructor(client, raise) {
        this.client = client;
        const methods = {Next: 'next', Previous: 'prev', Pause: 'pause', PlayPause: 'toggle', Stop: 'stop', Play: 'play'};
        const handler = {Seek() {}, SetPosition() {}, OpenUri() { throw Error('OpenUri unsupported'); }};
        for (const [method, action] of Object.entries(methods)) handler[method] = () => client.command(action);
        for (const name of Object.keys(types)) Object.defineProperty(handler, name, {
            get: () => this.properties()[name],
            set: value => {
                if (name === 'Volume' && Number.isFinite(value)) client.command('volume', Math.max(0, Math.min(1, value)));
                else if (name !== 'Rate' || value !== 1) throw Error('Unsupported property value');
            },
        });
        const xml = `<node><interface name="${player}">${Object.keys(methods).map(m => `<method name="${m}"/>`).join('')}
            <method name="Seek"><arg type="x" direction="in"/></method>
            <method name="SetPosition"><arg type="o" direction="in"/><arg type="x" direction="in"/></method>
            <method name="OpenUri"><arg type="s" direction="in"/></method>
            ${Object.entries(types).map(([n, t]) => `<property name="${n}" type="${t}" access="${['Volume', 'Rate'].includes(n) ? 'readwrite' : 'read'}"/>`).join('')}
            </interface></node>`;
        this.object = Gio.DBusExportedObject.wrapJSObject(xml, handler); this.object.export(Gio.DBus.session, path);
        const self = this;
        this.root = Gio.DBusExportedObject.wrapJSObject(`<node><interface name="${root}">
            <method name="Raise"/><method name="Quit"/>
            <property name="Identity" type="s" access="read"/><property name="CanRaise" type="b" access="read"/>
            <property name="CanQuit" type="b" access="read"/><property name="HasTrackList" type="b" access="read"/>
            <property name="SupportedUriSchemes" type="as" access="read"/><property name="SupportedMimeTypes" type="as" access="read"/>
            </interface></node>`, {Raise: raise, Quit() {}, CanRaise: true, CanQuit: false, HasTrackList: false,
            SupportedUriSchemes: [], SupportedMimeTypes: [], get Identity() { return self.identity; }});
        this.root.export(Gio.DBus.session, path);
        this.owner = Gio.bus_own_name_on_connection(Gio.DBus.session, `${root}.slopify`, Gio.BusNameOwnerFlags.NONE, null, null);
    }
    get identity() { return `Slopify — ${this.client.peer?.name || (this.client.selected ? this.client.selected + ' (offline)' : 'Laptop controls')}`; }
    properties() {
        const c = this.client, info = playingInfo(c.status, c.transport), ready = canTransport(c.status, c.transport);
        const metadata = {};
        if (info.title) {
            const hash = GLib.compute_checksum_for_string(GLib.ChecksumType.SHA256, `${info.source}\n${info.title}\n${info.artist}\n${info.album}`, -1);
            metadata['mpris:trackid'] = new GLib.Variant('o', `/io/codemowers/slopify/t${hash}`);
            metadata['xesam:title'] = new GLib.Variant('s', info.title);
            metadata['xesam:artist'] = new GLib.Variant('as', info.artist ? [info.artist] : []);
            metadata['xesam:album'] = new GLib.Variant('s', info.album || '');
        }
        return {PlaybackStatus: c.peer && !c.error ? info.playing ? 'Playing' : 'Paused' : 'Stopped', Metadata: metadata,
            Volume: volumeFraction(c.status.volume), Rate: 1, MinimumRate: 1, MaximumRate: 1, Position: 0,
            CanGoNext: ready, CanGoPrevious: ready, CanPlay: ready, CanPause: ready, CanSeek: false, CanControl: !!c.peer && !c.error};
    }
    update() {
        for (const [key, value] of Object.entries(this.properties())) this.object.emit_property_changed(key, new GLib.Variant(types[key], value));
        this.root.emit_property_changed('Identity', new GLib.Variant('s', this.identity));
    }
    destroy() { this.object.unexport(); this.root.unexport(); Gio.bus_unown_name(this.owner); }
}
