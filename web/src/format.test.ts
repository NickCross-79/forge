import { describe, expect, it } from "vitest";
import {
  formatBytes,
  formatDuration,
  formatRelative,
  isActive,
  isTerminal,
  percent,
  pluralise,
  splitLogLines,
  statusLabel,
} from "./format";

describe("formatDuration", () => {
  it("renders sub-second values in milliseconds", () => {
    expect(formatDuration(1)).toBe("1ms");
    expect(formatDuration(999)).toBe("999ms");
  });

  it("renders seconds with precision only where it helps", () => {
    expect(formatDuration(1500)).toBe("1.5s");
    expect(formatDuration(45000)).toBe("45s");
  });

  it("renders minutes and hours", () => {
    expect(formatDuration(90_000)).toBe("1m 30s");
    expect(formatDuration(3_930_000)).toBe("1h 5m");
  });

  it("treats missing and non-positive values as unknown", () => {
    expect(formatDuration(0)).toBe("—");
    expect(formatDuration(undefined)).toBe("—");
    expect(formatDuration(null)).toBe("—");
    expect(formatDuration(-5)).toBe("—");
  });
});

describe("formatBytes", () => {
  it("uses binary units", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(1024)).toBe("1.0 KiB");
    expect(formatBytes(1536)).toBe("1.5 KiB");
    expect(formatBytes(1024 * 1024)).toBe("1.0 MiB");
    expect(formatBytes(1024 ** 3)).toBe("1.0 GiB");
  });
});

describe("formatRelative", () => {
  const now = new Date("2026-01-15T12:00:00Z").getTime();
  const ago = (ms: number) => new Date(now - ms).toISOString();

  it("describes recent times as ages", () => {
    expect(formatRelative(ago(5_000), now)).toBe("just now");
    expect(formatRelative(ago(5 * 60_000), now)).toBe("5m ago");
    expect(formatRelative(ago(3 * 3_600_000), now)).toBe("3h ago");
    expect(formatRelative(ago(2 * 86_400_000), now)).toBe("2d ago");
  });

  it("falls back to a date for anything older than a month", () => {
    expect(formatRelative(ago(60 * 86_400_000), now)).not.toContain("ago");
  });

  it("handles missing and malformed input", () => {
    expect(formatRelative(undefined, now)).toBe("—");
    expect(formatRelative("not a date", now)).toBe("—");
  });

  it("never reports a future timestamp as a negative age", () => {
    expect(formatRelative(new Date(now + 10_000).toISOString(), now)).toBe("just now");
  });
});

describe("status helpers", () => {
  it("classifies terminal and active states", () => {
    for (const s of ["success", "failed", "cancelled", "skipped"] as const) {
      expect(isTerminal(s)).toBe(true);
      expect(isActive(s)).toBe(false);
    }
    for (const s of ["queued", "running", "retrying", "awaiting_manual"] as const) {
      expect(isTerminal(s)).toBe(false);
      expect(isActive(s)).toBe(true);
    }
  });

  it("spells out the manual gate", () => {
    expect(statusLabel("awaiting_manual")).toBe("awaiting approval");
    expect(statusLabel("success")).toBe("success");
  });
});

describe("percent and pluralise", () => {
  it("rounds ratios", () => {
    expect(percent(1)).toBe("100%");
    expect(percent(0.666)).toBe("67%");
    expect(percent(0)).toBe("0%");
    expect(percent(undefined)).toBe("0%");
  });

  it("agrees in number", () => {
    expect(pluralise(1, "run")).toBe("1 run");
    expect(pluralise(2, "run")).toBe("2 runs");
    expect(pluralise(0, "run")).toBe("0 runs");
    expect(pluralise(2, "entry", "entries")).toBe("2 entries");
  });
});

describe("splitLogLines", () => {
  it("numbers lines from one", () => {
    const lines = splitLogLines("a\nb\nc\n");
    expect(lines).toHaveLength(3);
    expect(lines[0]).toEqual({ n: 1, text: "a", command: false });
    expect(lines[2]).toEqual({ n: 3, text: "c", command: false });
  });

  it("does not invent a trailing blank line", () => {
    expect(splitLogLines("only\n")).toHaveLength(1);
    expect(splitLogLines("only")).toHaveLength(1);
  });

  it("flags command echoes so they can be styled as structure", () => {
    const lines = splitLogLines('$ echo hi\nhi\n');
    expect(lines[0]?.command).toBe(true);
    expect(lines[1]?.command).toBe(false);
  });

  it("preserves interior blank lines", () => {
    expect(splitLogLines("a\n\nb\n").map((l) => l.text)).toEqual(["a", "", "b"]);
  });

  it("returns nothing for empty content", () => {
    expect(splitLogLines("")).toEqual([]);
  });
});
