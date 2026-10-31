'use strict';
'require view';
'require form';
'require fs';
'require uci';

var KEYS = [
	[ '164', 'Play/Pause (164)' ], [ '200', 'Play CD (200)' ], [ '207', 'Play (207)' ], [ '201', 'Pause CD (201)' ], [ '119', 'Pause (119)' ],
	[ '166', 'Stop CD (166)' ], [ '128', 'Stop (128)' ], [ '165', 'Previous song (165)' ], [ '163', 'Next song (163)' ],
	[ '168', 'Rewind (168)' ], [ '208', 'Fast forward (208)' ], [ '167', 'Record (167)' ], [ '161', 'Eject (161)' ],
	[ '113', 'Mute (113)' ], [ '114', 'Volume down (114)' ], [ '115', 'Volume up (115)' ],
	[ '226', 'Media (226)' ], [ '158', 'Back (158)' ], [ '159', 'Forward (159)' ], [ '172', 'Home page (172)' ], [ '150', 'WWW (150)' ],
	[ '155', 'Mail (155)' ], [ '140', 'Calculator (140)' ], [ '157', 'Computer (157)' ], [ '142', 'Sleep (142)' ]
];

function controlsOf(audio, card) {
	var id = (card == 'auto' || !card) ? audio.auto : card;
	var found = audio.cards.filter(function(c) { return c.id == id; })[0];
	return found ? found.controls : null;
}

function speakerPage() {
	var port = (uci.get('slopify', 'player', 'local_listen') || '').match(/:(\d+)$/);
	if (!port)
		return E('p', {}, _('The speaker page is disabled.'));
	var url = '%s//%s:%s/'.format(window.location.protocol == 'https:' ? 'http:' : window.location.protocol, window.location.hostname, port[1]);
	return E('p', {}, [ E('a', { href: url, target: '_blank', rel: 'noopener' }, _('Open the speaker page')), ' ', _('for source selection, volume and meters.') ]);
}

