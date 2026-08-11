# web-v2 — the Velox dashboard

React + TypeScript + Vite single-page app: the operator dashboard (customers,
invoices, subscriptions, pricing, dunning, webhooks) plus the customer-facing
public pages (hosted invoice, payment update, shared cost dashboard).

## Setup

```bash
npm install        # .npmrc pins legacy-peer-deps — a plain install works
npm run dev        # Vite on :5173, proxying /v1 to the Go server on :8080
```

The dev server needs the backend running — from the repo root:
`make dev` (or the two commands in the root README's Quick start).

Node 22+ (CI runs 22; anything below 20.19 breaks Vite).

## The checks CI runs on this directory

```bash
npx tsc -b         # strict typecheck. NOT `tsc --noEmit` — that silently
                   # checks nothing here (composite project references).
npm run lint       # eslint
npm test           # vitest
npm run build      # tsc -b + vite build — the strictest of the four
```

## Generated code — do not hand-edit

`src/lib/gen/**` is generated from `api/openapi.yaml` at the repo root.
Change the spec, then run `make gen` from the root (npm-level generators are
not the same thing); CI's codegen-drift job fails any PR where the spec and
generated artifacts disagree.

## Conventions worth knowing before a first PR

- Relative-time / "now"-dependent UI must resolve "now" via
  `src/hooks/useClockFrozenMap.ts` (test clocks freeze time per entity);
  an eslint rule bans bare `Date.now()` in the affected surfaces.
- Money is formatted through `src/lib/priceDisplay.ts` — exact string math,
  never `parseFloat` on money strings.
- Every user-visible change updates `CHANGELOG.md`, and UI-visible behavior
  changes update the matching flow in `MANUAL_TEST.md` (same PR — see
  CONTRIBUTING.md).
