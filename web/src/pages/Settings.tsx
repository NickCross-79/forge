import { useState } from "react";
import { api, getToken, setToken } from "../api";
import { ErrorBanner, Panel } from "../components";
import { formatBytes, pluralise } from "../format";
import { useFetch, useTitle } from "../hooks";

export default function SettingsPage() {
  useTitle("Settings");
  const { data, error, reload } = useFetch(() => api.settings(), []);
  const metrics = useFetch(() => api.metrics(), [], 10000);

  const [token, setTokenInput] = useState(getToken());
  const [saved, setSaved] = useState(false);

  const saveToken = () => {
    setToken(token.trim());
    setSaved(true);
    reload();
    window.setTimeout(() => setSaved(false), 2000);
  };

  const totals = metrics.data?.totals;

  return (
    <>
      <div className="topbar">
        <h1>Settings</h1>
      </div>

      <div className="content stack">
        <ErrorBanner error={error} />

        <Panel title="Security">
          <div className="stack" style={{ gap: 14 }}>
            <div>
              <div className="row" style={{ gap: 8 }}>
                <span
                  className={`badge ${data?.loopback_only ? "st-success" : "st-cancelled"}`}
                >
                  <span className="dot" />
                  {data?.loopback_only ? "loopback only" : "exposed beyond loopback"}
                </span>
                <span className={`badge ${data?.auth_required ? "st-success" : "st-queued"}`}>
                  <span className="dot" />
                  {data?.auth_required ? "token required" : "no token"}
                </span>
              </div>
              <p className="muted small" style={{ marginTop: 8, maxWidth: "62ch" }}>
                forge executes pipeline commands through a shell with your operating-system
                permissions. Anyone who can reach this API can run commands as you, which is why
                it binds to <code>127.0.0.1</code> by default and refuses any other address unless
                a token is configured.
              </p>
            </div>

            <div>
              <label className="tile-label" htmlFor="token">
                API token
              </label>
              <div className="row" style={{ marginTop: 6 }}>
                <input
                  id="token"
                  className="input"
                  type="password"
                  value={token}
                  placeholder={data?.auth_required ? "required by this server" : "not required"}
                  onChange={(e) => setTokenInput(e.target.value)}
                  style={{ minWidth: 280 }}
                />
                <button className="btn btn-sm" onClick={saveToken}>
                  {saved ? "Saved" : "Save"}
                </button>
              </div>
              <p className="muted small" style={{ marginTop: 6 }}>
                Stored in this browser only, and sent as a bearer token on every request.
              </p>
            </div>
          </div>
        </Panel>

        <div className="grid grid-2">
          <Panel title="Execution">
            <dl className="kv">
              <dt>Concurrency</dt>
              <dd>{pluralise(data?.runner.concurrency ?? 0, "job")} at once</dd>
              <dt>Default executor</dt>
              <dd>{data?.runner.default_executor}</dd>
              <dt>Default timeout</dt>
              <dd>{data?.runner.default_timeout}</dd>
              <dt>Default retries</dt>
              <dd>{data?.runner.default_retries}</dd>
              <dt>Manual timeout</dt>
              <dd>
                {data?.runner.manual_timeout === "0s"
                  ? "waits indefinitely"
                  : data?.runner.manual_timeout}
              </dd>
              <dt>Workspaces</dt>
              <dd>
                {data?.workspace.mode}
                <div className="subtle small">
                  {data?.workspace.mode === "isolated"
                    ? "each job gets its own copy of the project"
                    : "all jobs in a run share one directory"}
                </div>
              </dd>
              <dt>Ignored when seeding</dt>
              <dd className="mono subtle small">{data?.workspace.ignore.join(", ")}</dd>
            </dl>
          </Panel>

          <Panel title="Environment and secrets">
            <dl className="kv">
              <dt>Passed through</dt>
              <dd className="mono subtle small">{data?.runner.env_passthrough.join(", ")}</dd>
              <dt>Secrets loaded</dt>
              <dd>
                {data && data.secret_names.length > 0 ? (
                  <div className="mono">{data.secret_names.join(", ")}</div>
                ) : (
                  <span className="subtle">none</span>
                )}
                <div className="subtle small" style={{ marginTop: 4 }}>
                  Names only. Values are never sent to the browser, never written to the database,
                  and are masked in captured logs.
                </div>
              </dd>
            </dl>
          </Panel>
        </div>

        <div className="grid grid-2">
          <Panel title="Storage">
            <dl className="kv">
              <dt>Project root</dt>
              <dd className="mono subtle">{data?.project_root}</dd>
              <dt>forge home</dt>
              <dd className="mono subtle">{data?.home}</dd>
              <dt>Database</dt>
              <dd className="mono subtle">{data?.database}</dd>
              <dt>Artifacts</dt>
              <dd className="mono subtle">{data?.artifacts_dir}</dd>
              <dt>Logs</dt>
              <dd className="mono subtle">{data?.logs_dir}</dd>
              <dt>Config file</dt>
              <dd className="mono subtle">{data?.config_file || "using defaults"}</dd>
            </dl>
          </Panel>

          <Panel title="Retention and usage">
            <dl className="kv">
              <dt>Artifact retention</dt>
              <dd>{data?.retention.artifacts}</dd>
              <dt>Log retention</dt>
              <dd>{data?.retention.logs}</dd>
              <dt>Runs kept</dt>
              <dd>{data?.workspace.keep_runs || "all"}</dd>
              <dt>Artifact bytes</dt>
              <dd>{formatBytes(totals?.artifact_bytes)}</dd>
              <dt>Log bytes</dt>
              <dd>{formatBytes(totals?.log_bytes)}</dd>
            </dl>
            <p className="muted small" style={{ marginTop: 12 }}>
              Run <code>forge prune</code> to apply retention now, or{" "}
              <code>forge prune --dry-run</code> to see what it would remove.
            </p>
          </Panel>
        </div>
      </div>
    </>
  );
}
