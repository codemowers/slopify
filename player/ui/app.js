const $ = (id) => document.getElementById(id);
function toast(message) { $("toast").textContent = message; }
const lanAudioTemplates = new Map(["lan-audio-connect", "lan-audio-direct", "lan-audio-persist", "lan-audio-check"]
  .map((id) => [id, $(id).textContent]));
// Display the endpoint used to reach this speaker; never interpolate shell syntax.
const speakerHost = location.hostname;
const shellHost = (/^[a-zA-Z0-9][a-zA-Z0-9.-]{0,252}$/.test(speakerHost) || /^\[[0-9a-fA-F:]+\]$/.test(speakerHost)) ? speakerHost : "SPEAKER_IP";
$("lan-audio-host").textContent = speakerHost;
// Named as Zeroconf discovery names it, so that either route gives the same sink.
function renderSenderCommands(state) {
 const host = /^[a-zA-Z0-9][a-zA-Z0-9-]{0,62}$/.test(state?.hostname || "") ? state.hostname : "speaker";
 const device = /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$/.test(state?.pulseSink || "") ? state.pulseSink : "slopify.output";
 const description = /^[a-zA-Z0-9][a-zA-Z0-9 _.+-]{0,62}$/.test(state?.name || "") ? state.name : host;
 for (const [id, template] of lanAudioTemplates) {
  const text = template.replaceAll("SPEAKER_IP", shellHost).replaceAll("SPEAKER_DEVICE", device)
   .replaceAll("SPEAKER_SINK", `tunnel.${host}.local.${device}`).replaceAll("SPEAKER_NAME", description);
  if ($(id).textContent !== text) $(id).textContent = text;
 }
}
renderSenderCommands();
function esoundUsable(port) {
 return Number.isInteger(port)&&/^[a-zA-Z0-9.-]+$/.test(location.hostname);
}
function renderEsoundHelp(port) {
 const host=location.hostname;
 if(!esoundUsable(port))return;
 const address=`${host}:${port}`;
 const command=`reg add "HKLM\\SOFTWARE\\Enlightened\\WinESD" /v ESPEAKER /t REG_SZ /d "${address}" /f`;
 const file=`REGEDIT4\n\n[HKEY_LOCAL_MACHINE\\SOFTWARE\\Enlightened\\WinESD]\n"ESPEAKER"="${address}"`;
 if($("esound-reg-add").textContent!==command)$("esound-reg-add").textContent=command;
 if($("esound-reg-file").textContent!==file)$("esound-reg-file").textContent=file;
}
const senderTabs=Array.from(document.querySelectorAll('.sender-tabs [role=tab]'));
function selectSenderTab(tab) {
 $("ubuntu-tools").hidden=tab.id==="tab-esound";
 for(const item of senderTabs) {
  const selected=item===tab;
  item.setAttribute("aria-selected",String(selected));item.tabIndex=selected?0:-1;
  $(item.getAttribute("aria-controls")).hidden=!selected;
 }
}
for(const tab of senderTabs) {
 tab.addEventListener("click",()=>selectSenderTab(tab));
 tab.addEventListener("keydown",event=>{
  const visible=senderTabs.filter(t=>!t.hidden),index=visible.indexOf(tab);
  const next=event.key==="ArrowRight"?(index+1)%visible.length:event.key==="ArrowLeft"?(index+visible.length-1)%visible.length:event.key==="Home"?0:event.key==="End"?visible.length-1:null;
  if(next===null)return;
  event.preventDefault();selectSenderTab(visible[next]);visible[next].focus();
 });
}
document.querySelectorAll("[data-copy-lan]").forEach((button) => button.addEventListener("click", async () => {
  const code = $(button.dataset.copyLan);
  const text = code.textContent;
  try {
    if (navigator.clipboard && window.isSecureContext) await navigator.clipboard.writeText(text);
    else {
      const input=document.createElement("textarea");input.value=text;input.readOnly=true;
      input.style.cssText="position:fixed;top:0;left:0;opacity:0";
      document.body.append(input);input.select();input.setSelectionRange(0,text.length);
      const copied=document.execCommand("copy");input.remove();
      if(!copied)throw new Error("Clipboard permission denied");
    }
    toast("Commands copied.");button.textContent="Copied";setTimeout(()=>button.textContent="Copy commands",1800);
  } catch (_) { toast("Copy failed. Select the commands and copy them manually."); }

}));



