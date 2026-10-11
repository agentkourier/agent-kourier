// Builds the v2 film: motion graphics from film.html, real footage from raw/, narration from audio/.
//
//   node build.mjs stills [16x9 ...]           review stills of every motion-graphics segment and footage frame
//   node build.mjs demo [16x9 9x16 1x1]        the demo cut
//   node build.mjs teaser [16x9 9x16 1x1]      the 20-second teaser
//   node build.mjs sheets                      contact sheets and posters from the finished files in out/
//   node build.mjs storyboard                  the storyboard, from the finished 16:9 demo
//
// Needs Playwright's Chromium and ffmpeg (libx264) on PATH, the recordings in raw/ and the narration in
// audio/<VOICE>/ (node narrate.mjs --voice brian). The picture is timed to the narration.
import { chromium } from 'playwright';
import { spawn, spawnSync } from 'node:child_process';
import { existsSync } from 'node:fs';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = join(HERE, 'out');
const WORK = join(HERE, 'build');
const PAGE = pathToFileURL(join(HERE, 'film.html')).href;
const FFMPEG = process.env.FFMPEG || 'ffmpeg';
// The narration set in audio/. Slide lengths below are timed to this voice; another voice needs them re-timed.
const VOICE = process.env.VOICE_DIR || 'elevenlabs-eleven-v4-eric-measured-deeper';
const FPS = 30;
// REDO=close,t-end rebuilds only those beats and reuses every other segment already in build/.
const REDO = process.env.REDO ? new Set(process.env.REDO.split(',')) : null;
const reuse = (id, file) => REDO && !REDO.has(id) && existsSync(file);

const FORMATS = { '16x9': [1920, 1080], '9x16': [1080, 1920], '1x1': [1080, 1080] };
// Where footage sits in each format.
// 9:16 stops at y 1680 so nothing sits under a phone app's own controls.
const ZONES = { '16x9': [860, 72, 1000, 936], '9x16': [60, 480, 960, 1200], '1x1': [40, 240, 1000, 760] };
// The recordings are 2880 x 1864 captures of a 1440-point screen; Slack's page starts 344 px down while the
// extension's "debugging this browser" bar is showing, which it is in every stretch used here.
const SRC = { scale: 2, oy: 344 };
// Framings in Slack page coordinates [x, y, w, h]. They stop short of the right and bottom edges, where the
// extension draws an orange glow, and start right of the sidebar, which carries the workspace name.
const FRAMINGS = { wide: [400, 40, 1036, 717], wideTight: [400, 150, 720, 607], landing: [404, 494, 640, 254], threadTop: [1030, 40, 404, 539], threadBottom: [1030, 217, 404, 539], threadLow: [1030, 452, 404, 278] };

