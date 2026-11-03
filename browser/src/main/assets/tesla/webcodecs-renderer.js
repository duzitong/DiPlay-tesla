const MAGIC = 0x31435657, HEADER = 32, MAX_FRAMES = 6, MAX_BYTES = 2 * 1024 * 1024;

// Annex B access units, no AVC description. Ownership ends only after paint (or teardown).
export function createWebCodecsRenderer({canvas,onFrame=()=>{},onStats=()=>{},onDiagnostic=()=>{},
  now=()=>performance.now(),Decoder=globalThis.VideoDecoder,Chunk=globalThis.EncodedVideoChunk}) {
  let active=false,epoch=0,socket=null,decoder=null,retry=null,session=null,token=null;
  let generation=null,highestGeneration=0,ready=false,needsKey=true,sequence=0,consumed=0,attempts=0;
  let pending=new Map(),bytes=0,decoded=0,painted=0,sampledAt=now(),sampledDecoded=0,sampledPainted=0;
  let displayed=false,lastProgress=null,deadline=null,errorReason=null;
  function stats() {
    const at=now(),elapsed=at-sampledAt;
    onStats({decodedFps:elapsed>0?(decoded-sampledDecoded)*1000/elapsed:null,
      presentedFps:elapsed>0?(painted-sampledPainted)*1000/elapsed:null,
      queueFrames:pending.size,queueBytes:bytes,decodedFrames:decoded,paintedFrames:painted,
      recovering:active && !ready,webcodecsError:errorReason});
    sampledAt=at;sampledDecoded=decoded;sampledPainted=painted;
  }
  function send(data) {
    if (!socket || socket.readyState!==WebSocket.OPEN) return false;
    try { socket.send(JSON.stringify(data));return true; }
    catch (error) { fail('video-send-failed',true);return false; }
  }
  function release() {
    epoch++;
    const ws=socket;socket=null;
    if(ws){ws.onopen=ws.onmessage=ws.onerror=ws.onclose=null;ws.close();}
    const old=decoder;decoder=null;
    if(old && old.state!=='closed')old.close();
    pending.clear();bytes=0;ready=false;generation=null;needsKey=true;
    displayed=false;deadline=lastProgress=null;
  }
  function fail(reason,retryable=false) {
    if(!active)return;
    onDiagnostic('webcodecs-failed',{reason:reason.split(':')[0]});
    release();sustainedAt=now();clearTimeout(retry);retry=null;errorReason=reason;
    if(retryable && attempts<3) {
      const delay=500*2**attempts++;
      const ticket=epoch;
      retry=setTimeout(()=>{retry=null;if(active && ticket===epoch)connect();},delay);
    } else { active=false;errorReason=retryable?'video-recovery-exhausted: '+reason:reason; }
    stats();
  }
  function failed(data) {
    if(!socket || !Number.isSafeInteger(data.generation))return;
    if(data.generation===generation || (generation===null && data.generation>highestGeneration)) {
      highestGeneration=Math.max(highestGeneration,data.generation);
      fail(String(data.reason||'video-failed'),data.retryable===true);
    }
  }
  async function configure(data,ws,ticket) {
    if(!Number.isSafeInteger(data.generation) || data.generation<1 ||
      typeof data.codec!=='string' || !/^avc1\.[0-9a-f]{6}$/i.test(data.codec)) {
      fail('invalid-video-config');return;
    }
    const old=decoder;decoder=null;
    if(old && old.state!=='closed')old.close();
    pending.clear();bytes=0;ready=false;needsKey=true;sequence=consumed=0;
    generation=data.generation;
    highestGeneration=Math.max(highestGeneration,generation);
    deadline=now();
    const configGeneration=generation,config={codec:data.codec,optimizeForLatency:true};
    const current=()=>active && socket===ws && epoch===ticket && generation===configGeneration;
    try {
      const support=await Decoder.isConfigSupported(config);
      if(!current())return;
      if(!support.supported){fail('unsupported-h264-config');return;}
      const drawing=canvas.getContext('2d',{alpha:false});
      if(!drawing)throw new Error('Canvas 2D unavailable');
      const instance=new Decoder({
        output(frame) {
          try {
            if(!current() || decoder!==instance)return;
            const item=pending.get(frame.timestamp);
            if(!item || item.sequence<=consumed){fail('invalid-video-output');return;}
            decoded++;
            if(canvas.width!==frame.displayWidth)canvas.width=frame.displayWidth;
            if(canvas.height!==frame.displayHeight)canvas.height=frame.displayHeight;
            drawing.drawImage(frame,0,0);
            canvas.hidden=false;painted++;displayed=true;lastProgress=now();
            consumed=item.sequence;
            for(const [timestamp,value] of pending)if(value.sequence<=consumed){bytes-=value.bytes;pending.delete(timestamp);}
            send({type:'video-ack',generation:configGeneration,sequence:consumed});
            onFrame(frame.displayWidth,frame.displayHeight);
          } catch (error) { if(current())fail('video-paint-failed: '+error.message); }
          finally { frame.close(); }
        },
        error(error) { if(current() && decoder===instance)fail('video-decode-failed: '+error.message,true); },
      });
      decoder=instance;instance.configure(config);ready=true;deadline=null;errorReason=null;
      send({type:'video-ready',generation});
    } catch(error) { if(current())fail('video-config-failed: '+error.message); }
  }
  function receive(buffer) {
    if(!ready || !decoder){fail('video-before-config');return;}
    if(buffer.byteLength<=HEADER || buffer.byteLength>HEADER+MAX_BYTES){fail('invalid-video-packet');return;}
    const header=new DataView(buffer),flags=header.getUint32(4,true);
    const gen=header.getFloat64(8,true),seq=header.getFloat64(16,true),timestamp=header.getFloat64(24,true);
    if(gen!==generation)return;
    if(header.getUint32(0,true)!==MAGIC || flags>1 || !Number.isSafeInteger(seq) ||
      seq!==sequence+1 || !Number.isSafeInteger(timestamp) || timestamp<0 || pending.has(timestamp)) {
      fail('invalid-video-packet');return;
    }
    if(needsKey && flags!==1){fail('video-keyframe-required',true);return;}
    const size=buffer.byteLength-HEADER;
    if(pending.size>=MAX_FRAMES || bytes+size>MAX_BYTES || decoder.decodeQueueSize>=MAX_FRAMES) {
      fail('video-decode-backlog',true);return;
    }
    sequence=seq;needsKey=false;
    pending.set(timestamp,{sequence:seq,bytes:size,at:now()});bytes+=size;
    try { decoder.decode(new Chunk({type:flags===1?'key':'delta',timestamp,data:new Uint8Array(buffer,HEADER)})); }
    catch(error) { fail('video-decode-failed: '+error.message,true); }
  }
  function connect() {
    if(!active || document.hidden)return;
    const ticket=++epoch;
    let ws;
    try { ws=new WebSocket(`${location.protocol==='https:'?'wss':'ws'}://${location.host}/api/video`); }
    catch(error) { fail('video-connect-failed: '+error.message,true);return; }
    socket=ws;ws.binaryType='arraybuffer';deadline=now();
    const current=()=>active && socket===ws && ticket===epoch;
    ws.onopen=()=>{if(current())send({type:'hello',token,session});};
    ws.onmessage=event=>{
      if(!current())return;
      if(event.data instanceof ArrayBuffer){receive(event.data);return;}
      let data;
      try { data=JSON.parse(event.data); } catch(error) { fail('invalid-video-message');return; }
      if(data?.type==='video-config')configure(data,ws,ticket);
      else if(data?.type==='video-reset') {
        const old=decoder;decoder=null;generation=null;ready=false;displayed=false;deadline=null;
        if(old && old.state!=='closed')old.close();
        pending.clear();bytes=0;needsKey=true;sequence=consumed=0;
        onDiagnostic('webcodecs-reset',{reason:'source-reset'});
      }
      else if(data?.type==='video-welcome' && generation===null)deadline=null;
      else if(data?.type==='video-progress' && data.generation===generation) {
        const progress={};
        for(const key of ['sent','acknowledged','inFlightFrames','inFlightBytes','expiredFrames',
          'overflowFrames','keyframeSkips','queueBudgetMs']) {
          if(Number.isSafeInteger(data[key]) && data[key]>=0)progress[key]=data[key];
        }
        onStats(progress);
      }
      else if(data?.type==='video-error')failed(data);
    };
    ws.onclose=()=>{if(current())fail('video-link-closed',true);};
    ws.onerror=()=>{if(current())fail('video-link-error',true);};
  }
  const timer=setInterval(()=>{
    if(!active)return;
    if(document.hidden){close();return;}
    const at=now();
    if(deadline!==null && at-deadline>=5000){fail('video-config-timeout',true);return;}
    const oldest=pending.values().next().value;
    if(oldest && at-oldest.at>=3000){fail('video-decode-timeout',true);return;}
    // A genuinely healthy period, not a single keyframe, restores the retry allowance.
    if(lastProgress!==null && at-lastProgress<2000 && at-sustainedAt>=60000)attempts=0;
    if(!oldest && lastProgress!==null && at-lastProgress>=2000)sustainedAt=at;
    send({type:'ping'});stats();
  },1000);
  let sustainedAt=now();
  function close() { active=false;clearTimeout(retry);retry=null;release(); }
  return {
    start(credential,lease) {
      close();token=credential;session=lease;attempts=0;errorReason=null;highestGeneration=0;
      decoded=painted=sampledDecoded=sampledPainted=0;sampledAt=sustainedAt=now();active=true;
      if(!globalThis.isSecureContext){fail('secure-https-required');return;}
      if(typeof Decoder!=='function' || typeof Chunk!=='function'){fail('webcodecs-unavailable');return;}
      connect();
    },
    close,
    failed,
    dispose(){close();clearInterval(timer);},
    get displayed(){return displayed;},
  };
}
