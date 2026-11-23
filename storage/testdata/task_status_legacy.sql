-- Generated from the complete sqlite_master schema of a fresh Rust gateway database.
-- Exporter: target/rust-schema-export (calls gateway_backend::db::open from crates/gateway-backend/src/db.rs:928-930).
-- Rust task schema: crates/gateway-backend/src/db.rs:1130-1165; task reviews: crates/gateway-backend/src/task_reviews.rs:12-36;
-- task lifecycle journal/triggers: crates/gateway-backend/src/task_events.rs:181-302; router schema: crates/gateway-backend/src/router.rs:32-70;
-- task outbox/triggers: crates/gateway-backend/src/plugins/tasks.rs:68-136; cancellation table/migration: crates/gateway-backend/src/task_cancel.rs:21-93.
-- Legacy CHECK variants mirror crates/gateway-backend/src/task_cancel.rs:21-24 and full_schema_upgrade_keeps_history_and_lifecycle_triggers (428-500).
-- Test setup replaces only the current tasks.status CHECK with each historical CHECK before loading this full schema.
CREATE TABLE agent_confirmations (
            agent_id       TEXT NOT NULL,
            project_ident  TEXT NOT NULL,
            message_id     INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
            confirmed_at   INTEGER NOT NULL,
            PRIMARY KEY (agent_id, project_ident, message_id)
        );
CREATE TABLE agents (
            project_ident  TEXT NOT NULL REFERENCES projects(ident),
            agent_id       TEXT NOT NULL,
            registered_at  INTEGER NOT NULL,
            PRIMARY KEY (project_ident, agent_id)
        );
CREATE TABLE artifact_actors (
            actor_id              TEXT PRIMARY KEY,
            actor_type            TEXT NOT NULL
                                  CHECK(actor_type IN ('user','agent','system')),
            agent_system          TEXT
                                  CHECK(agent_system IS NULL OR agent_system IN ('claude','codex','gemini','other')),
            agent_system_label    TEXT,
            agent_id              TEXT,
            host                  TEXT,
            display_name          TEXT NOT NULL,
            runtime_metadata      TEXT,
            created_at            INTEGER NOT NULL,
            updated_at            INTEGER NOT NULL
        );
CREATE TABLE artifact_chunks (
            chunk_id                  TEXT PRIMARY KEY,
            artifact_id               TEXT NOT NULL REFERENCES artifacts(artifact_id) ON DELETE CASCADE,
            artifact_version_id       TEXT NOT NULL REFERENCES artifact_versions(artifact_version_id),
            child_address             TEXT NOT NULL,
            text                      TEXT NOT NULL,
            embedding_model           TEXT,
            embedding_vector          BLOB,
            -- Retrieval filters (T002 §Chunk retrieval filters).
            app                       TEXT,
            label                     TEXT,
            kind                      TEXT,
            metadata_json             TEXT,
            superseded_by_chunk_id    TEXT REFERENCES artifact_chunks(chunk_id),
            created_at                INTEGER NOT NULL
        );
CREATE TABLE artifact_comments (
            comment_id                    TEXT PRIMARY KEY,
            artifact_id                   TEXT NOT NULL REFERENCES artifacts(artifact_id) ON DELETE CASCADE,
            target_kind                   TEXT NOT NULL
                                           CHECK(target_kind IN ('artifact','artifact_version','contribution')),
            target_id                     TEXT NOT NULL,
            -- child_address is only valid when target_kind = artifact_version.
            -- See T002 §Comment lifecycle. Block-level targets deferred to v2.
            child_address                 TEXT,
            parent_comment_id             TEXT REFERENCES artifact_comments(comment_id),
            actor_id                      TEXT NOT NULL REFERENCES artifact_actors(actor_id),
            body                          TEXT NOT NULL,
            state                         TEXT NOT NULL DEFAULT 'open'
                                           CHECK(state IN ('open','resolved')),
            resolved_by_actor_id          TEXT REFERENCES artifact_actors(actor_id),
            resolved_by_workflow_run_id   TEXT REFERENCES workflow_runs(workflow_run_id),
            resolved_at                   INTEGER,
            resolution_note               TEXT,
            idempotency_key               TEXT,
            created_at                    INTEGER NOT NULL,
            updated_at                    INTEGER NOT NULL,
            CHECK (child_address IS NULL OR target_kind = 'artifact_version')
        );
CREATE TABLE artifact_contributions (
            contribution_id        TEXT PRIMARY KEY,
            artifact_id            TEXT NOT NULL REFERENCES artifacts(artifact_id) ON DELETE CASCADE,
            target_kind            TEXT NOT NULL
                                   CHECK(target_kind IN ('artifact','artifact_version','contribution')),
            target_id              TEXT NOT NULL,
            contribution_kind      TEXT NOT NULL
                                   CHECK(contribution_kind IN ('review','synthesis','decision','note','completion','state_transition')),
            phase                  TEXT,
            role                   TEXT NOT NULL
                                   CHECK(role IN ('author','reviewer','analyst','implementer','coordinator','user')),
            actor_id               TEXT NOT NULL REFERENCES artifact_actors(actor_id),
            workflow_run_id        TEXT REFERENCES workflow_runs(workflow_run_id),
            read_set               TEXT,
            body_format            TEXT NOT NULL
                                   CHECK(body_format = 'markdown'),
            body                   TEXT NOT NULL,
            idempotency_key        TEXT,
            created_at             INTEGER NOT NULL
        );
CREATE TABLE artifact_links (
            link_id                      TEXT PRIMARY KEY,
            link_type                    TEXT NOT NULL,
            source_kind                  TEXT NOT NULL
                                          CHECK(source_kind IN ('artifact','artifact_version','contribution','comment','task','chunk','pattern','memory','commit','external_url')),
            source_id                    TEXT NOT NULL,
            source_version_id            TEXT REFERENCES artifact_versions(artifact_version_id),
            source_child_address         TEXT,
            target_kind                  TEXT NOT NULL
                                          CHECK(target_kind IN ('artifact','artifact_version','contribution','comment','task','chunk','pattern','memory','commit','external_url')),
            target_id                    TEXT NOT NULL,
            target_version_id            TEXT REFERENCES artifact_versions(artifact_version_id),
            target_child_address         TEXT,
            created_by_actor_id          TEXT NOT NULL REFERENCES artifact_actors(actor_id),
            created_via_workflow_run_id  TEXT REFERENCES workflow_runs(workflow_run_id),
            idempotency_key              TEXT,
            supersedes_link_id           TEXT REFERENCES artifact_links(link_id),
            created_at                   INTEGER NOT NULL
        );
CREATE TABLE artifact_versions (
            artifact_version_id          TEXT PRIMARY KEY,
            artifact_id                  TEXT NOT NULL REFERENCES artifacts(artifact_id) ON DELETE CASCADE,
            version_label                TEXT,
            parent_version_id            TEXT REFERENCES artifact_versions(artifact_version_id),
            body_format                  TEXT NOT NULL
                                          CHECK(body_format = 'markdown'),
            body                         TEXT,
            structured_payload           TEXT,
            source_format                TEXT,
            created_by_actor_id          TEXT NOT NULL REFERENCES artifact_actors(actor_id),
            created_via_workflow_run_id  TEXT REFERENCES workflow_runs(workflow_run_id) DEFERRABLE INITIALLY DEFERRED,
            version_state                TEXT NOT NULL DEFAULT 'draft'
                                          CHECK(version_state IN ('draft','under_review','accepted','superseded','rejected')),
            idempotency_key              TEXT,
            -- T004 §3 retention: when a body is purged the bytes go to NULL
            -- and the timestamp is recorded. ids and audit links survive.
            body_purged_at               INTEGER,
            created_at                   INTEGER NOT NULL
        );
