import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';
const requests = await readFile(new URL('./ui/requests.js', import.meta.url), 'utf8');
const sources = await readFile(new URL('./ui/sources.js', import.meta.url), 'utf8');

test('queued source request times out, releases controls, and is not replayed', async () => {
 let timeout, calls = 0, cleared = false;
 const nodes = new Map();
 const context = vm.createContext({AbortController,
  setTimeout(fn) { timeout = fn; return 1; }, clearTimeout() { cleared = true; },
  fetch(_url, {signal}) { calls++; return new Promise((_, reject) => signal.addEventListener('abort', () => reject(new Error('aborted')))); },
  document: {getElementById(id) { if (!nodes.has(id)) nodes.set(id, {textContent: '', addEventListener() {}}); return nodes.get(id); }},
  renderRoutingProgress() {},
 });
 vm.runInContext(requests + sources.slice(0, sources.indexOf("document.getElementById('source-mode').addEventListener")), context);
 vm.runInContext('sourceControl = {revision: 3};', context);
 const pending = vm.runInContext('sourceCommand({selected: "slopify"})', context);
 assert.equal(vm.runInContext('sourceBusy', context), true);
 timeout();
 await pending;
 assert.equal(vm.runInContext('sourceBusy', context), false);
 assert.match(nodes.get('source-error').textContent, /timed out/);
 assert.match(vm.runInContext('sourceRequestError', context), /Check the current state/);
 assert.equal(calls, 1);
 assert.equal(cleared, true);
});

test('successful no-content command clears its deadline', async () => {
 let cleared = false;
 const context = vm.createContext({AbortController,
  setTimeout() { return 1; }, clearTimeout() { cleared = true; },
  fetch: async () => ({ok: true, status: 204}),
 });
 vm.runInContext(requests, context);
 assert.equal(await vm.runInContext('speakerCommand("/api/sources", {})', context), null);
 assert.equal(cleared, true);
});

test('speaker tabs share SSE and keep it open until the last tab leaves', async () => {
 const worker = await readFile(new URL('./ui/events-worker.js', import.meta.url), 'utf8');
 const streams = [];
 class EventSource {
  constructor() { streams.push(this); }
  close() { this.closed = true; }
 }
 const context = vm.createContext({EventSource});
 vm.runInContext(worker, context);
 const ports = Array.from({length: 8}, () => ({messages: [], postMessage(message) { this.messages.push(message); }, start() {}, close() { this.closed=true; }}));
 for (const port of ports) { context.port=port;vm.runInContext('onconnect({ports:[port]})', context); }
 assert.equal(streams.length, 1);
 streams[0].onopen();
 streams[0].onmessage({data:'{"now":"test"}'});
 for (const port of ports) assert.equal(port.messages.at(-1).type, 'status');
 for (const port of ports.slice(0,-1)) port.onmessage({data:{type:'disconnect'}});
 assert.equal(streams[0].closed, undefined);
 ports.at(-1).onmessage({data:{type:'disconnect'}});
 assert.equal(streams[0].closed, true);
});


test('countdown and candidate connection clear when fallback is cancelled', () => {
 const nodes=new Map();
 const get=id=>{if(!nodes.has(id))nodes.set(id,{textContent:'',hidden:false,offsetWidth:800,setAttribute(){},querySelector(){return null;},getBoundingClientRect(){return {left:0,top:0,right:200,height:80};},replaceChildren(...paths){this.paths=paths;}});return nodes.get(id);};
 const context=vm.createContext({document:{getElementById:get,createElementNS(){return {attrs:{},setAttribute(k,v){this.attrs[k]=v;}};}},renderSourceStatuses(){},sourceBusy:false,sourceClock:1000,sourceColour:id=>id,
 sourceCards:new Map([['a',{root:get('card-a')}],['b',{root:get('card-b')}]]),
 sourceControl:{mode:'priority',selected:'a',sources:[{id:'a',name:'Current',connected:true,active:true},{id:'b',name:'Laptop',connected:true,active:false}],fallback:{sourceId:'b',at:new Date(6000).toISOString()}}});
 vm.runInContext(sources.slice(sources.indexOf('function renderRoutingProgress()'),sources.indexOf('new ResizeObserver(')),context);
 vm.runInContext('renderRoutingProgress();drawSourceConnections()',context);
 assert.match(get('routing-progress-text').textContent,/Switching to Laptop in 5 s/);
 assert.equal(get('switch-now').hidden,false);
 assert.equal(get('source-connections').paths.length,2);
 assert.equal(get('source-connections').paths[1].attrs['stroke-dasharray'],'5 5');
 vm.runInContext('sourceControl.fallback=null;renderRoutingProgress();drawSourceConnections()',context);
 assert.equal(get('routing-progress').hidden,true);
 assert.equal(get('switch-now').hidden,true);
 assert.equal(get('source-connections').paths.length,1);
 vm.runInContext('sourceControl.selected="b";sourceControl.sources[0].active=false;sourceControl.sources[1].active=true;renderRoutingProgress();drawSourceConnections()',context);
 assert.equal(get('source-connections').paths.length,1);
 assert.equal(get('source-connections').paths[0].attrs.stroke,'b');
 assert.equal(get('source-connections').paths[0].attrs['stroke-dasharray'],undefined);
});
