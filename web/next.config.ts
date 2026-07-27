import type { NextConfig } from "next";

// Proxy /api/* to the Go API. Rewrites run server-side, so the browser only
// ever talks to localhost:3000 — no CORS preflight, and the API origin is
// never baked into a client bundle.
const API_ORIGIN = process.env.API_ORIGIN ?? "http://localhost:8080";

const nextConfig: NextConfig = {
  async rewrites() {
    return [
      {
        source: "/api/:path*",
        destination: `${API_ORIGIN}/:path*`,
      },
    ];
  },
};

export default nextConfig;
