function sourceColour(id) {
 if (id==='mixed' || id==='transition') return '#888';
 const slot=id==='slopify'?0:/^lan_[1-7]$/.test(id)?Number(id.slice(4)):null;
 if(slot===null)return '#888';
 return `hsl(${(145+slot*137.508)%360} 64% 55%)`;
}
let sourceClock=Date.now();
let sourcePlayback=null;
let multicastSender=null;
let queueLink=null;
const multicastReceivers=new Map();
let slopifyServerHost="", slopifyServerOrigin="";
let sourceNodes=[];
let sourceRequestError="";
let sourceControl=null, sourceBusy=false, draggedSource=null, suppressSourceClickUntil=0;
const sourceCards=new Map(), envelopes=new Map(), outputEnvelope=[];
function drawEnvelope(canvas, points) {
 const ctx=canvas.getContext('2d'),w=Math.max(80,canvas.getBoundingClientRect().width),h=canvas.id==='output-envelope'?170:85,dpr=window.devicePixelRatio||1;
 canvas.style.height=`${h}px`;
 if(canvas.width!==Math.round(w*dpr) || canvas.height!==Math.round(h*dpr)){canvas.width=Math.round(w*dpr);canvas.height=Math.round(h*dpr);}
 ctx.setTransform(dpr,0,0,dpr,0,0);
 ctx.clearRect(0,0,w,h);
 ctx.font='11px system-ui';ctx.fillStyle='#888';
 for(const db of canvas.id==='output-envelope'?[0,-12,-24,-36,-48]:[0,-24,-48]) {const y=8+(-db/48)*(h-20);ctx.fillText(`${db} dBFS`,3,y+4);ctx.strokeStyle='#8883';ctx.beginPath();ctx.moveTo(60,y);ctx.lineTo(w,y);ctx.stroke();}
 const now=sourceClock,span=30000;
 // Adjoining observations fill solidly; a missing one stays visible as a gap.
 const slot=(w-60)/150,maxGap=500;
 let previous=null;
 for(const p of points) {
  if(p.id==='unavailable'){previous=null;continue;}
  const age=now-p.at;if(age<0 || age>span){previous=null;continue;}
  const x=60+(1-age/span)*(w-60),db=Math.max(-48,Math.min(0,p.db));
  if(p.id==='mixed' || p.id==='transition') {ctx.fillStyle='#8885';ctx.fillRect(x-Math.max(2,slot)/2,8,Math.max(2,slot),h-20);previous=null;continue;}
  const adjoining=previous && p.at-previous.at<=maxGap && previous.id===p.id;
  // Whole pixels: fractional edges would show hairlines between neighbours.
  const left=Math.floor(adjoining?previous.x:x-Math.max(2,slot)),right=Math.ceil(x),top=8+(-db/48)*(h-20);
  ctx.fillStyle=sourceColour(p.id);ctx.fillRect(left,top,Math.max(1,right-left),h-12-top);
  previous={at:p.at,id:p.id,x};
 }
}
async function sourceCommand(command) {
 if(sourceBusy || !sourceControl || sourceControl.fixed)return;
 sourceRequestError="";sourceBusy=true;renderRoutingProgress();
 try {
  await speakerCommand('/api/sources',{...command,revision:sourceControl.revision});
 } catch(e) {sourceRequestError=e.message;document.getElementById('source-error').textContent=e.message;document.getElementById('toast').textContent=e.message;}
 finally {sourceBusy=false;renderRoutingProgress();}
}
document.getElementById('switch-now').addEventListener('click',()=>sourceCommand({switchNow:true}));
document.getElementById('source-mode').addEventListener('change',e=>sourceCommand({autoMode:e.target.value}));
const soundDuration=document.getElementById('sound-duration'),soundDurationValue=document.getElementById('sound-duration-value');
let soundDurationEditing=false;
function showSoundDuration(value) {soundDurationValue.textContent=Number(value)===0?'Immediate':sourceControl?.autoMode==='priority'?`${Number(value)/1000} s`:`${value} ms`;soundDuration.setAttribute('aria-valuetext',soundDurationValue.textContent);}
soundDuration.addEventListener('input',()=>{soundDurationEditing=true;showSoundDuration(soundDuration.value);});
soundDuration.addEventListener('change',()=>{soundDurationEditing=false;sourceCommand(sourceControl?.autoMode==='priority'?{prioritySilenceMs:Number(soundDuration.value)}:{minimumSoundMs:Number(soundDuration.value)});});
const priorityTakeover=document.getElementById('priority-takeover-delay');
let priorityTakeoverEditing=false;
function showPriorityTakeover(value){const text=Number(value)===0?'Immediate':`${Number(value)/1000} s`;document.getElementById('priority-takeover-value').textContent=text;priorityTakeover.setAttribute('aria-valuetext',text);}
priorityTakeover.addEventListener('input',()=>{priorityTakeoverEditing=true;showPriorityTakeover(priorityTakeover.value);});
priorityTakeover.addEventListener('change',()=>{priorityTakeoverEditing=false;sourceCommand({priorityTakeoverMs:Number(priorityTakeover.value)});});
document.getElementById('transition-sounds').addEventListener('change',e=>sourceCommand({transitionSounds:e.target.checked}));
const sourceProtocols={pulse:'PULSEAUDIO/PIPEWIRE',esound:'ENLIGHTENED SOUND',upnp:'UPNP',rtp:'RTP MULTICAST',airplay:'AIRPLAY'};
function renderSources(control, nodes) {
 if(nodes)sourceNodes=nodes;
 document.getElementById('routing-settings').hidden=!control || !!control.fixed;
 const section=document.getElementById('sources');section.hidden=!control;section.classList.toggle('fixed',!!control?.fixed);
 if(!control)return;
 sourceControl=control;
 const priority=control.autoMode==='priority';
 const pinned=control.mode==='off', automaticDisabled=pinned;
 const modeSelect=document.getElementById('source-mode');
 modeSelect.disabled=automaticDisabled;
 modeSelect.querySelector('option[value=off]').hidden=!pinned;
 document.getElementById('sound-guard').classList.toggle('disabled',automaticDisabled);
 document.getElementById('priority-takeover').hidden=!priority || control.fixed;
 priorityTakeover.disabled=automaticDisabled;
 if(!priorityTakeoverEditing){priorityTakeover.value=control.priorityTakeoverMs??0;showPriorityTakeover(priorityTakeover.value);}
 document.getElementById('transition-sounds-label').hidden=!!control.fixed;
 document.getElementById('transition-sounds').checked=control.transitionSounds!==false;
 soundDuration.disabled=automaticDisabled;soundDuration.max=priority?'120000':'2000';soundDuration.step=priority?'1000':'100';
 document.getElementById('sound-duration-label').textContent=priority?'Silence before fallback':'Minimum sound duration';
 document.getElementById('sound-duration-help').textContent=priority?'Keep the selected source through gaps for this long before falling back to a lower-priority source. The separate takeover delay applies to higher-priority sources.':'Require sustained sound before switching. Short notifications won’t take over; notifications mixed into the selected source still play.';
 if(!soundDurationEditing) {soundDuration.value=priority?(control.prioritySilenceMs??30000):(control.minimumSoundMs??500);showSoundDuration(soundDuration.value);}
 if(!sourceBusy || pinned)modeSelect.value=pinned?'off':control.autoMode||'recent';
 document.getElementById('source-error').textContent=sourceRequestError||control.error||'';
 const active=control.sources.filter(s=>s.active), now=sourceClock;
 const hardware=sourceNodes.find(n=>n.kind==="Hardware output");
 const outputFormat=document.getElementById("output-format");
 if(hardware)renderPCM(outputFormat,hardware.format,"",` · ${hardware.state}`);
 else outputFormat.textContent="Output format unavailable";
 for(const [index,s] of control.sources.entries()) {
  let card=sourceCards.get(s.id);
  if(!card) {
   const root=document.createElement('div');root.className='source-card';root.style.setProperty('--source-colour',sourceColour(s.id));
   const reorder=document.createElement('div'),type=document.createElement('span'),name=document.createElement('strong'),badges=document.createElement('span'),details=document.createElement('p'),metadata=document.createElement('div'),choose=document.createElement('button'),up=document.createElement('button'),down=document.createElement('button'),forget=document.createElement('button'),canvas=document.createElement('canvas');
   choose.type=up.type=down.type=forget.type='button';forget.className='source-forget';forget.setAttribute('aria-label','Remove this source');forget.title='Remove this source';forget.innerHTML=`<svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13M10 11v6M14 11v6"/></svg>`;forget.addEventListener('click',e=>{e.stopPropagation();sourceCommand({forget:s.id});});choose.addEventListener('click',()=>sourceCommand(sourceControl?.mode==='off' && sourceControl?.selected===s.id?{unpin:true}:{selected:s.id}));
   up.textContent='↑';down.textContent='↓';up.setAttribute('aria-label','Move source up');down.setAttribute('aria-label','Move source down');
   const move=delta=>{const order=sourceControl.sources.map(p=>p.id),i=order.indexOf(s.id),j=i+delta;if(j<0||j>=order.length)return;[order[i],order[j]]=[order[j],order[i]];sourceCommand({order});};
   up.addEventListener('click',()=>move(-1));down.addEventListener('click',()=>move(1));
   canvas.width=700;canvas.height=100;canvas.setAttribute('aria-label','Recent source peak envelope');metadata.className='source-metadata';badges.className='source-badges';choose.className='source-lock';reorder.className='source-reorder';reorder.append(up,down);reorder.addEventListener('click',e=>e.stopPropagation());type.className='eyebrow source-type';root.append(choose,forget,type,name,badges,reorder,details,metadata,canvas);
   const ip=document.createElement('p'),state=document.createElement('span'),spinner=document.createElement('span');
   ip.className='source-ip';state.setAttribute('role','status');spinner.className='routing-spinner source-spinner';spinner.hidden=true;spinner.setAttribute('aria-hidden','true');
   root.insertBefore(ip,metadata);root.append(spinner);
   root.tabIndex=0;root.setAttribute('role','group');
   root.addEventListener('click',e=>{if(!e.target.closest('button,a') && !draggedSource && performance.now()>suppressSourceClickUntil)sourceCommand({selected:s.id});});
   root.addEventListener('keydown',e=>{if(e.target===root && (e.key==='Enter' || e.key===' ')){e.preventDefault();sourceCommand({selected:s.id});}});
   root.addEventListener('dragstart',e=>{
    if(sourceControl?.fixed || e.target.closest('a')){e.preventDefault();return;}
    draggedSource=s.id;e.dataTransfer.effectAllowed='move';e.dataTransfer.setData('text/plain',s.id);root.classList.add('dragging');
   });
   root.addEventListener('dragover',e=>{if(draggedSource && !sourceControl?.fixed){e.preventDefault();e.dataTransfer.dropEffect='move';}});
   root.addEventListener('drop',e=>{
    if(!draggedSource || sourceControl?.fixed)return;
    e.preventDefault();e.stopPropagation();
    const from=draggedSource;draggedSource=null;suppressSourceClickUntil=performance.now()+300;
    if(from===s.id)return;
    const order=sourceControl.sources.map(p=>p.id).filter(id=>id!==from);
    const box=root.getBoundingClientRect(),after=e.clientY>box.top+box.height/2;
    order.splice(order.indexOf(s.id)+(after?1:0),0,from);sourceCommand({order});
   });
   root.addEventListener('dragend',()=>{draggedSource=null;suppressSourceClickUntil=performance.now()+300;root.classList.remove('dragging');});
   card={root,reorder,forget,type,name,badges,details,metadata,choose,up,down,canvas,ip,state,spinner};sourceCards.set(s.id,card);if(!envelopes.has(s.id))envelopes.set(s.id,[]);
  }
  const displayName=s.id==="slopify" && slopifyServerHost?slopifyServerHost:s.name;
  card.forget.hidden=s.id==='slopify'||!!s.connected;
  card.type.textContent=s.id==='slopify'?'SLOPIFY':sourceProtocols[s.protocol]||'';
  card.type.hidden=!card.type.textContent;
  if(s.id==='slopify' && /^https?:\/\//.test(slopifyServerOrigin)) {
   let link=card.name.querySelector('a');
   if(!link){link=document.createElement('a');link.target='_blank';link.rel='noopener noreferrer';link.draggable=false;link.addEventListener('click',e=>e.stopPropagation());card.name.replaceChildren(link);}
   link.href=slopifyServerOrigin;link.textContent=displayName;
  } else card.name.textContent=displayName;
  const badges=[];
  if(control.selected===s.id && control.mode!=='off')badges.push(['➜','Selected automatically']);
  if(s.active)badges.push(['🔊','Linked to amplifier']);
  if(s.playing && !s.detached)badges.push(['▶','Slopify queue playing']);
  else if(s.paused && !s.detached)badges.push(['⏸','Slopify queue paused']);
  const badgeKey=JSON.stringify(badges);
  if(card.badgeKey!==badgeKey){card.badgeKey=badgeKey;card.badges.replaceChildren(...badges.map(([icon,label])=>{const badge=document.createElement('span');badge.textContent=icon;badge.title=label;badge.setAttribute('role','img');badge.setAttribute('aria-label',label);return badge;}));}
  card.root.classList.toggle('disconnected',!s.connected);card.root.classList.toggle('selected',control.selected===s.id);card.root.draggable=!control.fixed;
  card.root.setAttribute('aria-label',`${displayName}; click or press Enter to pin${!control.fixed?'; drag to reorder':''}`);
  card.root.hidden=!!s.senderUnavailable && control.selected!==s.id;
  card.source=s;
  if(!card.expand) {
   card.expand=document.createElement('button');card.expand.className='source-expand';card.expand.type='button';
   card.expand.addEventListener('click',e=>{e.stopPropagation();card.expanded=!card.expanded;renderSources(sourceControl);});card.root.append(card.expand);
   card.follow=document.createElement('button');card.follow.className='source-follow';card.follow.type='button';card.follow.textContent='Follow queue automatically';
   card.follow.addEventListener('click',e=>{e.stopPropagation();sourceCommand({selected:'slopify'});});card.root.append(card.follow);
   card.remote=document.createElement('div');card.remote.className='source-remote';card.remote.setAttribute('role','group');card.remote.setAttribute('aria-label','Sender playback');
   for(const [op,label,icon] of [['prev','Previous track','⏮'],['toggle','Play or pause','⏯'],['next','Next track','⏭']]) {
    const button=document.createElement('button');button.type='button';button.textContent=icon;button.title=label;button.setAttribute('aria-label',label);
    button.addEventListener('click',async e=>{e.stopPropagation();button.disabled=true;try{await speakerCommand('/api/playback',{op});}catch(error){document.getElementById('toast').textContent=error.message;}finally{button.disabled=false;}});
    card.remote.append(button);
   }
   card.root.append(card.remote);
  }
  const compact=!s.connected && control.selected!==s.id;
  card.root.classList.toggle('source-collapsed',compact && !card.expanded);
  card.expand.hidden=!compact;card.expand.textContent=card.expanded?'Less detail':'Details';card.expand.setAttribute('aria-expanded',String(!!card.expanded));
  card.follow.hidden=!(queueLink && s.protocol==='rtp' && control.mode==='off' && control.selected===s.id && slopifyServerHost);
  card.remote.hidden=!(s.remote && s.connected && control.selected===s.id); // Commands reach the selected sender only.

  card.root.classList.toggle('queue-upstream',queueLink?.sourceId===s.id && control.selected==='slopify');
  const host=s.id!=='slopify' && s.host!==s.name?s.host:"";
  if(s.format)renderPCM(card.details,s.format);
  else card.details.textContent="";
  card.details.append(card.state);
  if(s.input?.frames>0 && s.input?.rate>0 && !(s.id==='slopify' && queueLink?.sourceId)) {
   const bps=s.input.bytes*8*s.input.rate/s.input.frames;
   const rate=document.createElement('span');
   rate.textContent=` · ${Math.round(bps/1000)} kb/s input`;
   rate.title='Average encoded bytes consumed per decoded audio second; not PCM bitrate or download speed.';
   card.details.append(rate);
  }
  if(Number.isFinite(s.buffer?.syncOffsetMs)) {
   const offset=document.createElement('span');
   offset.textContent=` · ${s.buffer.syncOffsetMs} ms A/V offset`;
   offset.title='Estimated receiver buffering including the output device; excludes sender and network delay. Not a measured lip-sync error.';
   card.details.append(offset);
  }
  card.ip.textContent=s.id==='slopify'?'':[host,s.ip?`IP: ${s.ip}`:null].filter(Boolean).join(" · ");card.ip.hidden=!card.ip.textContent;
  renderSourceBuffer(card,s.buffer);
  const hiddenMetadata=new Set(['bitrate','bits','channels','codec','duration','kind','sampleRate','size','videoCodec']);
  const metadata=Object.fromEntries(Object.entries(s.metadata || (s.title?{title:s.title}:{})).filter(([key])=>!hiddenMetadata.has(key))),metadataKey=JSON.stringify(metadata);
  if(card.metadataKey!==metadataKey) {
   card.metadataKey=metadataKey;
   card.metadata.replaceChildren(...Object.entries(metadata).sort(([a],[b])=>a.localeCompare(b)).map(([key,value])=>{
    const line=document.createElement('div'),label=document.createElement('span');
    label.className='metadata-key';label.textContent=`${key}: `;
    line.append(label,document.createTextNode(String(value)));return line;
   }));
   card.metadata.hidden=!Object.keys(metadata).length;
  }
  const fresh=s.connected && now-Date.parse(s.level.at)<2000;
  const pinned=control.mode==='off' && control.selected===s.id;if(card.pinned!==pinned){card.pinned=pinned;card.choose.innerHTML=`<svg viewBox="0 0 28 28" width="28" height="28" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="4" y="12" width="18" height="13" rx="3"/><path d="${pinned?'M8 12V7a5 5 0 0 1 10 0v5':'M8 12V7a5 5 0 0 1 10 0'}"/><path d="M13 18v3"/></svg>`;}card.choose.title=pinned?'Unpin and resume automatic source selection':s.id==='slopify' && queueLink?'Follow queue automatically':s.protocol==='rtp'?`Stay with ${s.name.split(' · ')[0]}`:`Pin ${displayName}`;card.choose.setAttribute('aria-label',card.choose.title);card.choose.setAttribute('aria-pressed',String(pinned));
  card.reorder.hidden=!!control.fixed;card.up.hidden=index===0;card.down.hidden=index===control.sources.length-1;card.root.classList.toggle('has-reorder',!card.reorder.hidden);
  const points=envelopes.get(s.id);
  if(Array.isArray(s.windows)) {
   points.length=0;
   for(const w of s.windows)points.push({id:s.id,db:w.db,at:Date.parse(w.at),sequence:w.sequence});
  } else if(fresh && points.at(-1)?.sequence!==s.level.sequence) {
   const point={id:s.id,db:s.level.db,at:Date.parse(s.level.at),sequence:s.level.sequence};points.push(point);

  }
  if(control.fixed && !Array.isArray(s.windows)) {
   for(const w of control.output||[]) {
    if(w.sourceId===s.id && !points.some(q=>q.sequence===w.sequence))points.push({id:s.id,db:w.db,at:Date.parse(w.at),sequence:w.sequence});
   }
   points.sort((a,b)=>a.at-b.at);
  }
  while(points.length>150 || points.length && points[0].at<now-30000)points.shift();
  const parent=document.getElementById('source-cards');if(!draggedSource && parent.children[index]!==card.root)parent.insertBefore(card.root,parent.children[index]||null);drawEnvelope(card.canvas,points);
 }
 for(const [id,card] of sourceCards) if(!control.sources.some(s=>s.id===id)) { card.root.remove();sourceCards.delete(id);envelopes.delete(id); }
 renderOutputBuffer(control.sources);
 if(!control.outputDelta)outputEnvelope.length=0;
 for(const point of control.output||[])outputEnvelope.push({id:point.sourceId,at:Date.parse(point.at),db:point.db});
 while(outputEnvelope.length>150 || outputEnvelope.length && outputEnvelope[0].at<now-30000)outputEnvelope.shift();
 drawEnvelope(document.getElementById('output-envelope'),outputEnvelope);
 renderRoutingProgress();
 requestAnimationFrame(drawSourceConnections);
}

