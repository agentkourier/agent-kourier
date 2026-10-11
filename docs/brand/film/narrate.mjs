// Generates the film's narration from narration.json with a speech model on OpenRouter.
//
//   node narrate.mjs                         every demo and teaser line, default model and voice
//   node narrate.mjs --cut teaser            one cut
//   node narrate.mjs --only reveal,close     some lines
//   node narrate.mjs --voice george          another voice (written to its own folder)
//   node narrate.mjs --audition brian,george,matilda --only reveal    compare voices on one line
//   node narrate.mjs --plain                 send the text as written, without the pronunciation fixes
//   node narrate.mjs --force                 regenerate lines that already exist
//   node narrate.mjs --voice eric --direction "confident, measured, authoritative" --pitch -1 --bass 2 --name measured-deeper
//                                            a directed read, lowered a semitone with some low-end warmth, in its own folder
//
// One MP3 per line goes to audio/<model>-<voice>/<id>.mp3, with a manifest.json of durations: the
// picture is timed to those. A line that already exists is skipped, so a rerun costs nothing.
//
// The key is read from OPEN_ROUTER_API_KEY (or OPENROUTER_API_KEY), else from the repository's
// .env. It is never printed. Durations need ffmpeg on PATH, or FFMPEG=/path/to/ffmpeg.
import { spawnSync } from 'node:child_process';
import { existsSync } from 'node:fs';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const FFMPEG = process.env.FFMPEG || 'ffmpeg';
const ENDPOINT = 'https://openrouter.ai/api/v1/audio/speech';
const DEFAULT_MODEL = 'elevenlabs/eleven-v4';
const DEFAULT_VOICE = { elevenlabs: 'brian' }; // other providers: their own default unless --voice

function flags(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (!a.startsWith('--')) throw new Error(`unexpected argument ${a}`);
    const k = a.slice(2);
    if (k === 'force' || k === 'plain') out[k] = true;
    else out[k] = argv[++i];
  }
  return out;
}

async function apiKey() {
  const env = process.env.OPEN_ROUTER_API_KEY || process.env.OPENROUTER_API_KEY;
  if (env) return env;
  const file = join(HERE, '..', '..', '..', '.env');
  if (existsSync(file)) {
    for (const line of (await readFile(file, 'utf8')).split('\n')) {
      const m = line.match(/^\s*OPEN_?ROUTER_API_KEY\s*=\s*(.+?)\s*$/);
      if (m) return m[1].replace(/^["']|["']$/g, '');
    }
  }
  throw new Error('no OpenRouter key: set OPEN_ROUTER_API_KEY, or put it in the repository .env');
}

// The spoken form of a line: its own "say" if it has one, else the text with the pronunciation fixes.
function spoken(line, pronounce, plain, direction) {
  let s = line.say || line.text;
  if (!line.say && !plain) for (const [from, to] of Object.entries(pronounce)) s = s.split(from).join(to);
  // A bracketed direction is read by the model as how to say the line, not as words to say.
  return direction ? `[${direction}] ${s}` : s;
}

// Pitch in semitones (the tempo is put back, so only the pitch moves) and a low-shelf boost in dB, applied after generation.
function treatAudio(buf, file, pitch, bass) {
  if (!pitch && !bass) return writeFile(file, buf);
  const k = Math.pow(2, pitch / 12), chain = [];
  if (pitch) chain.push('aresample=44100', `asetrate=${(44100 * k).toFixed(1)}`, 'aresample=44100', `atempo=${(1 / k).toFixed(5)}`);
  if (bass) chain.push(`bass=g=${bass}:f=120`);
  const r = spawnSync(FFMPEG, ['-y', '-loglevel', 'error', '-nostdin', '-i', 'pipe:0', '-af', chain.join(','), '-c:a', 'libmp3lame', '-q:a', '2', file], { input: buf });
  if (r.status !== 0) throw new Error(`ffmpeg could not process ${file}: ${r.stderr}`);
}

function seconds(file) {
  const r = spawnSync(FFMPEG, ['-hide_banner', '-i', file, '-f', 'null', '-'], { encoding: 'utf8' });
  if (r.error) return null;
  const all = [...r.stderr.matchAll(/time=(\d+):(\d+):(\d+\.\d+)/g)];
  if (!all.length) return null;
  const [, h, m, s] = all[all.length - 1];
  return Math.round((+h * 3600 + +m * 60 + +s) * 100) / 100;
}

async function synthesize(key, model, voice, input) {
  const body = { model, input, response_format: 'mp3' };
  if (voice) body.voice = voice;
  const res = await fetch(ENDPOINT, {
    method: 'POST',
    headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    let message = await res.text();
    try { message = JSON.parse(message).error.message; } catch { /* keep the raw text */ }
    throw new Error(`HTTP ${res.status}: ${message}`);
  }
  return Buffer.from(await res.arrayBuffer());
}

try {
  const opt = flags(process.argv.slice(2));
  const script = JSON.parse(await readFile(join(HERE, 'narration.json'), 'utf8'));
  const model = opt.model || DEFAULT_MODEL;
  const voices = opt.audition ? opt.audition.split(',') : [opt.voice || DEFAULT_VOICE[model.split('/')[0]] || ''];
  const only = opt.only ? new Set(opt.only.split(',')) : null;
  const cuts = opt.cut ? [opt.cut] : ['demo', 'teaser'];
  const lines = cuts.flatMap(c => {
    if (!script[c]) throw new Error(`no cut named ${c}`);
    return script[c].map(l => ({ ...l, cut: c }));
  }).filter(l => !only || only.has(l.id));
  if (!lines.length) throw new Error('no lines selected');

  const key = await apiKey();
  for (const voice of voices) {
    const dir = join(HERE, 'audio', `${model}${voice ? `-${voice}` : ''}${opt.name ? `-${opt.name}` : ''}`.replace(/[^a-z0-9.]+/gi, '-'));
  const pitch = Number(opt.pitch || 0), bass = Number(opt.bass || 0), treat = `${pitch}/${bass}`;
    const manifestFile = join(dir, 'manifest.json');
    const manifest = existsSync(manifestFile) ? JSON.parse(await readFile(manifestFile, 'utf8')) : { model, voice, lines: {} };
    let chars = 0;
    for (const line of lines) {
      const file = join(dir, `${line.id}.mp3`);
      const say = spoken(line, script.pronounce || {}, opt.plain, opt.direction);
      const known = manifest.lines[line.id];
      if (existsSync(file) && known && known.say === say && (known.treat || '0/0') === treat && !opt.force) {
        console.log(`skip  ${line.id}  ${known.seconds ?? '?'} s`);
        continue;
      }
      const audio = await synthesize(key, model, voice, say);
      await mkdir(dir, { recursive: true });
      await treatAudio(audio, file, pitch, bass);
      chars += say.length;
      manifest.lines[line.id] = { cut: line.cut, text: line.text, say, treat, seconds: seconds(file) };
      console.log(`wrote ${line.id}  ${manifest.lines[line.id].seconds ?? '?'} s  ${say.length} chars`);
    }
    if (existsSync(dir)) await writeFile(manifestFile, JSON.stringify(manifest, null, 2) + '\n');
    for (const c of cuts) {
      const total = Object.values(manifest.lines).filter(l => l.cut === c).reduce((n, l) => n + (l.seconds || 0), 0);
      if (total) console.log(`${voice || 'default voice'}: ${c} narration ${total.toFixed(1)} s of speech`);
    }
    console.log(`${dir}: ${chars} characters sent`);
  }
} catch (err) {
  console.error(`narrate: ${err.message}`);
  process.exit(1);
}
