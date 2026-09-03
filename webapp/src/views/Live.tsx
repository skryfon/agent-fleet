import { useEffect, useRef, useState } from "react";
import { api, streamEvents, UnauthorizedError, type SSEFrame } from "../api";

interface RunEvent {
  id: number;
  run_id: string | null;
  task_id: string | null;
  kind: string;
  actor: string;
  at: string;
}

interface Run {
  id: string;
  task_id: string;
  role: string;
  model: string;
  state: string;
}

// MAX_EVENTS: this tab is meant to stay open all day (development-plan.md
// §7's done-condition) — an unbounded array here is a slow memory leak by
// the afternoon, so the oldest events fall off once the tail gets long.
const MAX_EVENTS = 500;
const FLAGGED = new Set(["policy_violation", "budget_breached"]);

export default function Live({ onUnauthorized }: { onUnauthorized: () => void }) {
  const [events, setEvents] = useState<RunEvent[]>([]);
  const [runs, setRuns] = useState<Run[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [runFilter, setRunFilter] = useState("");
  const [kindFilter, setKindFilter] = useState("");
  const lastAt = useRef<string | undefined>(undefined);

  useEffect(() => {
    const controller = new AbortController();

    function onEvent(frame: SSEFrame) {
      try {
        const ev = JSON.parse(frame.data) as RunEvent;
        lastAt.current = ev.at;
        setEvents((prev) => [...prev.slice(-(MAX_EVENTS - 1)), ev]);
      } catch {
        // keep-alive comment lines and malformed frames are silently dropped
      }
    }

    streamEvents(undefined, onEvent, controller.signal).catch((e) => {
      if (e instanceof UnauthorizedError) return onUnauthorized();
      if (controller.signal.aborted) return;
      setError(String(e));
    });

    return () => controller.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    let cancelled = false;

    async function poll() {
      try {
        const r = await api<Run[]>("/v1/runs");
        if (!cancelled) setRuns(r);
      } catch (e) {
        if (e instanceof UnauthorizedError) return onUnauthorized();
      }
    }

    poll();
    const id = setInterval(poll, 5000);

    return () => {
      cancelled = true;
      clearInterval(id);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const filtered = events.filter(
    (ev) =>
      (!runFilter || ev.run_id === runFilter) && (!kindFilter || ev.kind.includes(kindFilter)),
  );

  return (
    <div>
      {error && <p className="error">{error}</p>}

      <div className="card">
        <strong>Active runs</strong>
        <table>
          <thead>
            <tr>
              <th>role</th>
              <th>model</th>
              <th>state</th>
              <th>run</th>
            </tr>
          </thead>
          <tbody>
            {runs.map((r) => (
              <tr key={r.id}>
                <td>{r.role}</td>
                <td>{r.model}</td>
                <td>{r.state}</td>
                <td className="muted">{r.id.slice(0, 8)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <div className="row" style={{ marginBottom: "0.5rem" }}>
        <input placeholder="filter by run id" value={runFilter} onChange={(e) => setRunFilter(e.target.value)} />
        <input placeholder="filter by kind" value={kindFilter} onChange={(e) => setKindFilter(e.target.value)} />
      </div>

      <table>
        <thead>
          <tr>
            <th>at</th>
            <th>kind</th>
            <th>actor</th>
            <th>run</th>
          </tr>
        </thead>
        <tbody>
          {[...filtered].reverse().map((ev) => (
            <tr key={ev.id} className={FLAGGED.has(ev.kind) ? `flag-${ev.kind}` : ""}>
              <td className="muted">{new Date(ev.at).toLocaleTimeString()}</td>
              <td>{ev.kind}</td>
              <td>{ev.actor}</td>
              <td className="muted">{ev.run_id?.slice(0, 8) ?? "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
