import test from 'node:test';
import assert from 'node:assert/strict';
import {NavigationLoudness,normalizeNavigationDb,navigationAmplitude} from '../src/main/assets/tesla/navigation-loudness.js';

function render(input,db,rate=48000,block=128) {
  const dsp=new NavigationLoudness(rate);dsp.gainDb=db;
  const gain=navigationAmplitude(db),length=input[0].length+dsp.delay+block;
  const result=[new Float32Array(length),new Float32Array(length)];
  for(let offset=0;offset<length;offset+=block) {
    const size=Math.min(block,length-offset);
    const source=input.map(channel=>Float32Array.from({length:size},(_,i)=>(channel[offset+i]||0)*gain));
    const output=[new Float32Array(size),new Float32Array(size)];
    dsp.process(source,output);
    for(let c=0;c<2;c++)result[c].set(output[c],offset);
  }
  return {result,delay:dsp.delay};
}
const peak=values=>values.reduce((max,value)=>Math.max(max,Math.abs(value)),0);
const rms=values=>Math.sqrt(values.reduce((sum,value)=>sum+value*value,0)/values.length);

test('dB input is bounded, invalid storage is unity, and the lowest setting mutes',()=>{
  for(const value of [null,undefined,'','bad',Infinity,{},true])assert.equal(normalizeNavigationDb(value),0);
  assert.equal(normalizeNavigationDb(100),24);assert.equal(normalizeNavigationDb(-100),-60);
  assert.equal(navigationAmplitude(-60),0);
});
test('unity preserves navigation waveform apart from five milliseconds of delay',()=>{
  const input=Float32Array.from({length:48000},(_,i)=>.79*Math.sin(i*.07));
  const {result,delay}=render([input,input],0);
  assert.deepEqual(result[0].slice(delay,delay+input.length),input);
});
test('boost raises speech-like average level without sample clipping, including prompt starts and stereo transients',()=>{
  for(const rate of [24000,44100,48000,96000]) {
    const left=Float32Array.from({length:rate*2},(_,i)=>
      (i%Math.round(rate*.4)<Math.round(rate*.26)?1:0)*(.035+.12*(.5+.5*Math.sin(i/rate*17)))*Math.sin(2*Math.PI*400*i/rate));
    left[0]=.99;left[Math.floor(rate*.7)]=-.99;
    const right=Float32Array.from(left,(value,i)=>i===Math.floor(rate*.8)?.999:value*.3);
    const baseline=rms(left);
    for(const db of [6,12,24]) {
      const {result}=render([left,right],db,rate);
      assert.ok(rms(result[0])>baseline*1.25,`${rate}Hz +${db} dB must raise average loudness`);
      for(const channel of result)assert.ok(peak(channel)<=10**(-1/20)+1e-7,`${rate}Hz +${db} dB peak`);
    }
    const {result}=render([left,right],-60,rate);
    assert.equal(peak(result[0]),0);
  }
});
test('steady boosted tones retain a smooth waveform rather than flat-topping',()=>{
  const input=Float32Array.from({length:96000},(_,i)=>.79*Math.sin(2*Math.PI*1000*i/48000));
  const {result,delay}=render([input,input],24);
  const output=result[0].slice(48000+delay,96000+delay);
  const source=input.slice(48000);
  const scale=output.reduce((sum,value,i)=>sum+value*source[i],0)/source.reduce((sum,value)=>sum+value*value,0);
  const error=Float32Array.from(output,(value,i)=>value-source[i]*scale);
  assert.ok(rms(error)/rms(output)<.02,'steady-tone residual must stay below 2%');
  assert.ok(peak(output)<.9);
});
test('isolated peaks lower surrounding gain smoothly instead of clamping a single sample',()=>{
  for(const rate of [24000,44100,48000,96000]) {
    const dsp=new NavigationLoudness(rate),spike=rate/10|0;
    const input=new Float32Array(spike+dsp.delay*3).fill(.1);
    input[spike]=2;
    const output=[new Float32Array(input.length),new Float32Array(input.length)];
    dsp.process([input,input],output);
    const position=spike+dsp.delay,peakGain=output[0][position]/2;
    assert.ok(peak(output[0])<=dsp.ceiling+1e-7);
    for(const offset of [-1,1]) {
      const adjacentGain=output[0][position+offset]/.1;
      assert.ok(Math.abs(adjacentGain-peakGain)<.01,'peak must share a smooth gain envelope with adjacent samples');
    }
    for(let i=spike+1;i<position;i++) {
      assert.ok(output[0][i]<=output[0][i-1]+1e-7,'gain reduction must precede the transient');
      assert.ok(Math.abs(output[0][i]-output[0][i-1])<.001);
    }
  }
});
test('clustered peaks keep their ceiling throughout the complete lookahead window',()=>{
  for(const rate of [24000,44100,48000,96000]) {
    const dsp=new NavigationLoudness(rate);
    const input=Float32Array.from({length:rate},(_,i)=>
      i<rate/2?2-(i%dsp.delay)/dsp.delay:0);
    const output=[new Float32Array(input.length),new Float32Array(input.length)];
    dsp.process([input,input],output);
    assert.ok(peak(output[0])<=dsp.ceiling+1e-7);
  }
});
test('small positive settings do not turn loud guidance down by switching compression on',()=>{
  const rate=48000,input=Float32Array.from({length:rate},(_,i)=>.79*Math.sin(2*Math.PI*400*i/rate));
  const baseline=rms(input.slice(rate/2));
  for(const db of [1,2,3,6,12,24]) {
    const {result,delay}=render([input,input],db,rate);
    const steady=result[0].slice(rate/2+delay,rate+delay);
    assert.ok(rms(steady)>=baseline*.995,`+${db} dB must not attenuate the original guidance`);
    assert.ok(peak(steady)<=10**(-1/20)+1e-7);
  }
});
