# Feature: widget

```yaml agentfleet-tasks
version: v1
tasks:
  - external_ref: A
    lane: direct
    title: A
    intent: a
    depends_on: [B]
  - external_ref: B
    lane: direct
    title: B
    intent: b
    depends_on: [A]
```
