"use client";

import { useState } from "react";
import { search, sourceFileUrl, type SearchResult } from "@/lib/api";

type Phase = "idle" | "searching" | "done" | "error";

export function SearchPanel() {
  const [query, setQuery] = useState("");
  const [results, setResults] = useState<SearchResult[]>([]);
  const [phase, setPhase] = useState<Phase>("idle");
  const [error, setError] = useState("");
  const [lastQuery, setLastQuery] = useState("");

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    const trimmed = query.trim();
    if (!trimmed) return;

    setPhase("searching");
    setError("");
    try {
      const response = await search(trimmed, 5);
      setResults(response.results);
      setLastQuery(response.query);
      setPhase("done");
    } catch (err) {
      setPhase("error");
      setError(err instanceof Error ? err.message : "Search failed.");
    }
  };

  return (
    <section aria-labelledby="search-heading" className="space-y-4">
      <h2
        id="search-heading"
        className="font-mono text-xs uppercase tracking-[0.2em] text-neutral-500"
      >
        Search
      </h2>

      <form onSubmit={submit} className="flex gap-2">
        <input
          type="search"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Ask in your own words — meaning is matched, not keywords"
          aria-label="Search query"
          className="flex-1 rounded-md border border-neutral-300 bg-transparent px-3 py-2 text-sm
                     outline-none transition-colors placeholder:text-neutral-500
                     focus:border-neutral-500 dark:border-neutral-700"
        />
        <button
          type="submit"
          disabled={phase === "searching" || !query.trim()}
          className="rounded-md bg-neutral-900 px-4 py-2 text-sm font-medium text-white
                     transition-opacity hover:opacity-80 disabled:opacity-40
                     dark:bg-neutral-100 dark:text-neutral-900"
        >
          {phase === "searching" ? "Searching…" : "Search"}
        </button>
      </form>

      {phase === "error" && (
        <p role="alert" className="text-sm text-red-500">
          {error}
        </p>
      )}

      {phase === "done" && results.length === 0 && (
        <p className="text-sm text-neutral-500">
          No passages matched “{lastQuery}”. Try ingesting a source first.
        </p>
      )}

      {results.length > 0 && (
        <ol className="space-y-3">
          {results.map((result) => (
            <ResultCard key={result.chunk_id} result={result} />
          ))}
        </ol>
      )}
    </section>
  );
}

function ResultCard({ result }: { result: SearchResult }) {
  // Cosine similarity for normalized embeddings runs 1 (identical) to -1.
  // Anything below ~0.5 is usually a weak match worth signalling.
  const strong = result.similarity >= 0.6;

  return (
    <li className="rounded-md border border-neutral-200 p-4 transition-colors hover:border-neutral-400 dark:border-neutral-800 dark:hover:border-neutral-600">
      <div className="mb-2 flex items-baseline justify-between gap-3">
        <div className="min-w-0">
          <p className="truncate text-sm font-medium">{result.source_title}</p>
          {result.heading_path && (
            <p className="truncate font-mono text-xs text-neutral-500">
              {result.heading_path}
            </p>
          )}
        </div>
        <span
          className={`shrink-0 font-mono text-xs ${
            strong ? "text-emerald-500" : "text-amber-500"
          }`}
          title="Cosine similarity to your query"
        >
          {result.similarity.toFixed(3)}
        </span>
      </div>

      <p className="text-sm leading-relaxed text-neutral-700 dark:text-neutral-300">
        {truncate(result.content, 400)}
      </p>

      <ResultSourceLink result={result} />
    </li>
  );
}

/**
 * Links a result back to where it came from — the live page for web sources,
 * the stored original for uploads.
 *
 * Search results don't carry a has_file flag, so this offers the link for any
 * PDF source. Sources ingested before originals were retained will 404, which
 * the endpoint reports cleanly.
 */
function ResultSourceLink({ result }: { result: SearchResult }) {
  const isWebLink = result.source_url?.startsWith("http");
  const isStoredFile = result.source_type === "pdf";

  if (!isWebLink && !isStoredFile) {
    return null;
  }

  const href = isWebLink ? result.source_url! : sourceFileUrl(result.source_id);

  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className="mt-2 inline-block font-mono text-xs text-neutral-500 underline underline-offset-2 hover:text-neutral-900 dark:hover:text-neutral-100"
    >
      {isWebLink ? "source ↗" : "open original ↗"}
    </a>
  );
}

function truncate(text: string, limit: number): string {
  const collapsed = text.replace(/\s+/g, " ").trim();
  return collapsed.length > limit ? `${collapsed.slice(0, limit)}…` : collapsed;
}