function renderPlaybackProgress(playback) {
 sourcePlayback=playback;
 const status=document.getElementById('playback-progress');
 const label=playbackLabel();
 status.hidden=!!sourceControl || !label;
 status.querySelector('span:last-child').textContent=label||'';
 renderSourceStatuses();
}
// Queue fill: a stream emptying or a device at zero explains dropouts and repeats.
function bufferRow(label,value,max) {
 const name=document.createElement('span'),meter=document.createElement('meter'),text=document.createElement('span');
 name.textContent=label;meter.min=0;meter.max=max;meter.value=Math.min(value,max);text.textContent=`${value}/${max} ms`;
 return [name,meter,text];
}
function renderSourceBuffer(card,buffer) {
 if(!card.buffer){card.buffer=document.createElement('div');card.buffer.className='source-buffer';card.root.append(card.buffer);}
 card.buffer.hidden=!buffer?.streamMaxMs;
 if(card.buffer.hidden)return;
 const parts=bufferRow('Buffer',buffer.streamMs,buffer.streamMaxMs);
 const counters=document.createElement('span'),underflows=buffer.underflows||0,overflows=buffer.overflows||0;
 counters.className='source-buffer-events'+(underflows||overflows?' source-underflows':'');
 counters.textContent=`${underflows} underflow${underflows===1?'':'s'} · ${overflows} overflow${overflows===1?'':'s'}`;
 parts.push(counters);
 card.buffer.replaceChildren(...parts);
}
// The hardware's queue belongs to the output, whichever source is feeding it.
function renderOutputBuffer(sources) {
 const buffer=sources.find(s=>s.active&&s.buffer?.deviceMaxMs)?.buffer;
 let element=document.getElementById('output-buffer');
 if(!element){element=document.createElement('div');element.id='output-buffer';element.className='source-buffer';document.getElementById('source-output').append(element);}
 element.hidden=!buffer;
 if(buffer)element.replaceChildren(...bufferRow('Buffer',buffer.deviceMs,buffer.deviceMaxMs));
}
// The multicast copy of Slopify's audio, beside the hardware output.
function renderMulticast(sender) {
 multicastSender=sender;
 requestAnimationFrame(drawSourceConnections);
 const box=document.getElementById('source-multicast');
 box.hidden=!sender || (!sender.sending && !sender.error && !sender.reportError);
 if(!sender)return;
 const set=(id,text)=>{document.getElementById(id).textContent=text;};
 const receivers=sender.receivers||[];
 set('multicast-name',`${receivers.length} reporting receiver${receivers.length===1?'':'s'}`);
 document.getElementById('multicast-empty').hidden=receivers.length>0;
 const ids=new Set(receivers.map(r=>r.id));
 for(const [id,card] of multicastReceivers)if(!ids.has(id)){card.root.remove();multicastReceivers.delete(id);}
 for(const receiver of receivers) {
  let card=multicastReceivers.get(receiver.id);
  if(!card){
   const root=document.createElement('div'),name=document.createElement('h3'),ip=document.createElement('p'),status=document.createElement('p'),stats=document.createElement('p');
   root.className='multicast-receiver';ip.className=status.className=stats.className='muted';root.append(name,ip,status,stats);card={root,name,ip,status,stats};multicastReceivers.set(receiver.id,card);document.getElementById('multicast-receivers').append(root);
  }
  card.receiver=receiver;card.name.textContent=receiver.name;card.ip.textContent=receiver.ip;
  const age=Math.max(0,Math.floor((sourceClock-Date.parse(receiver.lastSeen))/1000));
  card.status.textContent=`${receiver.receiving?'Receiving packets':'Idle / awaiting packets'} · report ${age}s ago`;
  card.stats.textContent=`Loss ${Number(receiver.lossPercent).toFixed(1)}% · jitter ${Number(receiver.jitterMs).toFixed(1)} ms`;
  card.root.classList.toggle('inactive',!receiver.receiving);
 }

 set('multicast-group',sender.group);
 set('multicast-local-delay',`Additional local playback delay while multicasting: ${sender.localDelayMs||0} ms · best-effort alignment; device latency is additional`);
 set('multicast-format',sender.encoding?`${sender.channels}ch · ${sender.rate/1000}kHz · ${sender.encoding} · ${sender.sending?'sending':'idle'}`:'Waits for Slopify playback');
 set('multicast-counters',`${sender.packets} packets · ${(sender.bytes/1048576).toFixed(1)} MiB · ${sender.discards} discarded · ${sender.errors} errors`);
 const error=document.getElementById('multicast-error');
 const errors=[sender.error,sender.reportError].filter(Boolean);error.hidden=!errors.length;error.textContent=errors.join(' · ');
 const buffer=document.getElementById('multicast-buffer');
 buffer.hidden=!sender.queueMaxMs;
 if(sender.queueMaxMs)buffer.replaceChildren(...bufferRow('Buffer',sender.queueMs,sender.queueMaxMs));
}
function playbackLabel() {
 return {loading:'Loading Slopify audio…',buffering:'buffering',starting:'Opening audio output…'}[sourcePlayback?.state]||'';
}
function renderSourceStatuses() {
 for(const [id,card] of sourceCards) {
  const source=card.source;
  if(!source)continue;
  const selected=sourceControl?.selected===id;
  const pinned=selected && sourceControl?.mode==='off';
  const loading=id==='slopify' && !source.detached && !source.paused?playbackLabel():'';
  const label=loading || (selected && sourceBusy?'Updating source…':'');
  let state=source.active?'':source.connected?source.state||'Connected':'Disconnected';
  if(source.detached)state='Not joined to queue · click to join';
  else if(source.paused)state='Queue paused';
  else if(selected && !source.active && !loading)state=source.connected?'Waiting for audio':'Waiting for source to connect';
  else if(!selected && source.connected && !source.active)state='Available · not selected';
  if(id==='slopify' && queueLink && !source.detached && !source.paused) {
   state=queueLink.role==='follower'?`Following ${queueLink.leader}`:queueLink.listeners>0?`Sending to ${queueLink.listeners} player${queueLink.listeners===1?'':'s'}`:'Direct';
   if(queueLink.reason)state+=` · ${queueLink.reason}`;
  }
  if(id===queueLink?.sourceId && sourceControl?.selected==='slopify')state='Supplying Slopify automatically · pin to select directly';
  if(pinned)state+=(state?' · ':'')+(source.protocol==='rtp'?`staying with ${source.name.split(' · ')[0]}`:'source pinned');
  const status=label || state;
  card.state.textContent=(status && source.format?' · ':'')+status;
  card.spinner.hidden=!label;
  card.root.classList.toggle('source-pending',!!label);
 }
}

