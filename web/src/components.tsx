/** Shared presentational components. */

import { useEffect, useRef } from "react";
import { Link } from "react-router-dom";
import type { Job, Status } from "./api";
import { formatDuration, splitLogLines, statusLabel } from "./format";

export function StatusBadge({ status }: { status: Status }) {
  return (
    <span className={`badge st-${status}`}>
      <span className="dot" />
      {statusLabel(status)}
    </span>
  );
}

export function Tile({
  label,
  value,
  note,
  tone,
}: {
  label: string;
  value: string | number;
  note?: string;
  tone?: Status;
}) {
  return (
    <div className="tile">
      <div className="tile-label">{label}</div>
      <div className="tile-value" style={tone ? { color: `var(--st-${tone})` } : undefined}>
        {value}
      </div>
      {note && <div className="tile-note">{note}</div>}
    </div>
  );
}

export function Panel({
  title,
  actions,
  children,
  bodyless,
}: {
  title?: string;
  actions?: React.ReactNode;
  children: React.ReactNode;
  /** bodyless drops the padded body wrapper, for panels holding a full-bleed table. */
  bodyless?: boolean;
}) {
  return (
    <section className="panel">
      {title && (
        <header className="panel-head">
          <h2>{title}</h2>
          <div className="topbar-spacer" />
          {actions}
        </header>
      )}
      {bodyless ? children : <div className="panel-body">{children}</div>}
    </section>
  );
}

export function Empty({
  title,
  children,
}: {
  title: string;
  children?: React.ReactNode;
}) {
  return (
    <div className="empty">
      <div className="empty-title">{title}</div>
      {children}
    </div>
  );
}

export function Spinner({ label }: { label?: string }) {
  return (
    <div className="row muted small">
      <span className="spinner" />
      {label ?? "Loading…"}
    </div>
  );
}

export function ErrorBanner({ error }: { error: unknown }) {
  if (!error) return null;
  const message = error instanceof Error ? error.message : String(error);
  return <div className="error-banner">{message}</div>;
}

/* ---------- job graph ---------- */

const NODE_W = 168;
const NODE_H = 46;
const GAP_X = 60;
const GAP_Y = 16;
const PAD = 12;

/**
 * JobGraph draws the run's DAG as inline SVG.
 *
 * Layout comes from the server's topological layers: each layer is a column, and
 * jobs stack vertically within it. That is enough structure for CI graphs, which
 * are wide and shallow, and it avoids pulling in a graph-layout library for a
 * picture this regular. Edges are drawn as cubic curves between column edges so
 * crossings stay readable.
 */
export function JobGraph({
  layers,
  jobs,
  edges,
  selected,
  onSelect,
}: {
  layers: string[][];
  jobs: Job[];
  edges: { from: string; to: string; implicit: boolean }[];
  selected?: string;
  onSelect?: (name: string) => void;
}) {
  const byName = new Map(jobs.map((j) => [j.name, j]));

  // Position every node up front so edges can be drawn from the same source.
  const pos = new Map<string, { x: number; y: number }>();
  layers.forEach((layer, col) => {
    layer.forEach((name, row) => {
      pos.set(name, {
        x: PAD + col * (NODE_W + GAP_X),
        y: PAD + row * (NODE_H + GAP_Y),
      });
    });
  });

  const tallest = Math.max(1, ...layers.map((l) => l.length));
  const width = PAD * 2 + layers.length * NODE_W + Math.max(0, layers.length - 1) * GAP_X;
  const height = PAD * 2 + tallest * NODE_H + Math.max(0, tallest - 1) * GAP_Y;

  if (layers.length === 0) {
    return <Empty title="No jobs to draw" />;
  }

  return (
    <div className="graph-wrap">
      <svg
        width={width}
        height={height}
        viewBox={`0 0 ${width} ${height}`}
        role="img"
        aria-label="Job dependency graph"
      >
        <defs>
          <marker
            id="arrow"
            viewBox="0 0 8 8"
            refX="7"
            refY="4"
            markerWidth="6"
            markerHeight="6"
            orient="auto-start-reverse"
          >
            <path d="M 0 1 L 7 4 L 0 7 z" fill="var(--border-strong)" />
          </marker>
        </defs>

        {edges.map((edge, i) => {
          const from = pos.get(edge.from);
          const to = pos.get(edge.to);
          if (!from || !to) return null;
          const x1 = from.x + NODE_W;
          const y1 = from.y + NODE_H / 2;
          const x2 = to.x;
          const y2 = to.y + NODE_H / 2;
          const mid = (x2 - x1) / 2;
          return (
            <path
              key={`${edge.from}-${edge.to}-${i}`}
              className={`graph-edge${edge.implicit ? " implicit" : ""}`}
              d={`M ${x1} ${y1} C ${x1 + mid} ${y1}, ${x2 - mid} ${y2}, ${x2} ${y2}`}
              markerEnd="url(#arrow)"
            />
          );
        })}

        {layers.flatMap((layer) =>
          layer.map((name) => {
            const p = pos.get(name);
            if (!p) return null;
            const job = byName.get(name);
            const status = job?.status ?? "queued";
            const isSelected = selected === name;
            return (
              <g
                key={name}
                className="graph-node"
                transform={`translate(${p.x}, ${p.y})`}
                onClick={() => onSelect?.(name)}
              >
                <rect
                  width={NODE_W}
                  height={NODE_H}
                  rx={7}
                  fill={`var(--st-${status}-bg)`}
                  stroke={isSelected ? "var(--accent)" : `var(--st-${status})`}
                  strokeWidth={isSelected ? 2 : 1}
                />
                {/* A status stripe on the leading edge keeps the state readable
                    even when the fill wash is subtle. */}
                <rect width={3} height={NODE_H} rx={1.5} fill={`var(--st-${status})`} />
                <text
                  x={13}
                  y={19}
                  fontSize={12.5}
                  fontWeight={570}
                  fill="var(--fg)"
                  clipPath="inset(0 0 0 0)"
                >
                  {name.length > 20 ? `${name.slice(0, 19)}…` : name}
                </text>
                <text x={13} y={34} fontSize={11} fill={`var(--st-${status})`}>
                  {statusLabel(status)}
                </text>
                {job && job.duration_ms > 0 && (
                  <text x={NODE_W - 11} y={34} fontSize={10.5} fill="var(--fg-subtle)" textAnchor="end">
                    {formatDuration(job.duration_ms)}
                  </text>
                )}
              </g>
            );
          }),
        )}
      </svg>
    </div>
  );
}

