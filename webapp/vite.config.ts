import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Local dev proxies /v1 and /healthz to the control plane directly
// (localhost:8080, cmd/control-plane's default CONTROL_PLANE_ADDR) — the
// deployed build instead goes through Caddy (deploy/caddy/Caddyfile), which
// does the same proxying in front of the built static files.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/v1": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
    },
  },
});
