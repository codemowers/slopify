// Pure helpers shared by the extension and its small Node test.
export const transportActions = new Set(['toggle', 'play', 'pause', 'stop', 'next', 'prev']);
export function privateIPv4(address) {
    const a = address.split('.').map(Number);
    return a.length === 4 && a.every(n => Number.isInteger(n) && n >= 0 && n <= 255) &&
        (a[0] === 10 || a[0] === 172 && a[1] >= 16 && a[1] <= 31 ||
         a[0] === 192 && a[1] === 168 || a[0] === 169 && a[1] === 254);
}
export function canTransport(status, transport) {
    return !!transport.available && (!status.sourceControl || status.sourceControl.selected === 'slopify' || !!transport.source);
}
export function playingInfo(status, transport) {
    const source = status.sourceControl?.sources?.find(s => s.id === status.sourceControl.selected);
    if (!source || source.id === 'slopify')
        return {...transport, source: 'Slopify', playing: transport.state === 'playing'};
    const tags = source.metadata ?? {};
    return {title: tags['media.title'] || tags.title || tags['media.name'] || '',
        artist: tags['media.artist'] || tags.artist || '', album: tags['media.album'] || tags.album || '',
        source: sourceLabel(source), playing: !!source.active};
}
const protocolNames = {pulse: 'PulseAudio', rtp: 'Multicast', esound: 'ESound', upnp: 'UPnP', airplay: 'AirPlay'};
// sourceLabel names every input the same way: protocol, then who sends it.
export function sourceLabel(source) {
    if (source.id === 'slopify') return 'Slopify queue';
    const protocol = protocolNames[source.protocol] || 'Input';
    // Multicast names carry " · group"; the group adds nothing to who is sending.
    const sender = (source.host || source.name || source.ip || '').replace(/ · [0-9a-f.:]+$/i, '');
    return sender ? `${protocol} from ${sender}` : protocol;
}
export function volumeText(volume) {
    return volume?.muted ? 'Muted' : Number.isFinite(volume?.db) ? `${volume.db.toFixed(1)} dB` : 'Volume unavailable';
}
export function volumeFraction(v) {
    return v?.muted ? 0 : Math.max(0, Math.min(1, ((v?.level ?? 0) - (v?.min ?? 0)) / Math.max(1, (v?.max ?? 100) - (v?.min ?? 0))));
}

// Adjacent perceptual levels can map to the same hardware gain. In that case
// sending level - 1 can round straight back to the current level forever.
export function volumeStep(v, direction) {
    const current = v.steps?.find(step => step.level === v.level);
    let level = v.level;
    while (level + direction >= v.min && level + direction <= v.max) {
        level += direction;
        const step = v.steps?.find(step => step.level === level);
        if (!Number.isFinite(current?.db) || !Number.isFinite(step?.db) ||
            step.db !== current.db || !!step.muted !== !!current.muted) return level;
    }
    return level;
}

// Soup omits default ports from Host; Origin must use the same spelling.
export function controlOrigin(origin) {
    return origin.replace(/^(http:\/\/[^/]+):80$/, '$1').replace(/^(https:\/\/[^/]+):443$/, '$1');
}

// Resolve only against discovered control endpoints; sink text never supplies a URL.
export function outputPeer(output, peers) {
    if (!output) return '';
    const name = (output.name || '').toLowerCase();
    const description = output.description || '';
    if (!name.startsWith('tunnel.') && !name.startsWith('raop_sink.') &&
        !description.includes(' via PulseAudio')) return '';
    const matches = [...peers].filter(peer =>
        (peer.host && (name.startsWith(`tunnel.${peer.host}.`) || name.startsWith(`raop_sink.${peer.host}.`))) ||
        (peer.ip && (description.includes(`[${peer.ip}]:`) || name.startsWith(`tunnel.${peer.ip}.`)))
    );
    return matches.length === 1 ? matches[0].id : '';
}