/* ---------- log viewer ---------- */

/**
 * LogViewer renders captured output with line numbers, highlighting the command
 * echoes that mark the boundary between steps.
 *
 * It follows the tail while the job is running, but stops following as soon as
 * the reader scrolls up — pinning someone to the bottom while they are trying to
 * read an earlier failure is the classic mistake here.
 */
export function LogViewer({
  content,
  stream,
  follow,
  empty,
}: {
  content: string;
  stream: "stdout" | "stderr";
  follow: boolean;
  empty?: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const pinned = useRef(true);

  useEffect(() => {
    const el = ref.current;
    if (!el || !follow || !pinned.current) return;
    el.scrollTop = el.scrollHeight;
  }, [content, follow]);

  const onScroll = () => {
    const el = ref.current;
    if (!el) return;
    // A small tolerance keeps rounding from unpinning a view that is at the end.
    pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
  };

  const lines = splitLogLines(content);

  return (
    <div className={`logs log-${stream}`} ref={ref} onScroll={onScroll}>
      {lines.length === 0 ? (
        <div className="log-empty">{empty ?? "No output captured."}</div>
      ) : (
        lines.map((line) => (
          <div className="log-line" key={line.n}>
            <span className="log-num">{line.n}</span>
            <span className={`log-text${line.command ? " log-cmd" : ""}`}>{line.text || " "}</span>
          </div>
        ))
      )}
    </div>
  );
}

/* ---------- misc ---------- */

export function Crumbs({ items }: { items: { label: string; to?: string }[] }) {
  return (
    <nav className="crumbs">
      {items.map((item, i) => (
        <span key={`${item.label}-${i}`} className="row" style={{ gap: 7 }}>
          {i > 0 && <span className="subtle">/</span>}
          {item.to ? <Link to={item.to}>{item.label}</Link> : <span className="fg">{item.label}</span>}
        </span>
      ))}
    </nav>
  );
}

/** DurationBars is a compact trend of recent run durations, coloured by outcome. */
export function DurationBars({
  points,
}: {
  points: { run_id: number; duration_ms: number; status: Status; pipeline: string; number: number }[];
}) {
  if (points.length === 0) {
    return <div className="muted small">No finished runs yet.</div>;
  }
  const max = Math.max(...points.map((p) => p.duration_ms), 1);
  return (
    <div className="bars">
      {points.map((p) => (
        <Link
          key={p.run_id}
          to={`/runs/${p.run_id}`}
          className={`bar ${p.status}`}
          style={{ height: `${Math.max(4, (p.duration_ms / max) * 100)}%` }}
          title={`${p.pipeline} #${p.number} — ${formatDuration(p.duration_ms)} (${p.status})`}
        />
      ))}
    </div>
  );
}
