"use client";

import { useCallback, useEffect, useState } from "react";
import { IngestPanel } from "@/components/IngestPanel";
import { SearchPanel } from "@/components/SearchPanel";
import { SourceList } from "@/components/SourceList";
import { listSources, type Source } from "@/lib/api";
import { fetchReadiness, type ReadyResponse } from "@/lib/health";

export default function Workspace() {
  const [sources, setSources] = useState<Source[]>([]);
  const [readiness, setReadiness] = useState<ReadyResponse | null>(null);
  const [apiReachable, setApiReachable] = useState(true);

  const refreshSources = useCallback(async () => {
    try {
      const response = await listSources();
      setSources(response.sources ?? []);
      setApiReachable(true);
    } catch {
      setApiReachable(false);
    }
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;

    // Deferred so the first setState does not land inside the effect body,
    // and self-scheduling so a slow response cannot stack requests.
    const tick = async () => {
      await refreshSources();
      try {
        setReadiness(await fetchReadiness(controller.signal));
      } catch {
        setReadiness(null);
      }
      if (!controller.signal.aborted) {
        timer = setTimeout(tick, 10000);
      }
    };
    timer = setTimeout(tick, 0);

    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [refreshSources]);

  return (
    <main className="mx-auto w-full max-w-3xl flex-1 px-6 py-12">
      <header className="mb-10 flex items-end justify-between gap-4 border-b border-neutral-200 pb-6 dark:border-neutral-800">
        <div>
          <h1 className="text-3xl font-semibold tracking-tight">Cortex</h1>
          <p className="mt-1 text-sm text-neutral-500">
            Local-first knowledge graph and semantic search
          </p>
        </div>
        <HealthBadge readiness={readiness} apiReachable={apiReachable} />
      </header>

      {!apiReachable && (
        <div
          role="alert"
          className="mb-8 rounded-md border border-red-500/30 bg-red-500/5 p-4 text-sm"
        >
          <p className="font-medium text-red-500">Cannot reach the API</p>
          <p className="mt-1 text-neutral-500">
            Run{" "}
            <code className="rounded bg-neutral-500/15 px-1.5 py-0.5 font-mono text-xs">
              npm run dev
            </code>{" "}
            from the repository root.
          </p>
        </div>
      )}

      <div className="space-y-12">
        <IngestPanel onIngested={refreshSources} />
        <SearchPanel />
        <SourceList sources={sources} onChanged={refreshSources} />
      </div>
    </main>
  );
}

function HealthBadge({
  readiness,
  apiReachable,
}: {
  readiness: ReadyResponse | null;
  apiReachable: boolean;
}) {
  if (!apiReachable || !readiness) {
    return (
      <span className="flex items-center gap-2 font-mono text-xs text-red-500">
        <span aria-hidden="true" className="size-2 rounded-full bg-red-500" />
        offline
      </span>
    );
  }

  const healthy = readiness.status === "ok";
  const unhealthy = Object.entries(readiness.dependencies)
    .filter(([, dep]) => dep.status !== "ok")
    .map(([name]) => name);

  return (
    <span
      className={`flex items-center gap-2 font-mono text-xs ${
        healthy ? "text-emerald-500" : "text-amber-500"
      }`}
      title={healthy ? "All dependencies healthy" : `Unavailable: ${unhealthy.join(", ")}`}
    >
      <span
        aria-hidden="true"
        className={`size-2 rounded-full ${healthy ? "bg-emerald-500" : "bg-amber-500"}`}
      />
      {healthy ? "ready" : unhealthy.join(", ")}
    </span>
  );
}
