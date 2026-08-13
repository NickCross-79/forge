import { useCallback, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api, downloadUrl } from "../api";
import { Crumbs, Empty, ErrorBanner, JobGraph, Panel, StatusBadge, Tile } from "../components";
import { formatBytes, formatDuration, formatRelative, formatTimestamp, isActive } from "../format";
import { useEventStream, useFetch, useTitle } from "../hooks";

export default function RunDetail() {
  const { id } = useParams();
  const runId = Number(id);
  const [selected, setSelected] = useState<string | undefined>();
  const [actionError, setActionError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);

  // A slow poll is the safety net; SSE below is what makes the page feel live.
  const { data, error, reload } = useFetch(() => api.run(runId), [runId], 8000);

  const run = data?.run;
  const jobs = data?.jobs ?? [];
  const artifacts = data?.artifacts ?? [];
  const live = run ? isActive(run.status) : false;

  useTitle(run ? `${run.pipeline_name} #${run.number}` : "Run");

  // Every transition on the server wakes this stream, and each wake re-reads the
  // run. That keeps one code path for rendering whether data arrived by poll or
  // by event.
  const onEvent = useCallback(() => reload(), [reload]);
  const { connected } = useEventStream(live ? `/runs/${runId}/events/stream` : null, onEvent, reload);

  const cancel = async () => {
    setBusy(true);
    setActionError(null);
    try {
      await api.cancelRun(runId);
      reload();
    } catch (err) {
      setActionError(err);
    } finally {
      setBusy(false);
    }
  };

  const approve = async (name: string) => {
    setBusy(true);
    setActionError(null);
    try {
      await api.approveJob(runId, name);
      reload();
    } catch (err) {
      setActionError(err);
    } finally {
      setBusy(false);
    }
  };

  const awaiting = jobs.filter((j) => j.status === "awaiting_manual");
  const failed = jobs.filter((j) => j.status === "failed");

  return (
    <>
      <div className="topbar">
        <Crumbs
          items={[
            { label: "Runs", to: "/runs" },
            { label: run ? `${run.pipeline_name} #${run.number}` : "…" },
          ]}
        />
        <div className="topbar-spacer" />
        {live && (
          <span className="muted small row" style={{ gap: 6 }}>
            <span className="dot" style={{ background: connected ? "var(--st-running)" : "var(--st-queued)" }} />
            {connected ? "live" : "reconnecting"}
          </span>
        )}
        {live && (
          <button className="btn btn-sm btn-danger" disabled={busy} onClick={() => void cancel()}>
            Cancel run
          </button>
        )}
      </div>

      <div className="content stack">
        <ErrorBanner error={error ?? actionError} />

        {awaiting.length > 0 && (
          <div className="notice">
            <span>
              {awaiting.length === 1
                ? `Job "${awaiting[0]!.name}" is waiting for approval.`
                : `${awaiting.length} jobs are waiting for approval.`}
            </span>
            <div className="topbar-spacer" />
            <div className="btn-group">
              {awaiting.map((job) => (
                <button
                  key={job.id}
                  className="btn btn-sm btn-primary"
                  disabled={busy}
                  onClick={() => void approve(job.name)}
                >
                  Approve {job.name}
                </button>
              ))}
            </div>
          </div>
        )}

        {run?.error && failed.length > 0 && (
          <div className="error-banner">
            <strong>{run.error}</strong>
            <div style={{ marginTop: 6 }}>
              {failed.map((job) => (
                <div key={job.id} className="small">
                  {job.name}
                  {job.exit_code != null && ` — exit ${job.exit_code}`}
                  {job.error && `: ${job.error}`}
                </div>
              ))}
            </div>
          </div>
        )}

        <div className="grid grid-tiles">
          <Tile
            label="Status"
            value={run ? run.status.replace("_", " ") : "—"}
            tone={run?.status}
            note={run ? `trigger: ${run.trigger}` : undefined}
          />
          <Tile label="Duration" value={formatDuration(run?.duration_ms)} note={formatRelative(run?.started_at)} />
          <Tile
            label="Jobs"
            value={jobs.length}
            note={`${jobs.filter((j) => j.status === "success").length} passed · ${
              jobs.filter((j) => j.status === "skipped").length
            } skipped`}
          />
          <Tile
            label="Artifacts"
            value={artifacts.length}
            note={formatBytes(artifacts.reduce((sum, a) => sum + a.size, 0))}
          />
        </div>

        {data?.layers && data.layers.length > 0 && (
          <Panel title="Job graph" bodyless>
            <JobGraph
              layers={data.layers}
              jobs={jobs}
              edges={data.edges ?? []}
              selected={selected}
              onSelect={setSelected}
            />
          </Panel>
        )}

        <Panel title="Jobs" bodyless>
          {jobs.length === 0 ? (
            <Empty title="No jobs" />
          ) : (
            <div className="table-scroll">
              <table className="table">
                <thead>
                  <tr>
                    <th>Status</th>
                    <th>Job</th>
                    <th>Stage</th>
                    <th>Duration</th>
                    <th>Attempts</th>
                    <th>Executor</th>
                    <th>Detail</th>
                  </tr>
                </thead>
                <tbody>
                  {jobs.map((job) => (
                    <tr
                      key={job.id}
                      className={selected === job.name ? "clickable" : "clickable"}
                      style={
                        selected === job.name ? { background: "var(--bg-hover)" } : undefined
                      }
                      onMouseEnter={() => setSelected(job.name)}
                    >
                      <td>
                        <StatusBadge status={job.status} />
                      </td>
                      <td>
                        <Link to={`/runs/${runId}/jobs/${job.id}`} style={{ fontWeight: 540 }}>
                          {job.name}
                        </Link>
                      </td>
                      <td className="muted small">{job.stage}</td>
                      <td className="num">{formatDuration(job.duration_ms)}</td>
                      <td className="num">
                        {job.attempts}/{job.max_attempts}
                      </td>
                      <td className="muted small">{job.executor}</td>
                      <td className="muted small">
                        {job.error || (job.exit_code != null ? `exit ${job.exit_code}` : "—")}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>

        {artifacts.length > 0 && (
          <Panel title="Artifacts" bodyless>
            <div className="table-scroll">
              <table className="table">
                <thead>
                  <tr>
                    <th>Job</th>
                    <th>Path</th>
                    <th>Size</th>
                    <th>SHA-256</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {artifacts.map((a) => (
                    <tr key={a.id}>
                      <td className="muted">{a.job_name}</td>
                      <td className="mono">{a.path}</td>
                      <td className="num">{formatBytes(a.size)}</td>
                      <td className="mono subtle small" title={a.sha256}>
                        {a.sha256.slice(0, 12)}
                      </td>
                      <td style={{ textAlign: "right" }}>
                        <a className="btn btn-sm" href={downloadUrl(a.id)} download>
                          Download
                        </a>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </Panel>
        )}

        {run && (
          <Panel title="Details">
            <dl className="kv">
              <dt>Run ID</dt>
              <dd className="mono">{run.id}</dd>
              <dt>Pipeline</dt>
              <dd>
                <Link to={`/pipelines/${run.pipeline_id}`} style={{ color: "var(--accent)" }}>
                  {run.pipeline_name}
                </Link>
              </dd>
              <dt>Created</dt>
              <dd>{formatTimestamp(run.created_at)}</dd>
              <dt>Started</dt>
              <dd>{formatTimestamp(run.started_at)}</dd>
              <dt>Finished</dt>
              <dd>{formatTimestamp(run.finished_at)}</dd>
              <dt>Workspace</dt>
              <dd className="mono subtle">{run.work_dir}</dd>
            </dl>
          </Panel>
        )}
      </div>
    </>
  );
}
