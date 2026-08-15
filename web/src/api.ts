/**
 * Typed client for the forge REST API.
 *
 * Every request goes through `request`, so authentication, error shape and JSON
 * handling are defined once. The token, when the server requires one, is kept in
 * localStorage and also appended to SSE URLs, because EventSource cannot set
 * request headers.
 */

export type Status =
  | "queued"
  | "running"
  | "success"
  | "failed"
  | "cancelled"
  | "skipped"
  | "retrying"
  | "awaiting_manual";

export type Stream = "stdout" | "stderr";

export interface Pipeline {
  id: number;
  name: string;
  source_path: string;
  spec_yaml: string;
  checksum: string;
  description?: string;
  created_at: string;
  updated_at: string;
}

export interface PipelineStats {
  runs: number;
  successes: number;
  failures: number;
  last_status?: Status;
  last_run_id?: number;
  avg_duration_ms: number;
}

export interface PipelineWithStats extends Pipeline {
  stats: PipelineStats;
}

export interface Run {
  id: number;
  pipeline_id: number;
  number: number;
  status: Status;
  trigger: string;
  work_dir: string;
  error?: string;
  created_at: string;
  started_at?: string;
  finished_at?: string;
  duration_ms: number;
  pipeline_name?: string;
}

export interface Job {
  id: number;
  run_id: number;
  name: string;
  stage: string;
  status: Status;
  needs: string[];
  executor: string;
  image?: string;
  manual: boolean;
  allow_failure: boolean;
  attempts: number;
  max_attempts: number;
  exit_code?: number;
  error?: string;
  created_at: string;
  started_at?: string;
  finished_at?: string;
  duration_ms: number;
}

export interface Attempt {
  id: number;
  job_id: number;
  number: number;
  status: Status;
  exit_code?: number;
  error?: string;
  started_at?: string;
  finished_at?: string;
  duration_ms: number;
}

export interface LogRecord {
  id: number;
  attempt_id: number;
  job_id: number;
  run_id: number;
  stream: Stream;
  path: string;
  bytes: number;
  truncated: boolean;
  created_at: string;
}

export interface LogPayload {
  stream: Stream;
  attempt_id: number;
  content: string;
  bytes: number;
  offset: number;
  truncated: boolean;
}

export interface Artifact {
  id: number;
  run_id: number;
  job_id: number;
  job_name: string;
  path: string;
  size: number;
  sha256: string;
  mode: number;
  created_at: string;
  expires_at?: string;
}

export interface RunEvent {
  id: number;
  run_id: number;
  job_id?: number;
  job_name?: string;
  type: "run_status" | "job_status" | "log" | "artifact";
  from?: Status;
  to?: Status;
  message?: string;
  created_at: string;
}

export interface JobSummary {
  name: string;
  stage: string;
  needs: string[];
  commands: string[];
  image?: string;
  executor: string;
  when: string;
  if?: string;
  allow_failure: boolean;
  timeout?: string;
  retries: number;
  artifacts?: string[];
  implicit_needs: boolean;
}

export interface Edge {
  from: string;
  to: string;
  implicit: boolean;
}

export interface Totals {
  pipelines: number;
  runs_total: number;
  runs_succeeded: number;
  runs_failed: number;
  runs_cancelled: number;
  runs_active: number;
  jobs_total: number;
  jobs_succeeded: number;
  jobs_failed: number;
  jobs_skipped: number;
  jobs_running: number;
  jobs_queued: number;
  avg_run_duration_ms: number;
  max_run_duration_ms: number;
  avg_job_duration_ms: number;
  artifact_count: number;
  artifact_bytes: number;
  log_bytes: number;
  success_rate: number;
}

export interface ProcessMetrics {
  uptime_seconds: number;
  runs_started: number;
  active_runs: number;
  jobs_started: number;
  jobs_succeeded: number;
  jobs_failed: number;
  jobs_skipped: number;
  jobs_retried: number;
  active_jobs: number;
  queued_jobs: number;
  avg_job_duration_ms: number;
  artifacts_collected: number;
  artifact_bytes: number;
  executor_runs: Record<string, number>;
}

export interface DurationPoint {
  run_id: number;
  number: number;
  pipeline: string;
  status: Status;
  duration_ms: number;
  finished_at: number;
}

export interface Settings {
  project_root: string;
  home: string;
  database: string;
  artifacts_dir: string;
  logs_dir: string;
  config_file: string;
  loopback_only: boolean;
  auth_required: boolean;
  secret_names: string[];
  runner: {
    concurrency: number;
    default_executor: string;
    default_timeout: string;
    default_retries: number;
    manual_timeout: string;
    env_passthrough: string[];
  };
  workspace: { mode: string; ignore: string[]; keep_runs: number };
  retention: { artifacts: string; logs: string };
}