CREATE TABLE artifacts (
            artifact_id            TEXT PRIMARY KEY,
            project_ident          TEXT NOT NULL REFERENCES projects(ident),
            kind                   TEXT NOT NULL
                                   CHECK(kind IN ('design_review','spec')),
            subkind                TEXT,
            title                  TEXT NOT NULL,
            labels                 TEXT,
            -- T002 §State model: five independent state fields. None collapses.
            lifecycle_state        TEXT NOT NULL DEFAULT 'draft'
                                   CHECK(lifecycle_state IN ('draft','active','superseded','archived')),
            review_state           TEXT NOT NULL DEFAULT 'none'
                                   CHECK(review_state IN ('none','collecting_reviews','synthesizing','needs_user_decision','accepted','rejected')),
            implementation_state   TEXT NOT NULL DEFAULT 'not_applicable'
                                   CHECK(implementation_state IN ('not_applicable','not_started','in_progress','complete','blocked')),
            current_version_id     TEXT REFERENCES artifact_versions(artifact_version_id) DEFERRABLE INITIALLY DEFERRED,
            accepted_version_id    TEXT REFERENCES artifact_versions(artifact_version_id) DEFERRABLE INITIALLY DEFERRED,
            created_by_actor_id    TEXT NOT NULL REFERENCES artifact_actors(actor_id),
            created_at             INTEGER NOT NULL,
            updated_at             INTEGER NOT NULL
        );
CREATE TABLE assignment_offers (
            notify_uid TEXT PRIMARY KEY,
            task_id    TEXT NOT NULL,
            project    TEXT NOT NULL,
            session_id TEXT NOT NULL,
            reporter_source TEXT
        );
CREATE TABLE assignment_refusals (
            session_id TEXT PRIMARY KEY,
            output_at  INTEGER NOT NULL
        );
CREATE TABLE assignment_releases (
            task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
            session_id TEXT NOT NULL,
            instance_id TEXT NOT NULL,
            PRIMARY KEY(task_id,session_id,instance_id)
        );
CREATE TABLE assignment_turns (
            project     TEXT PRIMARY KEY,
            review_next INTEGER NOT NULL
        );
CREATE TABLE cursors (
            project_ident  TEXT PRIMARY KEY REFERENCES projects(ident),
            last_read_id   INTEGER NOT NULL DEFAULT 0,
            updated_at     INTEGER NOT NULL
        );
CREATE TABLE execution_attempts (
        run_id TEXT NOT NULL REFERENCES execution_runs(id) ON DELETE CASCADE,
        position INTEGER NOT NULL,
        body TEXT NOT NULL,
        PRIMARY KEY(run_id, position)
    );
CREATE TABLE execution_runs (
        id TEXT PRIMARY KEY,
        project_ident TEXT NOT NULL REFERENCES projects(ident),
        task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
        trigger TEXT NOT NULL,
        dedup_key TEXT UNIQUE NOT NULL,
        status TEXT NOT NULL,
        created_at INTEGER NOT NULL,
        finished_at INTEGER,
        output TEXT NOT NULL DEFAULT ''
    , started_at INTEGER, updated_at INTEGER, client TEXT, model TEXT, executor TEXT NOT NULL DEFAULT 'headless', progress TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '', summary_source TEXT NOT NULL DEFAULT 'missing', session_key TEXT, last_sequence INTEGER NOT NULL DEFAULT 0);
CREATE TABLE gateway_memories (
            id                 TEXT PRIMARY KEY,
            project_ident      TEXT NOT NULL REFERENCES projects(ident) ON DELETE CASCADE,
            server_revision    INTEGER NOT NULL,
            content            TEXT NOT NULL,
            memory_type        TEXT NOT NULL,
            tags               TEXT NOT NULL DEFAULT '[]',
            content_hash       TEXT NOT NULL,
            source_agent_id    TEXT,
            source_hostname    TEXT,
            source_client      TEXT,
            source_client_version TEXT,
            provenance_json    TEXT,
            client_created_at  INTEGER,
            client_updated_at  INTEGER,
            created_at         INTEGER NOT NULL,
            updated_at         INTEGER NOT NULL,
            tombstoned_at      INTEGER, concept_hash TEXT, okf_json TEXT,
            UNIQUE(project_ident, server_revision)
        );
CREATE TABLE gateway_memory_client_links (
            project_ident      TEXT NOT NULL REFERENCES projects(ident) ON DELETE CASCADE,
            client_id          TEXT NOT NULL,
            local_memory_id    TEXT NOT NULL,
            gateway_memory_id  TEXT NOT NULL REFERENCES gateway_memories(id) ON DELETE CASCADE,
            created_at         INTEGER NOT NULL,
            updated_at         INTEGER NOT NULL,
            PRIMARY KEY(project_ident, client_id, local_memory_id)
        );
CREATE TABLE inbound_cursors (
            project TEXT PRIMARY KEY REFERENCES projects(ident) ON DELETE CASCADE,
            event_seq INTEGER NOT NULL
        );
CREATE TABLE machine_checkouts (
            hostname      TEXT NOT NULL,
            project_ident TEXT NOT NULL,
            origin        TEXT,
            seen_at       INTEGER NOT NULL,
            PRIMARY KEY(hostname, project_ident)
        );
CREATE TABLE messages (
            id                   INTEGER PRIMARY KEY AUTOINCREMENT,
            project_ident        TEXT NOT NULL REFERENCES projects(ident),
            source               TEXT NOT NULL CHECK(source IN ('agent','user','system')),
            external_message_id  TEXT,
            content              TEXT NOT NULL,
            sent_at              INTEGER NOT NULL,
            deliver_to_agents    INTEGER NOT NULL DEFAULT 0
        , confirmed_at INTEGER, parent_message_id INTEGER, agent_id TEXT, message_type TEXT NOT NULL DEFAULT 'message', subject TEXT, hostname TEXT, event_at INTEGER, author_kind TEXT, resolved_at INTEGER, resolved_by TEXT);
CREATE TABLE notifier_processed (
            event_seq INTEGER PRIMARY KEY REFERENCES router_events(seq) ON DELETE CASCADE
         );
CREATE TABLE pattern_comments (
            id           TEXT PRIMARY KEY,
            pattern_id   TEXT NOT NULL REFERENCES patterns(id) ON DELETE CASCADE,
            author       TEXT NOT NULL,
            author_type  TEXT NOT NULL CHECK(author_type IN ('agent','user','system')),
            content      TEXT NOT NULL,
            created_at   INTEGER NOT NULL
        );
CREATE TABLE patterns (
            id          TEXT PRIMARY KEY,
            title       TEXT NOT NULL,
            slug        TEXT NOT NULL UNIQUE,
            summary     TEXT,
            body        TEXT NOT NULL,
            labels      TEXT,
            version     TEXT NOT NULL DEFAULT 'draft'
                        CHECK(version IN ('draft','latest','superseded')),
            state       TEXT NOT NULL DEFAULT 'active'
                        CHECK(state IN ('active','archived')),
            superseded_by TEXT,
            author      TEXT NOT NULL,
            created_at  INTEGER NOT NULL,
            updated_at  INTEGER NOT NULL
        );
CREATE TABLE presence_clock (
            id INTEGER PRIMARY KEY CHECK(id=1),
            observed_at INTEGER NOT NULL
        );
CREATE TABLE presence_sources (
            source TEXT PRIMARY KEY,
            heartbeat_seq INTEGER NOT NULL
        );
CREATE TABLE project_links (
            id             INTEGER PRIMARY KEY AUTOINCREMENT,
            project_ident  TEXT NOT NULL REFERENCES projects(ident) ON DELETE CASCADE,
            label          TEXT NOT NULL,
            url            TEXT NOT NULL,
            rank           INTEGER NOT NULL DEFAULT 0,
            created_at     INTEGER NOT NULL
         );
