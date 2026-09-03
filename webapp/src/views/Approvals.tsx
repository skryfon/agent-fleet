import { useEffect, useState } from "react";
import { api, UnauthorizedError } from "../api";

interface PendingApproval {
  task_id: string;
  title: string;
  intent: string;
  lane: string;
  role: string | null;
  feature_slug: string;
  zulip_topic: string | null;
  project_slug: string;
  artifact: { kind: string; uri: string; sha256: string } | null;
}

// short renders enough of a sha256 to eyeball a match without wrapping the
// card layout.
function short(sha: string): string {
  return sha.slice(0, 12);
}

export default function Approvals({ onUnauthorized }: { onUnauthorized: () => void }) {
  const [approvals, setApprovals] = useState<PendingApproval[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Per-task 409 flag: the artifact changed since this loaded — never
  // retried automatically, since that response IS the hash-binding
  // invariant working (internal/api/approvals.go), not a transient error.
  const [stale, setStale] = useState<Set<string>>(new Set());

  async function load() {
    try {
      setApprovals(await api<PendingApproval[]>("/v1/approvals/pending"));
    } catch (e) {
      if (e instanceof UnauthorizedError) return onUnauthorized();
      setError(String(e));
    }
  }

  useEffect(() => {
    load();
  }, []);

  async function decide(a: PendingApproval, decision: "APPROVED" | "REJECTED") {
    if (!a.artifact) return;

    try {
      await api("/v1/approvals", {
        method: "POST",
        body: JSON.stringify({
          subject_kind: "pr",
          subject_ref: a.artifact.uri,
          sha256: a.artifact.sha256,
          decision,
        }),
      });
      await load();
    } catch (e) {
      if (e instanceof UnauthorizedError) return onUnauthorized();

      if (String(e).startsWith("409")) {
        setStale((prev) => new Set(prev).add(a.task_id));
        return;
      }

      setError(String(e));
    }
  }

  if (error) return <p className="error">{error}</p>;
  if (!approvals) return <p className="muted">Loading…</p>;
  if (approvals.length === 0) return <p className="muted">Nothing waiting on review.</p>;

  return (
    <div>
      {approvals.map((a) => (
        <div className="card" key={a.task_id}>
          <div className="row" style={{ justifyContent: "space-between" }}>
            <strong>{a.title}</strong>
            <span className="muted">
              {a.project_slug}/{a.feature_slug} · {a.lane}
              {a.role ? ` · ${a.role}` : ""}
            </span>
          </div>
          <p>{a.intent}</p>
          {a.artifact ? (
            <p className="muted">
              <a href={a.artifact.uri} target="_blank" rel="noreferrer">
                {a.artifact.uri}
              </a>{" "}
              · {short(a.artifact.sha256)}
            </p>
          ) : (
            <p className="muted">no artifact recorded yet</p>
          )}
          {stale.has(a.task_id) && (
            <p className="error">the artifact changed since this loaded — refresh</p>
          )}
          <div className="row">
            <button className="action" disabled={!a.artifact} onClick={() => decide(a, "APPROVED")}>
              Approve
            </button>
            <button className="action danger" disabled={!a.artifact} onClick={() => decide(a, "REJECTED")}>
              Reject
            </button>
          </div>
        </div>
      ))}
    </div>
  );
}
