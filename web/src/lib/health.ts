// Types and fetching for the API readiness probe.
//
// Requests go to /api/* and are proxied to the Go API by next.config.ts, so the
// browser stays same-origin and the API address never ships in a client bundle.

export type DependencyState = "ok" | "error";

export interface DependencyStatus {
  status: DependencyState;
  detail?: string;
}

export interface ReadyResponse {
  status: "ok" | "degraded";
  dependencies: Record<string, DependencyStatus>;
}

const REQUEST_TIMEOUT_MS = 8000;

/**
 * Fetches the API readiness report.
 *
 * A 503 is a valid, expected response carrying the per-dependency breakdown, so
 * it is parsed rather than thrown. Only a transport failure or unreadable body
 * raises.
 */
export async function fetchReadiness(signal?: AbortSignal): Promise<ReadyResponse> {
  const timeout = AbortSignal.timeout(REQUEST_TIMEOUT_MS);
  const combined = signal ? AbortSignal.any([signal, timeout]) : timeout;

  const response = await fetch("/readyz", {
    signal: combined,
    cache: "no-store",
  });

  if (response.status !== 200 && response.status !== 503) {
    throw new Error(`Unexpected status ${response.status} from /readyz`);
  }

  return (await response.json()) as ReadyResponse;
}
