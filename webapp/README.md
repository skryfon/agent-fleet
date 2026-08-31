# webapp/

Approval queue and cost dashboards (development-plan.md §7 M7). `dsh web`
covers live run view, transcripts, and diffs for free — this app is
deliberately scoped to what it doesn't cover.

Not scaffolded yet: M7 is late in the sequence (§7), and building a React
app now would be speculative ahead of the control-plane API it depends on.
Scaffold this once M2's `/v1/events` SSE endpoint and `/v1/approvals` exist.
