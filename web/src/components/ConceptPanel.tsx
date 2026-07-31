"use client";

import { useEffect, useState } from "react";
import {
  loadConcept,
  sourceFileUrl,
  type ConceptDetail,
  type ConceptMention,
} from "@/lib/api";

interface ConceptPanelProps {
  /** Always set: the parent renders this only when a concept is selected, and
   *  keys it by slug so switching concepts remounts rather than resetting
   *  state from an effect. */
  slug: string;
  onSelectSlug: (slug: string) => void;
}

/**
 * Shows a selected concept with the passages that evidence it.
 *
 * The citations are the point: a concept extracted by a language model is a
 * claim, and the passages are what let you check it.
 */
export function ConceptPanel({ slug, onSelectSlug }: ConceptPanelProps) {
  const [detail, setDetail] = useState<ConceptDetail | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const controller = new AbortController();

    // Deferred so the state update does not land inside the effect body.
    const timer = setTimeout(async () => {
      try {
        const result = await loadConcept(slug);
        if (controller.signal.aborted) return;
        setDetail(result);
        setError("");
      } catch (err) {
        if (controller.signal.aborted) return;
        setDetail(null);
        setError(err instanceof Error ? err.message : "Could not load concept.");
      } finally {
        if (!controller.signal.aborted) setLoading(false);
      }
    }, 0);

    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [slug]);

  if (loading && !detail) {
    return (
      <aside className="p-6">
        <p className="font-mono text-xs text-neutral-500">Loading…</p>
      </aside>
    );
  }

  if (error) {
    return (
      <aside className="p-6">
        <p role="alert" className="text-sm text-red-500">
          {error}
        </p>
      </aside>
    );
  }

  if (!detail) return null;

  return (
    <aside className="h-full overflow-y-auto p-6">
      <header className="mb-5">
        <h2 className="text-xl font-semibold tracking-tight">{detail.concept.name}</h2>
        <p className="mt-1 font-mono text-xs text-neutral-500">
          {detail.concept.connection_count} connection
          {detail.concept.connection_count === 1 ? "" : "s"} · {detail.mentions.length}{" "}
          passage{detail.mentions.length === 1 ? "" : "s"}
        </p>
        <p className="mt-3 text-sm leading-relaxed text-neutral-700 dark:text-neutral-300">
          {detail.concept.summary}
        </p>
      </header>

      {detail.related.length > 0 && (
        <section className="mb-6">
          <h3 className="mb-2 font-mono text-xs uppercase tracking-[0.2em] text-neutral-500">
            Connected to
          </h3>
          <ul className="flex flex-wrap gap-1.5">
            {detail.related.map((related) => (
              <li key={related.id}>
                <button
                  type="button"
                  onClick={() => onSelectSlug(related.slug)}
                  className="rounded-full border border-neutral-300 px-2.5 py-1 text-xs
                             transition-colors hover:border-neutral-500 hover:bg-neutral-500/10
                             dark:border-neutral-700"
                >
                  {related.name}
                </button>
              </li>
            ))}
          </ul>
        </section>
      )}

      <section>
        <h3 className="mb-2 font-mono text-xs uppercase tracking-[0.2em] text-neutral-500">
          Where it appears
        </h3>
        <ul className="space-y-3">
          {detail.mentions.map((mention) => (
            <MentionCard key={mention.chunk_id} mention={mention} />
          ))}
        </ul>
      </section>
    </aside>
  );
}

function MentionCard({ mention }: { mention: ConceptMention }) {
  const isWebLink = mention.source_url?.startsWith("http");
  const href = isWebLink ? mention.source_url! : sourceFileUrl(mention.source_id);
  const canLink = isWebLink || mention.source_type === "pdf";

  return (
    <li className="rounded-md border border-neutral-200 p-3 dark:border-neutral-800">
      <div className="mb-1.5 flex items-baseline justify-between gap-2">
        <p className="truncate text-xs font-medium">{mention.source_title}</p>
        {canLink && (
          <a
            href={href}
            target="_blank"
            rel="noopener noreferrer"
            className="shrink-0 font-mono text-xs text-neutral-500 underline underline-offset-2
                       hover:text-neutral-900 dark:hover:text-neutral-100"
          >
            open ↗
          </a>
        )}
      </div>
      {mention.heading_path && (
        <p className="mb-1.5 truncate font-mono text-xs text-neutral-500">
          {mention.heading_path}
        </p>
      )}
      <p className="text-xs leading-relaxed text-neutral-600 dark:text-neutral-400">
        {excerpt(mention.content, 260)}
      </p>
    </li>
  );
}

function excerpt(text: string, limit: number): string {
  const collapsed = text.replace(/\s+/g, " ").trim();
  return collapsed.length > limit ? `${collapsed.slice(0, limit)}…` : collapsed;
}