const S1 = 'session-1', S4 = 'session-4-alert-retake';
// Where the Chrome extension's pointer rests in a shot, in page coordinates, so it can be painted out. It is only ever
// painted out over blank chrome (the thread header, the empty end of the composer); shots where it covered text are
// reframed or retimed instead, so no message pixel is altered.
const HEADER = [1200, 66], COMPOSER = [860, 672];
// A shot written as { wide, tall } differs between 16:9 and the tall formats (9:16, 1:1).
const shotFor = (ar, shot) => (Array.isArray(shot) ? shot : ar === '16x9' ? shot.wide : shot.tall);
// A beat is one narration line. mg beats are drawn and last lead + line + tail; rec beats are footage shots
// [source, in, length, out seconds, framing, pointer, command chip]. Thread close-ups are used in every format:
// they are legible, and they leave the channel's setup lines out.
const DEMO = [
  { mg: 'p1', line: 'p1', lead: 0.4, dur: 7.1 },
  { mg: 'p2', line: 'p2', lead: 0.2, dur: 4.5 },
  { mg: 'p3', line: 'p3', lead: 0.2, dur: 6.6 },
  { mg: 'reveal', line: 'reveal', lead: 0.4, dur: 7.0 },
  { mg: 'promise', line: 'promise', lead: 0.2, dur: 7.8 },
  { rec: 'r1', line: 'r1', lead: 0.3, cap: 'An alert opens a session.', sub: 'Nobody typed a prompt.',
    shots: [{ wide: [S4, 39.2, 2.4, 2.4, 'landing'], tall: [S4, 43.4, 2.4, 2.4, 'threadTop', HEADER] },
      { wide: [S4, 43.4, 15.2, 7.6, 'threadTop', HEADER], tall: [S4, 45.8, 12.8, 7.6, 'threadTop', HEADER] }, [S4, 61.0, 4.0, 4.0, 'threadTop', HEADER]] },
  { rec: 'r2', line: 'r2', lead: 0.3, cap: 'Reply in the thread.', sub: 'Same session, full context.',
    shots: [[S4, 105.2, 1.4, 2.0, 'threadBottom'], [S4, 106.6, 18.0, 6.0, 'threadTop', HEADER]] },
  { rec: 'r3', line: 'r3', lead: 0.3, cap: 'Or just ask.', sub: 'A mention opens its own thread.',
    shots: [{ wide: [S1, 18.0, 4.0, 2.0, 'wide', COMPOSER], tall: [S1, 18.0, 4.0, 2.0, 'wideTight', COMPOSER] },
      { wide: [S1, 26.6, 1.6, 1.6, 'wide', COMPOSER], tall: [S1, 26.6, 1.6, 1.6, 'wideTight', COMPOSER] }, [S1, 37.2, 2.4, 2.4, 'threadTop', HEADER]] },
  { rec: 'r4', line: 'r4', lead: 0.3, cap: 'It asks when it needs to.', sub: 'Answer in the thread and it carries on.',
    shots: [[S1, 147.5, 3.4, 3.4, 'threadTop', HEADER], [S1, 166.2, 13.8, 4.6, 'threadTop', HEADER]] },
  { rec: 'r5', line: 'r5', lead: 0.3, cap: 'A restart loses no message.', sub: 'The thread still finishes, once.',
    shots: [[S1, 226.0, 5.0, 2.5, 'threadTop', HEADER], [S1, 234.0, 2.5, 2.5, 'threadTop', HEADER, '$ kubectl delete pod -l app.kubernetes.io/instance=agent-kourier-sandbox'], [S1, 277.5, 4.0, 4.0, 'threadLow']] },
  { mg: 'yaml', line: 'yaml', lead: 0.3, dur: 7.6 },
  { mg: 'close', line: 'close', lead: 0.3, dur: 21.8, query: { endAt: 15.2 } },
];
const TEASER = [
  { mg: 't-problem', line: 't-problem', lead: 0.3, dur: 4.5 },
  { mg: 't-reveal', line: 't-reveal', lead: 0.2, dur: 6.6 },
  { rec: 't1', line: 't-proof', lead: 0.2, cap: 'An alert opens a session.', sub: '', shots: [[S4, 43.4, 6.0, 2.0, 'threadTop', HEADER]] },
  { rec: 't2', cap: 'Reply in the thread.', sub: '', shots: [[S4, 106.6, 6.0, 2.0, 'threadTop', HEADER]] },
  { rec: 't3', cap: 'Or just ask.', sub: '', shots: [[S1, 37.2, 2.0, 2.0, 'threadTop', HEADER]] },
  { mg: 't-end', dur: 2.9 },
];
const CUTS = { demo: DEMO, teaser: TEASER };

const even = n => 2 * Math.round(n / 2);
const frames = s => Math.round(s * FPS);

function run(cmd, args, input) {
  const r = spawnSync(cmd, args, { encoding: 'utf8', input, maxBuffer: 1 << 26 });
  if (r.status !== 0) throw new Error(`${cmd} ${args.slice(0, 6).join(' ')} ... failed:\n${(r.stderr || '').split('\n').slice(-8).join('\n')}`);
  return r;
}

// Where a shot's footage card, caption, lockup and note go. In 16:9 the caption and the card are centred as a pair, so a
// narrow thread card does not leave the caption stranded at the far left; in the other formats the caption sits above.
function layoutFor(ar, framing) {
  const [zx, zy, zw, zh] = ZONES[ar], [, , fw, fh] = FRAMINGS[framing];
  const k = Math.min(zw / fw, zh / fh), w = even(fw * k), h = even(fh * k);
  // The caption, lockup and note never move between shots; only the card's size follows the framing.
  if (ar === '16x9') return { card: [zx, even(zy + (zh - h) / 2), w, h], capx: 360, capy: 520, capw: 440, capalign: 'start', capvc: 1, lock: [360, 984], note: [zx, 1042, 'start'] };
  const card = [even(zx + (zw - w) / 2), zy, w, h];
  return ar === '1x1'
    ? { card, capx: 540, capy: 110, capw: 990, capalign: 'middle', lock: [44, 1040], note: [1036, 1046, 'end'] }
    : { card, capx: 70, capy: 240, capw: 940, capalign: 'start', lock: [60, 1750], note: [1020, 1756, 'end'] };
}

