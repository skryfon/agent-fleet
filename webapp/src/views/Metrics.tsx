import { useEffect, useState } from "react";
import { api, UnauthorizedError } from "../api";

interface SummaryResponse {
  drift: { deviations: number; tasks: number; rate: number };
  questions: { questions: number; per_run: number; per_feature: number };
  violations: number;
  cost_total_usd: number;
}

function Stat({ label, value, meaning }: { label: string; value: string; meaning: string }) {
  return (
    <div className="card metric">
      <div className="muted">{label}</div>
      <div className="value">{value}</div>
      <div className="muted">{meaning}</div>
    </div>
  );
}

export default function Metrics({ onUnauthorized }: { onUnauthorized: () => void }) {
  const [data, setData] = useState<SummaryResponse | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api<SummaryResponse>("/v1/metrics/summary")
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
    <div className="row" style={{ flexWrap: "wrap" }}>
      <Stat
        label="Drift rate"
        value={data.drift.rate.toFixed(2)}
        meaning="deviations per task — rising means specs are too thin"
      />
      <Stat
        label="Questions / run"
        value={data.questions.per_run.toFixed(2)}
        meaning="rising during implementation means planning underperformed"
      />
      <Stat
        label="Questions / feature"
        value={data.questions.per_feature.toFixed(2)}
        meaning="same signal, aggregated per feature"
      />
      <Stat label="Policy violations" value={String(data.violations)} meaning="should trend to zero" />
      <Stat
        label="Total cost"
        value={`$${data.cost_total_usd.toFixed(2)}`}
        meaning="see the Cost tab for the per-merged-PR number"
      />
    </div>
  );
}
