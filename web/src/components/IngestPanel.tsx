"use client";

import { useEffect, useRef, useState } from "react";
import {
  ingestPdf,
  ingestUrl,
  streamJob,
  type JobEvent,
} from "@/lib/api";

interface IngestPanelProps {
  onIngested: () => void;
}

type Phase = "idle" | "submitting" | "running" | "done" | "error";

export function IngestPanel({ onIngested }: IngestPanelProps) {
  const [url, setUrl] = useState("");
  const [phase, setPhase] = useState<Phase>("idle");
  const [stage, setStage] = useState("");
  const [progress, setProgress] = useState(0);
  const [message, setMessage] = useState("");
  const [jobId, setJobId] = useState<string | null>(null);

  const fileInput = useRef<HTMLInputElement>(null);
  const closeStream = useRef<(() => void) | null>(null);

  // Close any open stream when the component goes away, otherwise the
  // connection leaks for the lifetime of the page.
  useEffect(() => {
    return () => closeStream.current?.();
  }, []);

  useEffect(() => {
    if (!jobId) return;

    const handleEvent = (event: JobEvent) => {
      setStage(event.stage ?? "");
      setProgress(event.progress);

      if (event.type === "complete") {
        setPhase("done");
        setMessage(
          event.chunks ? `Indexed ${event.chunks} passages.` : "Ingestion complete.",
        );
        onIngested();
        return;
      }
      if (event.type === "failed") {
        setPhase("error");
        setMessage(event.error || "Ingestion failed.");
      }
    };

    const close = streamJob(jobId, handleEvent, (error) => {
      setPhase("error");
      setMessage(error);
    });
    closeStream.current = close;

    return () => {
      close();
      closeStream.current = null;
    };
  }, [jobId, onIngested]);

  const begin = () => {
    closeStream.current?.();
    setPhase("submitting");
    setMessage("");
    setStage("");
    setProgress(0);
    setJobId(null);
  };

  const fail = (error: unknown) => {
    setPhase("error");
    setMessage(error instanceof Error ? error.message : "Something went wrong.");
  };

  const submitUrl = async (event: React.FormEvent) => {
    event.preventDefault();
    const trimmed = url.trim();
    if (!trimmed) return;

    begin();
    try {
      const response = await ingestUrl(trimmed);
      setPhase("running");
      setJobId(response.job_id);
      setUrl("");
    } catch (error) {
      fail(error);
    }
  };

  const submitFile = async (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    if (!file) return;

    begin();
    try {
      const response = await ingestPdf(file);
      setPhase("running");
      setJobId(response.job_id);
    } catch (error) {
      fail(error);
    } finally {
      // Reset so selecting the same file again still fires a change event.
      if (fileInput.current) fileInput.current.value = "";
    }
  };

  const busy = phase === "submitting" || phase === "running";

  return (
    <section aria-labelledby="ingest-heading" className="space-y-4">
      <h2
        id="ingest-heading"
        className="font-mono text-xs uppercase tracking-[0.2em] text-neutral-500"
      >
        Add a source
      </h2>

      <form onSubmit={submitUrl} className="flex gap-2">
        <input
          type="url"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
          placeholder="https://example.com/article"
          aria-label="Article URL"
          disabled={busy}
          className="flex-1 rounded-md border border-neutral-300 bg-transparent px-3 py-2 text-sm
                     outline-none transition-colors placeholder:text-neutral-500
                     focus:border-neutral-500 disabled:opacity-50 dark:border-neutral-700"
        />
        <button
          type="submit"
          disabled={busy || !url.trim()}
          className="rounded-md bg-neutral-900 px-4 py-2 text-sm font-medium text-white
                     transition-opacity hover:opacity-80 disabled:opacity-40
                     dark:bg-neutral-100 dark:text-neutral-900"
        >
          Ingest
        </button>
      </form>

      <div className="flex items-center gap-3">
        <label
          className={`cursor-pointer rounded-md border border-dashed border-neutral-300 px-3 py-2
                      font-mono text-xs transition-colors hover:border-neutral-500
                      dark:border-neutral-700 ${busy ? "pointer-events-none opacity-50" : ""}`}
        >
          Upload PDF
          <input
            ref={fileInput}
            type="file"
            accept="application/pdf,.pdf"
            onChange={submitFile}
            disabled={busy}
            className="sr-only"
          />
        </label>
        <span className="text-xs text-neutral-500">or paste a URL above</span>
      </div>

      {(busy || phase === "done" || phase === "error") && (
        <div
          role="status"
          aria-live="polite"
          className="rounded-md border border-neutral-200 p-4 dark:border-neutral-800"
        >
          <div className="mb-2 flex items-baseline justify-between gap-4">
            <span className="font-mono text-xs">
              {phase === "error" ? "Failed" : stage || "Starting…"}
            </span>
            <span className="font-mono text-xs text-neutral-500">
              {phase === "error" ? "" : `${progress}%`}
            </span>
          </div>

          <div className="h-1 overflow-hidden rounded-full bg-neutral-200 dark:bg-neutral-800">
            <div
              className={`h-full transition-all duration-500 ${
                phase === "error" ? "bg-red-500" : "bg-emerald-500"
              }`}
              style={{ width: `${phase === "error" ? 100 : progress}%` }}
            />
          </div>

          {message && (
            <p
              className={`mt-2 text-xs ${
                phase === "error" ? "text-red-500" : "text-neutral-500"
              }`}
            >
              {message}
            </p>
          )}
        </div>
      )}
    </section>
  );
}
