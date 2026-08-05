import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'path'
import http from 'node:http'

// Keep-alive agent for the /v1 proxy. Without it, http-proxy opens a fresh
// (non-pooled) connection to the backend per request and stamps the CLIENT
// response with `Connection: close` — so the browser can't pool API
// connections at all (needless connection churn). With a keep-alive agent the
// proxy reuses backend sockets and preserves keep-alive end-to-end.
const backendKeepAlive = new http.Agent({ keepAlive: true, maxSockets: 64 })

// THE dev-hang fix (verified). Node's default Server.keepAliveTimeout is 5s: it
// closes an idle keep-alive connection after 5s, but browsers hold pooled
// connections for minutes. So after sitting on a page >5s (e.g. filling the
// login form) the browser reuses a socket Vite already closed → the request is
// sent into a dead connection and HANGS with no response (or ECONNRESET) until
// a long browser timeout. Reproduced directly: keep-alive reuse-after-6s-idle →
// ECONNRESET at the 5s default, OK with keepAliveTimeout=0. This was the actual
// cause of the recurring "pending forever" — NOT the network stack, dep
// optimization, or transforms (Vite sits at 0.1% CPU while it happens).
//
// 0 disables the idle close so dev connections stay alive as long as the
// browser wants them; headersTimeout=0 disables the companion guard so it can't
// trip either. Both are safe for a local single-user dev server.
function keepConnectionsAlive() {
  return {
    name: 'velox-dev-keepalive',
    configureServer(server: { httpServer: http.Server | null }) {
      if (server.httpServer) {
        server.httpServer.keepAliveTimeout = 0
        server.httpServer.headersTimeout = 0
      }
    },
  }
}

export default defineConfig({
  plugins: [react(), tailwindcss(), keepConnectionsAlive()],
  resolve: {
    alias: { '@': path.resolve(__dirname, './src') },
  },
  server: {
    // Bind all interfaces so `localhost`, 127.0.0.1, ::1, and the LAN IP all
    // reach the dev server (the default `localhost`-only bind resolves to an
    // IPv6-only socket on macOS, which a browser hitting 127.0.0.1 can't
    // reach). Access the dashboard at http://localhost:5173.
    host: true,
    port: Number(process.env.VELOX_DEV_PORT ?? 5173),
    proxy: {
      // Bounded + error-handled so a stalled/reset upstream surfaces as a 502
      // instead of leaving the browser request "pending" forever. timeout/
      // proxyTimeout sit just above the backend's 30s WriteTimeout so the
      // backend's own response wins when it's merely slow; the proxy only fires
      // on a genuinely dead/wedged upstream.
      '/v1': {
        target: process.env.VELOX_API_TARGET ?? 'http://localhost:8080',
        changeOrigin: true,
        agent: backendKeepAlive,
        timeout: 35_000,
        proxyTimeout: 35_000,
        configure: (proxy) => {
          proxy.on('error', (err, _req, res) => {
            // res is a ServerResponse for normal requests (not WS upgrades).
            const sr = res as unknown as {
              writeHead?: (s: number, h: Record<string, string>) => void
              end?: (b?: string) => void
              headersSent?: boolean
            }
            if (sr && typeof sr.writeHead === 'function' && !sr.headersSent) {
              sr.writeHead(502, { 'Content-Type': 'application/json' })
              sr.end?.(JSON.stringify({ error: { message: `dev proxy: upstream error or timeout (${(err as NodeJS.ErrnoException).code ?? err.message})` } }))
            }
          })
        },
      },
    },
  },
})
