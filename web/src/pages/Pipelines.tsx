import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { api } from "../api";
import { Empty, ErrorBanner, Panel, StatusBadge } from "../components";
import { formatDuration, percent } from "../format";
import { useFetch, useTitle } from "../hooks";

export default function Pipelines() {
  useTitle("Pipelines");
  const navigate = useNavigate();
  const { data, error, loading, reload } = useFetch(() => api.pipelines(), [], 5000);
  const [starting, setStarting] = useState<number | null>(null);
  const [startError, setStartError] = useState<unknown>(null);

  const pipelines = data?.pipelines ?? [];

  const start = async (id: number) => {
    setStarting(id);
    setStartError(null);
    try {
      const { run } = await api.startRun(id, {});
      navigate(`/runs/${run.id}`);
    } catch (err) {
      setStartError(err);
    } finally {
      setStarting(null);
    }
  };

  return (
    <>
      <div className="topbar">
        <h1>Pipelines</h1>
        <div className="topbar-spacer" />
        <button className="btn btn-sm" onClick={reload}>
          Refresh
        </button>
      </div>

      <div className="content stack">
        <ErrorBanner error={error ?? startError} />

        <Panel bodyless>
          {pipelines.length === 0 ? (
            <Empty title={loading ? "Loading pipelines…" : "No pipelines registered"}>
              {!loading && (
                <p className="muted">
                  forge learns about a pipeline the first time it runs one. Try{" "}
                  <code>forge run pipeline.yml</code>.
                </p>
              )}
            </Empty>
          ) : (
            <div className="table-scroll">
              <table className="table">
                <thead>
                  <tr>
                    <th>Pipeline</th>
                    <th>Last run</th>
                    <th>Runs</th>
                    <th>Success rate</th>
                    <th>Average</th>
                    <th>Source</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {pipelines.map((p) => {
                    const finished = p.stats.successes + p.stats.failures;
                    const rate = finished > 0 ? p.stats.successes / finished : 0;
                    return (
                      <tr key={p.id}>
                        <td>
                          <Link to={`/pipelines/${p.id}`} style={{ fontWeight: 560 }}>
                            {p.name}
                          </Link>
                          {p.description && (
                            <div className="muted small">{p.description}</div>
                          )}
                        </td>
                        <td>
                          {p.stats.last_status ? (
                            p.stats.last_run_id ? (
                              <Link to={`/runs/${p.stats.last_run_id}`}>
                                <StatusBadge status={p.stats.last_status} />
                              </Link>
                            ) : (
                              <StatusBadge status={p.stats.last_status} />
                            )
                          ) : (
                            <span className="subtle small">never run</span>
                          )}
                        </td>
                        <td className="num">{p.stats.runs}</td>
                        <td>
                          {finished > 0 ? (
                            <div style={{ minWidth: 92 }}>
                              <div className="small muted">{percent(rate)}</div>
                              <div className="meter">
                                <div
                                  className="meter-fill"
                                  style={{
                                    width: `${rate * 100}%`,
                                    background:
                                      rate >= 0.8 ? "var(--st-success)" : "var(--st-failed)",
                                  }}
                                />
                              </div>
                            </div>
                          ) : (
                            <span className="subtle">—</span>
                          )}
                        </td>
                        <td className="num">{formatDuration(p.stats.avg_duration_ms)}</td>
                        <td className="mono subtle" title={p.source_path}>
                          {p.source_path || "—"}
                        </td>
                        <td style={{ textAlign: "right" }}>
                          <button
                            className="btn btn-sm btn-primary"
                            disabled={starting === p.id}
                            onClick={() => void start(p.id)}
                          >
                            {starting === p.id ? "Starting…" : "Run"}
                          </button>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </Panel>
      </div>
    </>
  );
}
