// Typed client for the Cortex API.
//
// All requests go to /api/*, which next.config.ts proxies to the Go service.
// The browser therefore stays same-origin and the API address never ships in a
// client bundle.

export interface IngestResponse {
  job_id: string;
  source_id: string;
  status: string;
}

export interface Source {
  id: string;
  title: string;
  source_type: string;
  url_or_path?: string;
  status: string;
  /** True when the original upload is retained and can be opened. */
  has_file: boolean;
  original_filename?: string;
  file_size?: number;
}

/**
 * URL of a source's stored original.
 *
 * Addressed by source id, not by blob hash — the storage key stays server-side.
 */
export function sourceFileUrl(id: string): string {
  return `/api/v1/sources/${id}/file`;
}

export interface SearchResult {
  chunk_id: string;
  content: string;
  heading_path?: string;
  chunk_index: number;
  similarity: number;
  source_id: string;
  source_title: string;
  source_type: string;
  source_url?: string;
}

export interface SearchResponse {
  query: string;
  results: SearchResult[];
}

export interface Concept {
  id: string;
  name: string;
  slug: string;
  summary: string;
  connection_count: number;
  mention_count?: number;
}

export interface GraphNode {
  id: string;
  name: string;
  slug: string;
  summary: string;
  connection_count: number;
}

export interface GraphEdge {
  source: string;
  target: string;
  summary?: string;
}

export interface ConceptGraph {
  nodes: GraphNode[];
  edges: GraphEdge[];
}

export interface ConceptMention {
  chunk_id: string;
  content: string;
  heading_path?: string;
  source_id: string;
  source_title: string;
  source_type: string;
  source_url?: string;
}

export interface ConceptDetail {
  concept: Concept;
  mentions: ConceptMention[];
  related: Concept[];
}

export function loadGraph(limit = 150): Promise<ConceptGraph> {
  return request<ConceptGraph>(`/api/v1/concepts/graph?limit=${limit}`);
}

export function loadConcept(slug: string): Promise<ConceptDetail> {
  return request<ConceptDetail>(`/api/v1/concepts/${encodeURIComponent(slug)}`);
}

export function listConcepts(): Promise<{ concepts: Concept[] }> {
  return request<{ concepts: Concept[] }>("/api/v1/concepts");
}

export type JobEventType = "status" | "complete" | "failed";

export interface JobEvent {
  type: JobEventType;
  stage?: string;
  progress: number;
  source_id?: string;
  chunks?: number;
  error?: string;
}

/** ApiError carries the server's message so the UI can show something useful. */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let response: Response;
  try {
    response = await fetch(path, init);
  } catch {
    // A transport failure almost always means the Go API is not running.
    throw new ApiError("Cannot reach the API. Is it running?", 0);
  }

  if (!response.ok) {
    let message = `Request failed (${response.status})`;
    try {
      const body = (await response.json()) as { error?: string };
      if (body.error) message = body.error;
    } catch {
      // Response had no JSON body; the status-based message stands.
    }
    throw new ApiError(message, response.status);
  }

  return (await response.json()) as T;
}

export function ingestUrl(url: string): Promise<IngestResponse> {
  return request<IngestResponse>("/api/v1/sources/url", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ url }),
  });
}

export function ingestPdf(file: File): Promise<IngestResponse> {
  const form = new FormData();
  form.append("file", file);
  // No Content-Type header: the browser must set the multipart boundary.
  return request<IngestResponse>("/api/v1/sources/upload", {
    method: "POST",
    body: form,
  });
}

export function search(query: string, k = 5): Promise<SearchResponse> {
  return request<SearchResponse>("/api/v1/search", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ query, k }),
  });
}

export function listSources(): Promise<{ sources: Source[] }> {
  return request<{ sources: Source[] }>("/api/v1/sources");
}

export async function deleteSource(id: string): Promise<void> {
  const response = await fetch(`/api/v1/sources/${id}`, { method: "DELETE" });
  if (!response.ok) {
    throw new ApiError(`Could not delete source (${response.status})`, response.status);
  }
}

/**
 * Streams job progress.
 *
 * Returns a cleanup function that closes the connection. The stream ends by
 * itself on a terminal event, but an unmounting component must still close it.
 */
export function streamJob(
  jobId: string,
  onEvent: (event: JobEvent) => void,
  onError: (message: string) => void,
): () => void {
  const source = new EventSource(`/api/v1/jobs/${jobId}/stream`);

  const handle = (raw: MessageEvent<string>) => {
    try {
      onEvent(JSON.parse(raw.data) as JobEvent);
    } catch {
      // A malformed frame is not worth tearing the stream down for.
    }
  };

  source.addEventListener("status", handle);
  source.addEventListener("complete", (event) => {
    handle(event as MessageEvent<string>);
    source.close();
  });
  source.addEventListener("failed", (event) => {
    handle(event as MessageEvent<string>);
    source.close();
  });

  source.onerror = () => {
    // EventSource also fires onerror on a normal server-side close, so only
    // report while the connection was still meant to be open.
    if (source.readyState !== EventSource.CLOSED) {
      onError("Lost connection to the progress stream");
    }
    source.close();
  };

  return () => source.close();
}
