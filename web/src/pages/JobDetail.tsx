import { useState } from "react";
import { useParams } from "react-router-dom";
import { api, downloadUrl, type Stream } from "../api";
import { Crumbs, ErrorBanner, LogViewer, Panel, StatusBadge, Tile } from "../components";
import { formatBytes, formatDuration, formatTimestamp, isActive } from "../format";
import { useFetch, useLogStream, useTitle } from "../hooks";

export default function JobDetail() {
  const { runId, jobId } = useParams();
  const id = Number(jobId);
  const [stream, setStream] = useState<Stream>("stdout");

  const { data, error } = useFetch(() => api.job(id), [id], 5000);
  const job = data?.job;
  const live = job ? isActive(job.status) : false;

  useTitle(job ? `${job.name} · job` : "Job");

  // A finished job's output is fetched once; a running job's is streamed.
  const stored = useFetch(() => api.jobLogs(id, stream), [id, stream], live ? undefined : 0);
  const storedContent = stored.data?.logs?.map((l) => l.content).join("") ?? "";
  const { content, connected } = useLogStream(id, stream, live, storedContent);

  const attempts = data?.attempts ?? [];
  const artifacts = data?.artifacts ?? [];
  const logRecords = data?.logs ?? [];

  const bytesFor = (s: Stream) => {
    // The log record's byte count is written when the stream closes, so during a
    // run it reads zero. For the stream being watched, the streamed text is the
    // better answer.
    if (live && s === stream && content.length > 0) {
      return new TextEncoder().encode(content).length;
    }
    return logRecords.filter((r) => r.stream === s).reduce((sum, r) => sum + r.bytes, 0);
  };
  const truncated = logRecords.some((r) => r.stream === stream && r.truncated);

  return (
    <>
      <div className="topbar">
        <Crumbs
          items={[
            { label: "Runs", to: "/runs" },
            { label: `Run ${runId}`, to: `/runs/${runId}` },
            { label: job?.name ?? "…" },
          ]}
        />
        <div className="topbar-spacer" />
        {live && (
          <span className="muted small row" style={{ gap: 6 }}>
            <span
              className="dot"
              style={{ background: connected ? "var(--st-running)" : "var(--st-queued)" }}
            />
            {connected ? "streaming" : "connecting"}
          </span>
        )}
      </div>

      <div className="content stack">
        <ErrorBanner error={error} />

        {job?.error && (
          <div className="error-banner">
            <strong>Job failed</strong>
            <div style={{ marginTop: 4 }}>{job.error}</div>
          </div>
        )}

        <div className="grid grid-tiles">
          <Tile label="Status" value={job ? job.status.replace("_", " ") : "—"} tone={job?.status} />
          <Tile label="Duration" value={formatDuration(job?.duration_ms)} />
          <Tile
            label="Attempts"
            value={job ? `${job.attempts}/${job.max_attempts}` : "—"}
            note={job && job.attempts > 1 ? "retried" : undefined}
          />
          <Tile
            label="Exit code"
            value={job?.exit_code != null ? job.exit_code : "—"}
            tone={job?.exit_code ? "failed" : undefined}
          />
        </div>

        <Panel bodyless>
          <div className="tabs">
            <button
              className={`tab${stream === "stdout" ? " active" : ""}`}
              onClick={() => setStream("stdout")}
            >
              stdout
              <span className="tab-count">{formatBytes(bytesFor("stdout"))}</span>
            </button>
            <button
              className={`tab${stream === "stderr" ? " active" : ""}`}
              onClick={() => setStream("stderr")}
            >
              stderr
              <span className="tab-count">{formatBytes(bytesFor("stderr"))}</span>
            </button>
          </div>
          <div style={{ padding: 16 }}>
            {truncated && (
              <div className="notice" style={{ marginBottom: 10 }}>
                This stream hit the configured size limit and was truncated.
              </div>
            )}
            <LogViewer
              content={content}
              stream={stream}
              follow={live}
              empty={live ? "Waiting for output…" : "No output captured on this stream."}
            />
          </div>
        </Panel>

        {attempts.length > 1 && (
          <Panel title="Attempts" bodyless>
            <div className="table-scroll">
              <table className="table">
                <thead>
                  <tr>
                    <th>#</th>
                    <th>Status</th>
                    <th>Duration</th>
                    <th>Exit</th>
                    <th>Started</th>
                    <th>Error</th>
                  </tr>
                </thead>
                <tbody>
                  {attempts.map((a) => (
                    <tr key={a.id}>
                      <td className="num">{a.number}</td>
                      <td>
                        <StatusBadge status={a.status} />
                      </td>
                      <td className="num">{formatDuration(a.duration_ms)}</td>
                      <td className="num">{a.exit_code ?? "—"}</td>
                      <td className="muted small">{formatTimestamp(a.started_at)}</td>
                      <td className="muted small">{a.error || "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </Panel>
        )}

        {artifacts.length > 0 && (
          <Panel title="Artifacts from this job" bodyless>
            <div className="table-scroll">
              <table className="table">
                <thead>
                  <tr>
                    <th>Path</th>
                    <th>Size</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {artifacts.map((a) => (
                    <tr key={a.id}>
                      <td className="mono">{a.path}</td>
                      <td className="num">{formatBytes(a.size)}</td>
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

        {job && (
          <Panel title="Details">
            <dl className="kv">
              <dt>Stage</dt>
              <dd>{job.stage}</dd>
              <dt>Executor</dt>
              <dd>
                {job.executor}
                {job.image && <span className="mono subtle"> · {job.image}</span>}
              </dd>
              <dt>Depends on</dt>
              <dd>{job.needs.length > 0 ? job.needs.join(", ") : "nothing"}</dd>
              <dt>Allow failure</dt>
              <dd>{job.allow_failure ? "yes" : "no"}</dd>
              <dt>Manual</dt>
              <dd>{job.manual ? "yes" : "no"}</dd>
              <dt>Started</dt>
              <dd>{formatTimestamp(job.started_at)}</dd>
              <dt>Finished</dt>
              <dd>{formatTimestamp(job.finished_at)}</dd>
            </dl>
          </Panel>
        )}
      </div>
    </>
  );
}
