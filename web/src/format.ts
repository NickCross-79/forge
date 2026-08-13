/** Formatting helpers shared by every view, kept pure so they can be tested. */

import type { Status } from "./api";

/** formatDuration renders a millisecond count the way a person reads a clock. */
export function formatDuration(ms: number | undefined | null): string {
  if (!ms || ms <= 0) return "—";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const seconds = ms / 1000;
  if (seconds < 60) return `${seconds.toFixed(seconds < 10 ? 1 : 0)}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${Math.round(seconds % 60)}s`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h ${minutes % 60}m`;
}

/** formatBytes renders a byte count in binary units. */
export function formatBytes(bytes: number | undefined | null): string {
  if (!bytes || bytes <= 0) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value.toFixed(unit === 0 ? 0 : 1)} ${units[unit]}`;
}

/**
 * formatRelative renders a timestamp as an age. Run lists are read as "what
 * happened recently", so an age is more useful than a wall-clock time.
 */
export function formatRelative(iso: string | undefined | null, now = Date.now()): string {
  if (!iso) return "—";
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "—";

  const diff = now - then;
  if (diff < 0) return "just now";

  const seconds = Math.floor(diff / 1000);
  if (seconds < 45) return "just now";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  return new Date(then).toLocaleDateString();
}

/** formatTimestamp renders an absolute local time, for detail views. */
export function formatTimestamp(iso: string | undefined | null): string {
  if (!iso) return "—";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "—";
  return date.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/** statusLabel turns a status into display text. */
export function statusLabel(status: Status): string {
  return status === "awaiting_manual" ? "awaiting approval" : status;
}

/** isTerminal reports whether a status will no longer change. */
export function isTerminal(status: Status): boolean {
  return ["success", "failed", "cancelled", "skipped"].includes(status);
}

/** isActive reports whether a status represents work pending or in flight. */
export function isActive(status: Status): boolean {
  return !isTerminal(status);
}

/** percent renders a 0..1 ratio. */
export function percent(ratio: number | undefined | null): string {
  if (!ratio || Number.isNaN(ratio)) return "0%";
  return `${Math.round(ratio * 100)}%`;
}

/** pluralise picks a suffix without needing a template at each call site. */
export function pluralise(count: number, singular: string, plural = `${singular}s`): string {
  return `${count} ${count === 1 ? singular : plural}`;
}

/**
 * splitLogLines turns raw captured output into renderable lines, flagging the
 * `$ command` echoes the executor writes so the viewer can style them as
 * structure rather than as output.
 */
export interface LogLine {
  n: number;
  text: string;
  command: boolean;
}

export function splitLogLines(content: string, startAt = 1): LogLine[] {
  if (!content) return [];
  const withoutTrailingNewline = content.endsWith("\n") ? content.slice(0, -1) : content;
  return withoutTrailingNewline.split("\n").map((text, i) => ({
    n: startAt + i,
    text,
    command: text.startsWith("$ "),
  }));
}