function pcmLabel(value) {
 // Drop the separator in endian suffixes. Keep container widths.
 return String(value || "").toUpperCase().replace(/_(LE|BE)$/, "$1");
}
function pcmDescription(value) {
 const name=pcmLabel(value), match=/^([SUF])(\d+)(?:_(\d+))?(LE|BE|P)?$/.exec(name);
 if(!match)return name;
 const [,kind,bits,container,layout]=match;
 return `${bits}-bit ${kind==="F"?"floating-point":kind==="U"?"unsigned integer":"signed integer"} PCM${container?`, ${container}-bit container`:""}${layout==="LE"?", little-endian":layout==="BE"?", big-endian":layout==="P"?", planar":""}`;
}
function renderPCM(element, format, prefix="", suffix="") {
 const parts=[];
 if(prefix)parts.push(document.createTextNode(prefix));
 if(!format)parts.push(document.createTextNode("PCM format unknown"));
 else {
  if(format.codec)parts.push(document.createTextNode(`${format.codec} · `));
  if(format.channels)parts.push(document.createTextNode(`${format.channels}ch · `));
  const lossy=format.codec&&!format.format;
  parts.push(document.createTextNode(format.rate?`${format.rate/1000}kHz${lossy?"":" · "}`:"rate unknown · "));
  if(!lossy) {
   const label=document.createElement("span");
   label.textContent=pcmLabel(format.format)||"format unknown";
   if(format.format)label.title=pcmDescription(format.format);
   parts.push(label);
  }
 }
 if(suffix)parts.push(document.createTextNode(suffix));
 element.replaceChildren(...parts);
}
let events = null;
function closeSpeakerEvents() {
 events?.close();
 events = null;
}
function speakerConnected() { $("connection").textContent = "";$("connection").hidden=true;restoreSourceHistory(); }
function speakerDisconnected() { $("connection").hidden=false;$("connection").textContent = "Speaker connection lost; reconnecting…"; }
function connectSpeakerEvents() {
 if (document.readyState !== "complete" || events) return;
 if (typeof SharedWorker !== "undefined") {
  try {
   const worker = new SharedWorker("/events-worker.js", {name:"slopify-speaker-events"});
   const shared = {close() { worker.port.postMessage({type:"disconnect"});worker.port.close(); }};
   events = shared;
   worker.port.onmessage = event => {
    if (events !== shared) return;
    if (event.data.type === "open") speakerConnected();
    else if (event.data.type === "error") speakerDisconnected();
    else if (event.data.type === "status") renderSpeakerEvent({data:event.data.data});
   };
   worker.onerror = () => {
    if (events !== shared) return;
    closeSpeakerEvents();connectDirectSpeakerEvents();
   };
   worker.port.start();
   return;
  } catch (_) { /* Unsupported worker policy: use a tab-local connection. */ }
 }
 connectDirectSpeakerEvents();
}
function connectDirectSpeakerEvents() {
 if (events) return;
 const stream = new EventSource("/api/events");
 events = stream;
 stream.onopen = speakerConnected;
 stream.onerror = speakerDisconnected;
 stream.onmessage = renderSpeakerEvent;
}
function renderSpeakerEvent(event) {
 const state = JSON.parse(event.data);
 sourceClock = Date.parse(state.now) || Date.now();
 const pulseEnabled=state.pulseEnabled!==false,airplayEnabled=!!state.airplayEnabled;
 $("tab-airplay").hidden=!airplayEnabled;
 $("tab-zeroconf").hidden=!pulseEnabled;
 $("tab-fixed").hidden=!pulseEnabled;
 $("tab-esound").hidden=!esoundUsable(state.esoundPort);
 const availableTabs=senderTabs.filter(t=>!t.hidden);
 if(senderTabs.some(t=>t.hidden&&t.getAttribute("aria-selected")==="true")) {
  if(availableTabs.length)selectSenderTab(availableTabs[0]);
  else for(const tab of senderTabs)$(tab.getAttribute("aria-controls")).hidden=true;
 }
 $("sender-intro").textContent=pulseEnabled&&airplayEnabled?"Ubuntu 24.04/26.04 can send through Pulse TCP or AirPlay/RAOP. Macs, iPhones and iPads use AirPlay. Keep sender and speaker on the same reachable LAN.":airplayEnabled?"Send audio using AirPlay/RAOP. Keep sender and speaker on the same reachable LAN.":pulseEnabled?"Ubuntu 24.04/26.04 can send through Pulse TCP. Keep sender and speaker on a reachable network.":"LAN audio receivers are disabled in this deployment.";
 slopifyServerHost=state.serverHost || "";
 $("output-device").hidden=!state.outputDevice;
 $("output-device").textContent=state.outputDevice?.path || "";
 $("output-name").textContent=state.outputDevice?.name || "Amplifier";
 renderSources(state.sourceControl && !state.serverHost ? {...state.sourceControl, sources:state.sourceControl.sources.filter(source=>source.id!=="slopify")} : state.sourceControl, state.nodes);

 renderPlaybackProgress(state.playback);
 renderSenderCommands(state);
 renderEsoundHelp(state.esoundPort);
 document.querySelector("h1").textContent = state.name || "Slopify speaker";
 document.title = state.name || "Slopify speaker";
 const v = state.volume;
 const volumeParent = state.sourceControl ? $("source-output") : $("volume-fallback");
 if ($("hardware-volume").parentElement !== volumeParent) volumeParent.append($("hardware-volume"));
 renderVolume(v);
 renderAmp(state.amp);
 $("volume").textContent = v ? `Hardware volume: ${v.muted ? "−∞ dB" : v.db == null ? "dB unavailable" : `${v.db.toFixed(1)} dB`}` : "Hardware volume control disabled";
 $("status-error").textContent = state.error || "";
 $("status-error").hidden = !state.error;
 $("standalone-output").hidden = !!state.sourceControl;
 $("streams").hidden=!!state.sourceControl;
 if (state.sourceControl) return;
 const ordered = [...state.nodes].sort((a,b)=>(a.kind==="Hardware output")-(b.kind==="Hardware output"));
 const rows = ordered.map((node) => {
  const row = document.createElement("div"); row.className = "stream";
  const title = document.createElement("strong");
  title.textContent = `${node.name} · ${node.state}`;
  const format = document.createElement("p"); renderPCM(format,node.format,`${node.kind}: `,node.formatOrigin ? ` · ${node.formatOrigin}` : "");
  row.append(title, format);
  if (node.sourceIP || node.title) { const details=document.createElement("p"); details.textContent=[node.sourceIP ? `Sender: ${node.sourceIP}` : null,node.title ? `Title: ${node.title}` : null].filter(Boolean).join(" · ");row.append(details); }
  if (node.mode) { const p=document.createElement("p");p.className="muted";p.textContent=`Adapter mode: ${node.mode}`;row.append(p); }
  if (node.graphFormat) { const p=document.createElement("p"); p.className="muted";renderPCM(p,node.graphFormat,"Graph processing: ");row.append(p); }
  return row;
 });
 if (!rows.length) { const p=document.createElement("p");p.textContent="Nothing is playing.";rows.push(p); }
 $("streams").replaceChildren(...rows);
 $("streams").hidden=!!state.sourceControl;
};

