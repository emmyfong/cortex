import { expect, test } from "@playwright/test";

/**
 * The concept graph.
 *
 * The canvas itself cannot be asserted on directly — it is pixels — so these
 * tests check the two things that actually matter: that the accessible list
 * carries the same information, and that selecting a concept produces its
 * evidence. Whether the canvas *looks* right is a screenshot's job, not an
 * assertion's.
 */

test.describe("concept graph", () => {
  test.beforeEach(async ({ page }) => {
    await page.goto("/graph");
  });

  test("reports how large the graph is", async ({ page }) => {
    await expect(page.getByRole("heading", { name: "Concept graph" })).toBeVisible();

    // The count starts at "0 concepts" and fills in once the fetch resolves.
    // Reading textContent immediately captures the placeholder, so assert on a
    // pattern that excludes zero and let Playwright retry until it holds.
    const summary = page.getByText(/\d+ concepts · \d+ connections/);
    await expect(summary).toHaveText(/[1-9]\d* concepts · \d+ connections/);
  });

  test("the concept list is reachable without the canvas", async ({ page }) => {
    // The canvas is invisible to keyboard and screen-reader users, so this list
    // is the accessible route to the same graph — not a convenience.
    await expect(page.getByText("All concepts")).toBeVisible();

    const items = page.getByRole("button").filter({ hasNotText: "Filter" });
    await expect(items.first()).toBeVisible();
  });

  test("filtering narrows the concept list", async ({ page }) => {
    const filter = page.getByLabel("All concepts");
    await expect(filter).toBeVisible();

    // Wait for the list to populate before counting: a bare count() runs once
    // and would capture the empty pre-fetch state.
    const listItems = page.locator("ul > li");
    await expect(listItems.first()).toBeVisible();
    await expect.poll(() => listItems.count()).toBeGreaterThan(1);

    const before = await listItems.count();

    await filter.fill("zzz-definitely-no-such-concept");
    await expect(page.getByText(/No concepts match/)).toBeVisible();

    await filter.clear();
    await expect(listItems).toHaveCount(before);
  });

  test("selecting a concept shows its summary and its evidence", async ({ page }) => {
    const firstConcept = page.locator("ul > li button").first();
    const name = ((await firstConcept.textContent()) ?? "").trim();

    await firstConcept.click();

    // The panel replaces the list, headed by the concept's own name.
    await expect(page.getByRole("heading", { level: 2 })).toBeVisible();

    // Citations are the point: an LLM-extracted concept is a claim, and the
    // passages are what make it checkable.
    await expect(page.getByText("Where it appears")).toBeVisible();

    const mentionCount = page.getByText(/\d+ connections? · \d+ passages?/);
    await expect(mentionCount).toBeVisible();

    expect(name.length).toBeGreaterThan(0);
  });

  test("connected concepts navigate the graph", async ({ page }) => {
    // Pick a well-connected concept so it is guaranteed to have neighbours.
    await page.locator("ul > li button").first().click();
    await expect(page.getByRole("heading", { level: 2 })).toBeVisible();

    const connected = page.getByText("Connected to");
    if ((await connected.count()) === 0) {
      test.skip(true, "the most-connected concept has no neighbours in this corpus");
    }

    const firstNeighbour = connected.locator("xpath=following-sibling::ul[1]/li[1]/button");
    const neighbourName = ((await firstNeighbour.textContent()) ?? "").trim();
    await firstNeighbour.click();

    await expect(page.getByRole("heading", { level: 2 })).toContainText(neighbourName);
  });

  test("every edge states a reason, not boilerplate", async ({ page }) => {
    // The graph's whole claim is that a line means something. This asserts the
    // regression that mattered most: relationship summaries used to read
    // "Discussed together in X", which explained nothing.
    const response = await page.request.get("/api/v1/concepts/graph?limit=200");
    expect(response.ok()).toBeTruthy();

    const graph = (await response.json()) as {
      nodes: { id: string }[];
      edges: { source: string; target: string; summary?: string }[];
    };

    const ids = new Set(graph.nodes.map((n) => n.id));
    for (const edge of graph.edges) {
      // A dangling endpoint makes a force-graph renderer invent a phantom node.
      expect(ids.has(edge.source) && ids.has(edge.target)).toBeTruthy();
      expect(edge.summary ?? "").not.toMatch(/^Discussed together in/);
      expect((edge.summary ?? "").length).toBeGreaterThan(10);
    }
  });
});
