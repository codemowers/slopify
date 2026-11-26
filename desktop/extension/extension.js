import Clutter from 'gi://Clutter';
import Gio from 'gi://Gio';
import GLib from 'gi://GLib';
import St from 'gi://St';
import {Extension} from 'resource:///org/gnome/shell/extensions/extension.js';
import * as Main from 'resource:///org/gnome/shell/ui/main.js';
import * as PanelMenu from 'resource:///org/gnome/shell/ui/panelMenu.js';
import * as PopupMenu from 'resource:///org/gnome/shell/ui/popupMenu.js';
import * as Volume from 'resource:///org/gnome/shell/ui/status/volume.js';
import {Slider} from 'resource:///org/gnome/shell/ui/slider.js';
import {Client} from './client.js';
import {Keys} from './keys.js';
import {Mpris} from './mpris.js';
import {canTransport, playingInfo, sourceLabel, volumeFraction, volumeText} from './model.js';

export default class Slopify extends Extension {
    enable() {
        this.dragging = this.rendering = false;
        this.icon = new Gio.FileIcon({file: this.dir.get_child('slopify.svg')});
        this.button = new PanelMenu.Button(0.0, 'Slopify Remote');
        this.button.add_child(new St.Icon({gicon: this.icon, style_class: 'system-status-icon'}));
        this.button.menu.box.add_style_class_name('slopify-menu');
        const infoItem = new PopupMenu.PopupBaseMenuItem({reactive: false, can_focus: false});
        const info = new St.BoxLayout({vertical: true, x_expand: true, style_class: 'slopify-info'});
        infoItem.add_child(info); this.button.menu.addMenuItem(infoItem);
        this.labels = {};
        for (const key of ['speaker', 'title', 'artist', 'album', 'source']) {
            const label = new St.Label({text: '', style_class: `slopify-${key}`});
            label.clutter_text.set_line_wrap(true); info.add_child(label); this.labels[key] = label;
        }
        const controls = new PopupMenu.PopupBaseMenuItem({reactive: false, can_focus: false});
        this.controls = new Map();
        for (const [action, icon, label] of [['prev', 'media-skip-backward-symbolic', 'Previous track'],
            ['toggle', 'media-playback-start-symbolic', 'Play / pause'], ['next', 'media-skip-forward-symbolic', 'Next track']]) {
            const button = new St.Button({style_class: 'button slopify-control', can_focus: true,
                accessible_name: label, x_expand: true, child: new St.Icon({icon_name: icon, icon_size: 20})});
            button.connect('clicked', () => this.client.command(action)); controls.add_child(button); this.controls.set(action, button);
        }
        this.button.menu.addMenuItem(controls);
        const volumeItem = new PopupMenu.PopupBaseMenuItem({activate: false});
        this.mute = new St.Button({can_focus: true, accessible_name: 'Mute / unmute',
            child: new St.Icon({icon_name: 'audio-volume-high-symbolic', icon_size: 20})});
        this.mute.connect('clicked', () => this.client.command('mute'));
        volumeItem.add_child(this.mute);
        this.slider = new Slider(0); this.slider.x_expand = true; this.slider.accessible_name = 'Speaker volume';
        volumeItem.add_child(this.slider);
        this.db = new St.Label({text: '—', y_align: Clutter.ActorAlign.CENTER, style_class: 'slopify-db'});
        volumeItem.add_child(this.db); this.button.menu.addMenuItem(volumeItem);
        this.slider.connect('drag-begin', () => { this.dragging = true; });
        this.slider.connect('drag-end', () => { this.dragging = false; this.sendVolume(); });
        this.slider.connect('notify::value', () => {
            if (this.rendering) return;
            this.volumeGeneration = this.client.generation;
            if (this.volumeTimer) GLib.source_remove(this.volumeTimer);
            this.volumeTimer = GLib.timeout_add(GLib.PRIORITY_DEFAULT, 100, () => {
                this.volumeTimer = 0; this.sendVolume(); return GLib.SOURCE_REMOVE;
            });
        });
        this.button.menu.addMenuItem(new PopupMenu.PopupSeparatorMenuItem());
        this.players = new PopupMenu.PopupSubMenuMenuItem('Player'); this.button.menu.addMenuItem(this.players);
        this.sources = new PopupMenu.PopupSubMenuMenuItem('Input source'); this.button.menu.addMenuItem(this.sources);
        this.open = this.button.menu.addAction('Open player…', () => {
            const url = this.client.peer?.url;
            if (url) Gio.AppInfo.launch_default_for_uri_async(url, global.create_app_launch_context(0, -1), null, (_o, res) => {
                try { Gio.AppInfo.launch_default_for_uri_finish(res); } catch (_) { Main.notify('Slopify', 'Could not open player'); }
            });
        });
        this.button.menu.addAction('Use laptop controls', () => this.client.select(''));
        this.keys = new Keys(action => this.client.command(action));
        this.client = new Client(() => this.render(), volume => this.showVolume(volume));
        this.mixer = Volume.getMixerControl();
        const syncOutput = () => {
            const sink = this.mixer.get_default_sink();
            this.client.setOutput(sink ? {name: sink.get_name(), description: sink.get_description()} : null);
        };
        this.mixerSignals = ['default-sink-changed', 'state-changed', 'stream-changed', 'stream-added', 'stream-removed']
            .map(signal => this.mixer.connect(signal, syncOutput));
        syncOutput();
        this.mpris = new Mpris(this.client, () => this.button.menu.toggle());
        Main.panel.addToStatusArea(this.uuid, this.button);
        this.render();
    }
    sendVolume() {
        if (this.volumeTimer) { GLib.source_remove(this.volumeTimer); this.volumeTimer = 0; }
        if (this.volumeGeneration === this.client.generation && this.client.status.volume)
            this.client.command('volume', this.slider.value);
    }
    showVolume(volume) {
        Main.osdWindowManager.show(-1, this.icon,
            `${this.client.peer?.name || 'Slopify'} · ${volumeText(volume)}`, volumeFraction(volume), 1);
    }
    render() {
        if (!this.client) return;
        const c = this.client, info = playingInfo(c.status, c.transport), v = c.status.volume;
        if (c.selected) this.keys.capture(); else this.keys.release();
        this.labels.speaker.text = c.peer?.name || c.selected || 'Slopify · laptop controls';
        this.labels.title.text = c.error || info.title || (c.peer ? 'No track information' : 'Select a player');
        this.labels.artist.text = info.artist || ''; this.labels.album.text = info.album || '';
        this.labels.artist.visible = !!info.artist; this.labels.album.visible = !!info.album;
        this.labels.source.text = c.peer ? `Input: ${info.source}` : c.selected ? 'Player offline' : 'Keyboard controls this laptop';
        const transport = canTransport(c.status, c.transport);
        for (const button of this.controls.values()) { button.reactive = transport; button.can_focus = transport; button.opacity = transport ? 255 : 80; }
        this.controls.get('toggle').child.icon_name = info.playing ? 'media-playback-pause-symbolic' : 'media-playback-start-symbolic';
        this.rendering = true;
        if (!this.dragging && !this.volumeTimer) this.slider.value = volumeFraction(v);
        this.rendering = false;
        this.slider.reactive = !!v; this.slider.can_focus = !!v; this.slider.opacity = v ? 255 : 80;
        this.db.text = v ? volumeText(v) : !c.selected ? 'Laptop' : !c.peer ? 'Offline' : c.error ? 'Reconnecting…' : c.status.name ? 'No mixer' : 'Connecting…'; this.mute.reactive = !!v?.muteAvailable; this.mute.can_focus = this.mute.reactive;
        this.mute.child.icon_name = v?.muted ? 'audio-volume-muted-symbolic' : 'audio-volume-high-symbolic';
        this.open.setSensitive(!!c.peer);
        const peers = [...c.peers.values()].sort((a, b) => a.name.localeCompare(b.name));
        const peerKey = JSON.stringify([c.followOutput, c.selected, peers.map(p => [p.id, p.name])]);
        if (peerKey !== this.peerKey) {
            this.peerKey = peerKey; this.players.menu.removeAll();
            const follow = this.players.menu.addAction('Follow system audio output', () => c.follow());
            follow.setOrnament(c.followOutput ? PopupMenu.Ornament.DOT : PopupMenu.Ornament.NONE);
            for (const p of [{id: '', name: 'Laptop controls'}, ...peers]) {
                const item = this.players.menu.addAction(p.name, () => c.select(p.id));
                item.setOrnament(!c.followOutput && p.id === c.selected ? PopupMenu.Ornament.DOT : PopupMenu.Ornament.NONE);
            }
        }
        this.players.label.text = `${c.followOutput ? 'Following output' : 'Player'}: ${c.peer?.name || (c.selected ? 'Offline' : 'Laptop')}`;
        const policy = c.status.sourceControl;
        const sources = policy?.sources?.filter(s => !s.senderUnavailable || s.id === policy.selected) || [];
        const sourceKey = JSON.stringify([policy?.selected, policy?.mode, sources.map(s => [s.id, sourceLabel(s), s.connected])]);
        if (sourceKey !== this.sourceKey) {
            this.sourceKey = sourceKey; this.sources.menu.removeAll();
            for (const source of sources) {
                const item = this.sources.menu.addAction(sourceLabel(source) + (source.connected ? '' : ' (offline)'), () => c.command('source', source.id));
                item.setOrnament(source.id === policy.selected ? PopupMenu.Ornament.DOT : PopupMenu.Ornament.NONE);
            }
            if (sources.length) this.sources.menu.addAction('Resume automatic selection', () => c.command('source', null));
        }
        this.sources.setSensitive(sources.length > 0);
        this.mpris?.update();
    }
    disable() {
        if (this.volumeTimer) GLib.source_remove(this.volumeTimer);
        this.volumeTimer = 0;
        for (const signal of this.mixerSignals || []) this.mixer.disconnect(signal);
        this.mixerSignals = []; this.mixer = null;
        this.client?.destroy(); this.mpris?.destroy(); this.keys?.destroy(); this.button?.destroy();
        this.client = this.mpris = this.keys = this.button = null;
        this.peerKey = this.sourceKey = null;
    }
}