async function openPage(browser, ar, query) {
  const [w, h] = FORMATS[ar];
  const page = await browser.newPage({ viewport: { width: w, height: h }, deviceScaleFactor: 1 });
  const q = new URLSearchParams({ ar, ...query });
  await page.goto(`${PAGE}?${q}`);
  await page.waitForFunction(() => window.READY === true);
  await page.evaluate(() => document.fonts.ready);
  return page;
}

async function renderMg(browser, ar, beat, file) {
  const page = await openPage(browser, ar, { seg: beat.mg, t: 0, ...(beat.query || {}) });
  const ff = spawn(FFMPEG, ['-y', '-loglevel', 'error', '-f', 'image2pipe', '-framerate', String(FPS), '-i', '-',
    '-c:v', 'libx264', '-preset', 'veryfast', '-crf', '12', '-pix_fmt', 'yuv420p', '-r', String(FPS), '-an', file], { stdio: ['pipe', 'inherit', 'inherit'] });
  const done = new Promise((res, rej) => ff.on('close', c => (c === 0 ? res() : rej(new Error(`ffmpeg exited ${c}`)))));
  for (let i = 0; i < frames(beat.dur); i++) {
    await page.evaluate(t => window.renderAt(t), i / FPS);
    const buf = await page.screenshot({ type: 'png' });
    if (!ff.stdin.write(buf)) await new Promise(r => ff.stdin.once('drain', r));
  }
  ff.stdin.end();
  await done;
  await page.close();
}

async function renderOverlay(browser, ar, beat, shot, file) {
  const [, , len, out, framing, , cmd] = shot, lay = layoutFor(ar, framing), card = lay.card, speed = len / out;
  const query = { overlay: 1, cap: beat.cap || '', sub: beat.sub || '', ...lay, card: card.join(','), lock: lay.lock.join(','), note: lay.note.join(',') };
  // The note under the footage says it is sped up in places. SPEED_CHIP=1 also labels each sped-up shot.
  if (process.env.SPEED_CHIP && speed > 1.05) query.speed = `${Math.round(speed * 10) / 10}\u00d7 speed`;
  if (cmd) query.cmd = cmd;
  const page = await openPage(browser, ar, query);
  await page.screenshot({ path: file, type: 'png', omitBackground: true });
  await page.close();
  return card;
}

function cutShot(ar, shot, card, overlay, file) {
  const [src, tin, len, out, framing, pointer] = shot, [W, H] = FORMATS[ar];
  const [fx, fy, fw, fh] = FRAMINGS[framing], k = SRC.scale;
  // The pointer is an arrow about 20 x 28 points whose tip is where it was aimed, with a soft glow around it. It is
  // covered with the same 38 x 46 point box of blank chrome taken 60 points to its right, in the same rows.
  const bx = (pointer ? pointer[0] - 12 : 0) * k, by = SRC.oy + (pointer ? pointer[1] - 13 : 0) * k;
  const unpoint = pointer ? `split[m][p];[p]crop=${38 * k}:${46 * k}:${bx + 60 * k}:${by}[pp];[m][pp]overlay=${bx}:${by},` : '';
  const crop = `${fw * k}:${fh * k}:${fx * k}:${SRC.oy + fy * k}`, [cx, cy, cw, ch] = card;
  const filter = `[0:v]${unpoint}crop=${crop},setpts=(PTS-STARTPTS)*${(out / len).toFixed(6)},fps=${FPS},scale=${cw}:${ch}:flags=lanczos[fg];` +
    `color=c=0x0A1530:s=${W}x${H}:r=${FPS}[bg];[bg][fg]overlay=${cx}:${cy}:eof_action=repeat[a];[a][1:v]overlay=0:0[v]`;
  run(FFMPEG, ['-y', '-loglevel', 'error', '-nostdin', '-ss', String(tin), '-t', String(len), '-i', join(HERE, 'raw', `${src}.mp4`),
    '-loop', '1', '-i', overlay, '-filter_complex', filter, '-map', '[v]', '-frames:v', String(frames(out)),
    '-c:v', 'libx264', '-preset', 'veryfast', '-crf', '12', '-pix_fmt', 'yuv420p', '-r', String(FPS), '-an', file]);
}

