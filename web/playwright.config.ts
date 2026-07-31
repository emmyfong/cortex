import { defineConfig, devices } from "@playwright/test";

/**
 * E2E runs against the real stack — Go API, worker, Postgres, Redis, Ollama.
 *
 * These tests deliberately do not mock the backend. Everything below the
 * browser already has unit and integration coverage; the gap E2E fills is
 * whether the assembled product works in a real browser, and a mocked API
 * would not answer that.
 *
 * Consequence: they need `npm run dev` (or the equivalent services) running,
 * and they assert on whatever data the corpus currently holds rather than on
 * fixtures. Tests that need specific content ingest it themselves.
 */
export default defineConfig({
  testDir: "./e2e",
  // Full parallelism would have several specs ingesting into one shared
  // database at once. The corpus is global state; treat it as such.
  fullyParallel: false,
  workers: 1,

  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? "github" : "list",

  timeout: 60_000,
  expect: { timeout: 15_000 },

  use: {
    baseURL: process.env.E2E_BASE_URL ?? "http://localhost:3000",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },

  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
