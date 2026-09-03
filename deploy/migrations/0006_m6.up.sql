-- M6: manifest, multi-project (development-plan.md §5/§7 M6). Additive
-- over 0001-0005, per .claude/CLAUDE.md's never-edit-a-shipped-migration
-- convention.

-- The compiled .agentfleet/project.yaml (internal/domain/manifest.Manifest,
-- JSON-marshaled), NOT the raw YAML — internal/api parses and validates on
-- write (POST /v1/projects, PUT /v1/projects/{slug}/manifest), so every
-- reader gets a manifest that has already passed schema + cross-field
-- validation, never a string it has to re-parse. Default '{}' means "no
-- agents declared" (Manifest's own zero value) so a project registered
-- before M6 (or a test that never sets one) still round-trips through
-- manifest.Parse-shaped code without a NULL special case.
ALTER TABLE project ADD COLUMN manifest jsonb NOT NULL DEFAULT '{}'::jsonb;

-- Which prompts/<role>@vN this run launched with — an audit column, not a
-- resolution key (internal/domain/prompts.Get is the resolver). NULL for a
-- run launched before M6 or for a manifest-less project's fallback launch.
ALTER TABLE run ADD COLUMN prompt_version text;
