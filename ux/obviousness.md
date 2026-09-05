# Obviousness — what corrallm's screens owe the person reading them

The standard a Lenny run scores against (`~/doc/patterns/lenny.md`, canon
LEN-10). `bin/ux` copies the rule list below into every run's `BRIEF.md`, so the
evaluator can cite a rule without reading this file — or say that none fits,
which is worth more than a forced citation and is how this list grows.

**Who these are for.** corrallm's dashboard is read by somebody whose machine has
stopped behaving, usually in a hurry, usually not by the person who configured
it. Every rule below came from that reader, not from a design preference.

**Where findings go.** `plan/` — §6 if something active needs them, §7 if they
name a known gap, `icebox.md` if nothing requires them yet. Never here: this file
is the standard, not the queue.

## The rules a finding can cite

- **OB-1** Every control says what it will do, before it is pressed.
- **OB-2** A control that touches live traffic does not look like one that does
  not. Unloading, evicting, pausing and cancelling reach requests somebody is
  waiting on, and the consequence goes ON the control, or once above the group.
- **OB-3** The screen speaks the operator's words. `placement`, `lane`,
  `fairshare`, `degrade`, `residency`, `procKey` are the gloss, never the label.
- **OB-4** A number carries its unit and its window. "812" is not a duration and
  "43%" is not a period.
- **OB-5** The state of the box is one sentence, in plain words, on arrival:
  serving, struggling, or stopped.
- **OB-6** A fault names whose it is and what to do next. A red thing the reader
  cannot act on is not shown as one.
- **OB-7** A machine identifier is never the most prominent thing on a card —
  not a hash, not a process key, not a UUID.
- **OB-8** No screen is a dead end: it names the next thing and can start it.
- **OB-9** Live and historical are never drawn the same way, and a view that has
  stopped updating says so.
- **OB-12** A property that reaches live traffic explains itself, the same way a
  control that reaches it does. A word in a table cell can cost somebody their
  request — `interruptible: yes`, `weight: 1`, a quality rank — and the reader has
  no button to hesitate over, so nothing makes the cost visible. Asked for by Lenny
  run 5, on finding his own team's key in the cheapest, interruptible group with
  nothing on the page saying what either word does.
- **OB-11** The answer comes before the explanation. A screen that holds what somebody
  came for puts it above the material that explains, justifies or configures it — a
  person who stops reading at the first paragraph in a vocabulary they do not have never
  reaches it. Asked for by Lenny run 4, on a page whose one useful sentence sat below
  four paragraphs of implementation notes.
- **OB-10** Nothing exists only on hover. A label that appears when a pointer
  rests on an icon was never written.
