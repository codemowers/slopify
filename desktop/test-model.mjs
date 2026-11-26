import assert from 'node:assert/strict';
import {privateIPv4, canTransport, playingInfo, volumeFraction, controlOrigin, volumeText, sourceLabel, volumeStep, outputPeer} from './extension/model.js';
assert(privateIPv4('10.0.0.2')); assert(privateIPv4('192.168.1.2'));
for (const address of ['127.0.0.1', '8.8.8.8', '239.1.1.1', '10.999.0.1']) assert(!privateIPv4(address));
assert(!canTransport({sourceControl: {selected: 'lan_2'}}, {available: true}));
assert(canTransport({sourceControl: {selected: 'slopify'}}, {available: true}));
assert.equal(playingInfo({sourceControl: {selected: 'lan_2', sources: [{id: 'lan_2', name: 'Amp2', active: true,
    metadata: {'media.title': 'Track', 'media.artist': 'Artist', 'media.album': 'Album'}}]}}, {}).title, 'Track');
assert.equal(volumeFraction({min: 0, max: 40, level: 20}), 0.5);
assert.equal(volumeFraction({min: 0, max: 40, level: 20, muted: true}), 0);
// Amp4's 59 and 60 both produce -13.5 dB and read back as 60.
const amp4 = {min: 0, max: 60, level: 60, steps: [
    {level: 58, db: -14}, {level: 59, db: -13.5}, {level: 60, db: -13.5},
]};
assert.equal(volumeStep(amp4, -1), 58);
assert.equal(volumeStep(amp4, 1), 60);
assert.equal(volumeStep({...amp4, level: 58}, 1), 59);
assert.equal(volumeStep({min: 0, max: 60, level: 0}, -1), 0);
assert.equal(volumeStep({min: 0, max: 60, level: 34}, -1), 33);
assert.equal(volumeStep({min: 0, max: 60, level: 34}, 1), 35);
assert.equal(volumeStep({min: 0, max: 2, level: 2, steps: [
    {level: 0, db: -10, muted: true}, {level: 1, db: -10}, {level: 2, db: -10},
]}, -1), 0);
console.log('Model checks passed');

assert.equal(controlOrigin('http://10.0.0.2:80'), 'http://10.0.0.2');
assert.equal(controlOrigin('http://speaker.local:8080'), 'http://speaker.local:8080');
assert.equal(controlOrigin('https://speaker.local:443'), 'https://speaker.local');
assert.equal(volumeText({db: -24}), '-24.0 dB');
assert.equal(volumeText({db: -103.5}), '-103.5 dB');
assert.equal(volumeText({db: -24, muted: true}), 'Muted');
assert.equal(sourceLabel({id: 'slopify', name: 'Slopify'}), 'Slopify queue');
assert.equal(sourceLabel({id: 'lan_4', protocol: 'rtp', name: 'lauri-t14s · 239.192.193.62'}), 'Multicast from lauri-t14s');
assert.equal(sourceLabel({id: 'lan_1', protocol: 'pulse', name: 'lauri-t14s', host: 'lauri-t14s'}), 'PulseAudio from lauri-t14s');
assert.equal(sourceLabel({id: 'lan_6', protocol: 'esound', name: 'WinESD'}), 'ESound from WinESD');
assert.equal(sourceLabel({id: 'lan_8', protocol: 'airplay', name: 'iPhone'}), 'AirPlay from iPhone');

const peers = [{id: 'amp', host: 'amp.local', ip: '10.0.0.2'},
    {id: 'dac', host: 'dac.local', ip: '10.0.0.3'}];
assert.equal(outputPeer({name: 'tunnel.amp.local.slopify.output'}, peers), 'amp');
assert.equal(outputPeer({name: 'raop_sink.dac.local.10.0.0.3.5000'}, peers), 'dac');
assert.equal(outputPeer({name: 'custom', description: 'Amp via PulseAudio on  [10.0.0.2]:4713'}, peers), 'amp');
assert.equal(outputPeer({name: 'tunnel.10.0.0.3.slopify.output'}, peers), 'dac');
assert.equal(outputPeer({name: 'alsa_output.local', description: 'Amp'}, peers), '');
assert.equal(outputPeer({name: 'tunnel.amp.local.evil.slopify.output'}, []), '');
assert.equal(outputPeer({name: 'tunnel.amp.local.slopify.output'}, [...peers, {...peers[0], id: 'duplicate'}]), '');
assert.equal(outputPeer({name: 'tunnel.amp.local.slopify.output', description: 'Other via PulseAudio on  [10.0.0.3]:4713'}, peers), '');
assert.equal(outputPeer(null, peers), '');
assert.equal(outputPeer({name: 'tunnel.10.0.0.20.output'}, peers), '');
