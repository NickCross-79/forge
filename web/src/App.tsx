import { NavLink, Navigate, Route, Routes } from "react-router-dom";
import { useFetch } from "./hooks";
import { api } from "./api";
import Overview from "./pages/Overview";
import Pipelines from "./pages/Pipelines";
import PipelineDetail from "./pages/PipelineDetail";
import RunDetail from "./pages/RunDetail";
import JobDetail from "./pages/JobDetail";
import History from "./pages/History";
import Artifacts from "./pages/Artifacts";
import SettingsPage from "./pages/Settings";

const NAV = [
  { to: "/", label: "Overview", icon: "◈", end: true },
  { to: "/pipelines", label: "Pipelines", icon: "⌥" },
  { to: "/runs", label: "Runs", icon: "▤" },
  { to: "/artifacts", label: "Artifacts", icon: "⬓" },
  { to: "/settings", label: "Settings", icon: "⚙" },
];

export default function App() {
  // A slow poll of health drives the connection indicator and the active-run
  // count in the sidebar, which is the cheapest way to show the server is alive.
  const { data: health } = useFetch(() => api.health(), [], 5000);
  const activeRuns = health?.active_runs?.length ?? 0;

  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="brand">
          <span className="brand-mark">F</span>
          <span>forge</span>
        </div>

        <nav className="nav">
          <div className="nav-label">Workspace</div>
          {NAV.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) => (isActive ? "active" : "")}
            >
              <span className="nav-icon" aria-hidden="true">
                {item.icon}
              </span>
              {item.label}
            </NavLink>
          ))}
        </nav>

        <div className="sidebar-foot">
          <div className="row" style={{ gap: 6 }}>
            <span
              className="dot"
              style={{
                background: health ? "var(--st-success)" : "var(--st-failed)",
              }}
            />
            {health ? "connected" : "disconnected"}
          </div>
          {activeRuns > 0 && (
            <div>
              {activeRuns} run{activeRuns === 1 ? "" : "s"} in flight
            </div>
          )}
          <div>local · $0/month</div>
        </div>
      </aside>

      <main className="main">
        <Routes>
          <Route path="/" element={<Overview />} />
          <Route path="/pipelines" element={<Pipelines />} />
          <Route path="/pipelines/:id" element={<PipelineDetail />} />
          <Route path="/runs" element={<History />} />
          <Route path="/runs/:id" element={<RunDetail />} />
          <Route path="/runs/:runId/jobs/:jobId" element={<JobDetail />} />
          <Route path="/artifacts" element={<Artifacts />} />
          <Route path="/settings" element={<SettingsPage />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </main>
    </div>
  );
}