let currentVolume = null, pendingVolume = null, volumeSending = false, volumeEditing = false;
function dbText(step) { return step?.muted ? "−∞ dB" : step?.db == null ? "dB unavailable" : `${step.db.toFixed(1)} dB`; }
function renderAmp(amp) {
 let box=$("amp-control");
 if(!amp) { if(box)box.hidden=true; return; }
 if(!box) {
  box=document.createElement("div");box.id="amp-control";
  const label=document.createElement("span");label.id="amp-steps";
  const step=(text,name,delta)=>{const b=document.createElement("button");b.type="button";b.textContent=text;b.setAttribute("aria-label",name);
   b.addEventListener("click",()=>fetch("/api/amp",{method:"POST",headers:{"Content-Type":"application/json","X-Slopify-Control":"1"},body:JSON.stringify({delta})}).catch(()=>{}));return b;};
  box.append(step("−","Amplifier volume down",-1),label,step("+","Amplifier volume up",1));
  $("source-output").append(box);
 }
 box.hidden=false;
 $("amp-steps").textContent=amp.phase==="unknown"?" Amp volume: level unknown ":` Amp volume: step ${amp.steps} of ${amp.maxSteps} `;
}

function renderVolume(v) {
 currentVolume = v;
 $("hardware-volume").hidden = !v;
 $("volume-controls").hidden = !v;
 if (!v) return;
 const slider = $("volume-slider");
 slider.min=v.min; slider.max=v.max;
 if (!volumeEditing && !volumeSending) slider.value=v.level;
 slider.setAttribute("aria-valuetext", dbText(v.steps.find(s=>s.level===Number(slider.value))));
 $("mute").hidden=!v.muteAvailable; $("mute").setAttribute("aria-pressed",String(v.switchMuted));
 const mute=$("mute"),muteLabel=v.switchMuted?'Unmute':'Mute';
 mute.title=muteLabel;mute.setAttribute('aria-label',muteLabel);
 if(mute.dataset.muted!==String(v.switchMuted)) {
  mute.dataset.muted=String(v.switchMuted);
  mute.innerHTML=`<svg viewBox="0 0 24 24" width="22" height="22" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M11 5 6 9H3v6h3l5 4Z"/>${v.switchMuted?'<path d="m17 9 5 6m0-6-5 6"/>':'<path d="M15 8a6 6 0 0 1 0 8m3-11a10 10 0 0 1 0 14"/>'}</svg>`;
 }
}
function volumeCommand(command) { return speakerCommand("/api/volume", command); }

