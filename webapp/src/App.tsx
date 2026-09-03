import { useState } from "react";
import { getToken, setToken } from "./api";
import Approvals from "./views/Approvals";
import Live from "./views/Live";
import Cost from "./views/Cost";
import Metrics from "./views/Metrics";

const TABS = ["Approvals", "Live", "Cost", "Metrics"] as const;
type Tab = (typeof TABS)[number];

function TokenGate({ onReady }: { onReady: () => void }) {
  const [input, setInput] = useState("");

  return (
    <div className="gate">
      <h1>AgentFleet</h1>
      <p className="muted">Paste the control plane's ADMIN_TOKEN.</p>
      <input
        type="password"
        value={input}
        onChange={(e) => setInput(e.target.value)}
        placeholder="admin token"
        onKeyDown={(e) => {
          if (e.key === "Enter" && input) {
            setToken(input);
            onReady();
          }
        }}
      />
      <button
        className="action"
        disabled={!input}
        onClick={() => {
          setToken(input);
          onReady();
        }}
      >
        Continue
      </button>
    </div>
  );
}

export default function App() {
  const [hasToken, setHasToken] = useState(() => !!getToken());
  const [tab, setTab] = useState<Tab>("Live");

  if (!hasToken) {
    return <TokenGate onReady={() => setHasToken(true)} />;
  }

  return (
    <>
      <h1>AgentFleet</h1>
      <nav>
        {TABS.map((t) => (
          <button key={t} className={t === tab ? "active" : ""} onClick={() => setTab(t)}>
            {t}
          </button>
        ))}
      </nav>
      {tab === "Approvals" && <Approvals onUnauthorized={() => setHasToken(false)} />}
      {tab === "Live" && <Live onUnauthorized={() => setHasToken(false)} />}
      {tab === "Cost" && <Cost onUnauthorized={() => setHasToken(false)} />}
      {tab === "Metrics" && <Metrics onUnauthorized={() => setHasToken(false)} />}
    </>
  );
}