CREATE TABLE projects (
            ident         TEXT PRIMARY KEY,
            channel_name  TEXT NOT NULL,
            room_id       TEXT NOT NULL,
            last_msg_id   TEXT,
            created_at    INTEGER NOT NULL
        , repo_provider TEXT, repo_namespace TEXT, repo_name TEXT, repo_full_name TEXT, kind TEXT NOT NULL DEFAULT 'repo', archived_at INTEGER, canonical_remote TEXT);
CREATE TABLE reminder_nudges (
            task_id TEXT PRIMARY KEY,
            sent_at INTEGER NOT NULL
        );
CREATE TABLE reminder_restarts (
            source TEXT NOT NULL,
            session_id TEXT NOT NULL,
            instance_id TEXT NOT NULL,
            client_run TEXT NOT NULL,
            PRIMARY KEY(source,session_id,instance_id)
        );
CREATE TABLE remote_consumers (
            consumer_id   TEXT PRIMARY KEY,
            connection_id TEXT,
            cursor        INTEGER NOT NULL,
            last_seen_at  INTEGER NOT NULL
        );
CREATE TABLE retention_runs (
            id INTEGER PRIMARY KEY CHECK(id=1),
            completed_at INTEGER NOT NULL
        );
CREATE TABLE router_deliveries (
    event_seq       INTEGER NOT NULL REFERENCES router_events(seq) ON DELETE CASCADE,
    plugin          TEXT NOT NULL,
    state           TEXT NOT NULL CHECK(state IN
        ('pending','running','retrying','completed','stopped','skipped','failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    last_error      TEXT,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY(event_seq, plugin)
);
CREATE TABLE router_events (
            seq         INTEGER PRIMARY KEY AUTOINCREMENT,
            uid         TEXT NOT NULL UNIQUE,
            id          TEXT NOT NULL,
            version     INTEGER NOT NULL,
            payload     TEXT NOT NULL,
            source      TEXT NOT NULL,
            emitted_at  INTEGER NOT NULL,
            correlation TEXT
        );
CREATE TABLE router_task_outbox (
            id         INTEGER PRIMARY KEY AUTOINCREMENT,
            kind       TEXT NOT NULL,
            project    TEXT NOT NULL,
            task_json  TEXT NOT NULL,
            comment_json TEXT,
            actor_json TEXT,
            created_at INTEGER NOT NULL
        );
CREATE TABLE settings (
            key   TEXT PRIMARY KEY,
            value TEXT NOT NULL
        );
CREATE TABLE skills (
            name        TEXT PRIMARY KEY,
            zip_data    BLOB NOT NULL,
            size        INTEGER NOT NULL,
            checksum    TEXT NOT NULL,
            uploaded_at INTEGER NOT NULL
        , kind TEXT NOT NULL DEFAULT 'skill', content TEXT);
CREATE TABLE task_cancellations (
            task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
            project_ident TEXT NOT NULL, reason TEXT NOT NULL,
            prior_status TEXT NOT NULL, prior_owner_agent_id TEXT,
            prior_owner_origin_json TEXT, prior_started_at INTEGER,
            reporter_origin_json TEXT NOT NULL, cancelled_at INTEGER NOT NULL
        );
CREATE TABLE task_comments (
            id           TEXT PRIMARY KEY,
            task_id      TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
            author       TEXT NOT NULL,
            author_type  TEXT NOT NULL CHECK(author_type IN ('agent','user','system')),
            content      TEXT NOT NULL,
            created_at   INTEGER NOT NULL
        , origin_json TEXT, hostname TEXT);
CREATE TABLE task_completion_owners (
        task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
        origin_json TEXT NOT NULL
    );
CREATE TABLE task_confirmations (
        task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
        token TEXT NOT NULL, origin_json TEXT NOT NULL, snapshot_json TEXT NOT NULL,
        request_json TEXT NOT NULL, mutated INTEGER NOT NULL DEFAULT 0
    );
CREATE TABLE task_delegations (
            id                    TEXT PRIMARY KEY,
            source_project_ident  TEXT NOT NULL REFERENCES projects(ident),
            source_task_id        TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
            target_project_ident  TEXT NOT NULL REFERENCES projects(ident),
            target_task_id        TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
            requester_agent_id    TEXT,
            requester_hostname    TEXT,
            created_at            INTEGER NOT NULL,
            completed_at          INTEGER,
            completion_message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL
        );
CREATE TABLE task_dispatch_events (
        id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL,
        project_ident TEXT NOT NULL, instance_id TEXT NOT NULL, session_id TEXT NOT NULL,
        operation TEXT NOT NULL, authentication_json TEXT NOT NULL, document_json TEXT NOT NULL, created_at INTEGER NOT NULL
    );
CREATE TABLE task_dispatch_requests (
        project_ident TEXT NOT NULL, instance_id TEXT NOT NULL, session_id TEXT NOT NULL,
        request_id TEXT NOT NULL, fingerprint TEXT NOT NULL, response_json TEXT NOT NULL,
        PRIMARY KEY(project_ident,instance_id,session_id,request_id)
    );
CREATE TABLE task_dispatches (
        task_id TEXT PRIMARY KEY REFERENCES tasks(id), project_ident TEXT NOT NULL,
        document_json TEXT NOT NULL
    );
CREATE TABLE task_event_deliveries (
        consumer_id TEXT NOT NULL REFERENCES task_stream_consumers(consumer_id),
        event_id INTEGER NOT NULL REFERENCES task_events(id), status TEXT NOT NULL,
        workspace_id TEXT, surface_id TEXT, message TEXT NOT NULL DEFAULT '',
        summary TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL,
        PRIMARY KEY(consumer_id,event_id)
    );
CREATE TABLE task_events (
        id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL,
        project_ident TEXT NOT NULL, canonical_remote TEXT, task_json TEXT NOT NULL,
        comment_json TEXT, created_at INTEGER NOT NULL
    , origin_json TEXT);
CREATE TABLE task_machines (
        project_ident TEXT NOT NULL REFERENCES projects(ident), instance_id TEXT NOT NULL,
        os TEXT NOT NULL, key_hash TEXT NOT NULL UNIQUE, revoked INTEGER NOT NULL DEFAULT 0,
        PRIMARY KEY(project_ident,instance_id)
    );
CREATE TABLE task_mutation_context (id INTEGER PRIMARY KEY CHECK(id=1), origin_json TEXT);
CREATE TABLE task_reviews (
        source_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
        source_done_at INTEGER NOT NULL,
        review_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
        stage TEXT NOT NULL DEFAULT 'code',
        PRIMARY KEY(source_task_id,source_done_at,stage));
CREATE TABLE task_sessions (
        project_ident TEXT NOT NULL, instance_id TEXT NOT NULL, session_id TEXT NOT NULL,
        origin_json TEXT NOT NULL, generation TEXT NOT NULL, session_key_hash TEXT NOT NULL, seen_at INTEGER NOT NULL,
        PRIMARY KEY(project_ident,instance_id,session_id,generation),
        FOREIGN KEY(project_ident,instance_id) REFERENCES task_machines(project_ident,instance_id)
    );
CREATE TABLE task_stream_consumers (
        consumer_id TEXT PRIMARY KEY, cursor INTEGER NOT NULL, connection_id TEXT,
        connected INTEGER NOT NULL DEFAULT 0, last_seen_at INTEGER NOT NULL
    );
CREATE TABLE task_subtasks (
        parent_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
        child_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
        CHECK(parent_id != child_id)
    );
CREATE TABLE task_target_presence (
    project_ident TEXT NOT NULL, instance_id TEXT NOT NULL, session_id TEXT NOT NULL,
    origin_json TEXT NOT NULL, generation TEXT NOT NULL,
    seen_at INTEGER NOT NULL DEFAULT 0, canonical_remote TEXT NOT NULL DEFAULT '',
    display_name TEXT, hostname TEXT, advertised INTEGER NOT NULL DEFAULT 1,
    source TEXT, agent_json TEXT, report_seen_at INTEGER, output_at INTEGER,
    draft INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'unknown' CHECK(state IN ('unknown','busy','idle')),
    identity_seq INTEGER NOT NULL DEFAULT 0, activity_seq INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(project_ident,instance_id,session_id)
);
CREATE TABLE tasks (
            id              TEXT PRIMARY KEY,
            project_ident   TEXT NOT NULL REFERENCES projects(ident),
            title           TEXT NOT NULL,
            description     TEXT,
            details         TEXT,
            status          TEXT NOT NULL DEFAULT 'todo'
                            CHECK(status IN ('todo','in_progress','user_review','done','cancelled')),
            rank            INTEGER NOT NULL DEFAULT 0,
            labels          TEXT,
            hostname        TEXT,
            owner_agent_id  TEXT,
            reporter        TEXT NOT NULL,
            created_at      INTEGER NOT NULL,
            updated_at      INTEGER NOT NULL,
            started_at      INTEGER,
            done_at         INTEGER,
            kind            TEXT NOT NULL DEFAULT 'normal',
            delegated_to_project_ident TEXT,
            delegated_to_task_id TEXT
        , goal TEXT, routing_json TEXT NOT NULL DEFAULT '{"mode":"auto"}', review_action TEXT, task_type TEXT NOT NULL DEFAULT 'software' CHECK(task_type IN ('general','software')), changeset_json TEXT, requester_json TEXT, owner_origin_json TEXT);
CREATE TABLE users (
            id             INTEGER PRIMARY KEY AUTOINCREMENT,
            username       TEXT NOT NULL UNIQUE,
            display_name   TEXT NOT NULL,
            role           TEXT NOT NULL DEFAULT 'member' CHECK(role IN ('admin','member')),
            password_hash  TEXT NOT NULL,
            session_epoch  INTEGER NOT NULL DEFAULT 1,
            disabled_at    INTEGER,
            created_at     INTEGER NOT NULL,
            last_login_at  INTEGER
        );
CREATE TABLE workflow_runs (
            workflow_run_id               TEXT PRIMARY KEY,
            artifact_id                   TEXT NOT NULL REFERENCES artifacts(artifact_id) ON DELETE CASCADE,
            workflow_kind                 TEXT NOT NULL
                                           CHECK(workflow_kind IN ('design_review_round','spec_iteration','spec_acceptance','spec_task_generation')),
            phase                         TEXT,
            round_id                      TEXT,
            coordinator_actor_id          TEXT NOT NULL REFERENCES artifact_actors(actor_id),
            participant_actor_ids         TEXT,
            source_artifact_version_id    TEXT REFERENCES artifact_versions(artifact_version_id),
            read_set                      TEXT,
            idempotency_key               TEXT,
            -- T003 §5.1 resumable rule: a kind that opts into resumability
            -- may transition failed -> succeeded under the same idempotency
            -- scope while finishing missing fan-out work. cancelled is
            -- always terminal regardless of is_resumable.
            is_resumable                  INTEGER NOT NULL DEFAULT 0,
            state                         TEXT NOT NULL DEFAULT 'started'
                                           CHECK(state IN ('started','succeeded','failed','cancelled')),
            generated_contribution_ids    TEXT,
            generated_version_ids         TEXT,
            generated_task_ids            TEXT,
            generated_link_ids            TEXT,
            generated_chunk_ids           TEXT,
            failure_reason                TEXT,
            started_at                    INTEGER NOT NULL,
            ended_at                      INTEGER
        );
CREATE INDEX assignment_offers_task ON assignment_offers(task_id);
CREATE INDEX dispatch_recovery ON task_dispatch_events(project_ident,instance_id,id);
CREATE INDEX execution_queue ON execution_runs(status, created_at);
CREATE INDEX idx_agent_conf_project
            ON agent_confirmations(project_ident, message_id);
CREATE UNIQUE INDEX idx_artifact_actors_identity
            ON artifact_actors(
                actor_type,
                COALESCE(agent_system, ''),
                COALESCE(agent_id, ''),
                COALESCE(host, '')
            );
CREATE INDEX idx_artifact_chunks_app_label
            ON artifact_chunks(app, label, kind);
CREATE INDEX idx_artifact_chunks_artifact_current
            ON artifact_chunks(artifact_id, superseded_by_chunk_id);
CREATE UNIQUE INDEX idx_artifact_chunks_natural_key
            ON artifact_chunks(artifact_version_id, child_address);
CREATE INDEX idx_artifact_comments_artifact_state
            ON artifact_comments(artifact_id, state, created_at DESC);
CREATE UNIQUE INDEX idx_artifact_comments_idempotency
            ON artifact_comments(target_kind, target_id, actor_id, idempotency_key)
            WHERE idempotency_key IS NOT NULL;
CREATE INDEX idx_artifact_comments_target
            ON artifact_comments(target_kind, target_id);
CREATE UNIQUE INDEX idx_artifact_links_idempotency
            ON artifact_links(created_via_workflow_run_id, idempotency_key)
            WHERE created_via_workflow_run_id IS NOT NULL AND idempotency_key IS NOT NULL;
CREATE INDEX idx_artifact_links_source
            ON artifact_links(source_kind, source_id);
CREATE INDEX idx_artifact_links_target
            ON artifact_links(target_kind, target_id);
CREATE INDEX idx_artifact_links_type
            ON artifact_links(link_type, created_at DESC);
CREATE INDEX idx_artifact_versions_by_artifact
            ON artifact_versions(artifact_id, created_at DESC);
CREATE UNIQUE INDEX idx_artifact_versions_idempotency
            ON artifact_versions(artifact_id, created_via_workflow_run_id, idempotency_key)
            WHERE idempotency_key IS NOT NULL;
CREATE INDEX idx_artifact_versions_state
            ON artifact_versions(artifact_id, version_state);
CREATE INDEX idx_artifacts_project_kind
            ON artifacts(project_ident, kind, updated_at DESC);
CREATE INDEX idx_artifacts_project_lifecycle
            ON artifacts(project_ident, lifecycle_state, updated_at DESC);
CREATE INDEX idx_contributions_artifact
            ON artifact_contributions(artifact_id, created_at DESC);
CREATE UNIQUE INDEX idx_contributions_idempotency_run
            ON artifact_contributions(workflow_run_id, idempotency_key)
            WHERE workflow_run_id IS NOT NULL AND idempotency_key IS NOT NULL;
CREATE UNIQUE INDEX idx_contributions_idempotency_runless
            ON artifact_contributions(artifact_id, actor_id, idempotency_key)
            WHERE workflow_run_id IS NULL AND idempotency_key IS NOT NULL;
CREATE INDEX idx_contributions_run
            ON artifact_contributions(workflow_run_id);
CREATE INDEX idx_contributions_target
            ON artifact_contributions(target_kind, target_id);
CREATE INDEX idx_gateway_memories_project_hash
            ON gateway_memories(project_ident, content_hash);
CREATE INDEX idx_gateway_memories_project_revision
            ON gateway_memories(project_ident, server_revision);
CREATE INDEX idx_gateway_memories_project_type
            ON gateway_memories(project_ident, memory_type);
CREATE INDEX idx_gateway_memories_project_updated
            ON gateway_memories(project_ident, updated_at DESC);
CREATE INDEX idx_gateway_memory_client_links_memory
            ON gateway_memory_client_links(project_ident, gateway_memory_id);
CREATE INDEX idx_messages_open_roots
             ON messages(project_ident, source, id) WHERE resolved_at IS NULL;
CREATE INDEX idx_messages_project
            ON messages(project_ident, id);
CREATE INDEX idx_messages_unconfirmed
             ON messages(project_ident, id) WHERE confirmed_at IS NULL;
CREATE INDEX idx_pattern_comments_pattern
            ON pattern_comments(pattern_id, created_at);
CREATE INDEX idx_patterns_updated
            ON patterns(updated_at DESC);
CREATE INDEX idx_project_links_project
             ON project_links(project_ident, rank);
CREATE UNIQUE INDEX idx_projects_ident_nocase
            ON projects(ident COLLATE NOCASE);
CREATE UNIQUE INDEX idx_projects_repo_mapping_nocase
            ON projects(repo_provider COLLATE NOCASE, repo_full_name COLLATE NOCASE)
            WHERE repo_provider IS NOT NULL AND repo_full_name IS NOT NULL;
CREATE INDEX idx_remote_consumers_cursor ON remote_consumers(cursor);
CREATE INDEX idx_router_deliveries_open
        ON router_deliveries(state, next_attempt_at);
CREATE INDEX idx_router_deliveries_plugin_open
        ON router_deliveries(plugin, event_seq)
        WHERE state IN ('pending','running','retrying');
CREATE INDEX idx_task_comments_task
            ON task_comments(task_id, created_at);
CREATE UNIQUE INDEX idx_task_delegations_source
            ON task_delegations(source_project_ident, source_task_id);
CREATE UNIQUE INDEX idx_task_delegations_target
            ON task_delegations(target_project_ident, target_task_id);
CREATE INDEX idx_tasks_project_rank
            ON tasks(project_ident, status, rank);
CREATE INDEX idx_tasks_project_status
            ON tasks(project_ident, status);
CREATE INDEX idx_workflow_runs_artifact
            ON workflow_runs(artifact_id, started_at DESC);
CREATE UNIQUE INDEX idx_workflow_runs_idempotency
            ON workflow_runs(coordinator_actor_id, artifact_id, workflow_kind, idempotency_key)
            WHERE idempotency_key IS NOT NULL;
CREATE INDEX idx_workflow_runs_state
            ON workflow_runs(state, started_at DESC);
CREATE INDEX presence_session ON task_target_presence(session_id,instance_id);
CREATE INDEX presence_source ON task_target_presence(source);
CREATE INDEX task_events_project ON task_events(project_ident,id);
CREATE INDEX task_subtasks_parent ON task_subtasks(parent_id);
CREATE TRIGGER assignment_explicit_release AFTER UPDATE ON tasks
        WHEN NEW.status='todo' AND OLD.status IN ('todo','in_progress')
          AND NEW.owner_origin_json IS NULL AND NEW.owner_agent_id IS NULL
          AND json_extract(OLD.owner_origin_json,'$.session_id') =
              (SELECT json_extract(origin_json,'$.session_id') FROM task_mutation_context WHERE id=1)
          AND json_extract(OLD.owner_origin_json,'$.instance_id') =
              (SELECT json_extract(origin_json,'$.instance_id') FROM task_mutation_context WHERE id=1)
        BEGIN
            INSERT OR IGNORE INTO assignment_releases(task_id,session_id,instance_id)
            VALUES(NEW.id,json_extract(OLD.owner_origin_json,'$.session_id'),
                   json_extract(OLD.owner_origin_json,'$.instance_id'));
        END;
CREATE TRIGGER assignment_offer_comment_deleted AFTER DELETE ON task_comments BEGIN
            DELETE FROM assignment_offers WHERE task_id=OLD.task_id;
        END;
CREATE TRIGGER assignment_offer_comment_updated AFTER UPDATE ON task_comments BEGIN
            DELETE FROM assignment_offers WHERE task_id IN (OLD.task_id,NEW.task_id);
        END;
CREATE TRIGGER assignment_offer_commented AFTER INSERT ON task_comments BEGIN
            DELETE FROM assignment_offers WHERE task_id=NEW.task_id;
        END;
CREATE TRIGGER assignment_offer_task_deleted AFTER DELETE ON tasks BEGIN
            DELETE FROM assignment_offers WHERE task_id=OLD.id;
        END;
CREATE TRIGGER assignment_offer_task_updated AFTER UPDATE ON tasks BEGIN
            DELETE FROM assignment_offers WHERE task_id=NEW.id;
        END;
CREATE TRIGGER assignment_release_commented AFTER INSERT ON task_comments
        WHEN NOT (NEW.author='amux' AND NEW.author_type='system')
        BEGIN
            DELETE FROM assignment_releases WHERE task_id=NEW.task_id
              AND (session_id IS NOT json_extract(NEW.origin_json,'$.session_id')
                   OR instance_id IS NOT json_extract(NEW.origin_json,'$.instance_id'));
        END;
CREATE TRIGGER assignment_release_task_changed AFTER UPDATE ON tasks
        WHEN NEW.title IS NOT OLD.title OR NEW.description IS NOT OLD.description
          OR NEW.details IS NOT OLD.details OR NEW.labels IS NOT OLD.labels
          OR NEW.goal IS NOT OLD.goal OR NEW.routing_json IS NOT OLD.routing_json
          OR NEW.rank IS NOT OLD.rank OR NEW.status IN ('done','cancelled','user_review')
        BEGIN
            DELETE FROM assignment_releases WHERE task_id=NEW.id;
        END;
CREATE TRIGGER presence_project_deleted AFTER DELETE ON projects BEGIN
            DELETE FROM task_target_presence WHERE project_ident=OLD.ident;
         END;
CREATE TRIGGER prevent_artifact_contribution_update
        BEFORE UPDATE ON artifact_contributions
        FOR EACH ROW
        BEGIN
            SELECT RAISE(ABORT, 'artifact_contribution rows are immutable');
        END;
CREATE TRIGGER prevent_artifact_version_body_repopulate
        BEFORE UPDATE OF body ON artifact_versions
        FOR EACH ROW
        WHEN OLD.body_purged_at IS NOT NULL AND NEW.body IS NOT NULL
        BEGIN
            SELECT RAISE(ABORT, 'artifact_version body cannot be repopulated after purge');
        END;
CREATE TRIGGER prevent_artifact_version_body_update
        BEFORE UPDATE OF body ON artifact_versions
        FOR EACH ROW
        WHEN OLD.body_purged_at IS NULL
             AND NEW.body IS NOT OLD.body
             AND NOT (NEW.body IS NULL AND NEW.body_purged_at IS NOT NULL)
        BEGIN
            SELECT RAISE(ABORT, 'artifact_version body is immutable');
        END;
CREATE TRIGGER prevent_artifact_version_meta_update
        BEFORE UPDATE OF body_format, source_format, parent_version_id, artifact_id, idempotency_key, created_via_workflow_run_id ON artifact_versions
        FOR EACH ROW
        WHEN
            NEW.body_format IS NOT OLD.body_format
            OR NEW.source_format IS NOT OLD.source_format
            OR NEW.parent_version_id IS NOT OLD.parent_version_id
            OR NEW.artifact_id IS NOT OLD.artifact_id
            OR NEW.idempotency_key IS NOT OLD.idempotency_key
            OR NEW.created_via_workflow_run_id IS NOT OLD.created_via_workflow_run_id
        BEGIN
            SELECT RAISE(ABORT, 'artifact_version metadata is immutable');
        END;
CREATE TRIGGER prevent_artifact_version_payload_update
        BEFORE UPDATE OF structured_payload ON artifact_versions
        FOR EACH ROW
        WHEN OLD.body_purged_at IS NULL AND NEW.structured_payload IS NOT OLD.structured_payload
        BEGIN
            SELECT RAISE(ABORT, 'artifact_version structured_payload is immutable');
        END;
CREATE TRIGGER prevent_workflow_run_state_after_terminal
        BEFORE UPDATE OF state ON workflow_runs
        FOR EACH ROW
        WHEN
            (OLD.state = 'cancelled' AND NEW.state IS NOT OLD.state)
            OR (OLD.state = 'succeeded' AND NEW.state IS NOT OLD.state)
            OR (OLD.state = 'failed' AND NEW.state = 'succeeded' AND OLD.is_resumable = 0)
            OR (OLD.state = 'failed' AND NEW.state IN ('started','cancelled'))
        BEGIN
            SELECT RAISE(ABORT, 'invalid workflow_run state transition');
        END;
CREATE TRIGGER reject_doc_publish_insert
BEFORE INSERT ON workflow_runs WHEN NEW.workflow_kind = 'doc_publish'
BEGIN SELECT RAISE(ABORT, 'documentation feature retired'); END;
CREATE TRIGGER reject_doc_publish_update
BEFORE UPDATE OF workflow_kind ON workflow_runs WHEN NEW.workflow_kind = 'doc_publish'
BEGIN SELECT RAISE(ABORT, 'documentation feature retired'); END;
CREATE TRIGGER reject_documentation_artifact_insert
BEFORE INSERT ON artifacts WHEN NEW.kind = 'documentation'
BEGIN SELECT RAISE(ABORT, 'documentation feature retired'); END;
CREATE TRIGGER reject_documentation_artifact_update
BEFORE UPDATE OF kind ON artifacts WHEN NEW.kind = 'documentation'
BEGIN SELECT RAISE(ABORT, 'documentation feature retired'); END;
CREATE TRIGGER reject_documentation_contribution_format
BEFORE INSERT ON artifact_contributions WHEN NEW.body_format != 'markdown'
BEGIN SELECT RAISE(ABORT, 'documentation formats retired'); END;
CREATE TRIGGER reject_documentation_version_format
BEFORE INSERT ON artifact_versions WHEN NEW.body_format != 'markdown'
BEGIN SELECT RAISE(ABORT, 'documentation formats retired'); END;
CREATE TRIGGER router_task_assigned AFTER UPDATE OF owner_agent_id,owner_origin_json ON tasks
         WHEN (NEW.owner_agent_id IS NOT NULL OR NEW.owner_origin_json IS NOT NULL)
           AND (NEW.owner_agent_id IS NOT OLD.owner_agent_id
             OR NEW.owner_origin_json IS NOT OLD.owner_origin_json) BEGIN INSERT INTO router_task_outbox(kind,project,task_json,comment_json,actor_json,created_at)
             VALUES ('assigned',NEW.project_ident,json_object('id',NEW.id,'project_ident',NEW.project_ident,'title',NEW.title,'goal',NEW.goal,'description',NEW.description,'review_action',NEW.review_action,'details',NEW.details,'status',NEW.status,'rank',NEW.rank,'hostname',NEW.hostname,'owner_agent_id',NEW.owner_agent_id,'reporter',NEW.reporter,'created_at',NEW.created_at,'updated_at',NEW.updated_at,'started_at',NEW.started_at,'done_at',NEW.done_at,'kind',NEW.kind,'delegated_to_project_ident',NEW.delegated_to_project_ident,'delegated_to_task_id',NEW.delegated_to_task_id,'changeset',json(NEW.changeset_json),'labels',json(CASE WHEN json_valid(NEW.labels) THEN NEW.labels ELSE '[]' END),'owner_origin',json(NEW.owner_origin_json),'routing',json(NEW.routing_json),'requester',json(NEW.requester_json),'parent',(SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title)
        FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id=NEW.id)),NULL,(SELECT origin_json FROM task_mutation_context WHERE id=1),CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)); END;
