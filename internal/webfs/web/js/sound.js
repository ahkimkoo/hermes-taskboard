// Sound notifications using Web Audio API (synthesized — no asset downloads needed).
const ctxRef = { ctx: null };
let prefs = { enabled: true, volume: 0.7, events: { execute_start: true, needs_input: true, done: true } };

export function setPrefs(p) {
  prefs = Object.assign({}, prefs, p);
  if (p && p.events) prefs.events = Object.assign({}, prefs.events, p.events);
}

function ctx() {
  if (!ctxRef.ctx) {
    const C = window.AudioContext || window.webkitAudioContext;
    if (C) ctxRef.ctx = new C();
  }
  return ctxRef.ctx;
}

function tone(freq, duration, type = 'sine') {
  if (!prefs.enabled) return;
  const ac = ctx();
  if (!ac) return;
  const osc = ac.createOscillator();
  const gain = ac.createGain();
  osc.type = type;
  osc.frequency.setValueAtTime(freq, ac.currentTime);
  gain.gain.setValueAtTime(0, ac.currentTime);
  gain.gain.linearRampToValueAtTime(prefs.volume, ac.currentTime + 0.01);
  gain.gain.exponentialRampToValueAtTime(0.0001, ac.currentTime + duration);
  osc.connect(gain).connect(ac.destination);
  osc.start();
  osc.stop(ac.currentTime + duration);
}

export function play(kind) {
  if (!prefs.enabled) return;
  if (!prefs.events[kind]) return;
  try {
    if (kind === 'execute_start') { tone(660, 0.15); }
    else if (kind === 'needs_input') { setTimeout(() => tone(880, 0.12, 'triangle'), 0); setTimeout(() => tone(880, 0.12, 'triangle'), 180); }
    else if (kind === 'done') { setTimeout(() => tone(660, 0.12), 0); setTimeout(() => tone(990, 0.18), 120); }
  } catch { /* autoplay blocked until user gesture */ }
}

// Unlock audio context on first user gesture (required by browsers).
let unlocked = false;
function unlock() {
  if (unlocked) return;
  const ac = ctx();
  if (ac && ac.state === 'suspended') ac.resume();
  unlocked = true;
}
document.addEventListener('click', unlock, { once: true });
document.addEventListener('touchstart', unlock, { once: true });