/** ApiError carries the server's error code so callers can branch on it. */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

const TOKEN_KEY = "forge.token";

export function getToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) ?? "";
  } catch {
    // Private browsing modes can throw on localStorage access.
    return "";
  }
}

export function setToken(token: string): void {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token);
    else localStorage.removeItem(TOKEN_KEY);
  } catch {
    /* not fatal: the session simply will not remember the token */
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  const token = getToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);
  if (init?.body) headers.set("Content-Type", "application/json");

  const resp = await fetch(`/api/v1${path}`, { ...init, headers });

  if (resp.status === 204) return undefined as T;

  const text = await resp.text();
  let body: unknown = null;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = null;
    }
  }

  if (!resp.ok) {
    const err = (body as { error?: { message?: string; code?: string } } | null)?.error;
    throw new ApiError(
      err?.message ?? `request failed with ${resp.status}`,
      resp.status,
      err?.code ?? "unknown",
    );
  }
  return body as T;
}

/** streamUrl builds an SSE URL, carrying the token as a query parameter. */
export function streamUrl(path: string): string {
  const token = getToken();
  const url = new URL(`/api/v1${path}`, window.location.origin);
  if (token) url.searchParams.set("token", token);
  return url.toString();
}

/** downloadUrl points at an artifact's bytes. */
export function downloadUrl(artifactId: number): string {
  const token = getToken();
  const url = new URL(`/api/v1/artifacts/${artifactId}/download`, window.location.origin);
  if (token) url.searchParams.set("token", token);
  return url.toString();
}

export const api = {
  health: () =>
    request<{ status: string; schema_version: string; active_runs: number[] }>("/health"),

  settings: () => request<Settings>("/settings"),

  metrics: () =>
    request<{ process: ProcessMetrics; totals: Totals; durations: DurationPoint[] | null }>(
      "/metrics",
    ),

  pipelines: () => request<{ pipelines: PipelineWithStats[] }>("/pipelines"),

  pipeline: (id: number) =>
    request<{
      pipeline: Pipeline;
      jobs?: JobSummary[];
      stages?: string[];
      layers?: string[][];
      runs: Run[];
      spec_error?: string;
    }>(`/pipelines/${id}`),

  startRun: (id: number, body: { concurrency?: number; variables?: Record<string, string> }) =>
    request<{ run: Run }>(`/pipelines/${id}/runs`, {
      method: "POST",
      body: JSON.stringify(body),
    }),

  runs: (params: { limit?: number; offset?: number; status?: string; pipeline_id?: number } = {}) => {
    const q = new URLSearchParams();
    if (params.limit) q.set("limit", String(params.limit));
    if (params.offset) q.set("offset", String(params.offset));
    if (params.status) q.set("status", params.status);
    if (params.pipeline_id) q.set("pipeline_id", String(params.pipeline_id));
    const suffix = q.toString() ? `?${q}` : "";
    return request<{ runs: Run[]; total: number; limit: number; offset: number }>(`/runs${suffix}`);
  },

  run: (id: number) =>
    request<{
      run: Run;
      jobs: Job[];
      artifacts: Artifact[];
      active: boolean;
      layers?: string[][];
      edges?: Edge[];
    }>(`/runs/${id}`),

  cancelRun: (id: number) => request<{ cancelled: number }>(`/runs/${id}/cancel`, { method: "POST" }),

  deleteRun: (id: number) => request<void>(`/runs/${id}`, { method: "DELETE" }),

  approveJob: (runId: number, name: string) =>
    request<{ approved: string }>(`/runs/${runId}/jobs/${encodeURIComponent(name)}/approve`, {
      method: "POST",
    }),

  runArtifacts: (id: number) => request<{ artifacts: Artifact[] }>(`/runs/${id}/artifacts`),

  events: (id: number, after = 0) =>
    request<{ events: RunEvent[] }>(`/runs/${id}/events?after=${after}`),

  job: (id: number) =>
    request<{ job: Job; attempts: Attempt[]; logs: LogRecord[]; artifacts: Artifact[] }>(
      `/jobs/${id}`,
    ),

  jobLogs: (id: number, stream?: Stream) =>
    request<{ logs: LogPayload[] }>(`/jobs/${id}/logs${stream ? `?stream=${stream}` : ""}`),
};
