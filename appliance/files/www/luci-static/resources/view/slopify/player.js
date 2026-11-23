'use strict';
'require view';
'require form';
'require fs';
'require uci';
'require ui';

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
			uci.load([ 'slopify', 'system' ]),
			fs.exec('/usr/libexec/slopify/hat-info').then(function(res) {
				if (res.code) throw new Error(res.stderr);
				return JSON.parse(res.stdout);
			}).catch(function(err) { return { error: err.message }; }),
   fs.exec('/usr/libexec/slopify/ir-info').then(function(res) {
    if (res.code) throw new Error(res.stderr);
    return JSON.parse(res.stdout);
   }).catch(function() { return []; })
		]);
	},

	render: function(data) {
		var audio = data[0];
		var irAvailable = audio.irSetup === true;
		var m = new form.Map('slopify', _('Slopify player'), _('Saving restarts the player, which interrupts playback.'));
		m.chain('system');
		var s = m.section(form.NamedSection, 'player', 'player');
		s.addremove = false;
		s.tab('general', _('General'));
		s.tab('sources', _('Sources'), _('Configure Slopify queue playback and local queue sharing. Incoming LAN audio protocols are configured in Receivers.'));
		s.tab('sinks', _('Sinks'), _('Configure local audio output and multicast transmission to other speakers.'));
		s.tab('receivers', _('Receivers'), _('Enable incoming LAN audio protocols and configure their input buffers.'));
		s.tab('volume', _('Volume control'));
		s.tab('rotary', _('Rotary'), _('Select a relative-axis rotary encoder for volume. Its push button uses the standard media key configured in the GPIO overlay.'));
		s.tab('ir', irAvailable ? _('IR receive') : _('IR receive (unavailable)'), _('Receive handheld remote buttons and map them to standard Linux multimedia keys for Slopify. This does not transmit commands to an amplifier.'));
		var o;

  o = s.taboption('general', form.DummyValue, '_media_inputs', _('Media keys'));
  o.renderWidget = function() { return E('p', {}, _('Standard multimedia keys are read automatically from all input devices. Connecting or removing an input device restarts the player. Ordinary keys and mouse movement are ignored.')); };
  o = s.taboption('rotary', form.ListValue, 'rotary_input', _('Rotary encoder'), _('Relative-axis movement controls volume only on this selected device. Enable its GPIO overlay first.'));
  o.value('', _('Disabled'));
  var inputNames = (audio.inputs || []).slice();
  var rotary = uci.get('slopify', 'player', 'rotary_input');
  if (rotary && inputNames.indexOf(rotary) < 0) inputNames.push(rotary);
  inputNames.forEach(function(name) { o.value(name); });

		o = s.taboption('general', form.DummyValue, '_speaker', _('Speaker page'));
		o.renderWidget = speakerPage;
		o = s.taboption('general', form.DummyValue, '_log', _('Player log'));
		o.renderWidget = function() {
			return E('p', {}, [ E('a', { href: L.url('admin/status/syslog') }, _('Open the system log')), ' ',
				_('and look for ujail lines, which carry the player\'s messages. Over SSH: logread -e ujail')]);
		};

		var system = uci.sections('system', 'system')[0];
		if (system) {
			o = s.taboption('general', form.Value, '_description', _('Speaker name'), _('Shown on the speaker page, in Slopify and to senders. This is the OpenWrt system description; empty uses the hostname.'));
			o.uciconfig = 'system';
			o.ucisection = system['.name'];
			o.ucioption = 'description';
			o.placeholder = system.hostname;
		}

		o = s.taboption('sources', form.Value, 'server', _('Slopify server'), _('Empty plays LAN sources only.'));
		o.placeholder = 'https://slopify.example.com';
		o.validate = function(section, value) {
			if (value != '' && !/^https?:\/\/[^\s'\\]+$/.test(value))
				return _('Must be an http(s) origin');
			if (value == '' && !this.section.formvalue(section, 'local_listen'))
				return _('Needs a server or a speaker page address');
			return true;
		};

		o = s.taboption('sinks', form.ListValue, 'card', _('Sound card'));
		o.value('auto', audio.auto ? _('Automatic (%s)').format(audio.auto) : _('Automatic'));
		audio.cards.forEach(function(c) { o.value(c.id, c.name ? '%s (%s)'.format(c.name, c.id) : c.id); });

		o = s.taboption('sinks', form.Value, 'output_buffer_ms', _('Output buffer'), _('ALSA kernel buffer capacity for all sources, in milliseconds, 40 to 10000 (10 seconds). Empty negotiates 40 to 80 for LAN sources and 200 to 500 for Slopify originals. Larger buffers add scheduling margin and playback delay. Input and download buffers are separate. The sound card may negotiate a different capacity.'));
		o.datatype = 'range(40,10000)';
		o.placeholder = 'auto';

		o = s.taboption('sinks', form.Flag, 'allow_truncate', _('Allow 16-bit fallback'),
			_('If no output format preserves the input precision, permit discarding low bits to play at 16 bits. Sample rate stays unchanged. Disabled by default.'));
		o.default = '0';
		o.rmempty = false;

		var hat = data[2];
		if (hat.available) {
			o = s.taboption('sinks', form.ListValue, '_hat_overlay', _('Audio HAT overlay'),
				_('Auto lets Raspberry Pi firmware select the HAT from its EEPROM. Off disables HAT overlays; USB and onboard audio remain available. Save, then reboot to apply.'));
			o.value('off', _('Off'));
			var detected = hat.eeprom ? hat.product + (hat.active ? ' — ' + hat.active : '') : _('no EEPROM detected');
			o.value('auto', _('Auto (%s)').format(detected));
			(Array.isArray(hat.overlays) ? hat.overlays : []).forEach(function(name) { o.value(name); });
			o.rmempty = false;
			o.cfgvalue = function() { return hat.selected; };
			o.readonly = !!hat.error;
			o.write = function(section, value) {
				return fs.exec('/usr/libexec/slopify/hat-set', [ value, hat.revision ]).then(function(res) {
					if (res.code) throw new Error(res.stderr || _('Could not save HAT overlay'));
					hat = JSON.parse(res.stdout);
					ui.addNotification(null, E('p', {}, _('Audio HAT overlay saved. Reboot from System → Reboot to apply.')), 'info');
				});
			};
			if (hat.error) {
				o = s.taboption('sinks', form.DummyValue, '_hat_error', _('HAT configuration'));
				o.renderWidget = function() { return E('p', { 'class': 'alert-message warning' }, hat.error); };
			}
		}

		o = s.taboption('general', form.Value, 'local_listen', _('Speaker page address'), _('Empty disables the speaker page and every receiver.'));
		o.placeholder = ':80';
		o = s.taboption('general', form.Value, 'local_sse_interval_ms', _('Speaker status interval (ms)'), _('Slower status updates reduce CPU use. Chart measurements remain every 200 ms.'));
		o.datatype = 'range(100,10000)';
		o.placeholder = '200';
		o = s.taboption('general', form.ListValue, 'metrics', _('Prometheus metrics'), _('Disabling skips collection and the /metrics endpoint.'));
		o.value('', _('Automatic (off on MIPS)'));
		o.value('1', _('Enabled'));
		o.value('0', _('Disabled'));

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

  o = s.taboption('ir', form.DummyValue, '_ir_requirements', _('IR input support'));
  o.renderWidget = function() {
   return E('p', { role: 'note' }, irAvailable
    ? _('Kernel IR support and ir-keytable are present. Enable a receiver overlay and reboot before selecting it below. Select the protocol, Save & Apply, Learn each button, then Save & Apply again. Volume and mute need amplifier volume control configured in the Volume control tab.')
    : _('IR input is unavailable on this image. Official Image Builder release images do not include the required kernel IR support. Adding a GPIO overlay or installing ir-keytable alone cannot enable it. A separately built kernel with matching RC-core, LIRC, GPIO IR and protocol decoders, plus ir-keytable, is required. RECS80 and Denon also need built-in RC-core and BPF LIRC decoding. For HiFiBerry Amp4, select hifiberry-dacplus-std in Sinks; wire a suitable receiver to a free GPIO (the documented setup uses BCM26 / pin 37), enable gpio-ir,gpio_pin=26 in /boot/config.txt, and reboot. Then select the receiver and protocol here, Save & Apply, Learn each button and Save & Apply again. Configure amplifier volume in Volume control. See appliance/README.md for the full setup.'));
  };
  var receivers = data[3] || [];
  o = s.taboption('ir', form.ListValue, 'ir_receiver', _('Remote receiver'), _('Select a receiver and protocol, then Save & Apply once. Use Learn beside each action below, then Save & Apply the bindings. A learning press is temporarily isolated from playback controls. GPIO overlays must be enabled first.'));
  o.value('', _('Disabled'));
  receivers.forEach(function(r) { o.value(r.key, '%s (%s)'.format(r.name || r.id, r.id)); });
  var configuredReceiver = uci.get('slopify', 'player', 'ir_receiver');
  if (configuredReceiver && !receivers.some(function(r) { return r.key === configuredReceiver; }))
   o.value(configuredReceiver, _('Configured receiver unavailable'));
  o = s.taboption('ir', form.ListValue, 'ir_protocol', _('Remote protocol'));
  [['rc5', 'RC-5'], ['rc6', 'RC-6'], ['nec', 'NEC'], ['recs80', 'RECS80'], ['recs80ext', 'RECS80EXT'], ['kaseikyo-denon', 'Denon Kaseikyo']].forEach(function(p) { o.value(p[0], p[1]); });
  o.default = 'rc5';
  o.retain = true;
  o.depends({ ir_receiver: '', '!reverse': true });
  var remoteActions = [['volume_up', _('Volume up'), 'KEY_VOLUMEUP'], ['volume_down', _('Volume down'), 'KEY_VOLUMEDOWN'], ['mute', _('Mute'), 'KEY_MUTE'], ['playpause', _('Play/Pause'), 'KEY_PLAYPAUSE'], ['play', _('Play'), 'KEY_PLAY'], ['pause', _('Pause'), 'KEY_PAUSECD'], ['previous', _('Previous'), 'KEY_PREVIOUSSONG'], ['next', _('Next'), 'KEY_NEXTSONG']];
  var learning = false;
  remoteActions.forEach(function(a) {
   o = s.taboption('ir', form.Value, 'ir_key_' + a[0], a[1] + ' · ' + a[2], _('Remote scancode in decimal or 0x hexadecimal. Denon Kaseikyo uses the full 48-bit code; RECS80 toggle variants are mapped automatically. Empty leaves this action unmapped. Volume uses the configured amplifier control.'));
   o.depends({ ir_receiver: '', '!reverse': true });
   o.retain = true;
   o.renderWidget = function(section, index, value) {
    var field = this;
    var widget = form.Value.prototype.renderWidget.call(field, section, index, value);
    var feedback = E('span', { role: 'status', style: 'margin-left:.5em' });
    var learn = E('button', { type:'button', class:'cbi-button cbi-button-action' }, _('Learn'));
    var clear = E('button', { type:'button', class:'cbi-button' }, _('Clear'));
    learn.disabled = clear.disabled = !irAvailable;
    learn.addEventListener('click', function() {
     if (learning || !irAvailable) return;
     var receiver = field.section.formvalue(section, 'ir_receiver');
     var protocol = field.section.formvalue(section, 'ir_protocol') || 'rc5';
     if (!receiver || uci.get('slopify', 'player', 'ir_receiver') !== receiver || (uci.get('slopify', 'player', 'ir_protocol') || 'rc5') !== protocol) {
      feedback.textContent = _('Save & Apply the receiver and protocol first.');
      return;
     }
     learning = true;
     learn.disabled = clear.disabled = true;
     learn.classList.add("spinning");
     feedback.textContent = _('Press and release the remote button now…');
     fs.exec('/usr/libexec/slopify/ir-learn', [receiver, protocol]).then(function(result) {
      if (result.code) throw new Error(result.stderr || _('Learning failed'));
      var learned = JSON.parse(result.stdout);
      if (!/^0x[0-9a-f]+$/.test(learned.scancode || '')) throw new Error(_('Invalid learned code'));
      field.getUIElement(section).setValue(learned.scancode);
      field.triggerValidation(section);
      feedback.textContent = _('Learned. Save & Apply to use it.');
     }).catch(function(error) { feedback.textContent = error.message; }).finally(function() {
      learning = false;
      learn.disabled = clear.disabled = false;
      learn.classList.remove("spinning");
     });
    });
    clear.addEventListener('click', function() {
     if (learning || !irAvailable) return;
     field.getUIElement(section).setValue('');
     field.triggerValidation(section);
     feedback.textContent = '';
    });
    return E('div', {}, [widget, ' ', learn, ' ', clear, feedback]);
   };
   o.validate = function(section, value) {
    if (value === '') return true;
    var protocol = this.section.formvalue(section, 'ir_protocol');
    var limit = protocol === 'kaseikyo-denon' ? 0xffffffffffff : protocol === 'recs80' ? 0x3ff : protocol === 'recs80ext' ? 0xfff : 0xffffffff;
    if (!/^(0x[0-9a-fA-F]+|[0-9]+)$/.test(value) || Number(value) > limit)
     return _('Scancode exceeds the selected protocol range.');
    var toggle = protocol === 'recs80' ? 0x200 : protocol === 'recs80ext' ? 0x400 : 0;
    function canonical(v) { return toggle ? Number(v) & ~toggle : Number(v); }
    var number = canonical(value);
    var duplicate = remoteActions.some(function(other) {
     var v = this.section.formvalue(section, 'ir_key_' + other[0]);
     return other[0] !== a[0] && v !== '' && v != null && canonical(v) === number;
    }, this);
    return !duplicate || _('Another action already uses this scancode.');
   };
  });
  o = s.taboption('ir', form.DummyValue, '_ir_carrier_help', _('Receiver carrier'));
  o.renderWidget = function() {
   return E('p', {}, _('Demodulating GPIO receivers have a fixed hardware frequency, commonly 36 or 38 kHz; software cannot retune them. Carrier fields appear only for receivers that advertise tuning support. Use Learn above to capture buttons without SSH.'));
  };
  [['min', _('Receive carrier / lower bound (kHz)')], ['max', _('Receive carrier upper bound (kHz)')]].forEach(function(b) {
   o = s.taboption('ir', form.Value, 'ir_rx_carrier_' + b[0] + '_khz', b[1], _('Optional hardware carrier filter. For a 36–38 kHz range, enter 36 and 38. Empty keeps the hardware default.'));
   o.datatype = 'and(uinteger,range(10,1000))';
   receivers.forEach(function(r) { if (r.carrier && (b[0] === 'min' || r.carrierRange)) o.depends('ir_receiver', r.key); });
   if (!receivers.some(function(r) { return r.carrier && (b[0] === 'min' || r.carrierRange); })) o.depends('ir_receiver', '__unsupported__');
   o.validate = function(section, value) {
    if (value === '') return true;
    var low = this.section.formvalue(section, 'ir_rx_carrier_min_khz'), high = this.section.formvalue(section, 'ir_rx_carrier_max_khz');
    return !low || !high || Number(low) <= Number(high) || _('The lower carrier bound must not exceed the upper bound.');
   };
  });

		// Keep stored bindings intact when booted into an Image Builder release.
		s.children.forEach(function(option) {
			if (option.tab === 'ir' && !irAvailable) {
				option.readonly = true;
				option.parse = function() { return Promise.resolve(); };
			}
		});
		o = s.taboption('receivers', form.Flag, 'pulse', _('PulseAudio/PipeWire'), _('TCP 4713, advertised over mDNS. No authentication.'));
		o.rmempty = false;

		o = s.taboption('receivers', form.Flag, 'esound', _('ESound'), _('TCP 16001. No authentication.'));
		o.rmempty = false;
		o = s.taboption('receivers', form.Flag, 'upnp', _('UPnP renderer'), _('TCP 49494 and UDP 1900. No authentication.'));
		o.rmempty = false;
		o = s.taboption('receivers', form.Flag, 'raop', _('AirPlay'), _('AirPlay 1 (RAOP) audio on TCP 5000 and per-session UDP ports, advertised over mDNS. PipeWire and PulseAudio send it unencrypted. No authentication.'));
		o.rmempty = false;
		o = s.taboption('receivers', form.Flag, 'raop_crypto', _('AirPlay encryption'), _('Needed by iPhone, iPad and Mac. Downloads the RAOP key from the shairport-sync repository once the network is up, then restarts the player; only the genuine key is accepted.'));
		o.rmempty = false;
		o.depends('raop', '1');
		o = s.taboption('receivers', form.Flag, 'rtp', _('Multicast RTP receiver'), _('Receives uncompressed PCM (L16/L24) audio. Sessions announced over SAP on UDP 9875. No authentication.'));
		o.rmempty = false;
		o = s.taboption('sinks', form.Flag, 'rtp_send', _('Multicast RTP sender'), _('Sends selected, locally decoded Slopify audio as uncompressed PCM (L16/L24) and track titles to the LAN. Incoming LAN sources disable transmission and advertisement. No encryption.'));
		o.rmempty = false;
		[
			['pulse_buffer_ms', 'Pulse input buffer (ms)', '0', 'range(0,2000)', 'pulse', 'Minimum stream buffer, 0–2000 ms. 0 honours the sender. PipeWire tunnels may tick above about 100 ms.'],
			['esound_buffer_ms', 'ESound input buffer (ms)', '250', 'range(20,2000)', 'esound', 'Stream buffer, 20–2000 ms.'],
			['esound_socket_ms', 'ESound socket buffer (ms)', '0', 'range(0,2000)', 'esound', 'Kernel receive queue cap, 0–2000 ms. 0 uses the system default.'],
			['rtp_buffer_ms', 'Multicast RTP input buffer (ms)', '200', 'range(20,1000)', 'rtp', 'Initial playout delay, 20–1000 ms. Capacity is twice this value to absorb clock drift.'],
			['raop_buffer_ms', 'AirPlay input buffer (ms)', '300', 'range(20,2000)', 'raop', 'Playout delay, 20–2000 ms. Capacity is twice this value to absorb clock drift.']
		].forEach(function(b) {
			o = s.taboption('receivers', form.Value, b[0], _(b[1]), _(b[5]));
			o.placeholder = b[2];
			o.datatype = b[3];
			o.depends(b[4], '1');
			o.retain = true;
		});

		o = s.taboption('sources', form.Flag, 'queue_share', _('Share queue audio locally'), _('Players on the same queue elect a multicast sender. Followers reuse its audio and fall back to direct playback when it disappears. Enable Multicast RTP receiver in Receivers first.'));
		o.depends('rtp', '1');
		o.rmempty = false;
		o = s.taboption('sinks', form.Value, 'rtp_local_delay_ms', _('Local playback delay while multicasting (ms)'), _('Additional delay before local output, 0–2000 ms; default 200. Zero disables. Tune to match receiver input and output delays. Best-effort alignment resets at track boundaries and after interruptions; device clocks can drift. Does not change ALSA capacity or incoming LAN audio.'));
		o.placeholder = '200';
		o.datatype = 'range(0,2000)';
		o.depends('rtp_send', '1');
		o.depends('queue_share', '1');
		o.retain = true;
		o = s.taboption('sinks', form.Value, 'rtp_send_group', _('Multicast sender group'), _('Generated once and saved. IPv4 multicast address, optionally with a port (default 5004).'));
		o.depends('rtp_send', '1');
		o.depends('queue_share', '1');
		o.rmempty = false;
		o.validate = function(section, value) {
			var parts = /^(\d+)\.(\d+)\.(\d+)\.(\d+)(?::(\d+))?$/.exec(value || '');
			if (!parts) return _('Enter an IPv4 multicast address or address:port.');
			var octets = parts.slice(1, 5).map(Number), port = parts[5] ? Number(parts[5]) : 5004;
			return (octets[0] >= 224 && octets[0] <= 239 && octets.every(function(n) { return n <= 255; }) && port >= 1024 && port <= 65535 && port !== 9875) || _('Use a multicast address and port 1024–65535, excluding SAP port 9875.');
		};


		return m.render().then(function(node) {
			if (!irAvailable) {
				// Keep the tab clickable so its setup explanation remains accessible.
				node.appendChild(E('style', {}, '.cbi-tabmenu li[data-tab="ir"] > a { color: #777; } [data-tab="ir"] .cbi-value:not([id$="._ir_requirements"]) { opacity: .6; }'));
			}
			return node;
		});
	}
});
