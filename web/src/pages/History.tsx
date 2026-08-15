import { useState } from "react";
import { Link } from "react-router-dom";
import { api, type Status } from "../api";
import { Empty, ErrorBanner, Panel, StatusBadge } from "../components";
import { formatDuration, formatRelative, pluralise } from "../format";
import { useFetch, useTitle } from "../hooks";

const FILTERS: { label: string; value: string }[] = [
  { label: "All", value: "" },
  { label: "Running", value: "running,queued,retrying,awaiting_manual" },
  { label: "Passed", value: "success" },
  { label: "Failed", value: "failed" },
  { label: "Cancelled", value: "cancelled" },
];

const PAGE = 25;

export default function History() {
  useTitle("Runs");
  const [status, setStatus] = useState("");
  const [page, setPage] = useState(0);

  const { data, error, reload } = useFetch(
    () => api.runs({ limit: PAGE, offset: page * PAGE, status: status || undefined }),
    [status, page],
    4000,
  );

  const runs = data?.runs ?? [];
  const total = data?.total ?? 0;
  const pages = Math.max(1, Math.ceil(total / PAGE));

  const choose = (value: string) => {
    setStatus(value);
    setPage(0);
  };

  return (
    <>
      <div className="topbar">
        <h1>Runs</h1>
        <div className="topbar-spacer" />
        <span className="muted small">{pluralise(total, "run")}</span>
        <button className="btn btn-sm" onClick={reload}>
          Refresh
        </button>
      </div>

      <div className="content stack">
        <ErrorBanner error={error} />

        <div className="row" style={{ flexWrap: "wrap" }}>
          {FILTERS.map((f) => (
            <button
              key={f.label}
              className={`btn btn-sm${status === f.value ? " btn-primary" : ""}`}
              onClick={() => choose(f.value)}
            >
              {f.label}
            </button>
          ))}
        </div>

        <Panel bodyless>
          {runs.length === 0 ? (
            <Empty title="No runs match this filter" />
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
                    <th>Reason</th>
                  </tr>
                </thead>
                <tbody>
                  {runs.map((run) => (
                    <tr key={run.id}>
                      <td>
                        <StatusBadge status={run.status as Status} />
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
                      <td className="muted small">{run.error || "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>

        {pages > 1 && (
          <div className="row">
            <button className="btn btn-sm" disabled={page === 0} onClick={() => setPage((p) => p - 1)}>
              Previous
            </button>
            <span className="muted small">
              Page {page + 1} of {pages}
            </span>
            <button
              className="btn btn-sm"
              disabled={page + 1 >= pages}
              onClick={() => setPage((p) => p + 1)}
            >
              Next
            </button>
          </div>
        )}
      </div>
    </>
  );
}
