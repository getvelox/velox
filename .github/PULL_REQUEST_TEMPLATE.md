<!-- Thanks for contributing! The checklist mirrors what CI enforces plus
     the two house rules CI can't check. Delete lines that don't apply. -->

## What & why

## Checklist

- [ ] Tests cover the new behavior (unit; integration too if a store or money path changed)
- [ ] `go test ./... -race -short -count=1` passes
- [ ] `make lint` passes (golangci-lint; see CONTRIBUTING's "What CI runs on your PR" for the full gate list)
- [ ] `CHANGELOG.md` entry added — required for any user-visible change
- [ ] `MANUAL_TEST.md` flow added/updated — required if UI-visible behavior changed
- [ ] `make gen` re-run — required if `api/openapi.yaml` changed
- [ ] Frontend touched? `cd web-v2 && npm run build && npm test && npm run lint` passes

## Money path only

<!-- Invoices, payments, credits, dunning, subscriptions, or tax?
     Per docs/dev/money-path-robustness-playbook.md §2, list the state's
     site-set here: every writer, effect-firer, gated reader, and crash
     point your change touches. Delete this section otherwise. -->
