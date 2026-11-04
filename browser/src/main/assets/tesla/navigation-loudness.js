export const NAVIGATION_MIN_DB=-60, NAVIGATION_MAX_DB=24;
export function normalizeNavigationDb(value) {
  if(!['number','string'].includes(typeof value) || String(value).trim()==='')return 0;
  const db=Number(value);
  return Number.isFinite(db)?Math.max(NAVIGATION_MIN_DB,Math.min(NAVIGATION_MAX_DB,Math.round(db))):0;
}
export const navigationAmplitude=db=>db<=NAVIGATION_MIN_DB?0:10**(db/20);

// Linked stereo dynamics, followed by a delayed peak limiter. No per-sample clipping.
export class NavigationLoudness {
  constructor(rate) {
    this.delay=Math.ceil(rate*.005);
    this.left=new Float32Array(this.delay);this.right=new Float32Array(this.delay);
    this.limitHistory=new Float64Array(this.delay+1).fill(1);
    this.limitSum=this.limitHistory.length;this.limitPosition=0;
    this.peakGains=new Float64Array(this.delay+2);this.peakFrames=new Float64Array(this.delay+2);
    this.peakHead=0;this.peakTail=0;this.frame=0;
    this.position=0;this.compression=1;this.limit=1;
    this.attack=Math.exp(-1/(rate*.003));this.release=Math.exp(-1/(rate*.04));
    this.limitRelease=Math.exp(-1/(rate*.02));
    this.ceiling=10**(-1/20);
    this.gainDb=0;
  }
  process(input,output) {
    const left=input[0],right=input[1]||left;
    for(let i=0;i<output[0].length;i++) {
      const l=left?.[i]||0,r=right?.[i]||0,peak=Math.max(Math.abs(l),Math.abs(r));
      const over=20*Math.log10(Math.max(peak,1e-12))+12;
      const reduction=over<=-3?0:over>=3?over*(2/3):(over+3)**2/18;
      const target=10**(-Math.min(Math.max(0,this.gainDb),reduction)/20);
      const coefficient=target<this.compression?this.attack:this.release;
      this.compression=target+(this.compression-target)*coefficient;
      const a=l*this.compression,b=r*this.compression,p=Math.max(Math.abs(a),Math.abs(b));
      const needed=p>this.ceiling?this.ceiling/p:1;
      // Sliding minimum holds every peak through the lookahead window in amortized constant time.
      const capacity=this.peakGains.length;
      while(this.peakHead!==this.peakTail && this.peakFrames[this.peakHead]<this.frame-this.delay)
        this.peakHead=(this.peakHead+1)%capacity;
      while(this.peakHead!==this.peakTail) {
        const last=(this.peakTail+capacity-1)%capacity;
        if(this.peakGains[last]<needed)break;
        this.peakTail=last;
      }
      this.peakGains[this.peakTail]=needed;this.peakFrames[this.peakTail]=this.frame++;
      this.peakTail=(this.peakTail+1)%capacity;
      this.limit=Math.min(this.peakGains[this.peakHead],1+(this.limit-1)*this.limitRelease);
      // The average reaches each required gain before that sample leaves the audio delay.
      this.limitSum+=this.limit-this.limitHistory[this.limitPosition];
      this.limitHistory[this.limitPosition]=this.limit;
      this.limitPosition=(this.limitPosition+1)%this.limitHistory.length;
      const gain=this.limitSum/this.limitHistory.length;
      const oldLeft=this.left[this.position],oldRight=this.right[this.position];
      this.left[this.position]=a;this.right[this.position]=b;
      this.position=(this.position+1)%this.delay;
      output[0][i]=oldLeft*gain;
      if(output[1])output[1][i]=oldRight*gain;
    }
  }
}

if(typeof registerProcessor==='function') {
  class NavigationProcessor extends AudioWorkletProcessor {
    constructor(){
      super();this.dynamics=new NavigationLoudness(sampleRate);
      this.port.onmessage=event=>{if(event.data.type==='gain')this.dynamics.gainDb=normalizeNavigationDb(event.data.db);};
    }
    process(inputs,outputs){this.dynamics.process(inputs[0],outputs[0]);return true;}
  }
  registerProcessor('diplay-navigation-loudness',NavigationProcessor);
}