function renderRoutingProgress() {
 renderSourceStatuses();
 const status=document.getElementById('routing-progress');
 if(!status || !sourceControl)return;
 const switching=sourceBusy;
 const fallback=sourceControl.mode==='priority'?sourceControl.fallback:null;
 const target=fallback && sourceControl.sources.find(s=>s.id===fallback.sourceId);
 const deadline=fallback?Date.parse(fallback.at):NaN;
 const waiting=!!target && Number.isFinite(deadline);
 const switchNow=document.getElementById('switch-now');
 switchNow.hidden=!waiting || switching;switchNow.disabled=sourceBusy;
 status.hidden=!switching && !waiting;
 document.getElementById('routing-progress-text').textContent=sourceBusy?'Updating source…':waiting?`Switching to ${target.name} in ${Math.max(0,Math.ceil((deadline-sourceClock)/1000))} s…`:'';
 document.getElementById('source-routing').setAttribute('aria-busy',String(switching||waiting));
}

function drawSourceConnections() {
 const container=document.getElementById('source-routing'),svg=document.getElementById('source-connections'),output=document.getElementById('source-output');
 if(!sourceControl || !container.offsetWidth)return;
 const box=container.getBoundingClientRect();
 svg.setAttribute('viewBox',`0 0 ${box.width} ${box.height}`);
 const paths=[];
 function connect(source,destination,active,opacity=1) {
  const card=sourceCards.get(source.id)?.root;
  if(!card || card.hidden || destination.hidden)return;
  const rect=card.getBoundingClientRect(),target=destination.getBoundingClientRect();
  const x1=rect.right-box.left,y1=rect.top+rect.height/2-box.top,x2=target.left-box.left,y2=target.top+target.height/2-box.top;
  const path=document.createElementNS('http://www.w3.org/2000/svg','path');
  const bend=Math.max(24,Math.abs(x2-x1)*.55);
  path.setAttribute('d',`M ${x1} ${y1} C ${x1+bend} ${y1}, ${x2-bend} ${y2}, ${x2} ${y2}`);
  path.setAttribute('fill','none');path.setAttribute('stroke',sourceColour(source.id));path.setAttribute('stroke-width',active?'3':'2');
  path.setAttribute('stroke-opacity',String(opacity));
  if(!active)path.setAttribute('stroke-dasharray','5 5');
  paths.push(path);
 }
 const following=queueLink?.sourceId && sourceControl.selected==='slopify';
 for(const s of sourceControl.sources) {
  if(following && (s.id===queueLink.sourceId || s.id==='slopify'))continue;
  const fallback=sourceControl.mode==='priority' && sourceControl.fallback?.sourceId===s.id;
  const pending=!s.active && (fallback || (sourceControl.selected===s.id && (s.connected || sourceControl.mode==='off')));
  if(s.active || pending)connect(s,output,s.active);
 }
 // RTP currently copies Slopify playback, independently of the hardware selection.
 const slopify=sourceControl.sources.find(s=>s.id==='slopify');
 if(following) {
  const upstream=sourceControl.sources.find(s=>s.id===queueLink.sourceId);
  if(upstream)connect(upstream,output,true,.2);
 }
 if(multicastSender?.sending && slopify)for(const card of multicastReceivers.values())if(card.receiver.receiving)connect(slopify,card.root,true);
 svg.replaceChildren(...paths);
}
new ResizeObserver(()=>requestAnimationFrame(drawSourceConnections)).observe(document.getElementById('source-routing'));
window.addEventListener('scroll',()=>requestAnimationFrame(drawSourceConnections),{passive:true});

