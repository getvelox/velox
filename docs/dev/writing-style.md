# Writing style for Velox docs

How to write docs in this repo so they are easy to read and still precise.
It applies to every Markdown file: README, guides, runbooks, ADRs, design
docs, CHANGELOG, MANUAL_TEST and the files AI agents read (CLAUDE.md,
playbooks). Use it when you write a new doc and when you touch an old one.

## Who reads our docs

All our readers are technical. Many read English as a second language, and
most are short of time.

| Reader | What they need | Main docs |
|---|---|---|
| Evaluator: an engineer or founder deciding if Velox fits, usually not a billing expert | What it is, who it is for, how to try it, in 5 minutes | README, self-host, architecture |
| Integrator: a developer wiring a product to the API | Exact requests, status codes, copy-paste examples | API docs, webhooks, integrations |
| Operator: an SRE running Velox, sometimes during an incident | Symptom, then command, then how to check the fix | self-host, deploy, `docs/ops/` |
| Contributor: an engineer making a first change | Where the code is, the rules, and why | CONTRIBUTING, architecture, ADRs |
| Reviewer: a senior engineer skimming for 2 minutes | The result, the evidence, the reasoning | ENGINEERING.md, benchmarks |
| AI agent working in the repo | Rules it can extract fast and follow exactly | CLAUDE.md, playbooks |
| Maintainer, months later | A decision or a change, found fast | ADRs, CHANGELOG, MANUAL_TEST |

The effort a reader spends should go into the ideas, never into untangling
the sentences.

## Keep

- **Precise technical terms.** Idempotency, at-least-once, RLS, fencing
  token, p99, DLQ. Never replace a precise term with a vague one. Define it
  instead (see rule 3).
- **Every number, command, code block, link and table value.**
- **The reasoning.** Engineers come for the why. Keep it; put it after the
  what.
- **The limits.** "What this does not show" sections, negative controls and
  withdrawn claims build trust. Keep them, in one line where one line is
  enough.

## Change

1. **Point first.** The first one to three lines of a doc or section say
   what it is, who it is for, and what the reader gets. History comes after,
   or behind a link.
2. **One idea per sentence.** Aim for 15 to 22 words on average. A sentence
   over 35 words needs a reason. Most long sentences are chains of dashes,
   semicolons and parentheses: break the chain first.
3. **Define words a cold reader cannot know.** Billing terms (dunning,
   proration, clawback, credit note, prepaid commit) get a short definition
   on first use in reader-facing docs. Project words (lane, parked, catch-up,
   site-set) are defined once in a glossary or replaced with plain words.
   Write the meaning first and keep the term in parentheses, so search and
   experts still find it.
4. **Keep history out of the reader's path.** PR numbers, incident stories
   and ADR numbers used as the *reason* for a rule go behind a link: "See
   ADR-041", not "per ADR-041, which removed the fallback in #312".
<!-- vale off -->
5. **Write global English.** No idioms ("bolted on", "gold-plate",
   "hairball"). No stacked "not X, but Y" reversals. A careful reader whose
   first language is not English should get each sentence in one pass.
6. **Keep emphasis quiet.** Bold is for scan anchors. Do not shout with
   ALL-CAPS or with bold in running text. Leave out defensive words
   ("honestly", "this is not a lie"); the evidence makes the point.
<!-- vale on -->
7. **Fit the shape to the job.** Tasks are numbered steps. Reference is a
   table. Concepts are short paragraphs, plus a diagram where it helps.
   Decisions follow the ADR template.
8. **No walls.** A paragraph over about 120 words, or a table cell over about
   40, becomes a list, a table or a sub-section.

**The one-pass test.** Read the sentence once, at normal speed, as a new
reader would. If you had to read it twice, or hold a parenthesis in your head
until the end, rewrite it.

## Shapes for common docs

**ADR.** Follow [`docs/adr/TEMPLATE.md`](../adr/TEMPLATE.md). The Summary at
the top says what was decided and why in two to five plain sentences. When a
later change amends the decision, update the Summary's current rule and add
the amendment at the end, dated.

**CHANGELOG entry.** One entry per user-visible change, at most about 50
words:

```markdown
- **Every dunning retry now counts against `max_retry_attempts`.** During a
  leader handover one charge could go uncounted, so a run could exceed its
  retry limit. No charge was doubled. ([#NNN])
```

A bold outcome, then who is affected and what changes for them, then any
action they must take, then a link. The mechanism, the tests and the history
go in the PR description or the ADR.

**MANUAL_TEST box.** The instruction is one observable. Evidence and traps go
in sub-bullets underneath, so the step stays readable:

```markdown
- [~] **Advance dialog blocks a target beyond +1y**: the Advance button is
  disabled and the inline error names the maximum target.
  - Evidence (2026-08-04): frozen Feb 1 2026, target Mar 1 2028 → disabled.
  - Walk trap: commit the date (Enter or blur) before judging.
```

**Runbook section.** Symptom, Why, Check (commands), Fix, Verify. A procedure
never lives inside a table cell.

**Results doc (benchmark, drill).** Open with the result and its conditions
in one short paragraph. Then the setup, the runs, what it does not show, and
any corrections. The story of how the result was reached goes in an appendix.

## Checks

- **[Vale](https://vale.sh)** lints prose against rules 2, 5 and 6
  (`.vale.ini`, rules in `.vale/styles/Velox/`). CI comments only on lines a
  PR adds and never fails the build. Run `vale <file>` locally.
- **[lychee](https://lychee.cli.rs)** checks that every internal link and
  `#anchor` resolves. CI fails on a broken one. Run
  `lychee --offline --include-fragments '**/*.md'` locally.

## Changing existing docs safely

- **Clarity and truth go in separate PRs.** A clarity PR changes no facts. A
  truth PR fixes a doc that disagrees with the code, and re-derives each
  claim from the code.
- **Prove a clarity PR changed no facts.** Compare the numbers, code, code
  blocks and link targets of each changed file against the base branch, and
  explain every difference. Check that no link or `#anchor` broke.
- **Do not rewrite records.** ADR bodies, old CHANGELOG entries and dated
  audit or benchmark records stay as written. Add a summary or a status
  banner on top instead.
