# Feature: widget

Some human prose describing the feature. Ignored by the parser.

```yaml agentfleet-tasks
version: v1
tasks:
  - external_ref: T1
    lane: spec
    title: Do the thing
    intent: because reasons
  - external_ref: T2
    lane: direct
    title: Depends on T1
    intent: x
    depends_on: [T1]
```

More prose after the block.