CREATE TRIGGER router_task_cancelled AFTER UPDATE OF status ON tasks
             WHEN NEW.status='cancelled' AND OLD.status!='cancelled' BEGIN INSERT INTO router_task_outbox(kind,project,task_json,comment_json,actor_json,created_at)
             VALUES ('cancelled',NEW.project_ident,json_object('id',NEW.id,'project_ident',NEW.project_ident,'title',NEW.title,'goal',NEW.goal,'description',NEW.description,'review_action',NEW.review_action,'details',NEW.details,'status',NEW.status,'rank',NEW.rank,'hostname',NEW.hostname,'owner_agent_id',NEW.owner_agent_id,'reporter',NEW.reporter,'created_at',NEW.created_at,'updated_at',NEW.updated_at,'started_at',NEW.started_at,'done_at',NEW.done_at,'kind',NEW.kind,'delegated_to_project_ident',NEW.delegated_to_project_ident,'delegated_to_task_id',NEW.delegated_to_task_id,'changeset',json(NEW.changeset_json),'labels',json(CASE WHEN json_valid(NEW.labels) THEN NEW.labels ELSE '[]' END),'owner_origin',json(NEW.owner_origin_json),'routing',json(NEW.routing_json),'requester',json(NEW.requester_json),'parent',(SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title)
        FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id=NEW.id)),NULL,(SELECT origin_json FROM task_mutation_context WHERE id=1),CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)); END;
