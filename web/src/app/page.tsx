"use client";

import { useCallback, useEffect, useState } from "react";
import {
  fetchReadiness,
  type DependencyStatus,
  type ReadyResponse,
} from "@/lib/health";

const POLL_INTERVAL_MS = 5000;

// Describes what each dependency is for, so the page is diagnostic rather than
// a row of opaque green dots.
const DEPENDENCY_ROLES: Record<string, string> = {
  postgres: "Schema, chunks, vectors",
  redis: "Ingestion job queue",
  ollama: "Local embedding model",
};

type LoadState = "loading" | "loaded" | "unreachable";

export default function StatusPage() {
  const [readiness, setReadiness] = useState<ReadyResponse | null>(null);
  const [loadState, setLoadState] = useState<LoadState>("loading");
  const [checkedAt, setCheckedAt] = useState<string | null>(null);

  const poll = useCallback(async (signal: AbortSignal) => {
    try {
      const result = await fetchReadiness(signal);
      if (signal.aborted) return;
      setReadiness(result);
      setLoadState("loaded");
      setCheckedAt(new Date().toLocaleTimeString());
    } catch {
      if (signal.aborted) return;
      // The API itself is down — distinct from the API reporting a sick
      // dependency, and the common case when nothing is running yet.
      setReadiness(null);
      setLoadState("unreachable");
      setCheckedAt(new Date().toLocaleTimeString());
    }
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;

    // Each poll schedules the next only after it settles, so a slow or hanging
    // request cannot stack overlapping fetches the way setInterval would.
    // The initial tick is deferred rather than called inline: setState during
    // an effect body triggers a cascading render.
    const tick = async () => {
      await poll(controller.signal);
      if (!controller.signal.aborted) {
        timer = setTimeout(tick, POLL_INTERVAL_MS);
      }
    };
    timer = setTimeout(tick, 0);

    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [poll]);

  return (
    <main className="mx-auto flex w-full max-w-3xl flex-1 flex-col justify-center px-6 py-16">
      <header className="mb-12">
        <p className="font-mono text-xs uppercase tracking-[0.25em] text-neutral-500">
          Milestone 1 &middot; Infrastructure
        </p>
        <h1 className="mt-3 text-5xl font-semibold tracking-tight">Cortex</h1>
        <p className="mt-3 max-w-prose text-neutral-500">
          Local-first knowledge graph and RAG system. The ingestion pipeline
          lands in Milestone 2 &mdash; this page confirms the foundation is
          wired up.
        </p>
      </header>

      <section aria-labelledby="deps-heading">
        <div className="mb-4 flex items-baseline justify-between border-b border-neutral-200 pb-3 dark:border-neutral-800">
          <h2
            id="deps-heading"
            className="font-mono text-sm uppercase tracking-widest"
          >
            Dependencies
          </h2>
          <OverallBadge loadState={loadState} readiness={readiness} />
        </div>

        <DependencyList loadState={loadState} readiness={readiness} />

        <p className="mt-6 font-mono text-xs text-neutral-500">
          {checkedAt ? `Last checked ${checkedAt}` : "Checking…"}
          {" · "}
          polling every {POLL_INTERVAL_MS / 1000}s
        </p>
      </section>
    </main>
  );
}

function OverallBadge({
  loadState,
  readiness,
}: {
  loadState: LoadState;
  readiness: ReadyResponse | null;
}) {
  if (loadState === "loading") {
    return <span className="font-mono text-sm text-neutral-500">checking</span>;
  }
  if (loadState === "unreachable" || !readiness) {
    return <span className="font-mono text-sm text-red-500">api offline</span>;
  }
  return (
    <span
      className={`font-mono text-sm ${
        readiness.status === "ok" ? "text-emerald-500" : "text-amber-500"
      }`}
    >
      {readiness.status}
    </span>
  );
}

function DependencyList({
  loadState,
  readiness,
}: {
  loadState: LoadState;
  readiness: ReadyResponse | null;
}) {
  if (loadState === "unreachable") {
    return (
      <div className="rounded-lg border border-red-500/30 bg-red-500/5 p-5">
        <p className="font-medium text-red-500">Cannot reach the API</p>
        <p className="mt-2 text-sm text-neutral-500">
          Start it with{" "}
          <code className="rounded bg-neutral-500/15 px-1.5 py-0.5 font-mono text-xs">
            npm run dev
          </code>{" "}
          from the repository root.
        </p>
      </div>
    );
  }

  const names = readiness
    ? Object.keys(readiness.dependencies).sort()
    : Object.keys(DEPENDENCY_ROLES);

  return (
    <ul className="divide-y divide-neutral-200 dark:divide-neutral-800">
      {names.map((name) => (
        <DependencyRow
          key={name}
          name={name}
          dependency={readiness?.dependencies[name]}
          isLoading={loadState === "loading"}
        />
      ))}
    </ul>
  );
}

function DependencyRow({
  name,
  dependency,
  isLoading,
}: {
  name: string;
  dependency: DependencyStatus | undefined;
  isLoading: boolean;
}) {
  const isPending = isLoading || !dependency;
  const isOk = dependency?.status === "ok";

  return (
    <li className="flex items-center gap-4 py-4">
      <span
        aria-hidden="true"
        className={`size-2.5 shrink-0 rounded-full ${
          isPending ? "bg-neutral-400" : isOk ? "bg-emerald-500" : "bg-red-500"
        }`}
      />
      <div className="min-w-0 flex-1">
        <p className="font-mono text-sm font-medium">{name}</p>
        <p className="truncate text-xs text-neutral-500">
          {DEPENDENCY_ROLES[name] ?? "—"}
        </p>
      </div>
      <span
        className={`font-mono text-xs ${
          isPending
            ? "text-neutral-500"
            : isOk
              ? "text-emerald-500"
              : "text-red-500"
        }`}
      >
        {isPending ? "checking" : dependency.status}
        {dependency?.detail ? ` · ${dependency.detail}` : ""}
      </span>
    </li>
  );
}
