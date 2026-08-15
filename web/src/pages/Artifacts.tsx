import { api, downloadUrl } from "../api";
import { Empty, ErrorBanner, Panel } from "../components";
import { formatBytes, formatRelative, pluralise } from "../format";
import { useFetch, useTitle } from "../hooks";
import { Link } from "react-router-dom";

/**
 * Artifacts across all recent runs.
 *
 * The API scopes artifacts to a run, so this page fans out over the most recent
 * runs rather than adding a global endpoint that would be hard to paginate
 * sensibly. Twenty runs is plenty for a local tool and keeps the request count
 * bounded.
 */
const RUN_WINDOW = 20;

export default function Artifacts() {
  useTitle("Artifacts");

  const { data, error, loading } = useFetch(async () => {
    const { runs } = await api.runs({ limit: RUN_WINDOW });
    const perRun = await Promise.all(
      runs.map(async (run) => {
        const { artifacts } = await api.runArtifacts(run.id);
        return artifacts.map((a) => ({ artifact: a, run }));
      }),
    );
    return perRun.flat();
  }, [], 10000);

  const rows = data ?? [];
  const totalBytes = rows.reduce((sum, r) => sum + r.artifact.size, 0);

  return (
    <>
      <div className="topbar">
        <h1>Artifacts</h1>
        <div className="topbar-spacer" />
        <span className="muted small">
          {pluralise(rows.length, "file")} · {formatBytes(totalBytes)}
        </span>
      </div>

      <div className="content stack">
        <ErrorBanner error={error} />

        <Panel bodyless>
          {rows.length === 0 ? (
            <Empty title={loading ? "Loading…" : "No artifacts collected"}>
              {!loading && (
                <p className="muted">
                  Add an <code>artifacts.paths</code> block to a job to keep files after it runs.
                </p>
              )}
            </Empty>
          ) : (
            <div className="table-scroll">
              <table className="table">
                <thead>
                  <tr>
                    <th>Pipeline</th>
                    <th>Run</th>
                    <th>Job</th>
                    <th>Path</th>
                    <th>Size</th>
                    <th>Collected</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {rows.map(({ artifact, run }) => (
                    <tr key={artifact.id}>
                      <td className="muted">{run.pipeline_name}</td>
                      <td>
                        <Link to={`/runs/${run.id}`} className="num">
                          #{run.number}
                        </Link>
                      </td>
                      <td className="muted">{artifact.job_name}</td>
                      <td className="mono">{artifact.path}</td>
                      <td className="num">{formatBytes(artifact.size)}</td>
                      <td className="muted small">{formatRelative(artifact.created_at)}</td>
                      <td style={{ textAlign: "right" }}>
                        <a className="btn btn-sm" href={downloadUrl(artifact.id)} download>
                          Download
                        </a>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>

        <p className="muted small">
          Showing artifacts from the {RUN_WINDOW} most recent runs. Older artifacts stay on disk
          until their retention window expires or <code>forge prune</code> removes them.
        </p>
      </div>
    </>
  );
}