CREATE TRIGGER router_task_commented AFTER INSERT ON task_comments BEGIN
            INSERT INTO router_task_outbox(kind,project,task_json,comment_json,actor_json,created_at)
            SELECT 'commented',t.project_ident,json_object('id',t.id,'project_ident',t.project_ident,'title',t.title,'goal',t.goal,'description',t.description,'review_action',t.review_action,'details',t.details,'status',t.status,'rank',t.rank,'hostname',t.hostname,'owner_agent_id',t.owner_agent_id,'reporter',t.reporter,'created_at',t.created_at,'updated_at',t.updated_at,'started_at',t.started_at,'done_at',t.done_at,'kind',t.kind,'delegated_to_project_ident',t.delegated_to_project_ident,'delegated_to_task_id',t.delegated_to_task_id,'changeset',json(t.changeset_json),'labels',json(CASE WHEN json_valid(t.labels) THEN t.labels ELSE '[]' END),'owner_origin',json(t.owner_origin_json),'routing',json(t.routing_json),'requester',json(t.requester_json),'parent',(SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title)
        FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id=t.id)),json_object('id',NEW.id,'task_id',NEW.task_id,'author',NEW.author,'author_type',NEW.author_type,'content',NEW.content,'created_at',NEW.created_at,'origin',json(NEW.origin_json)),(SELECT origin_json FROM task_mutation_context WHERE id=1),CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER) FROM tasks t WHERE t.id=NEW.task_id;
         END;
