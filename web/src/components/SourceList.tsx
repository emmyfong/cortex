"use client";

import { deleteSource, sourceFileUrl, type Source } from "@/lib/api";
import { useState } from "react";

/**
 * Links to the original document: the stored PDF for uploads, the live page for
 * web sources. Renders nothing when neither exists.
 */
function SourceOriginalLink({ source }: { source: Source }) {
  const isWebLink = source.url_or_path?.startsWith("http");

  if (!source.has_file && !isWebLink) {
    return null;
  }

  const href = source.has_file ? sourceFileUrl(source.id) : source.url_or_path!;
  const label = source.has_file ? "original" : "visit";

  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      aria-label={`Open the original of ${source.title}`}
      className="shrink-0 rounded px-2 py-1 font-mono text-xs text-neutral-500
                 transition-colors hover:bg-neutral-500/10 hover:text-neutral-900
                 dark:hover:text-neutral-100"
    >
      {label} ↗
    </a>
  );
}

interface SourceListProps {
  sources: Source[];
  onChanged: () => void;
}

const STATUS_COLORS: Record<string, string> = {
  ready: "text-emerald-500",
  processing: "text-amber-500",
  pending: "text-neutral-500",
  failed: "text-red-500",
};

export function SourceList({ sources, onChanged }: SourceListProps) {
  const [removing, setRemoving] = useState<string | null>(null);

  const remove = async (id: string) => {
    setRemoving(id);
    try {
      await deleteSource(id);
      onChanged();
    } finally {
      setRemoving(null);
    }
  };

  return (
    <section aria-labelledby="sources-heading" className="space-y-4">
      <h2
        id="sources-heading"
        className="font-mono text-xs uppercase tracking-[0.2em] text-neutral-500"
      >
        Sources ({sources.length})
      </h2>

      {sources.length === 0 ? (
        <p className="text-sm text-neutral-500">
          Nothing ingested yet. Add a URL or PDF above.
        </p>
      ) : (
        <ul className="divide-y divide-neutral-200 dark:divide-neutral-800">
          {sources.map((source) => (
            <li key={source.id} className="flex items-center gap-3 py-3">
              <span className="w-10 shrink-0 font-mono text-xs uppercase text-neutral-500">
                {source.source_type}
              </span>

              <div className="min-w-0 flex-1">
                <p className="truncate text-sm">{source.title}</p>
                <p className="truncate font-mono text-xs text-neutral-500">
                  {source.url_or_path?.startsWith("http")
                    ? source.url_or_path
                    : source.original_filename ?? ""}
                </p>
              </div>

              <SourceOriginalLink source={source} />

              <span
                className={`shrink-0 font-mono text-xs ${
                  STATUS_COLORS[source.status] ?? "text-neutral-500"
                }`}
              >
                {source.status}
              </span>

              <button
                type="button"
                onClick={() => remove(source.id)}
                disabled={removing === source.id}
                aria-label={`Delete ${source.title}`}
                className="shrink-0 rounded px-2 py-1 font-mono text-xs text-neutral-500
                           transition-colors hover:bg-red-500/10 hover:text-red-500 disabled:opacity-40"
              >
                {removing === source.id ? "…" : "delete"}
              </button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
