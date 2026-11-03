import test from 'node:test';
import assert from 'node:assert/strict';
import {createWebCodecsRenderer} from '../src/main/assets/tesla/webcodecs-renderer.js';

const flush=async()=>{for(let i=0;i<8;i++)await Promise.resolve();};
function fixture({secure=true,support=true}={}) {
  const originals=new Map(),sockets=[],decoders=[],timers=new Map(),stats=[],draws=[];
  let clock=0,timerId=0;
  const install=(key,value)=>{
    originals.set(key,Object.getOwnPropertyDescriptor(globalThis,key));
    Object.defineProperty(globalThis,key,{configurable:true,writable:true,value});
  };
  class Socket {
    static OPEN=1;
    constructor(url){this.url=url;this.readyState=0;this.sent=[];sockets.push(this);}
    send(raw){this.sent.push(JSON.parse(raw));}
    open(){this.readyState=1;this.onopen?.();}
    message(data){this.onmessage?.({data:data instanceof ArrayBuffer?data:JSON.stringify(data)});}
    close(){this.readyState=3;this.onclose?.();}
  }
  class Decoder {
    static isConfigSupported=async config=>({supported:support,config});
    constructor(callbacks){this.callbacks=callbacks;this.state='unconfigured';this.decodeQueueSize=0;this.chunks=[];decoders.push(this);}
    configure(config){this.config=config;this.state='configured';}
    decode(chunk){this.chunks.push(chunk);this.decodeQueueSize++;}
    paint(timestamp) {
      this.decodeQueueSize--;
      const frame={timestamp,displayWidth:640,displayHeight:360,closed:0,close(){this.closed++;}};
      this.callbacks.output(frame);return frame;
    }
    close(){this.state='closed';}
  }
  install('WebSocket',Socket);install('isSecureContext',secure);
  install('document',{hidden:false});install('location',{protocol:'https:',host:'carplay.test'});
  install('setInterval',fn=>{const id=++timerId;timers.set(id,{fn,interval:true});return id;});
  install('clearInterval',id=>timers.delete(id));
  install('setTimeout',(fn,delay)=>{const id=++timerId;timers.set(id,{fn,at:clock+delay});return id;});
  install('clearTimeout',id=>timers.delete(id));
  const canvas={width:640,height:360,hidden:true,getContext(){return {drawImage(frame){draws.push(frame);}};}};
  const renderer=createWebCodecsRenderer({canvas,Decoder,Chunk:class {constructor(data){Object.assign(this,data);}},
    now:()=>clock,onStats:data=>stats.push(data)});
  renderer.start('paired-token','owner-lease');
  function packet(generation,sequence,key=sequence===1) {
    const bytes=new ArrayBuffer(38),view=new DataView(bytes);
    view.setUint32(0,0x31435657,true);view.setUint32(4,key?1:0,true);
    view.setFloat64(8,generation,true);view.setFloat64(16,sequence,true);view.setFloat64(24,sequence*1000,true);
    new Uint8Array(bytes,32).set([0,0,0,1,key?0x65:0x41,1]);return bytes;
  }
  async function configure(generation=1) {
    const socket=sockets.at(-1);socket.open();socket.message({type:'video-config',generation,codec:'avc1.42001f'});await flush();
    return socket;
  }
  async function advance(ms) {
    clock+=ms;
    for(const [id,timer] of [...timers]) {
      if(timer.interval)timer.fn();
      else if(timer.at<=clock){timers.delete(id);timer.fn();}
    }
    await flush();
  }
  return {renderer,canvas,sockets,decoders,stats,draws,packet,configure,advance,
    dispose(){renderer.dispose();for(const [key,descriptor] of originals){if(descriptor)Object.defineProperty(globalThis,key,descriptor);else delete globalThis[key];}}};
}

test('actual Annex B config is checked and each painted frame is closed before cumulative ACK progress',async()=>{
  const f=fixture();
  try {
    const socket=await f.configure(),decoder=f.decoders[0];
    assert.equal(socket.url,'wss://carplay.test/api/video');
    assert.equal(decoder.config.codec,'avc1.42001f');assert.equal('description' in decoder.config,false);
    socket.message(f.packet(1,1));
    assert.equal(socket.sent.filter(data=>data.type==='video-ack').length,0);
    const frame=decoder.paint(1000);assert.equal(frame.closed,1);assert.equal(f.canvas.hidden,false);
    assert.equal(f.renderer.displayed,true);assert.equal(socket.sent.at(-1).sequence,1);
    await f.advance(1000);assert.equal(f.stats.at(-1).paintedFrames,1);
  } finally {f.dispose();}
});

