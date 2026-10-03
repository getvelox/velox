# API key types

This page lists the three kinds of API key, how Velox stores and rotates them,
and how to add a tenant.

| Type        | Prefix            | What it can do                                           |
|-------------|-------------------|----------------------------------------------------------|
| Platform    | `vlx_platform_`   | Tenant management only — not issuable in-product         |
| Secret      | `vlx_secret_`     | Full tenant access (server-side)                         |
| Publishable | `vlx_pub_`        | Authenticate-only — no tenant data access (browser-safe) |

Use a Secret key from your server. A Publishable key is safe to put in a
browser: it can authenticate, but it cannot read or write tenant data.

New keys also carry a mode after the type prefix, such as `vlx_secret_test_` or `vlx_secret_live_`. The key's mode decides whether a request works on test-mode or live-mode data.

API keys are salted-SHA-256 hashed at rest. Rotation supports an optional
grace window, so in-flight requests keep authenticating while the old key
winds down. By default there is no window: the old key stops working
immediately. If you set a window, it can be up to 7 days.

Platform keys are deliberately not issuable in-product. A principal scoped to
one tenant must not be able to mint one for itself and so gain access to every
tenant. As a result, `/v1/tenants` is unreachable in a stock deployment.

**The supported way to add a tenant is the bootstrap CLI.** Re-run it with a
different owner email:

```bash
make bootstrap VELOX_BOOTSTRAP_EMAIL=tenant-b@local \
  VELOX_BOOTSTRAP_PASSWORD='choose-a-password' \
  VELOX_BOOTSTRAP_TENANT='Tenant B'
```
