// One background status stream per speaker origin, shared by all its tabs.
const ports = new Set();
let stream = null, latest = null, connection = 'connecting';
function send(port, message) {
 try { port.postMessage(message); } catch (_) { ports.delete(port); }
}
function broadcast(message) { for (const port of ports) send(port, message); }
function stopIfUnused() {
 if (!ports.size) { stream?.close(); stream=null; latest=null; connection='connecting'; }
}
onconnect = event => {
 const port = event.ports[0];
 if (ports.size >= 16) { port.postMessage({type:'error'});port.close();return; }
 ports.add(port);
 port.onmessage = event => {
  if (event.data?.type === 'disconnect') { ports.delete(port);port.close();stopIfUnused(); }
 };
 port.onmessageerror = () => { ports.delete(port);port.close();stopIfUnused(); };
 port.start();
 send(port,{type:connection});
 if (latest !== null) send(port,{type:'status',data:latest});
 if (stream) return;
 stream = new EventSource('/api/events');
 stream.onopen = () => { connection='open';broadcast({type:'open'}); };
 stream.onerror = () => { connection='error';broadcast({type:'error'}); };
 stream.onmessage = event => { latest=event.data;broadcast({type:'status',data:latest}); };
};