async function lineSeconds() {
  const m = JSON.parse(await readFile(join(HERE, 'audio', VOICE, 'manifest.json'), 'utf8'));
  return Object.fromEntries(Object.entries(m.lines).map(([k, v]) => [k, v.seconds]));
}
const beatDur = b => (b.mg ? b.dur : b.shots.reduce((n, s) => n + shotFor('16x9', s)[3], 0));

async function buildCut(browser, name, ar) {
  const beats = CUTS[name], dir = join(WORK, `${name}-${ar}`), secs = await lineSeconds();
  await mkdir(dir, { recursive: true });
  const segs = [], cues = [];
  let at = 0;
  for (const [i, beat] of beats.entries()) {
    const dur = beatDur(beat);
    if (beat.line) {
      cues.push({ line: beat.line, at: at + beat.lead });
      // A line may run past its own beat only into footage, never past the next line.
      if (!secs[beat.line]) throw new Error(`no narration for ${beat.line}; run narrate.mjs`);
    }
    if (beat.mg) {
      const f = join(dir, `${String(i).padStart(2, '0')}-${beat.mg}.mp4`);
      if (!reuse(beat.mg, f)) await renderMg(browser, ar, beat, f);
      segs.push(f);
    } else {
      for (const [j, variants] of beat.shots.entries()) {
        const shot = shotFor(ar, variants);
        const base = join(dir, `${String(i).padStart(2, '0')}-${beat.rec}-${j}`);
        if (!reuse(beat.rec, `${base}.mp4`)) cutShot(ar, shot, await renderOverlay(browser, ar, beat, shot, `${base}.png`), `${base}.png`, `${base}.mp4`);
        segs.push(`${base}.mp4`);
      }
    }
    console.log(`${name} ${ar}: ${beat.mg || beat.rec} ${at.toFixed(1)}-${(at + dur).toFixed(1)} s`);
    at += dur;
  }
  for (const [k, c] of cues.entries()) {
    const end = c.at + secs[c.line], next = cues[k + 1] ? cues[k + 1].at : at;
    if (end > next + 0.01) throw new Error(`${name}: line ${c.line} ends at ${end.toFixed(2)} s, after ${next.toFixed(2)} s`);
  }
  // Picture.
  const list = join(dir, 'segments.txt');
  await writeFile(list, segs.map(f => `file '${f.replace(/'/g, "'\\''")}'`).join('\n') + '\n');
  // Narration: each line at its cue, then levelled.
  const ain = cues.flatMap(c => ['-i', join(HERE, 'audio', VOICE, `${c.line}.mp3`)]);
  const mixf = cues.map((c, k) => `[${k + 1}:a]aresample=48000,adelay=${Math.round(c.at * 1000)}:all=1[a${k}]`).join(';') +
    `;${cues.map((_, k) => `[a${k}]`).join('')}amix=inputs=${cues.length}:normalize=0,apad=whole_dur=${at.toFixed(3)},loudnorm=I=-16:TP=-1.5:LRA=11,aformat=channel_layouts=stereo[a]`;
  await mkdir(OUT, { recursive: true });
  const file = join(OUT, `agent-kourier-${name}-${ar}.mp4`);
  run(FFMPEG, ['-y', '-loglevel', 'error', '-nostdin', '-f', 'concat', '-safe', '0', '-i', list, ...ain, '-filter_complex', mixf,
    '-map', '0:v', '-map', '[a]', '-c:v', 'libx264', '-preset', 'slow', '-crf', '18', '-pix_fmt', 'yuv420p', '-r', String(FPS),
    '-c:a', 'aac', '-b:a', '192k', '-ar', '48000', '-t', at.toFixed(3), '-movflags', '+faststart', '-metadata', 'title=Agent Kourier', file]);
  console.log(`wrote ${file} (${at.toFixed(1)} s)`);
}

