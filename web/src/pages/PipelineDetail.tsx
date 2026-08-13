import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { api } from "../api";
import { Crumbs, Empty, ErrorBanner, JobGraph, Panel, StatusBadge } from "../components";
import { formatDuration, formatRelative } from "../format";
import { useFetch, useTitle } from "../hooks";

export default function PipelineDetail() {
  const { id } = useParams();
  const pipelineId = Number(id);
  const navigate = useNavigate();

  const { data, error, reload } = useFetch(() => api.pipeline(pipelineId), [pipelineId], 5000);
  const [starting, setStarting] = useState(false);
  const [startError, setStartError] = useState<unknown>(null);
  const [showSpec, setShowSpec] = useState(false);

  useTitle(data?.pipeline.name ?? "Pipeline");

  const start = async () => {
    setStarting(true);
    setStartError(null);
    try {
      const { run } = await api.startRun(pipelineId, {});
      navigate(`/runs/${run.id}`);
    } catch (err) {
      setStartError(err);
      setStarting(false);
    }
  };

  const pipeline = data?.pipeline;
  const summaries = data?.jobs ?? [];
  const layers = data?.layers ?? [];
  const runs = data?.runs ?? [];

  // The pipeline view has no run to colour the graph with, so every node is drawn
  // in the neutral queued state and the graph reads as structure, not status.
  const placeholderJobs = summaries.map((s) => ({
    id: 0,
    run_id: 0,
    name: s.name,
    stage: s.stage,
    status: "queued" as const,
    needs: s.needs ?? [],
    executor: s.executor,
    manual: s.when === "manual",
    allow_failure: s.allow_failure,
    attempts: 0,
    max_attempts: s.retries + 1,
    created_at: "",
    duration_ms: 0,
  }));

  const edges = summaries.flatMap((s) =>
    (s.needs ?? []).map((need) => ({ from: need, to: s.name, implicit: s.implicit_needs })),
  );

  return (
    <>
      <div className="topbar">
        <Crumbs
          items={[{ label: "Pipelines", to: "/pipelines" }, { label: pipeline?.name ?? "…" }]}
        />
        <div className="topbar-spacer" />
        <button className="btn btn-sm" onClick={() => setShowSpec((v) => !v)}>
          {showSpec ? "Hide YAML" : "View YAML"}
        </button>
        <button className="btn btn-sm btn-primary" disabled={starting} onClick={() => void start()}>
          {starting ? "Starting…" : "Run pipeline"}
        </button>
      </div>

      <div className="content stack">
        <ErrorBanner error={error ?? startError ?? data?.spec_error} />

        {layers.length > 0 && (
          <Panel title="Job graph" bodyless>
            <JobGraph layers={layers} jobs={placeholderJobs} edges={edges} />
            <div className="panel-body" style={{ borderTop: "1px solid var(--border)" }}>
              <span className="muted small">
                Columns run left to right. A dashed edge is ordering implied by stages; a solid
                edge is an explicit <code className="mono">needs</code>.
              </span>
            </div>
          </Panel>
        )}

        {showSpec && pipeline && (
          <Panel title="pipeline definition">
            <pre className="logs" style={{ padding: 14, margin: 0 }}>
              {pipeline.spec_yaml}
            </pre>
          </Panel>
        )}

        <Panel title="Jobs" bodyless>
          {summaries.length === 0 ? (
            <Empty title="No jobs" />
          ) : (
            <div className="table-scroll">
              <table className="table">
                <thead>
                  <tr>
                    <th>Job</th>
                    <th>Stage</th>
                    <th>Needs</th>
                    <th>Executor</th>
                    <th>When</th>
                    <th>Retries</th>
                    <th>Artifacts</th>
                  </tr>
                </thead>
                <tbody>
                  {summaries.map((job) => (
                    <tr key={job.name}>
                      <td style={{ fontWeight: 540 }}>
                        {job.name}
                        {job.if && (
                          <div className="mono subtle small">if: {job.if}</div>
                        )}
                      </td>
                      <td className="muted">{job.stage}</td>
                      <td className="muted small">
                        {(job.needs ?? []).length > 0 ? job.needs.join(", ") : "—"}
                        {job.implicit_needs && <span className="subtle"> (by stage)</span>}
                      </td>
                      <td className="muted small">
                        {job.executor}
                        {job.image && <div className="mono subtle">{job.image}</div>}
                      </td>
                      <td className="muted small">
                        {job.when}
                        {job.allow_failure && (
                          <div className="subtle">allow_failure</div>
                        )}
                      </td>
                      <td className="num">{job.retries}</td>
                      <td className="mono subtle small">
                        {job.artifacts?.join(", ") || "—"}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>

        <Panel
          title="Run history"
          bodyless
          actions={
            <button className="btn btn-sm" onClick={reload}>
              Refresh
            </button>
          }
        >
          {runs.length === 0 ? (
            <Empty title="This pipeline has not run yet" />
          ) : (
            <div className="table-scroll">
              <table className="table">
                <thead>
                  <tr>
                    <th>Status</th>
                    <th>Run</th>
                    <th>Duration</th>
                    <th>Started</th>
                    <th>Trigger</th>
                    <th>Reason</th>
                  </tr>
                </thead>
                <tbody>
                  {runs.map((run) => (
                    <tr key={run.id}>
                      <td>
                        <StatusBadge status={run.status} />
                      </td>
                      <td>
                        <Link to={`/runs/${run.id}`} className="num" style={{ fontWeight: 560 }}>
                          #{run.number}
                        </Link>
                      </td>
                      <td className="num">{formatDuration(run.duration_ms)}</td>
                      <td className="muted small">{formatRelative(run.started_at)}</td>
                      <td className="muted small">{run.trigger}</td>
                      <td className="muted small">{run.error || "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>
      </div>
    </>
  );
}