CREATE TRIGGER router_task_completed AFTER UPDATE OF status ON tasks
             WHEN NEW.status='done' AND OLD.status!='done' BEGIN INSERT INTO router_task_outbox(kind,project,task_json,comment_json,actor_json,created_at)
             VALUES ('completed',NEW.project_ident,json_object('id',NEW.id,'project_ident',NEW.project_ident,'title',NEW.title,'goal',NEW.goal,'description',NEW.description,'review_action',NEW.review_action,'details',NEW.details,'status',NEW.status,'rank',NEW.rank,'hostname',NEW.hostname,'owner_agent_id',NEW.owner_agent_id,'reporter',NEW.reporter,'created_at',NEW.created_at,'updated_at',NEW.updated_at,'started_at',NEW.started_at,'done_at',NEW.done_at,'kind',NEW.kind,'delegated_to_project_ident',NEW.delegated_to_project_ident,'delegated_to_task_id',NEW.delegated_to_task_id,'changeset',json(NEW.changeset_json),'labels',json(CASE WHEN json_valid(NEW.labels) THEN NEW.labels ELSE '[]' END),'owner_origin',json(NEW.owner_origin_json),'routing',json(NEW.routing_json),'requester',json(NEW.requester_json),'parent',(SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title)
        FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id=NEW.id)),NULL,(SELECT origin_json FROM task_mutation_context WHERE id=1),CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)); END;
