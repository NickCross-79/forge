/** Data-fetching hooks: polling for snapshots, SSE for live updates. */

import { useCallback, useEffect, useRef, useState } from "react";
import { streamUrl } from "./api";

export interface Fetched<T> {
  data: T | null;
  error: unknown;
  loading: boolean;
  reload: () => void;
}

/**
 * useFetch loads a value and optionally re-polls it.
 *
 * Polling is the fallback for views that have no live stream (lists, metrics).
 * Detail views layer SSE on top and use a slow poll only as a safety net, so a
 * missed event still converges instead of leaving a stale screen.
 */
export function useFetch<T>(
  load: () => Promise<T>,
  deps: unknown[],
  intervalMs?: number,
): Fetched<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [loading, setLoading] = useState(true);
  const [nonce, setNonce] = useState(0);

  // Keeping the loader in a ref lets callers pass an inline closure without
  // making every render re-trigger the effect.
  const loadRef = useRef(load);
  loadRef.current = load;

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    let cancelled = false;

    const run = async () => {
      try {
        const result = await loadRef.current();
        if (cancelled) return;
        setData(result);
        setError(null);
      } catch (err) {
        if (!cancelled) setError(err);
      } finally {
        if (!cancelled) setLoading(false);
      }
    };

    void run();

    if (!intervalMs) return () => { cancelled = true; };

    const timer = setInterval(() => void run(), intervalMs);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce, intervalMs]);

  return { data, error, loading, reload };
}

/**
 * useEventStream subscribes to a run's SSE timeline and calls onEvent for each
 * transition.
 *
 * The connection is only opened while the run is live. EventSource reconnects on
 * its own and replays from Last-Event-ID, so a dropped connection resumes
 * without duplicating events.
 */
export function useEventStream(
  path: string | null,
  onEvent: (data: unknown) => void,
  onDone?: () => void,
): { connected: boolean } {
  const [connected, setConnected] = useState(false);

  const eventRef = useRef(onEvent);
  eventRef.current = onEvent;
  const doneRef = useRef(onDone);
  doneRef.current = onDone;

  useEffect(() => {
    if (!path) {
      setConnected(false);
      return;
    }

    const source = new EventSource(streamUrl(path));

    source.addEventListener("open", () => setConnected(true));

    const handle = (e: MessageEvent) => {
      // Receiving a frame is itself proof the stream is open. Relying on the
      // `open` event alone left the indicator reading "connecting" while output
      // was visibly arriving.
      setConnected(true);
      try {
        eventRef.current(JSON.parse(e.data));
      } catch {
        /* a malformed frame is not worth tearing the stream down for */
      }
    };
    source.addEventListener("event", handle as EventListener);
    source.addEventListener("log", handle as EventListener);

    source.addEventListener("done", () => {
      setConnected(false);
      source.close();
      doneRef.current?.();
    });

    source.addEventListener("error", () => {
      // EventSource retries by itself; reflect the gap in the UI meanwhile.
      setConnected(false);
    });

    return () => {
      source.close();
      setConnected(false);
    };
  }, [path]);

  return { connected };
}

/**
 * useLogStream accumulates a job's output from SSE.
 *
 * Chunks are appended in arrival order, and the server sends byte offsets, so
 * the text assembles exactly as it was written even across a reconnect.
 */
export function useLogStream(
  jobId: number | null,
  stream: "stdout" | "stderr",
  live: boolean,
  initial: string,
): { content: string; connected: boolean } {
  const [chunks, setChunks] = useState<string>("");

  // Reset whenever the source changes, so switching tabs cannot show the
  // previous stream's tail.
  useEffect(() => {
    setChunks("");
  }, [jobId, stream]);

  const path = live && jobId ? `/jobs/${jobId}/logs/stream?stream=${stream}` : null;

  const { connected } = useEventStream(path, (data) => {
    const payload = data as { content?: string; stream?: string };
    if (payload.content) setChunks((prev) => prev + payload.content);
  });

  // While streaming, SSE is the whole picture: the handler starts from offset 0
  // and delivers everything already on disk before tailing.
  return { content: path ? chunks : initial, connected };
}

/** useTitle keeps the document title in step with the current view. */
export function useTitle(title: string): void {
  useEffect(() => {
    document.title = title ? `${title} · forge` : "forge";
  }, [title]);
}
