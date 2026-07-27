import type { NextConfig } from "next";

// Proxy /api/* to the Go API. Rewrites run server-side, so the browser only
// ever talks to localhost:3000 — no CORS preflight, and the API origin is
// never baked into a client bundle.
const API_ORIGIN = process.env.API_ORIGIN ?? "http://localhost:8080";

const nextConfig: NextConfig = {
  async rewrites() {
    return [
      // Paths are preserved rather than rebased, so the browser calls the same
      // path the Go service publishes (/api/v1/search, /readyz).
      {
        source: "/api/:path*",
        destination: `${API_ORIGIN}/api/:path*`,
      },
      {
        source: "/readyz",
        destination: `${API_ORIGIN}/readyz`,
      },
    ];
  },
};

export default nextConfig;
