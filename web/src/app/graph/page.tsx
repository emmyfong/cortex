"use client";

import dynamic from "next/dynamic";
import Link from "next/link";
import { useCallback, useEffect, useMemo, useState } from "react";
import { ConceptPanel } from "@/components/ConceptPanel";
import { loadGraph, type ConceptGraph as GraphData, type GraphNode } from "@/lib/api";

// The force graph reaches for window at import time, so it must not render on
// the server.
const ConceptGraph = dynamic(
  () => import("@/components/ConceptGraph").then((m) => m.ConceptGraph),
  {
    ssr: false,
    loading: () => (
      <div className="flex h-full items-center justify-center">
        <p className="font-mono text-xs text-neutral-500">Loading graph…</p>
      </div>
    ),
  },
);

type Phase = "loading" | "ready" | "empty" | "error";

export default function GraphPage() {
  const [graph, setGraph] = useState<GraphData>({ nodes: [], edges: [] });
  const [phase, setPhase] = useState<Phase>("loading");
  const [error, setError] = useState("");
  const [selected, setSelected] = useState<GraphNode | null>(null);
  const [filter, setFilter] = useState("");

  useEffect(() => {
    const controller = new AbortController();

    const timer = setTimeout(async () => {
      try {
        const result = await loadGraph(150);
        if (controller.signal.aborted) return;
        setGraph(result);
        setPhase(result.nodes.length === 0 ? "empty" : "ready");
      } catch (err) {
        if (controller.signal.aborted) return;
        setPhase("error");
        setError(err instanceof Error ? err.message : "Could not load the graph.");
      }
    }, 0);

    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, []);

  const handleSelect = useCallback((node: GraphNode | null) => setSelected(node), []);

  const selectBySlug = useCallback(
    (slug: string) => {
      const node = graph.nodes.find((n) => n.slug === slug);
      if (node) setSelected(node);
    },
    [graph.nodes],
  );

  // The canvas is not reachable by keyboard or screen reader, so this list is
  // the accessible route to the same information, not a decorative extra.
  const listed = useMemo(() => {
    const needle = filter.trim().toLowerCase();
    const matching = needle
      ? graph.nodes.filter((n) => n.name.toLowerCase().includes(needle))
      : graph.nodes;
    return [...matching].sort((a, b) => b.connection_count - a.connection_count);
  }, [filter, graph.nodes]);

  return (
    <main className="flex h-[100dvh] flex-col">
      <header className="flex shrink-0 items-baseline justify-between gap-4 border-b border-neutral-200 px-6 py-4 dark:border-neutral-800">
        <div className="flex items-baseline gap-4">
          <h1 className="text-lg font-semibold tracking-tight">Concept graph</h1>
          <span className="font-mono text-xs text-neutral-500">
            {graph.nodes.length} concepts · {graph.edges.length} connections
          </span>
        </div>
        <Link
          href="/"
          className="font-mono text-xs text-neutral-500 underline underline-offset-4 hover:text-neutral-900 dark:hover:text-neutral-100"
        >
          ← sources &amp; search
        </Link>
      </header>

      {phase === "error" && (
        <div role="alert" className="p-6 text-sm text-red-500">
          {error}
        </div>
      )}

      {phase === "empty" && (
        <div className="flex flex-1 items-center justify-center p-8 text-center">
          <div className="max-w-sm">
            <p className="text-sm font-medium">No concepts yet</p>
            <p className="mt-2 text-sm text-neutral-500">
              Concepts are extracted in the background after a document is ingested.
              Add a source, wait for extraction to finish, then come back.
            </p>
            <Link
              href="/"
              className="mt-4 inline-block rounded-md bg-neutral-900 px-4 py-2 text-sm font-medium text-white dark:bg-neutral-100 dark:text-neutral-900"
            >
              Add a source
            </Link>
          </div>
        </div>
      )}

      {(phase === "ready" || phase === "loading") && (
        <div className="grid min-h-0 flex-1 grid-cols-1 lg:grid-cols-[minmax(0,1fr)_360px]">
          <div className="relative min-h-[320px] border-b border-neutral-200 dark:border-neutral-800 lg:border-b-0 lg:border-r">
            <ConceptGraph
              graph={graph}
              selectedId={selected?.id ?? null}
              onSelect={handleSelect}
            />

            <div className="pointer-events-none absolute bottom-3 left-3 font-mono text-[10px] text-neutral-500">
              drag to pan · scroll to zoom · click a node
            </div>
          </div>

          <div className="flex min-h-0 flex-col">
            {selected ? (
              <ConceptPanel
                key={selected.slug}
                slug={selected.slug}
                onSelectSlug={selectBySlug}
              />
            ) : (
              <div className="flex min-h-0 flex-col p-6">
                <label
                  htmlFor="concept-filter"
                  className="mb-2 font-mono text-xs uppercase tracking-[0.2em] text-neutral-500"
                >
                  All concepts
                </label>
                <input
                  id="concept-filter"
                  type="search"
                  value={filter}
                  onChange={(e) => setFilter(e.target.value)}
                  placeholder="Filter…"
                  className="mb-3 rounded-md border border-neutral-300 bg-transparent px-3 py-1.5 text-sm
                             outline-none transition-colors placeholder:text-neutral-500
                             focus:border-neutral-500 dark:border-neutral-700"
                />
                <ul className="min-h-0 flex-1 divide-y divide-neutral-200 overflow-y-auto dark:divide-neutral-800">
                  {listed.map((node) => (
                    <li key={node.id}>
                      <button
                        type="button"
                        onClick={() => setSelected(node)}
                        className="flex w-full items-baseline justify-between gap-3 py-2 text-left
                                   transition-colors hover:text-neutral-900 dark:hover:text-neutral-100"
                      >
                        <span className="truncate text-sm">{node.name}</span>
                        <span className="shrink-0 font-mono text-xs text-neutral-500">
                          {node.connection_count}
                        </span>
                      </button>
                    </li>
                  ))}
                  {listed.length === 0 && (
                    <li className="py-3 text-sm text-neutral-500">
                      No concepts match “{filter}”.
                    </li>
                  )}
                </ul>
              </div>
            )}
          </div>
        </div>
      )}
    </main>
  );
}