// One still per motion-graphics segment and one footage frame per rec beat, to look at before a full build.
async function stills(browser, ars) {
  const dir = join(WORK, 'stills');
  await mkdir(dir, { recursive: true });
  const shots = [['p1', 5.5], ['p2', 3.6], ['p3', 5.6], ['reveal', 4.5], ['promise', 6.6], ['yaml', 4.0], ['close', 13.0], ['close', 19.0], ['t-problem', 3.6], ['t-reveal', 5.4], ['t-end', 3.0]];
  for (const ar of ars) {
    for (const [seg, t] of shots) {
      const page = await openPage(browser, ar, { seg, t, ...(seg === 'close' ? { endAt: 15.2 } : {}) });
      await page.screenshot({ path: join(dir, `${ar}-${seg}-${t}.png`) });
      await page.close();
    }
    for (const [beat, j] of DEMO.filter(b => b.rec).flatMap(b => b.shots.map((_, j) => [b, j]))) {
      const shot = shotFor(ar, beat.shots[j]), base = join(dir, `${ar}-${beat.rec}-${j}`);
      const card = await renderOverlay(browser, ar, beat, shot, `${base}-overlay.png`);
      const one = [shot[0], shot[1] + shot[2] * 0.8, 0.2, 0.2, shot[4], shot[5], shot[6]];
      cutShot(ar, one, card, `${base}-overlay.png`, `${base}.mp4`);
      run(FFMPEG, ['-y', '-loglevel', 'error', '-nostdin', '-i', `${base}.mp4`, '-frames:v', '1', `${base}.png`]);
    }
    console.log(`stills for ${ar} in ${dir}`);
  }
}

function sheets() {
  for (const name of Object.keys(CUTS)) for (const ar of Object.keys(FORMATS)) {
    const file = join(OUT, `agent-kourier-${name}-${ar}.mp4`);
    if (!existsSync(file)) continue;
    const [w, h] = FORMATS[ar], tw = ar === '9x16' ? 216 : 384, th = even(tw * h / w), step = name === 'demo' ? 3 : 1, cols = ar === '9x16' ? 8 : 6;
    const secs = Number(run('ffprobe', ['-v', 'error', '-show_entries', 'format=duration', '-of', 'csv=p=0', file]).stdout), rows = Math.ceil(Math.ceil(secs / step) / cols);
    run(FFMPEG, ['-y', '-loglevel', 'error', '-nostdin', '-i', file, '-vf', `fps=1/${step},scale=${tw}:${th},tile=${cols}x${rows}:padding=6:margin=6:color=0x0A1530`,
      '-frames:v', '1', join(OUT, `contact-sheet-${name}-${ar}.png`)]);
    if (name === 'demo') run(FFMPEG, ['-y', '-loglevel', 'error', '-nostdin', '-sseof', '-1.5', '-i', file, '-frames:v', '1', join(OUT, `poster-${ar}.png`)]);
    console.log(`sheet and poster for ${name} ${ar}`);
  }
}

