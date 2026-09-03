import { useEffect, useState } from "react";
import { api, UnauthorizedError } from "../api";

interface CostGroup {
  label: string;
  cost_usd: number;
  tokens_in: number;
  tokens_out: number;
  runs: number;
}

interface CostResponse {
  by_feature: CostGroup[];
  by_role: CostGroup[];
  by_model: CostGroup[];
  total_usd: number;
  per_merged_pr: number;
}

function usd(n: number): string {
  return `$${n.toFixed(2)}`;
}

// Bars scale to the biggest row in their own group — a plain CSS width%,
// no chart library.
function Bars({ title, rows }: { title: string; rows: CostGroup[] }) {
  const max = Math.max(1, ...rows.map((r) => r.cost_usd));

  return (
    <div className="card">
      <strong>{title}</strong>
      <table>
        <tbody>
          {rows.map((r) => (
            <tr key={r.label}>
              <td>{r.label}</td>
              <td style={{ width: "40%" }}>
                <div className="bar-track">
                  <div className="bar-fill" style={{ width: `${(r.cost_usd / max) * 100}%` }} />
                </div>
              </td>
              <td className="muted">{usd(r.cost_usd)}</td>
              <td className="muted">{r.runs} runs</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export default function Cost({ onUnauthorized }: { onUnauthorized: () => void }) {
  const [data, setData] = useState<CostResponse | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api<CostResponse>("/v1/metrics/cost")
      .then(setData)
      .catch((e) => {
        if (e instanceof UnauthorizedError) return onUnauthorized();
        setError(String(e));
      });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  if (error) return <p className="error">{error}</p>;
  if (!data) return <p className="muted">Loading…</p>;

  return (
    <div>
      <div className="card row" style={{ justifyContent: "space-between" }}>
        <div className="metric">
          <div className="muted">total spend</div>
          <div className="value">{usd(data.total_usd)}</div>
        </div>
        <div className="metric">
          <div className="muted">cost per merged PR — §11's only cost number that means anything</div>
          <div className="value">{usd(data.per_merged_pr)}</div>
        </div>
      </div>
      <Bars title="By feature" rows={data.by_feature} />
      <Bars title="By role" rows={data.by_role} />
      <Bars title="By model" rows={data.by_model} />
    </div>
  );
}
