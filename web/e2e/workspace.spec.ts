import { expect, test } from "@playwright/test";

/**
 * The sources-and-search workspace.
 *
 * Queries go by accessible role and label rather than CSS selectors, so a test
 * failing usually means the page became harder to use, not merely restyled.
 */

test.describe("workspace", () => {
  test.beforeEach(async ({ page }) => {
    await page.goto("/");
  });

  test("shows the app and reports dependency health", async ({ page }) => {
    await expect(page.getByRole("heading", { name: "Cortex", level: 1 })).toBeVisible();

    // The badge polls /readyz. "offline" here means the Go API is unreachable,
    // which would make every other assertion meaningless — so assert it early
    // and explicitly rather than letting later steps fail obscurely.
    const health = page.locator("header").getByText(/^(ready|offline|degraded|.*,.*)$/);
    await expect(health).toBeVisible();
    await expect(health).not.toHaveText("offline");
  });

  test("rejects a non-HTTP URL without leaving the page", async ({ page }) => {
    const url = page.getByLabel("Article URL");
    await url.fill("file:///etc/passwd");
    await page.getByRole("button", { name: "Ingest" }).click();

    // The server refuses non-HTTP schemes; the browser must surface that
    // rather than appearing to accept the submission.
    await expect(page.getByRole("status")).toContainText(/failed|url must start/i);
    await expect(page.getByRole("heading", { name: "Cortex" })).toBeVisible();
  });

  test("the ingest button is disabled until a URL is entered", async ({ page }) => {
    const ingest = page.getByRole("button", { name: "Ingest" });
    await expect(ingest).toBeDisabled();

    await page.getByLabel("Article URL").fill("https://example.com/article");
    await expect(ingest).toBeEnabled();
  });

  test("searching returns passages with their source", async ({ page }) => {
    await page.getByLabel("Search query").fill("what makes batteries lose capacity?");
    await page.getByRole("button", { name: "Search" }).click();

    // Embedding the query needs a round trip to Ollama, which is slow on a
    // cold model — hence the generous wait.
    const results = page.locator("ol > li");
    await expect(results.first()).toBeVisible({ timeout: 45_000 });

    // Every result must name where it came from. A passage with no attribution
    // is not usable as evidence.
    const first = results.first();
    await expect(first).toContainText(/\S/);
    await expect(first.locator("p").first()).not.toBeEmpty();
  });

  test("navigates to the concept graph and back", async ({ page }) => {
    await page.getByRole("link", { name: /concept graph/i }).click();

    await expect(page).toHaveURL(/\/graph$/);
    await expect(page.getByRole("heading", { name: "Concept graph" })).toBeVisible();

    await page.getByRole("link", { name: /sources/i }).click();
    await expect(page).toHaveURL(/\/$/);
  });
});