return view.extend({
	load: function() {
		return Promise.all([
			fs.exec('/usr/libexec/slopify/audio-info').then(function(res) {
				return JSON.parse(res.stdout);
			}).catch(function() {
				return { auto: '', cards: [], inputs: [], lirc: [] };
			}),
			uci.load([ 'slopify', 'system' ])
		]);
	},

	render: function(data) {
		var audio = data[0];
		var m = new form.Map('slopify', _('Slopify player'), _('Saving restarts the player, which interrupts playback.'));
		m.chain('system');
		var s = m.section(form.NamedSection, 'player', 'player');
		s.addremove = false;
		s.tab('general', _('General'));
		s.tab('volume', _('Volume control'));
		s.tab('ir', _('Infrared'), _('Available when no volume control is selected. Drives a motorised amp knob by remote codes.'));
		s.tab('receivers', _('Receivers'));
		s.tab('inputs', _('Controls'));
		var o;

		function inputDevice(tab, option, title, description) {
			var v = s.taboption(tab, form.Value, option, title, description);
			v.value('', _('None'));
			audio.inputs.forEach(function(name) { v.value(name); });
			return v;
		}

		function button(action, title, key) {
			inputDevice('inputs', action + '_input', title);
			var k = s.taboption('inputs', form.Value, action + '_key', _('%s key').format(title), _('Pick a media key or type a <a href="%s" target="_blank" rel="noopener">Linux key code</a> (the KEY_ values). button@19 sends 164.').format('https://github.com/torvalds/linux/blob/master/include/uapi/linux/input-event-codes.h'));
			KEYS.forEach(function(entry) { k.value(entry[0], entry[1]); });
			k.placeholder = key;
			k.datatype = 'range(1,767)';
			k.validate = function(section, value) {
				var mine = this.section.formvalue(section, action + '_input') + ':' + (value || key);
				var clash = [ [ 'playpause', '164' ], [ 'previous', '165' ], [ 'next', '163' ], [ 'mute', '113' ] ].some(function(other) {
					if (other[0] == action)
						return false;
					var device = this.section.formvalue(section, other[0] + '_input');
					return device && device + ':' + (this.section.formvalue(section, other[0] + '_key') || other[1]) == mine;
				}, this);
				return clash ? _('Another button already uses this key on this device') : true;
			};
		}

		function irCode(option, title, placeholder) {
			var c = s.taboption('ir', form.Value, option, title);
			c.placeholder = placeholder;
			c.retain = true;
			c.validate = function(section, value) {
				return (value == '' || /^(0x[0-9a-fA-F]+|[0-9]+)$/.test(value)) ? true : _('Must be a decimal or 0x number');
			};
		}

		o = s.taboption('general', form.DummyValue, '_speaker', _('Speaker page'));
		o.renderWidget = speakerPage;

		var system = uci.sections('system', 'system')[0];
		if (system) {
			o = s.taboption('general', form.Value, '_description', _('Speaker name'), _('Shown on the speaker page, in Slopify and to senders. This is the OpenWrt system description; empty uses the hostname.'));
			o.uciconfig = 'system';
			o.ucisection = system['.name'];
			o.ucioption = 'description';
			o.placeholder = system.hostname;
		}

		o = s.taboption('general', form.Value, 'server', _('Slopify server'), _('Empty plays LAN sources only.'));
		o.placeholder = 'https://slopify.example.com';
		o.validate = function(section, value) {
			if (value != '' && !/^https?:\/\/[^\s'\\]+$/.test(value))
				return _('Must be an http(s) origin');
			if (value == '' && !this.section.formvalue(section, 'local_listen'))
				return _('Needs a server or a speaker page address');
			return true;
		};

		o = s.taboption('general', form.ListValue, 'card', _('Sound card'));
		o.value('auto', audio.auto ? _('Automatic (%s)').format(audio.auto) : _('Automatic'));
		audio.cards.forEach(function(c) { o.value(c.id, c.name ? '%s (%s)'.format(c.name, c.id) : c.id); });

		o = s.taboption('general', form.Value, 'local_listen', _('Speaker page address'), _('Empty disables the speaker page and every receiver.'));
		o.placeholder = ':8080';

		o = s.taboption('volume', form.ListValue, 'volume_control', _('Volume control'),
			_('Hardware mixer control. None leaves the mixers untouched: over SSH, run <code>alsamixer -c &lt;card&gt;</code> and set the card\'s volume control to maximum, then set the level on the amplifier. Turn the amplifier down first.'));
		o.value('', _('None'));
		audio.cards.forEach(function(c) {
			c.controls.forEach(function(ctl) {
				o.value(ctl.name, '%s (%s%s)'.format(ctl.name, c.id, ctl.db ? ', dB' : ''));
			});
		});
		o.validate = function(section, value) {
			if (!value)
				return true;
			var controls = controlsOf(audio, this.section.formvalue(section, 'card'));
			if (controls && !controls.some(function(c) { return c.name == value; }))
				return _('The selected sound card has no such control');
			return true;
		};

		[ [ 'volume_min', _('Minimum volume') ], [ 'volume_max', _('Maximum volume') ], [ 'volume_initial', _('Initial volume') ] ].forEach(function(v) {
			o = s.taboption('volume', form.Value, v[0], v[1], _('alsamixer displayed scale, 0 to 100.'));
			o.datatype = 'range(0,100)';
			o.rmempty = false;
			o.retain = true;
			o.depends({ volume_control: '', '!reverse': true });
		});

		o = inputDevice('volume', 'rotary_input', _('Rotary'), _('A relative-axis device that turns this volume. For a bare encoder on a Raspberry Pi, uncomment the <code>dtoverlay=rotary-encoder</code> line in <code>/boot/config.txt</code> (A on BCM23, B on BCM24, common to ground; <code>dtoverlay=gpio-key</code> adds the push button on BCM25), reboot, then pick <code>rotary@17</code>. A device plugged in after the player started needs a player restart.'));
		o.retain = true;
		o.depends({ volume_control: '', '!reverse': true });
		o.depends({ volume_control: '', ir_device: '', '!reverse': true });

		o = s.taboption('ir', form.Value, 'ir_device', _('IR output'), _('A LIRC transmitter. The current OpenWrt kernel has no LIRC support, so none is listed.'));
		o.retain = true;
		o.value('', _('None'));
		audio.lirc.forEach(function(d) { o.value(d); });
		o.validate = function(section, value) {
			if (value == '')
				return true;
			if (!/^\/dev\/lirc[0-9]+$/.test(value))
				return _('Must be a /dev/lirc device');
			return this.section.formvalue(section, 'local_listen') ? true : _('IR amp control needs the speaker page address');
		};
		irCode('ir_address', _('IR address'), '0x877C');
		irCode('ir_volume_up', _('IR volume up'), '0x88');
		irCode('ir_volume_down', _('IR volume down'), '0x8C');
		irCode('ir_power_on', _('IR power on'), '0x25');
		irCode('ir_power_off', _('IR power off'), '0xC8');

		o = s.taboption('ir', form.Value, 'ir_sweep', _('Calibration time'), _('How long to hold volume down to reach zero from anywhere. Longer than the knob\'s full travel.'));
		o.placeholder = '6s';
		o.retain = true;
		o.validate = function(section, value) {
			return (value == '' || /^[0-9]+(ms|s|m)$/.test(value)) ? true : _('Must be a duration such as 6s');
		};

		o = s.taboption('receivers', form.Flag, 'pulse', _('PulseAudio/PipeWire'), _('TCP 4713, advertised over mDNS. No authentication.'));
		o.rmempty = false;
		o = s.taboption('receivers', form.Value, 'live_buffer_ms', _('Output buffer'), _('Milliseconds of audio queued at the sound card for these receivers, 40 to 2000. Lower keeps video closer to sound; too low causes dropouts. Empty lets the card choose, 150 to 250.'));
		o.datatype = 'range(40,2000)';
		o.placeholder = 'auto';

		o = s.taboption('receivers', form.Flag, 'esound', _('ESound'), _('TCP 16001. No authentication.'));
		o.rmempty = false;
		o = s.taboption('receivers', form.Flag, 'upnp', _('UPnP renderer'), _('TCP 49494 and UDP 1900. No authentication.'));
		o.rmempty = false;

		button('playpause', _('Play/Pause'), '164');
		button('previous', _('Previous'), '165');
		button('next', _('Next'), '163');
		button('mute', _('Mute'), '113');

		return m.render();
	}
});
