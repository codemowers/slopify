import Gio from 'gi://Gio';
import GLib from 'gi://GLib';
import Meta from 'gi://Meta';
import Shell from 'gi://Shell';
import * as Main from 'resource:///org/gnome/shell/ui/main.js';
import {readJSON, writeJSON} from './client.js';

const schema = 'org.gnome.settings-daemon.plugins.media-keys';
const actions = {'volume-up': 'volume-up', 'volume-down': 'volume-down', 'volume-mute': 'mute',
    play: 'toggle', pause: 'pause', stop: 'stop', next: 'next', previous: 'prev'};
export class Keys {
    constructor(command) {
        this.settings = new Gio.Settings({schema_id: schema}); this.grabs = new Map();
        this.restore();
        this.signal = global.display.connect('accelerator-activated', (_display, action) => {
            const op = this.grabs.get(action); if (op) command(op);
        });
    }
    capture() {
        if (this.active) return;
        this.active = true;
        const keys = {}, bindings = new Map();
        for (const [key, op] of Object.entries(actions)) {
            for (const name of [key, `${key}-static`]) {
                if (!this.settings.settings_schema.has_key(name)) continue;
                // An empty binding is usually one a crashed session left captured: fall
                // back to GNOME's default, so the keys and the backup recover.
                const current = this.settings.get_strv(name);
                keys[name] = current.length ? current : this.settings.settings_schema.get_key(name).get_default_value().deepUnpack();
                for (const binding of keys[name]) if (binding) bindings.set(binding, op);
            }
        }
        writeJSON('keyboard-backup', {keys, paths: []});
        for (const key of Object.keys(keys)) this.settings.set_strv(key, []);
        // gsd-media-keys must first release its grabs after the settings update.
        let attempts = 0;
        this.timer = GLib.timeout_add(GLib.PRIORITY_DEFAULT, 150, () => {
            for (const [binding, op] of bindings) {
                const id = global.display.grab_accelerator(binding, Meta.KeyBindingFlags.NONE);
                if (!id) continue;
                this.grabs.set(id, op); bindings.delete(binding);
                Main.wm.allowKeybinding(Meta.external_binding_name_for_action(id), Shell.ActionMode.NORMAL | Shell.ActionMode.OVERVIEW);
            }
            if (++attempts < 10 && bindings.size) return GLib.SOURCE_CONTINUE;
            this.timer = 0;
            if (bindings.size) { this.release(); Main.notify('Slopify', 'Could not capture media keys; laptop bindings restored'); }
            return GLib.SOURCE_REMOVE;
        });
    }
    restore() {
        const backup = readJSON('keyboard-backup', null);
        if (!backup) return;
        for (const [name, values] of Object.entries(backup.keys))
            if (this.settings.settings_schema.has_key(name) && !this.settings.get_strv(name).length) this.settings.set_strv(name, values);
        // Also migrates the retired Python applet's custom shortcut bindings.
        const paths = backup.paths || [];
        this.settings.set_strv('custom-keybindings', this.settings.get_strv('custom-keybindings').filter(p => !paths.includes(p)));
        for (const path of paths) {
            const settings = new Gio.Settings({schema_id: `${schema}.custom-keybinding`, path});
            for (const key of ['name', 'command', 'binding']) settings.reset(key);
        }
        Gio.Settings.sync();
        Gio.File.new_for_path(`${GLib.get_user_config_dir()}/slopify-remote/keyboard-backup.json`).delete(null);
    }
    release() {
        if (this.timer) { GLib.source_remove(this.timer); this.timer = 0; }
        for (const id of this.grabs.keys()) {
            global.display.ungrab_accelerator(id);
            Main.wm.allowKeybinding(Meta.external_binding_name_for_action(id), Shell.ActionMode.NONE);
        }
        this.grabs.clear(); this.active = false; this.restore();
    }
    destroy() { this.release(); global.display.disconnect(this.signal); }
}