test('config replacements and disconnects close decoders and ignore stale generations and outputs',async()=>{
  const f=fixture();
  try {
    const socket=await f.configure(),old=f.decoders[0];
    socket.message(f.packet(1,1));
    socket.message({type:'video-config',generation:2,codec:'avc1.42001f'});await flush();
    assert.equal(old.state,'closed');assert.equal(old.paint(1000).closed,1);assert.equal(f.draws.length,0);
    socket.message(f.packet(1,2));assert.equal(f.decoders[1].chunks.length,0);
    socket.message(f.packet(2,1));assert.equal(f.decoders[1].paint(1000).closed,1);
    f.renderer.close();assert.equal(f.decoders[1].state,'closed');
    assert.equal(f.decoders[1].paint(2000).closed,1);assert.equal(f.draws.length,1);
    assert.equal(f.canvas.hidden,false,'recovery retains the last image');
  } finally {f.dispose();}
});

test('decode backlog discards the prediction chain, reconnects within limits and requires a fresh keyframe',async()=>{
  const f=fixture();
  try {
    let socket=await f.configure();
    for(let sequence=1;sequence<=7;sequence++)socket.message(f.packet(1,sequence));
    assert.equal(f.decoders[0].state,'closed');assert.equal(f.decoders[0].chunks.length,6);
    assert.equal(f.stats.at(-1).queueFrames,0);
    await f.advance(500);socket=await f.configure(2);
    socket.message(f.packet(2,1,false));assert.equal(f.decoders[1].chunks.length,0);
    await f.advance(1000);socket=await f.configure(3);socket.close();
    await f.advance(2000);socket=await f.configure(4);socket.close();
    await f.advance(10000);assert.equal(f.sockets.length,4);
    assert.match(f.stats.at(-1).webcodecsError,/recovery-exhausted/);
  } finally {f.dispose();}
});

test('idle input is not a decode timeout, but pending work is; painting errors still close the frame',async()=>{
  const f=fixture();
  try {
    const socket=await f.configure();await f.advance(10000);
    assert.equal(f.sockets.length,1);assert.equal(f.decoders[0].state,'configured');
    socket.message(f.packet(1,1));await f.advance(3000);
    assert.equal(f.decoders[0].state,'closed');
    await f.advance(500);const replacement=await f.configure(2);
    replacement.message(f.packet(2,1));
    Object.defineProperty(f.canvas,'width',{set(){throw new Error('Canvas allocation failed');},get(){return 1;}});
    const frame=f.decoders[1].paint(1000);assert.equal(frame.closed,1);
  } finally {f.dispose();}
});

test('insecure contexts and unsupported actual H264 configs fail explicitly without retry or JPEG',async()=>{
  for(const options of [{secure:false},{support:false}]) {
    const f=fixture(options);
    try {
      if(options.secure===false)assert.equal(f.sockets.length,0);
      else await f.configure();
      await f.advance(10000);
      assert.match(f.stats.at(-1).webcodecsError,/secure-https|required|unsupported-h264/);
      assert.equal(f.decoders.length,0);
    } finally {f.dispose();}
  }
});

test('an authenticated video link with no active CarPlay source does not exhaust recovery retries',async()=>{
  const f=fixture();
  try {
    const socket=f.sockets[0];socket.open();socket.message({type:'video-welcome'});
    await f.advance(60000);
    assert.equal(f.sockets.length,1);assert.equal(f.stats.at(-1).webcodecsError,null);
    socket.message({type:'video-config',generation:1,codec:'avc1.42001f'});await flush();
    socket.message(f.packet(1,1));assert.equal(f.decoders[0].paint(1000).closed,1);
  } finally {f.dispose();}
});

test('source teardown invalidates pending decoder callbacks without resetting the video socket',async()=>{
  const f=fixture();
  try {
    const socket=await f.configure(),old=f.decoders[0];
    socket.message(f.packet(1,1));socket.message({type:'video-reset',generation:2});
    assert.equal(old.state,'closed');assert.equal(old.paint(1000).closed,1);
    assert.equal(f.draws.length,0);assert.equal(socket.readyState,1);
    await f.advance(10000);assert.equal(f.sockets.length,1);
    socket.message({type:'video-config',generation:3,codec:'avc1.42001f'});await flush();
    socket.message(f.packet(3,1));assert.equal(f.decoders[1].paint(1000).closed,1);
  } finally {f.dispose();}
});

test('duplicate video errors on the independent control link cannot consume another retry',async()=>{
  const f=fixture();
  try {
    let socket=await f.configure();
    const failure={type:'video-error',generation:1,reason:'video-reference-lost',retryable:true};
    socket.message(failure);f.renderer.failed(failure);
    await f.advance(500);
    assert.equal(f.sockets.length,2);
    f.renderer.failed(failure);
    socket=await f.configure(2);
    socket.message(f.packet(2,1));assert.equal(f.decoders[1].paint(1000).closed,1);
    socket.close();await f.advance(1000);
    assert.equal(f.sockets.length,3,'Only one retry was used by the first failure');
  } finally {f.dispose();}
});
