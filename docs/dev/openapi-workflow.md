# OpenAPI Workflow

Velox treats `api/openapi.yaml` as the single source of truth for the
HTTP API. The Go server interface, the TypeScript types, the
`@tanstack/react-query` hooks, and the `/docs/api` Scalar viewer all
regenerate from that one file. CI fails the build if any of those
generated artifacts drift away from what the spec describes.

This is the same shape Stripe, Twilio, GitHub, and Kubernetes use to
keep their SDKs aligned with their HTTP surface.

## Files at a glance

| Path | What it is | Hand-edit? |
| ---- | ---------- | ---------- |
| `api/openapi.yaml` | OpenAPI 3.0.3 source of truth | Yes — this is the contract |
| `api/cfg.yaml` | `oapi-codegen` config (Go) | Rarely |
| `tools/tools.go` | Build-tagged import that pins `oapi-codegen` to `go.mod` | When bumping codegen |
| `internal/api/generated/api.gen.go` | Generated Go types + chi-compat `ServerInterface` | No — regenerate |
| `web-v2/orval.config.ts` | `orval` config (TS hooks) | Rarely |
| `web-v2/scripts/copy-openapi-spec.mjs` | Copies `api/openapi.yaml` into `web-v2/public/openapi.yaml` so the Scalar viewer and codegen read the same file | No |
| `web-v2/src/lib/api.gen.ts` | Generated raw TS types from `openapi-typescript` | No — regenerate |
| `web-v2/src/lib/gen/queries.gen.ts` | Generated react-query hooks from `orval` | No — regenerate |
| `web-v2/src/lib/gen/schemas/` | Generated TS schema types (one file per `components.schemas` entry) | No — regenerate |
| `web-v2/src/lib/orvalClient.ts` | Hand-written `orval` mutator that delegates to the existing `apiRequest` fetch wrapper. Keeps generated hooks on the dashboard's session-cookie auth without duplicating that logic into orval's default Axios client | Yes |

## Adding or changing an endpoint

1. Edit `api/openapi.yaml`. Add the path, the `operationId`, the
   request body schema (under `components.schemas` if reusable, inline
   otherwise), and the response shape — including error responses
   like `404` so the generated TS types narrow on status (callers
   get the right body type per status code).
2. Run `make gen` from the repo root. This runs both Go and TS
   codegen, refreshes `web-v2/public/openapi.yaml`, and rewrites the
   generated files in place.
3. Implement the handler. The Go side: add a method on the relevant
   handler that matches the generated `ServerInterface` signature
   (e.g. `func (h *Handler) GetInvoice(w, r, id string)`). A
   `var _ <interface> = (*Handler)(nil)` assertion (a compile-time
   check that the handler implements the generated interface) turns
   spec-vs-code drift into a build error rather than a runtime 404.
4. Migrate the dashboard. Replace ad-hoc `useQuery({ queryFn: () =>
   api.foo() })` calls with the generated hook (e.g.
   `useGetInvoice(id)`). The hook handles the URL, query keys, and the
   typed response shape; the mutator (`orvalClient` — the function
   every generated request goes through) sends the request through
   `apiRequest` so cookies, error envelope parsing, and
   `Velox-Request-Id` capture all keep working unchanged.
5. Run the local gates: `go build ./...`, `go vet ./...`,
   `go test ./... -short`, `cd web-v2 && npx tsc -b` (NOT `--noEmit`, which is a no-op under this repo's composite project references).
6. Commit the spec change and the regenerated files together. CI runs
   `make gen` and then `git diff --exit-code`; an uncommitted
   regenerated file fails the build.

## Why a build-time copy of the spec into `web-v2/public/`

The Scalar viewer at `/docs/api` serves the spec from a stable
same-origin URL (`/openapi.yaml`). `web-v2/scripts/copy-openapi-spec.mjs`
copies `api/openapi.yaml` into `web-v2/public/openapi.yaml` and is
wired as a `predev` / `prebuild` / `gen` hook in
`web-v2/package.json`. Symlinks would be more elegant but break on
hosts that flatten symlinks during checkout. An explicit copy is
portable, and it shows up in the package.json scripts list as a clear
dependency.

## Why a custom orval mutator

`orval` defaults to building its own Axios client. We delegate to
`apiRequest` (in `web-v2/src/lib/api.ts`) because it already carries
session-cookie auth, parsing of the Stripe-style error envelope (the
`{ error: {…} }` JSON wrapper on failure responses) into
`ApiError`, and the `Velox-Request-Id` header capture into
`setLastRequestId` for "Report an issue" trace handles. Reusing the
wrapper keeps generated hooks on the same auth posture as every
hand-written caller in the dashboard. Stripe-node uses the same
trade-off — the SDK is generated from the spec, the transport is
hand-written and reused.

## Why two TS generators

`openapi-typescript` produces a flat paths-as-keys type tree (one
big type indexed by URL path and method) at
`web-v2/src/lib/api.gen.ts`. It's useful for endpoints we haven't yet
migrated to typed react-query hooks — pages can still pull a precise
type for the payload by indexing into `paths['/v1/foo']['get']`.
`orval` produces the typed hooks layer. The two regenerate from the
same spec on `make gen`; their roles are complementary, not
competing.

## CI gate

`.github/workflows/ci.yml` runs `make gen` and then
`git diff --exit-code`. The first command regenerates everything from
the canonical spec; the second fails if anything in the working tree
changed that wasn't already committed. That means: a spec edit
without regenerated files fails CI, and a code-only edit to the
generated files (someone hand-touching `api.gen.go`) also fails CI.
The rule is simple — generated files must equal what `make gen`
produces from the committed spec.

## Trade-offs and known sharp edges

- `prefer-skip-optional-pointer: true` is set in `api/cfg.yaml` so
  optional fields render as plain types with `omitempty` JSON tags
  rather than `*string`. A `*""` non-nil pointer marshals as `""`,
  which would break wire-shape parity with the existing
  `domain.Invoice` JSON output. The trade-off: callers can't
  distinguish "field absent" from "field present and empty" at the
  type level. For Velox's domain types (where every empty string is
  semantically equivalent to "absent"), that's the right call.
- `output.override.fetch.includeHttpResponseReturnType: false` keeps
  the orval hook return types as the bare body
  (`Promise<InvoiceWithLineItems>`) rather than the
  `{ data, status, headers }` envelope. Our mutator throws on non-2xx
  via `ApiError` and resolves with the body on 2xx; the bare-body
  type matches what the mutator actually returns, and removes a
  `.data` indirection at every call site.
- Generated files live under `web-v2/src/lib/gen/` rather than
  alongside hand-written siblings in `web-v2/src/lib/`. orval's
  `clean: true` would recursively delete the parent of `target`,
  and pointing it at `src/lib/queries.gen.ts` with `clean: true`
  would delete every hand-written file in `src/lib/` on the next
  regen. The `gen/` subdirectory contains the blast radius even if
  `clean` is ever turned back on.
