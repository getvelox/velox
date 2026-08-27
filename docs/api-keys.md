# API key types

| Type        | Prefix            | What it can do                                           |
|-------------|-------------------|----------------------------------------------------------|
| Platform    | `vlx_platform_`   | Tenant management only — not issuable in-product         |
| Secret      | `vlx_secret_`     | Full tenant access (server-side)                         |
| Publishable | `vlx_pub_`        | Authenticate-only — no tenant data access (browser-safe) |

API keys are salted-SHA-256 hashed at rest; rotation supports an optional
grace window (immediate by default, up to 7 days) so in-flight requests keep
authenticating while the old key winds down.

Platform keys are deliberately not issuable in-product — a single-tenant
principal must not be able to self-mint one and escalate to every tenant —
so `/v1/tenants` is unreachable in a stock deployment. **The supported way
to add a tenant is the bootstrap CLI**, re-run with a different owner email:

```bash
make bootstrap VELOX_BOOTSTRAP_EMAIL=tenant-b@local \
  VELOX_BOOTSTRAP_PASSWORD='choose-a-password' \
  VELOX_BOOTSTRAP_TENANT='Tenant B'
```
