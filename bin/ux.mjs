// The browser half of bin/ux. Driven by it, never run directly — it reads the
// admin token out of the environment and has no argument parsing of its own.
//
// It captures TWO things per step and no third: a screenshot, and every word that
// was visible plus every control by the label it wore. That pair is the whole
// input the evaluator gets, because it is the whole input the person gets.
// Nothing here inspects state, asserts, or explains — the moment this file starts
// writing down what a screen MEANT, the evaluation is measuring this file instead
// of the product.
import { readFileSync, writeFileSync } from 'node:fs'
import { basename } from 'node:path'
import { pathToFileURL } from 'node:url'

const env = (k) => {
  const v = process.env[k]
  if (!v) {
    console.error(`bin/ux.mjs: run it through bin/ux (${k} is unset)`)
    process.exit(2)
  }
  return v
}

// Imported BY PATH. playwright lives outside this tree on purpose, and ESM does
// not consult NODE_PATH, so bin/ux resolves the entry file and passes it in
// rather than leaving node to fail with ERR_MODULE_NOT_FOUND.
const mod = await import(pathToFileURL(env('CORRALLM_UX_PLAYWRIGHT')).href)
// playwright's entry is CJS, so a dynamic import puts its exports on `.default`
// and a named destructure silently yields undefined.
const chromium = mod.chromium ?? mod.default?.chromium
if (!chromium) {
  console.error('bin/ux.mjs: playwright loaded but exports no chromium')
  process.exit(2)
}

const token = process.env.CORRALLM_UX_TOKEN || ''
const base = env('CORRALLM_UX_BASE')
const model = process.env.CORRALLM_UX_MODEL || ''
const version = process.env.CORRALLM_UX_VERSION || 'unknown'
const out = env('CORRALLM_UX_OUT')
const walk = JSON.parse(readFileSync(env('CORRALLM_UX_WALK'), 'utf8'))
const headed = process.env.CORRALLM_UX_HEADED === '1'
const exe = process.env.CORRALLM_UX_CHROME

// TWO FILES, AND THE SPLIT IS THE METHOD (canon LEN-1). `ux/lenny.md` is the
// CONSTANT — capability, familiarity, temperament — identical in every project
// and never edited to suit a run. `ux/scenarios/<name>.md` is what VARIES: who he
// is this time, his role, and an external goal that is not "use the product".
// Composed here rather than kept in one file, because one file is how a scenario
// quietly makes him more patient and stops two runs being comparable.
//
// FROM THE FIRST `## ` ONWARD, and nothing above it. Each file opens with a
// preamble addressed to whoever maintains it, naming repo paths and the canon
// path — copied into the brief, that preamble hands the evaluator files to go and
// read, which is the one thing the scoping exists to prevent, and it arrives
// wearing the persona's voice so the agent has no reason to distrust it.
const bodyOf = (path, from = '\n## ') => {
  const raw = readFileSync(path, 'utf8')
  const at = raw.indexOf(from)
  if (at < 0) {
    console.error(`bin/ux.mjs: ${path} has no "${from.trim()}" section — nothing to put in the brief`)
    process.exit(2)
  }
  return raw.slice(at + 1).trim()
}
const scenarioPath = `ux/scenarios/${walk.scenario}.md`
const lenny = bodyOf('ux/lenny.md')
const scenario = bodyOf(scenarioPath)
// The rules come from the standard rather than from a copy in here, so growing
// the standard grows what a finding may cite (canon LEN-10).
const rules = bodyOf('ux/obviousness.md', '\n## The rules a finding can cite')

const browser = await chromium.launch({
  headless: !headed,
  ...(exe ? { executablePath: exe } : {}),
})

// A DESKTOP VIEWPORT. The thing being judged is a reading measure, and the
// default 1280x720 crops a dashboard row mid-table.
const viewport = { width: 1440, height: 1000 }
const { hostname } = new URL(base)
// TWO STORES, BOTH REQUIRED, because the dashboard uses both: localStorage for
// the Bearer header on every query, and the cookie for the SSE stream that
// cannot set headers. One without the other loads a signed-in app whose live
// panels are permanently empty — a finding this file would have invented.
const cookie = {
  name: 'corrallm_token',
  value: token,
  domain: hostname,
  path: '/',
  sameSite: 'Strict',
}

