// Latch status patches independently of chart history. The wire state's output
// is a batch of new points; expanding it must not mutate the next patch's base.
class SpeakerStatus {
 constructor() { this.state=null;this.output=[];this.sources=new Map(); }
 apply(message) {
  if(Object.hasOwn(message,'snapshot'))this.state=message.snapshot;
  else {
   if(!this.state)throw new Error('Status patch before snapshot');
   for(const change of message.patch) {
    const path=change[0].slice(1).split('/').map(key=>key.replace(/~1/g,'/').replace(/~0/g,'~'));
    let parent=this.state;
    for(const key of path.slice(0,-1)) {
     if(!Object.hasOwn(parent,key))throw new Error('Invalid status path');
     parent=parent[key];
    }
    const key=path.at(-1);
    if(change.length===1)delete parent[key];
    else Object.defineProperty(parent,key,{value:change[1],enumerable:true,writable:true,configurable:true});
   }
  }
  const state=this.state, control=state.sourceControl;
  if(!control) { this.output=[];this.sources.clear();return state; }
  this.output=control.outputDelta?this.output.concat(control.output||[]):control.output||[];
  this.output=this.output.filter(point=>Date.parse(point.at)>=Date.parse(state.now)-30000).slice(-150);
  const sources=(control.sources||[]).map(source=>{
   let windows=source.windowsDelta?(this.sources.get(source.id)||[]).concat(source.windows||[]):source.windows||[];
   windows=windows.filter(point=>Date.parse(point.at)>=Date.parse(state.now)-30000).slice(-150);
   this.sources.set(source.id,windows);
   return {...source,windows,windowsDelta:false};
  });
  for(const id of this.sources.keys())if(!sources.some(source=>source.id===id))this.sources.delete(id);
  return {...state,sourceControl:{...control,sources,output:this.output,outputDelta:false}};
 }
}