async function restoreSourceHistory() {
 try {
  const response=await fetch('/api/history');if(!response.ok)return;
  const history=await response.json();
  for(const [id,samples] of Object.entries(history)) {
   if(!/^(slopify|lan_[1-7])$/.test(id)||!Array.isArray(samples))continue;
   const merged=new Map();
   for(const s of samples.slice(-150)){const at=Date.parse(s.at);if(Number.isFinite(at))merged.set(`${at}:${s.sequence}`,{id,db:s.db,at,sequence:s.sequence});}
   for(const point of envelopes.get(id)||[])merged.set(`${point.at}:${point.sequence}`,point);
   envelopes.set(id,Array.from(merged.values()).sort((a,b)=>a.at-b.at).slice(-150));
  }
  if(sourceControl)renderSources(sourceControl);
 } catch(_) { /* Live SSE continues when history cannot be loaded. */ }
}

// The origin comes from the browser, never a sender's untrusted metadata.
const multicastSDPURL=new URL('/multicast.sdp',location.origin).href;
document.getElementById('multicast-listen-url').textContent=multicastSDPURL;
// Restrict shell snippets to ordinary LAN host/IP URLs.
if(/^https?:\/\/[a-z0-9.:[\]-]+\/multicast\.sdp$/i.test(multicastSDPURL)) {
 document.getElementById('multicast-vlc-windows').textContent=`"%ProgramFiles%\\VideoLAN\\VLC\\vlc.exe" "${multicastSDPURL}"`;
 document.getElementById('multicast-vlc').textContent=`vlc '${multicastSDPURL}'`;
 document.getElementById('multicast-mpv').textContent=`mpv --demuxer-lavf-o='protocol_whitelist=[file,http,https,tcp,tls,udp,rtp]' '${multicastSDPURL}'`;
}
for(const button of document.querySelectorAll('[data-multicast-playlist]'))button.addEventListener('click',()=>{
 const ext=button.dataset.multicastPlaylist;
 const content=ext==='m3u'?`#EXTM3U\r\n#EXTINF:-1,Slopify multicast\r\n${multicastSDPURL}\r\n`:`[playlist]\r\nNumberOfEntries=1\r\nFile1=${multicastSDPURL}\r\nTitle1=Slopify multicast\r\nLength1=-1\r\nVersion=2\r\n`;
 const url=URL.createObjectURL(new Blob([content],{type:ext==='m3u'?'audio/x-mpegurl':'audio/x-scpls'}));
 const a=document.createElement('a');a.href=url;a.download=`slopify-multicast.${ext}`;document.body.append(a);a.click();a.remove();setTimeout(()=>URL.revokeObjectURL(url),1000);
});