/** visibleText is what a person could read, and the controls they could press.
 *  innerText rather than textContent, because innerText honours what CSS hid — a
 *  collapsed panel the person cannot see must not reach the evaluator. Labels
 *  come off the rendered text for the same reason: a chip reading BUSY through
 *  text-transform is what they saw, whatever the source says. */
const capture = (page) =>
  page.evaluate(() => {
    const seen = new Set()
    const controls = []
    for (const el of document.querySelectorAll(
      'button, a[href], [role=button], [role=tab], input, textarea, select',
    )) {
      const r = el.getBoundingClientRect()
      if (!r.width || !r.height) continue
      const style = getComputedStyle(el)
      if (style.visibility === 'hidden' || style.display === 'none') continue
      // A CONTROL WITH NO WORDS ON IT IS RECORDED AS HAVING NONE, and that is
      // the point on this dashboard: the nav is an icon rail whose labels live
      // in tooltips, and Lenny does not hover (canon LEN-9, OB-10). An
      // aria-label read here would put a word on his screen that his screen does
      // not have, so it is captured separately and marked as unread.
      const label = (el.innerText || el.labels?.[0]?.innerText || el.getAttribute('placeholder') || el.value || '')
        .trim()
        .replace(/\s+/g, ' ')
      const hidden = (el.getAttribute('aria-label') || el.getAttribute('title') || '').trim()
      const kind = el.getAttribute('role') || el.tagName.toLowerCase()
      const key = `${kind} ${label} ${hidden}`
      if (seen.has(key)) continue
      seen.add(key)
      controls.push({ kind, label, hidden, disabled: !!el.disabled })
    }
    return { text: document.body.innerText, controls }
  })

const describe = (c) =>
  c.label
    ? `  [${c.kind}] ${c.label}${c.disabled ? '   (greyed out)' : ''}`
    : `  [${c.kind}] (no words on it${c.hidden ? ' — it only says "' + c.hidden + '" if you rest the pointer on it, which you do not' : ''})${c.disabled ? '   (greyed out)' : ''}`

const steps = []
let n = 0
for (const step of walk.steps) {
  n += 1
  const nn = String(n).padStart(2, '0')
  const ctx = await browser.newContext({ viewport })
  if (!step.anon && token) {
    await ctx.addCookies([cookie])
    await ctx.addInitScript((t) => localStorage.setItem('corrallm_token', t), token)
  }
  const page = await ctx.newPage()

  const url = base + (step.goto || '/').replace('$MODEL', encodeURIComponent(model))
  // `domcontentloaded`, NOT `networkidle`. Every signed-in page holds the live
  // event stream open, so the network never goes idle and the wait times out on
  // a page that rendered fine seconds earlier — the run would report a blank
  // screen the person never saw. The fixed settle below is what actually waits
  // for React: the app renders after its queries resolve, and no load event
  // knows about that.
  await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 30000 })
  await page.waitForTimeout(3500).catch(() => {})

  const shot = `${nn}-${step.name}.png`
  await page.screenshot({ path: `${out}/${shot}`, fullPage: true })
  const { text, controls } = await capture(page)

  const txt = `${nn}-${step.name}.txt`
  writeFileSync(
    `${out}/${txt}`,
    [
      `STEP ${nn} — ${step.name}`,
      step.anon ? 'You arrived here with nothing — not signed in.' : 'You arrived here directly, already signed in.',
      '',
      '--- THE CONTROLS YOU CAN SEE, BY THE WORDS ON THEM ---',
      ...controls.map(describe),
      '',
      '--- EVERY WORD VISIBLE ON THE SCREEN ---',
      text,
      '',
    ].join('\n'),
    'utf8',
  )

  steps.push({ nn, step, shot, txt, controls, url })
  console.log(`  ${nn} ${step.name} — ${controls.length} controls`)
  await ctx.close()
}

await browser.close()

