import { Link } from "react-router-dom";
import { api } from "../api";
import { DurationBars, Empty, ErrorBanner, Panel, StatusBadge, Tile } from "../components";
import { formatBytes, formatDuration, formatRelative, percent, pluralise } from "../format";
import { useFetch, useTitle } from "../hooks";

export default function Overview() {
  useTitle("Overview");

  // The overview is a live board, so it polls; individual run pages use SSE for
  // anything finer-grained than this.
  const metrics = useFetch(() => api.metrics(), [], 3000);
  const recent = useFetch(() => api.runs({ limit: 8 }), [], 3000);

  const totals = metrics.data?.totals;
  const process = metrics.data?.process;
  const durations = metrics.data?.durations ?? [];
  const runs = recent.data?.runs ?? [];

  const inFlight = (process?.active_jobs ?? 0) + (process?.queued_jobs ?? 0);

  return (
    <>
      <div className="topbar">
        <h1>Overview</h1>
        <div className="topbar-spacer" />
        {totals && totals.runs_active > 0 && (
          <span className="badge st-running">
            <span className="dot" />
            {pluralise(totals.runs_active, "run")} active
          </span>
        )}
      </div>

      <div className="content stack">
        <ErrorBanner error={metrics.error} />

        <div className="grid grid-tiles">
          <Tile
            label="Runs"
            value={totals?.runs_total ?? 0}
            note={`${pluralise(totals?.pipelines ?? 0, "pipeline")}`}
          />
          <Tile
            label="Success rate"
            value={percent(totals?.success_rate)}
            note={`${totals?.runs_succeeded ?? 0} passed · ${totals?.runs_failed ?? 0} failed`}
            tone={
              totals && totals.runs_failed > 0 && totals.success_rate < 0.8 ? "failed" : "success"
            }
          />
          <Tile
            label="Average run"
            value={formatDuration(totals?.avg_run_duration_ms)}
            note={`longest ${formatDuration(totals?.max_run_duration_ms)}`}
          />
          <Tile
            label="In flight"
            value={inFlight}
            note={`${process?.active_jobs ?? 0} running · ${process?.queued_jobs ?? 0} queued`}
            tone={inFlight > 0 ? "running" : undefined}
          />
          <Tile
            label="Artifacts"
            value={totals?.artifact_count ?? 0}
            note={formatBytes(totals?.artifact_bytes)}
          />
        </div>

        <div className="grid grid-2">
          <Panel title="Recent run durations">
            <DurationBars points={durations} />
            <div className="muted small" style={{ marginTop: 10 }}>
              Last {durations.length} finished {durations.length === 1 ? "run" : "runs"}, oldest
              first. Bar colour is the outcome.
            </div>
          </Panel>

          <Panel title="This process">
            <dl className="kv">
              <dt>Uptime</dt>
              <dd>{formatDuration((process?.uptime_seconds ?? 0) * 1000)}</dd>
              <dt>Jobs started</dt>
              <dd>{process?.jobs_started ?? 0}</dd>
              <dt>Retries</dt>
              <dd>{process?.jobs_retried ?? 0}</dd>
              <dt>Skipped</dt>
              <dd>{process?.jobs_skipped ?? 0}</dd>
              <dt>Executors</dt>
              <dd>
                {process && Object.keys(process.executor_runs ?? {}).length > 0
                  ? Object.entries(process.executor_runs)
                      .map(([name, count]) => `${name} ${count}`)
                      .join(" · ")
                  : "—"}
              </dd>
            </dl>
          </Panel>
        </div>

        <Panel
          title="Recent runs"
          bodyless
          actions={
            <Link to="/runs" className="btn btn-sm">
              View all
            </Link>
          }
        >
          {runs.length === 0 ? (
            <Empty title="Nothing has run yet">
              <p className="muted">
                Start a pipeline with <code>forge run pipeline.yml</code>, or launch one from the{" "}
                <Link to="/pipelines" style={{ color: "var(--accent)" }}>
                  Pipelines
                </Link>{" "}
                page.
              </p>
            </Empty>
          ) : (
            <div className="table-scroll">
              <table className="table">
                <thead>
                  <tr>
                    <th>Status</th>
                    <th>Pipeline</th>
                    <th>Run</th>
                    <th>Duration</th>
                    <th>Started</th>
                    <th>Trigger</th>
                  </tr>
                </thead>
                <tbody>
                  {runs.map((run) => (
                    <tr key={run.id}>
                      <td>
                        <StatusBadge status={run.status} />
                      </td>
                      <td>
                        <Link to={`/runs/${run.id}`} style={{ fontWeight: 540 }}>
                          {run.pipeline_name}
                        </Link>
                      </td>
                      <td className="num">#{run.number}</td>
                      <td className="num">{formatDuration(run.duration_ms)}</td>
                      <td className="muted small">{formatRelative(run.started_at)}</td>
                      <td className="muted small">{run.trigger}</td>
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
