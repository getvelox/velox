# ADR-NNN: Short title that states the decision

**Date:** YYYY-MM-DD
**Status:** Proposed | Accepted | Superseded by ADR-XXX | Deprecated

<!--
Status field uses one of:
- "Proposed": under discussion, not yet committed to.
- "Accepted": decision in effect.
- "Accepted (amended YYYY-MM-DD)": the ADR has an amendment section added
  later. Keep the reason in the amendment, not in the status line.
- "Superseded by ADR-XXX": a later ADR replaces this decision. Keep the
  file; the history is the point.
- "Deprecated": no longer in effect but not replaced by a new ADR.

Date is when the ADR was first written. Amendments go at the end of the
file under "## Amendment YYYY-MM-DD", and the Summary is updated to the
current rule.

Write for a reader who has not seen the code or the discussion. See
docs/dev/writing-style.md.
-->

## Summary

Two to five plain sentences: what was decided, why, and what it means
for the code or the operator. A reader who stops here should know the
current rule. Keep it current when the ADR is amended.

## Context

What changed in the world, the codebase, or the operator experience
that triggered this decision? Quote concrete signals: a bug report,
a verified cross-platform pattern, a customer ask. Don't invent
abstract justifications.

If the decision is anchored on industry shape ("Stripe parity", "best
practice"), this section must quote verified source lines from at
least 2-4 reference platforms. One platform is an anecdote, not
research. Verify the sources before writing.

## Decision

The decision itself, in one or two paragraphs. State what's done, not
why (that's the next section). Be precise: name the affected
interfaces, tables, files, migrations.

## Why this design

Why the chosen approach over alternatives. Name the principle it
applies in plain words, so a reader without project history can
follow it.

## Alternatives considered

For each alternative seriously discussed:
- **A. <Name>:** one-paragraph description, then the rejection reason.

Discard fake alternatives ("do nothing", "rewrite everything"). The
list should show that the chosen design was non-obvious.

## Consequences

### Positive
- What gets better, named concretely.

### Risks / open items
- What gets harder, what we're trading off, what's deferred and why.
- Schema migration risks (data loss, downtime).
- Operator-UX surprises (e.g. semantic changes to existing fields).
- Follow-up work that's NOT in this ADR's scope.

## References

- Related ADRs (cite by number)
- Migration numbers (`migration 00XX`)
- External docs / source lines (markdown links, NOT bare URLs)
