#!/usr/bin/gjs -m
// Per-user installation and migration from the retired Python applet.
import Gio from 'gi://Gio';
import GLib from 'gi://GLib';
import {readJSON} from './extension/client.js';

const uuid = 'slopify@codemowers.io';
const data = GLib.get_user_data_dir(), config = GLib.get_user_config_dir();
const here = Gio.File.new_for_uri(import.meta.url).get_parent();
const target = Gio.File.new_for_path(`${data}/gnome-shell/extensions/${uuid}`);
function run(args) {
    try { return Gio.Subprocess.new(args, Gio.SubprocessFlags.STDOUT_SILENCE | Gio.SubprocessFlags.STDERR_SILENCE).wait_check(null); }
    catch (_) { return false; }
}
// The old service's normal shutdown restores the keys. Recover its journal too
// in case the applet was crashed or launched outside systemd.
run(['systemctl', '--user', 'stop', 'slopify-remote']);
// A running extension shares the backup file: disabling it hands its keys back
// first, so the restore below never runs underneath live grabs.
run(['gnome-extensions', 'disable', uuid]);
run(['gdbus', 'call', '--session', '--dest', 'org.mpris.MediaPlayer2.slopify', '--object-path', '/org/mpris/MediaPlayer2', '--method', 'org.mpris.MediaPlayer2.Quit']);
const backup = readJSON('keyboard-backup', null);
if (backup) {
    const schema = 'org.gnome.settings-daemon.plugins.media-keys';
    const settings = new Gio.Settings({schema_id: schema});
    for (const [key, value] of Object.entries(backup.keys))
        if (settings.settings_schema.has_key(key) && !settings.get_strv(key).length) settings.set_strv(key, value);
    settings.set_strv('custom-keybindings', settings.get_strv('custom-keybindings').filter(p => !(backup.paths || []).includes(p)));
    for (const path of backup.paths || []) {
        const binding = new Gio.Settings({schema_id: `${schema}.custom-keybinding`, path});
        for (const key of ['name', 'command', 'binding']) binding.reset(key);
    }
    Gio.Settings.sync();
    Gio.File.new_for_path(`${config}/slopify-remote/keyboard-backup.json`).delete(null);
}
GLib.mkdir_with_parents(target.get_path(), 0o755);
for (const name of ['extension.js', 'client.js', 'keys.js', 'model.js', 'mpris.js', 'metadata.json', 'stylesheet.css', 'slopify.svg'])
    here.get_child(`extension/${name}`).copy(target.get_child(name), Gio.FileCopyFlags.OVERWRITE, null, null);
for (const path of [`${config}/autostart/slopify-remote.desktop`, `${data}/applications/slopify-remote.desktop`,
    `${data}/slopify-remote/slopify-remote.py`, `${data}/slopify-remote/slopify.svg`]) {
    const file = Gio.File.new_for_path(path); if (file.query_exists(null)) file.delete(null);
}
const settings = new Gio.Settings({schema_id: 'org.gnome.shell'});
settings.set_strv('disabled-extensions', settings.get_strv('disabled-extensions').filter(id => id !== uuid));
settings.set_strv('enabled-extensions', [...new Set([...settings.get_strv('enabled-extensions'), uuid])]);
Gio.Settings.sync();
print(`Installed ${target.get_path()}`);
print('Python applet retired; laptop shortcuts restored. Log out and back in to load the new extension on Wayland.');