// BRIEF.md IS THE EVALUATOR'S WHOLE WORLD. It names every file it may read, so
// "denied the codebase" is a fact about the input rather than an instruction the
// agent is trusted to obey. Nothing in here may explain the product: the persona,
// the goal, the promise each step was meant to keep, and the rubric. If a screen
// needs explaining, that is the finding.
const brief = `# ${walk.name} — what you saw

You are the person described below. You have never seen this product's source
code and cannot look at it. **The only files you may read are the ones listed at
the bottom of this page, all of them in this directory.**

## Your goal, today

${walk.goal}

${walk.note ? `> ${walk.note}\n` : ''}
${lenny}

${scenario}

## What you do with this

For each step below, look at the picture and read the text file. The text file is
every word that was visible on that screen and every control by the words written
on it. Then answer four questions, in your own voice, scored 0–3, **quoting the
words on the screen that earned each score**:

1. **What does this do?** For each control I can see, do I know what will happen
   if I press it?
2. **How do I proceed?** Do I know what to do next, without anyone telling me?
3. **What is being asked of me?** Do I know what this screen wants from me, and
   why it is being asked?
4. **Do I believe something will come of it?** Has anything promised me an
   outcome, and can I see that a previous one was delivered?

| | |
|---|---|
| **3** | I know, from what is on the screen |
| **2** | I worked it out, but I guessed once |
| **1** | I have a guess and I am not confident |
| **0** | I do not know — or I believe something that is false |

A score with no quote from the screen is an opinion. Quote verbatim, including
the capitalisation.

**Then the part only you can supply: what you did NOT press, and why.** For each
step, name any control you left alone, quote the words on it, and say what
stopped you. This is not a footnote — a control you would not touch is the
finding, and nobody watching a screen recording could tell it from a control you
had no reason to touch.

Finish with: **would I come back tomorrow?** Yes or no, and why.

## The rules a finding can cite

Cite one only if it fits. If nothing fits, say so — a rule that does not exist
yet is worth more than a forced citation.

${rules.replace(/^## The rules a finding can cite\n+/, '')}

## The steps, and what each was meant to keep

${steps
  .map(
    (s) =>
      `### ${s.nn} · ${s.step.name}\n\n` +
      `The promise: ${s.step.promise}\n\n` +
      `- picture: \`${s.shot}\`\n- what it said: \`${s.txt}\`\n`,
  )
  .join('\n')}
## Every file you may read

${steps.map((s) => `- \`${s.shot}\`\n- \`${s.txt}\``).join('\n')}

Nothing else. Not a source file, not a document elsewhere on this machine.
`

writeFileSync(`${out}/BRIEF.md`, brief, 'utf8')

// ACTIONS.md — THE RUNNER'S HALF OF THE ACTION LOG (canon LEN-5).
//
// Two halves, and this is the deterministic one: where the walk went, and what
// was on offer at each step that it did not touch. The other half is Lenny's, and
// it carries the signal — WHICH of these he refused and what words stopped him. A
// log of actions alone reports a user who did almost nothing, which is true and
// useless; the button he would not press is the finding, and the button is fine.
//
// It is written OUTSIDE the brief on purpose. The brief is what Lenny may read,
// and a list of every control on every screen, gathered in one place, is a map of
// the product he is supposed to be finding his way around without one.
//
// It also records WHAT WAS PHOTOGRAPHED: this walk runs against a live daemon, so
// the run is not reproducible and a later reader needs to know which box, which
// build, and when.
const actions = `# What the walk did — ${walk.name}

The mechanical half of the action log. Lenny's half — what he REFUSED to press,
and the words that stopped him — comes back in his report.

- base: \`${base}\`  ·  daemon version: \`${version}\`
- walked: ${new Date().toISOString()}
- model named by the walk: \`${model || '(none)'}\`
- read-only: no step pressed anything.

${steps
  .map(
    (s) =>
      `## ${s.nn} · ${s.step.name}\n\n` +
      `- went to: \`${s.url}\`${s.step.anon ? ' (signed out)' : ''}\n` +
      `- controls on offer: ${s.controls.length}\n` +
      s.controls
        .map((c) => `    - ${c.label || `(no words${c.hidden ? `, hover-only: "${c.hidden}"` : ''})`}${c.disabled ? ' (greyed out)' : ''}`)
        .join('\n') +
      '\n',
  )
  .join('\n')}`
writeFileSync(`${out}/ACTIONS.md`, actions, 'utf8')

console.log(`  BRIEF.md + ACTIONS.md — lenny + ${basename(scenarioPath)}, ${steps.length} steps`)