CREATE TRIGGER router_task_created AFTER INSERT ON tasks BEGIN INSERT INTO router_task_outbox(kind,project,task_json,comment_json,actor_json,created_at)
             VALUES ('created',NEW.project_ident,json_object('id',NEW.id,'project_ident',NEW.project_ident,'title',NEW.title,'goal',NEW.goal,'description',NEW.description,'review_action',NEW.review_action,'details',NEW.details,'status',NEW.status,'rank',NEW.rank,'hostname',NEW.hostname,'owner_agent_id',NEW.owner_agent_id,'reporter',NEW.reporter,'created_at',NEW.created_at,'updated_at',NEW.updated_at,'started_at',NEW.started_at,'done_at',NEW.done_at,'kind',NEW.kind,'delegated_to_project_ident',NEW.delegated_to_project_ident,'delegated_to_task_id',NEW.delegated_to_task_id,'changeset',json(NEW.changeset_json),'labels',json(CASE WHEN json_valid(NEW.labels) THEN NEW.labels ELSE '[]' END),'owner_origin',json(NEW.owner_origin_json),'routing',json(NEW.routing_json),'requester',json(NEW.requester_json),'parent',(SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title)
        FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id=NEW.id)),NULL,(SELECT origin_json FROM task_mutation_context WHERE id=1),CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)); END;
CREATE TRIGGER router_task_started AFTER UPDATE OF status ON tasks
             WHEN NEW.status='in_progress' AND OLD.status!='in_progress' BEGIN INSERT INTO router_task_outbox(kind,project,task_json,comment_json,actor_json,created_at)
             VALUES ('started',NEW.project_ident,json_object('id',NEW.id,'project_ident',NEW.project_ident,'title',NEW.title,'goal',NEW.goal,'description',NEW.description,'review_action',NEW.review_action,'details',NEW.details,'status',NEW.status,'rank',NEW.rank,'hostname',NEW.hostname,'owner_agent_id',NEW.owner_agent_id,'reporter',NEW.reporter,'created_at',NEW.created_at,'updated_at',NEW.updated_at,'started_at',NEW.started_at,'done_at',NEW.done_at,'kind',NEW.kind,'delegated_to_project_ident',NEW.delegated_to_project_ident,'delegated_to_task_id',NEW.delegated_to_task_id,'changeset',json(NEW.changeset_json),'labels',json(CASE WHEN json_valid(NEW.labels) THEN NEW.labels ELSE '[]' END),'owner_origin',json(NEW.owner_origin_json),'routing',json(NEW.routing_json),'requester',json(NEW.requester_json),'parent',(SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title)
        FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id=NEW.id)),NULL,(SELECT origin_json FROM task_mutation_context WHERE id=1),CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)); END;
CREATE TRIGGER router_task_unclaimed AFTER UPDATE OF status,owner_agent_id,owner_origin_json ON tasks
         WHEN NEW.status='todo' AND NEW.owner_agent_id IS NULL AND NEW.owner_origin_json IS NULL
           AND (OLD.owner_agent_id IS NOT NULL OR OLD.owner_origin_json IS NOT NULL
             OR OLD.status='in_progress') BEGIN INSERT INTO router_task_outbox(kind,project,task_json,comment_json,actor_json,created_at)
             VALUES ('unclaimed',NEW.project_ident,json_object('id',NEW.id,'project_ident',NEW.project_ident,'title',NEW.title,'goal',NEW.goal,'description',NEW.description,'review_action',NEW.review_action,'details',NEW.details,'status',NEW.status,'rank',NEW.rank,'hostname',NEW.hostname,'owner_agent_id',NEW.owner_agent_id,'reporter',NEW.reporter,'created_at',NEW.created_at,'updated_at',NEW.updated_at,'started_at',NEW.started_at,'done_at',NEW.done_at,'kind',NEW.kind,'delegated_to_project_ident',NEW.delegated_to_project_ident,'delegated_to_task_id',NEW.delegated_to_task_id,'changeset',json(NEW.changeset_json),'labels',json(CASE WHEN json_valid(NEW.labels) THEN NEW.labels ELSE '[]' END),'owner_origin',json(NEW.owner_origin_json),'routing',json(NEW.routing_json),'requester',json(NEW.requester_json),'parent',(SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title)
        FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id=NEW.id)),NULL,(SELECT origin_json FROM task_mutation_context WHERE id=1),CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)); END;
CREATE TRIGGER router_task_updated AFTER UPDATE ON tasks BEGIN INSERT INTO router_task_outbox(kind,project,task_json,comment_json,actor_json,created_at)
             VALUES ('updated',NEW.project_ident,json_object('id',NEW.id,'project_ident',NEW.project_ident,'title',NEW.title,'goal',NEW.goal,'description',NEW.description,'review_action',NEW.review_action,'details',NEW.details,'status',NEW.status,'rank',NEW.rank,'hostname',NEW.hostname,'owner_agent_id',NEW.owner_agent_id,'reporter',NEW.reporter,'created_at',NEW.created_at,'updated_at',NEW.updated_at,'started_at',NEW.started_at,'done_at',NEW.done_at,'kind',NEW.kind,'delegated_to_project_ident',NEW.delegated_to_project_ident,'delegated_to_task_id',NEW.delegated_to_task_id,'changeset',json(NEW.changeset_json),'labels',json(CASE WHEN json_valid(NEW.labels) THEN NEW.labels ELSE '[]' END),'owner_origin',json(NEW.owner_origin_json),'routing',json(NEW.routing_json),'requester',json(NEW.requester_json),'parent',(SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title)
        FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id=NEW.id)),NULL,(SELECT origin_json FROM task_mutation_context WHERE id=1),CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)); END;
CREATE TRIGGER router_task_user_review AFTER UPDATE OF status ON tasks
             WHEN NEW.status='user_review' AND OLD.status!='user_review' BEGIN INSERT INTO router_task_outbox(kind,project,task_json,comment_json,actor_json,created_at)
             VALUES ('user_review',NEW.project_ident,json_object('id',NEW.id,'project_ident',NEW.project_ident,'title',NEW.title,'goal',NEW.goal,'description',NEW.description,'review_action',NEW.review_action,'details',NEW.details,'status',NEW.status,'rank',NEW.rank,'hostname',NEW.hostname,'owner_agent_id',NEW.owner_agent_id,'reporter',NEW.reporter,'created_at',NEW.created_at,'updated_at',NEW.updated_at,'started_at',NEW.started_at,'done_at',NEW.done_at,'kind',NEW.kind,'delegated_to_project_ident',NEW.delegated_to_project_ident,'delegated_to_task_id',NEW.delegated_to_task_id,'changeset',json(NEW.changeset_json),'labels',json(CASE WHEN json_valid(NEW.labels) THEN NEW.labels ELSE '[]' END),'owner_origin',json(NEW.owner_origin_json),'routing',json(NEW.routing_json),'requester',json(NEW.requester_json),'parent',(SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title)
        FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id=NEW.id)),NULL,(SELECT origin_json FROM task_mutation_context WHERE id=1),CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)); END;
CREATE TRIGGER task_completion_owner_clear AFTER UPDATE OF status ON tasks
    WHEN NEW.status!='user_review'
    BEGIN DELETE FROM task_completion_owners WHERE task_id=NEW.id; END;
CREATE TRIGGER task_completion_owner_park BEFORE UPDATE OF status ON tasks
    WHEN NEW.status='user_review' AND OLD.status!='user_review' AND OLD.owner_origin_json IS NOT NULL
    BEGIN INSERT OR REPLACE INTO task_completion_owners(task_id,origin_json) VALUES(OLD.id,OLD.owner_origin_json); END;
CREATE TRIGGER task_event_cancelled AFTER UPDATE OF status ON tasks
    WHEN NEW.status='cancelled' AND OLD.status!='cancelled' BEGIN
        INSERT INTO task_events(kind,project_ident,canonical_remote,task_json,created_at,origin_json)
        VALUES ('task_cancelled',NEW.project_ident,(SELECT canonical_remote FROM projects WHERE ident=NEW.project_ident),json_object('id',NEW.id,'project_ident',NEW.project_ident,'title',NEW.title,'goal',NEW.goal,'description',NEW.description,'review_action',NEW.review_action,'details',NEW.details,'status',NEW.status,'rank',NEW.rank,'hostname',NEW.hostname,'owner_agent_id',NEW.owner_agent_id,'reporter',NEW.reporter,'created_at',NEW.created_at,'updated_at',NEW.updated_at,'started_at',NEW.started_at,'done_at',NEW.done_at,'kind',NEW.kind,'delegated_to_project_ident',NEW.delegated_to_project_ident,'delegated_to_task_id',NEW.delegated_to_task_id,'changeset',json(NEW.changeset_json),'labels',json(CASE WHEN json_valid(NEW.labels) THEN NEW.labels ELSE '[]' END),'owner_origin',json(NEW.owner_origin_json),'routing',json(NEW.routing_json),'requester',json(NEW.requester_json),'parent',(SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title)
        FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id=NEW.id)),NEW.updated_at,(SELECT origin_json FROM task_mutation_context WHERE id=1));
    END;
