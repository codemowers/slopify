import assert from 'node:assert/strict';
import {privateIPv4, canTransport, playingInfo, volumeFraction, controlOrigin, volumeText, sourceLabel} from './extension/model.js';
assert(privateIPv4('10.0.0.2')); assert(privateIPv4('192.168.1.2'));
for (const address of ['127.0.0.1', '8.8.8.8', '239.1.1.1', '10.999.0.1']) assert(!privateIPv4(address));
assert(!canTransport({sourceControl: {selected: 'lan_2'}}, {available: true}));
assert(canTransport({sourceControl: {selected: 'slopify'}}, {available: true}));
assert.equal(playingInfo({sourceControl: {selected: 'lan_2', sources: [{id: 'lan_2', name: 'Amp2', active: true,
    metadata: {'media.title': 'Track', 'media.artist': 'Artist', 'media.album': 'Album'}}]}}, {}).title, 'Track');
assert.equal(volumeFraction({min: 0, max: 40, level: 20}), 0.5);
assert.equal(volumeFraction({min: 0, max: 40, level: 20, muted: true}), 0);
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
