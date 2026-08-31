-- Initial schema, per development-plan.md §3.

CREATE TABLE project (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug          text NOT NULL UNIQUE,
    manifest_ref  text NOT NULL,
    manifest_hash text NOT NULL,
    repos         text[] NOT NULL DEFAULT '{}',
    status        text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE feature (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid NOT NULL REFERENCES project (id),
    slug        text NOT NULL,
    spec_ref    text,
    zulip_topic text,
    state       text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, slug)
);

CREATE TABLE task (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    feature_id           uuid NOT NULL REFERENCES feature (id),
    external_ref         text,
    lane                 text NOT NULL,
    title                text NOT NULL,
    intent               text NOT NULL,
    acceptance_criteria  jsonb NOT NULL DEFAULT '[]',
    touches              text[] NOT NULL DEFAULT '{}',
    depends_on           uuid[] NOT NULL DEFAULT '{}',
    spec_refs            jsonb NOT NULL DEFAULT '[]', -- [{path, anchor, sha256}]
    state                text NOT NULL,
    assignee             text,
    created_at           timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE run (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id            uuid NOT NULL REFERENCES task (id),
    parent_run_id      uuid REFERENCES run (id),
    role               text NOT NULL,
    model              text NOT NULL,
    container_id       text,
    dsh_session_id     text,
    state              text NOT NULL,
    checkpoint         jsonb,
    started_at         timestamptz,
    ended_at           timestamptz,
    tokens_in          bigint NOT NULL DEFAULT 0,
    tokens_out         bigint NOT NULL DEFAULT 0,
    cost_usd           numeric(12, 4) NOT NULL DEFAULT 0,
    created_at         timestamptz NOT NULL DEFAULT now()
);

-- Append-only: idempotent mirror of dsh session/event plus control-plane
-- decisions (development-plan.md D1). Never update or delete rows here.
CREATE TABLE event (
    id      bigserial PRIMARY KEY,
    run_id  uuid REFERENCES run (id),
    task_id uuid REFERENCES task (id),
    seq     bigint NOT NULL,
    kind    text NOT NULL,
    actor   text NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}',
    at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX event_run_id_seq_idx ON event (run_id, seq);

CREATE TABLE artifact (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id    uuid NOT NULL REFERENCES task (id),
    kind       text NOT NULL,
    uri        text NOT NULL,
    sha256     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE question (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id      uuid NOT NULL REFERENCES run (id),
    task_id     uuid NOT NULL REFERENCES task (id),
    kind        text NOT NULL,
    body        text NOT NULL,
    options     jsonb NOT NULL DEFAULT '[]',
    addressee   text,
    state       text NOT NULL,
    answer      text,
    answered_by text,
    asked_at    timestamptz NOT NULL DEFAULT now(),
    answered_at timestamptz
);

-- subject_sha256 is mandatory: approvals bind to content, not concepts
-- (development-plan.md §3).
CREATE TABLE approval (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_kind   text NOT NULL,
    subject_ref    text NOT NULL,
    subject_sha256 text NOT NULL,
    decision       text NOT NULL,
    actor          text NOT NULL,
    decided_at     timestamptz NOT NULL DEFAULT now(),
    note           text
);

CREATE TABLE budget (
    scope_kind      text NOT NULL,
    scope_id        uuid NOT NULL,
    usd_cap         numeric(12, 4) NOT NULL,
    minute_cap      integer NOT NULL,
    question_cap    integer NOT NULL,
    usd_spent       numeric(12, 4) NOT NULL DEFAULT 0,
    minutes_spent   integer NOT NULL DEFAULT 0,
    questions_asked integer NOT NULL DEFAULT 0,
    PRIMARY KEY (scope_kind, scope_id)
);

CREATE TABLE identity (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind          text NOT NULL,
    display_name  text NOT NULL,
    zulip_user_id text,
    github_login  text,
    role          text NOT NULL
);

-- Transactional outbox: state changes and their outbound side effects
-- commit in one transaction (development-plan.md §3).
CREATE TABLE outbox (
    id           bigserial PRIMARY KEY,
    topic        text NOT NULL,
    payload      jsonb NOT NULL,
    published_at timestamptz
);
CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL;