CREATE TRIGGER task_event_commented AFTER INSERT ON task_comments BEGIN
        INSERT INTO task_events(kind,project_ident,canonical_remote,task_json,comment_json,created_at,origin_json)
        SELECT 'task_commented',t.project_ident,p.canonical_remote,json_object('id',t.id,'project_ident',t.project_ident,'title',t.title,'goal',t.goal,'description',t.description,'review_action',t.review_action,'details',t.details,'status',t.status,'rank',t.rank,'hostname',t.hostname,'owner_agent_id',t.owner_agent_id,'reporter',t.reporter,'created_at',t.created_at,'updated_at',t.updated_at,'started_at',t.started_at,'done_at',t.done_at,'kind',t.kind,'delegated_to_project_ident',t.delegated_to_project_ident,'delegated_to_task_id',t.delegated_to_task_id,'changeset',json(t.changeset_json),'labels',json(CASE WHEN json_valid(t.labels) THEN t.labels ELSE '[]' END),'owner_origin',json(t.owner_origin_json),'routing',json(t.routing_json),'requester',json(t.requester_json),'parent',(SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title)
        FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id=t.id)),json_object('id',NEW.id,'task_id',NEW.task_id,'author',NEW.author,'author_type',NEW.author_type,'content',NEW.content,'created_at',NEW.created_at,'origin',json(NEW.origin_json)),NEW.created_at,(SELECT origin_json FROM task_mutation_context WHERE id=1)
        FROM tasks t JOIN projects p ON p.ident=t.project_ident WHERE t.id=NEW.task_id;
    END;
CREATE TRIGGER task_event_completed AFTER UPDATE OF status ON tasks
    WHEN NEW.status='done' AND OLD.status!='done' BEGIN
        INSERT INTO task_events(kind,project_ident,canonical_remote,task_json,comment_json,created_at,origin_json)
        VALUES ('task_completed',NEW.project_ident,(SELECT canonical_remote FROM projects WHERE ident=NEW.project_ident),json_object('id',NEW.id,'project_ident',NEW.project_ident,'title',NEW.title,'goal',NEW.goal,'description',NEW.description,'review_action',NEW.review_action,'details',NEW.details,'status',NEW.status,'rank',NEW.rank,'hostname',NEW.hostname,'owner_agent_id',NEW.owner_agent_id,'reporter',NEW.reporter,'created_at',NEW.created_at,'updated_at',NEW.updated_at,'started_at',NEW.started_at,'done_at',NEW.done_at,'kind',NEW.kind,'delegated_to_project_ident',NEW.delegated_to_project_ident,'delegated_to_task_id',NEW.delegated_to_task_id,'changeset',json(NEW.changeset_json),'labels',json(CASE WHEN json_valid(NEW.labels) THEN NEW.labels ELSE '[]' END),'owner_origin',json(NEW.owner_origin_json),'routing',json(NEW.routing_json),'requester',json(NEW.requester_json),'parent',(SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title)
        FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id=NEW.id)),
        (SELECT json_object('id',c.id,'task_id',c.task_id,'author',c.author,'author_type',c.author_type,'content',c.content,'created_at',c.created_at,'origin',json(c.origin_json)) FROM task_comments c WHERE c.task_id=NEW.id AND c.author_type!='system' ORDER BY c.created_at DESC,c.id DESC LIMIT 1),NEW.updated_at,(SELECT origin_json FROM task_mutation_context WHERE id=1));
    END;
CREATE TRIGGER task_event_created AFTER INSERT ON tasks BEGIN
        INSERT INTO task_events(kind,project_ident,canonical_remote,task_json,created_at,origin_json)
        VALUES ('task_created',NEW.project_ident,(SELECT canonical_remote FROM projects WHERE ident=NEW.project_ident),json_object('id',NEW.id,'project_ident',NEW.project_ident,'title',NEW.title,'goal',NEW.goal,'description',NEW.description,'review_action',NEW.review_action,'details',NEW.details,'status',NEW.status,'rank',NEW.rank,'hostname',NEW.hostname,'owner_agent_id',NEW.owner_agent_id,'reporter',NEW.reporter,'created_at',NEW.created_at,'updated_at',NEW.updated_at,'started_at',NEW.started_at,'done_at',NEW.done_at,'kind',NEW.kind,'delegated_to_project_ident',NEW.delegated_to_project_ident,'delegated_to_task_id',NEW.delegated_to_task_id,'changeset',json(NEW.changeset_json),'labels',json(CASE WHEN json_valid(NEW.labels) THEN NEW.labels ELSE '[]' END),'owner_origin',json(NEW.owner_origin_json),'routing',json(NEW.routing_json),'requester',json(NEW.requester_json),'parent',(SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title)
        FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id=NEW.id)),NEW.created_at,(SELECT origin_json FROM task_mutation_context WHERE id=1));
    END;
CREATE TRIGGER task_event_unclaimed AFTER UPDATE OF status,owner_agent_id,owner_origin_json ON tasks
    WHEN NEW.status='todo' AND NEW.owner_agent_id IS NULL AND NEW.owner_origin_json IS NULL
      AND (OLD.owner_agent_id IS NOT NULL OR OLD.owner_origin_json IS NOT NULL OR OLD.status='in_progress')
      AND NOT EXISTS(SELECT 1 FROM task_dispatches d WHERE d.task_id=NEW.id) BEGIN
        INSERT INTO task_events(kind,project_ident,canonical_remote,task_json,comment_json,created_at,origin_json)
        VALUES ('task_unclaimed',NEW.project_ident,(SELECT canonical_remote FROM projects WHERE ident=NEW.project_ident),json_object('id',NEW.id,'project_ident',NEW.project_ident,'title',NEW.title,'goal',NEW.goal,'description',NEW.description,'review_action',NEW.review_action,'details',NEW.details,'status',NEW.status,'rank',NEW.rank,'hostname',NEW.hostname,'owner_agent_id',NEW.owner_agent_id,'reporter',NEW.reporter,'created_at',NEW.created_at,'updated_at',NEW.updated_at,'started_at',NEW.started_at,'done_at',NEW.done_at,'kind',NEW.kind,'delegated_to_project_ident',NEW.delegated_to_project_ident,'delegated_to_task_id',NEW.delegated_to_task_id,'changeset',json(NEW.changeset_json),'labels',json(CASE WHEN json_valid(NEW.labels) THEN NEW.labels ELSE '[]' END),'owner_origin',json(NEW.owner_origin_json),'routing',json(NEW.routing_json),'requester',json(NEW.requester_json),'parent',(SELECT json_object('id',p.id,'project_ident',p.project_ident,'title',p.title)
        FROM task_subtasks s JOIN tasks p ON p.id=s.parent_id WHERE s.child_id=NEW.id)),
          (SELECT json_object('id',c.id,'task_id',c.task_id,'author',c.author,'author_type',c.author_type,'content',c.content,'created_at',c.created_at,'origin',json(c.origin_json)) FROM task_comments c WHERE c.task_id=NEW.id ORDER BY c.created_at DESC,c.id DESC LIMIT 1),NEW.updated_at,(SELECT origin_json FROM task_mutation_context WHERE id=1));
    END;
