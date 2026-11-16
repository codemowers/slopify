import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

let context=vm.createContext({JSON,Date,Set});
const load=name=>vm.runInContext(fs.readFileSync(new URL(`ui/${name}`,import.meta.url),'utf8'),context);
load('status-state.js');
const latch=vm.runInContext('new SpeakerStatus()',context);
const now=new Date().toISOString();
const point=sequence=>({sourceId:'slopify',sequence,at:now,db:-6});
const initial=()=>({now,sourceControl:{sources:[{id:'slopify',name:'Speaker',level:{peak:.5},error:'temporary',windows:[point(1),point(2)]}],output:[point(1),point(2)]}});
let state=latch.apply({snapshot:initial()});
assert.equal(state.sourceControl.output.length,2);
state=latch.apply({patch:[['/sourceControl/outputDelta',true],['/sourceControl/output',[point(3)]],['/sourceControl/sources/0/windowsDelta',true],['/sourceControl/sources/0/windows',[point(3)]],['/sourceControl/sources/0/level/peak',.75],['/sourceControl/sources/0/error']]});
assert.equal(state.sourceControl.sources[0].name,'Speaker');
assert.equal(state.sourceControl.sources[0].level.peak,.75);
assert.equal(Object.hasOwn(state.sourceControl.sources[0],'error'),false);
assert.equal(state.sourceControl.output.length,3);
assert.equal(state.sourceControl.sources[0].windows.length,3);
// The latched wire state must retain the delta batch, not expanded history.
assert.equal(latch.state.sourceControl.output.length,1);
assert.equal(latch.state.sourceControl.sources[0].windows.length,1);
state=latch.apply({patch:[['/sourceControl/output',null],['/sourceControl/sources/0/windows']]});
assert.equal(state.sourceControl.output.length,3);
assert.equal(state.sourceControl.sources[0].windows.length,3);
state=latch.apply({patch:[['/sourceControl/sources',[]]]});
assert.equal(state.sourceControl.sources.length,0);
state=latch.apply({snapshot:initial()});
assert.equal(state.sourceControl.output.length,2);
latch.apply({patch:[['/escaped~1key~0',true],['/__proto__',{polluted:true}]]});
assert.equal(latch.state['escaped/key~'],true);
assert.equal({}.polluted,undefined);

console.log('Status patches, source and output history, removals and reconnects passed');