// The storyboard: one row per beat of the demo cut, with a frame from the finished 16:9 file.
async function storyboard(browser) {
  const file = join(OUT, 'agent-kourier-demo-16x9.mp4'), dir = join(WORK, 'storyboard');
  await mkdir(dir, { recursive: true });
  const script = JSON.parse(await readFile(join(HERE, 'narration.json'), 'utf8'));
  const said = Object.fromEntries(script.demo.map(l => [l.id, l.text]));
  const rows = [];
  let at = 0;
  for (const [i, beat] of DEMO.entries()) {
    const dur = beatDur(beat), png = join(dir, `${i}.jpg`);
    run(FFMPEG, ['-y', '-loglevel', 'error', '-nostdin', '-ss', (at + dur * 0.82).toFixed(2), '-i', file, '-frames:v', '1', '-vf', 'scale=640:360', '-q:v', '3', png]);
    const img = `data:image/jpeg;base64,${(await readFile(png)).toString('base64')}`;
    const source = beat.mg ? 'Animation (film.html)' : `Real footage: ${[...new Set(beat.shots.map(s => shotFor('16x9', s)[0]))].join(', ')}, ${beat.shots.map(s => shotFor('16x9', s)).map(s => `${s[1]}–${(s[1] + s[2]).toFixed(1)} s${s[2] / s[3] > 1.05 ? ` at ${Math.round(s[2] / s[3] * 10) / 10}×` : ''}`).join('; ')}`;
    rows.push({ n: i + 1, id: beat.mg || beat.rec, span: `${at.toFixed(1)} – ${(at + dur).toFixed(1)} s`, img, said: said[beat.line] || '', cap: beat.cap ? `${beat.cap} ${beat.sub || ''}` : '', source });
    at += dur;
  }
  const esc = s => s.replace(/&/g, '&amp;').replace(/</g, '&lt;');
  const html = `<!doctype html><html lang="en"><meta charset="utf-8"><title>Agent Kourier storyboard</title><style>
    @page{size:A4 landscape;margin:10mm}
    body{margin:0;background:#0A1530;color:#fff;font:400 14px/1.45 Inter,"Helvetica Neue",Helvetica,Arial,sans-serif;padding:28px;-webkit-print-color-adjust:exact;print-color-adjust:exact}
    h1{font-size:26px;margin:0 0 6px} .lead{opacity:.8;margin:0 0 4px;max-width:1100px} .meta{font-family:"SF Mono",Menlo,monospace;font-size:12.5px;opacity:.65;margin:0 0 20px}
    .shot{display:grid;grid-template-columns:400px 1fr;gap:22px;padding:14px 0;border-top:1px solid #1D3F91;break-inside:avoid}
    img{width:400px;height:225px;border-radius:8px;display:block;outline:1px solid #1D3F91}
    .n{font-family:"SF Mono",Menlo,monospace;background:#326CE5;border-radius:4px;padding:1px 7px;font-size:13px;margin-right:8px}
    h2{font-size:17px;margin:2px 0 8px} .span{font-family:"SF Mono",Menlo,monospace;font-size:12.5px;opacity:.65;margin-left:6px;font-weight:400}
    dl{margin:0;display:grid;grid-template-columns:96px 1fr;gap:5px 12px} dt{opacity:.6;font-size:12px;text-transform:uppercase;letter-spacing:.06em;padding-top:2px} dd{margin:0}
    </style><h1>Agent Kourier · storyboard (demo cut)</h1>
    <p class="lead">Every thread, a live agent session. The always-on chat gateway for A2A agents on Kubernetes. Slack and Alertmanager first, built for more.</p>
    <p class="meta">${at.toFixed(1)} s · 30 fps · 16:9, 9:16, 1:1 · narration: ElevenLabs Eleven v4 "Eric" via OpenRouter, directed measured, lowered one semitone · footage: the kind sandbox, real Slack, real agent output, seeded faults</p>
    ${rows.map(r => `<section class="shot"><img src="${r.img}" alt=""><div><h2><span class="n">${r.n}</span>${esc(r.id)}<span class="span">${r.span}</span></h2><dl>
      ${r.said ? `<dt>Narration</dt><dd>${esc(r.said)}</dd>` : ''}${r.cap ? `<dt>On screen</dt><dd>${esc(r.cap)}</dd>` : ''}<dt>Source</dt><dd>${esc(r.source)}</dd></dl></div></section>`).join('')}</html>`;
  await writeFile(join(OUT, 'storyboard.html'), html);
  const page = await browser.newPage({ viewport: { width: 1280, height: 800 } });
  await page.setContent(html);
  await page.screenshot({ path: join(OUT, 'storyboard.png'), fullPage: true });
  await page.pdf({ path: join(OUT, 'storyboard.pdf'), format: 'A4', landscape: true, printBackground: true });
  await page.close();
  console.log('wrote storyboard');
}

const [cmd = 'demo', ...rest] = process.argv.slice(2);
const ars = rest.length ? rest : Object.keys(FORMATS);
if (cmd === 'sheets') sheets();
else {
  const browser = await chromium.launch();
  try {
    if (cmd === 'stills') await stills(browser, ars);
    else if (cmd === 'storyboard') await storyboard(browser);
    else if (CUTS[cmd]) for (const ar of ars) await buildCut(browser, cmd, ar);
    else throw new Error(`unknown command ${cmd}`);
  } finally {
    await browser.close();
  }
}