async function sendVolume() {
 if (volumeSending) return;
 volumeSending=true;
 try { while(pendingVolume!==null) { const level=pendingVolume;pendingVolume=null;renderVolume(await volumeCommand({level})); } }
 catch(e) {pendingVolume=null;toast(e.message);}
 finally {volumeSending=false;}
}
$("volume-slider").addEventListener("input", e=>{
 volumeEditing=true;pendingVolume=Number(e.target.value);
 const text=dbText(currentVolume?.steps.find(s=>s.level===pendingVolume));
 e.target.setAttribute("aria-valuetext",text);$("volume").textContent=`Hardware volume: ${text}`;
 sendVolume();
});
$("volume-slider").addEventListener("change",()=>{volumeEditing=false;});
$("mute").addEventListener("click",async()=>{
 $("mute").disabled=true;
 try {renderVolume(await volumeCommand({muted:!currentVolume.switchMuted}));}
 catch(e){toast(e.message);}finally{$("mute").disabled=false;}
});

// Start after document load so the long-lived request cannot hold the browser's
// initial loading indicator open. Keep SSE alive in background tabs and while
// commands run; only leaving the page closes this tab's connection.
window.addEventListener("load", connectSpeakerEvents, {once:true});
window.addEventListener("pagehide", closeSpeakerEvents);
window.addEventListener("pageshow", connectSpeakerEvents);
connectSpeakerEvents();
