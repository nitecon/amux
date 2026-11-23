package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nitecon/amux/projects"
)

const gatewayMemoryMigration = 45
const GatewayGlobalMemoryProjectIdent = "__global__"

type GatewayMemory struct {
	ID                  string          `json:"gateway_memory_id"`
	ProjectIdent        string          `json:"project_ident"`
	ServerRevision      int64           `json:"server_revision"`
	Content             string          `json:"content"`
	MemoryType          string          `json:"memory_type"`
	Tags                []string        `json:"tags"`
	ContentHash         string          `json:"content_hash"`
	ConceptHash         *string         `json:"concept_hash,omitempty"`
	OKF                 json.RawMessage `json:"okf,omitempty"`
	SourceAgentID       *string         `json:"source_agent_id"`
	SourceHostname      *string         `json:"source_hostname"`
	SourceClient        *string         `json:"source_client"`
	SourceClientVersion *string         `json:"source_client_version"`
	Provenance          any             `json:"provenance"`
	ClientCreatedAt     *int64          `json:"client_created_at"`
	ClientUpdatedAt     *int64          `json:"client_updated_at"`
	CreatedAt           int64           `json:"created_at"`
	UpdatedAt           int64           `json:"updated_at"`
	TombstonedAt        *int64          `json:"tombstoned_at"`
	Tombstone           bool            `json:"tombstone"`
}

type GatewayMemoryFilters struct {
	Query             string
	MemoryType        string
	Tag               string
	SinceRevision     *int64
	IncludeTombstones bool
	Limit             *uint64
}

type GatewayMemorySearchResponse struct {
	ProjectIdent   string          `json:"project_ident"`
	ServerRevision int64           `json:"server_revision"`
	Memories       []GatewayMemory `json:"memories"`
}

type GatewayMemoryProject struct {
	ProjectIdent   string `json:"project_ident"`
	MemoryCount    int64  `json:"memory_count"`
	ServerRevision int64  `json:"server_revision"`
}

type GatewayMemoryProjectsResponse struct {
	Projects []GatewayMemoryProject `json:"projects"`
}

type GatewayMemoryKnownRevision struct {
	GatewayMemoryID string  `json:"gateway_memory_id"`
	ServerRevision  int64   `json:"server_revision"`
	ContentHash     *string `json:"content_hash"`
}

type GatewayMemoryConflict struct {
	GatewayMemoryID      string `json:"gateway_memory_id"`
	BaseGatewayRevision  int64  `json:"base_gateway_revision"`
	RemoteServerRevision int64  `json:"remote_server_revision"`
	RemoteContentHash    string `json:"remote_content_hash"`
	RemoteUpdatedAt      int64  `json:"remote_updated_at"`
	Reason               string `json:"reason"`
}

type GatewayMemoryTombstone struct {
	GatewayMemoryID     string  `json:"gateway_memory_id"`
	ServerRevision      int64   `json:"server_revision"`
	ProjectIdent        string  `json:"project_ident"`
	ContentHash         string  `json:"content_hash"`
	TombstonedAt        int64   `json:"tombstoned_at"`
	UpdatedAt           int64   `json:"updated_at"`
	SourceAgentID       *string `json:"source_agent_id"`
	SourceHostname      *string `json:"source_hostname"`
	SourceClient        *string `json:"source_client"`
	SourceClientVersion *string `json:"source_client_version"`
}

type GatewayMemoryPullRequest struct {
	SinceRevision     *int64
	Known             []GatewayMemoryKnownRevision
	PageSize          *uint64
	IncludeTombstones bool
}

type GatewayMemoryPullResponse struct {
	ProjectIdent      string                   `json:"project_ident"`
	ServerRevision    int64                    `json:"server_revision"`
	SinceRevision     int64                    `json:"since_revision"`
	NextSinceRevision int64                    `json:"next_since_revision"`
	HasMore           bool                     `json:"has_more"`
	Memories          []GatewayMemory          `json:"memories"`
	Tombstones        []GatewayMemoryTombstone `json:"tombstones"`
	Conflicts         []GatewayMemoryConflict  `json:"conflicts"`
}

func migrateGatewayMemories(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS gateway_memories (
		id TEXT PRIMARY KEY,
		project_ident TEXT NOT NULL REFERENCES projects(ident) ON DELETE CASCADE,
		server_revision INTEGER NOT NULL,
		content TEXT NOT NULL,
		memory_type TEXT NOT NULL,
		tags TEXT NOT NULL DEFAULT '[]',
		content_hash TEXT NOT NULL,
		source_agent_id TEXT,
		source_hostname TEXT,
		source_client TEXT,
		source_client_version TEXT,
		provenance_json TEXT,
		client_created_at INTEGER,
		client_updated_at INTEGER,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		tombstoned_at INTEGER,
		UNIQUE(project_ident,server_revision)
	);
	CREATE INDEX IF NOT EXISTS idx_gateway_memories_project_revision ON gateway_memories(project_ident,server_revision);
	CREATE INDEX IF NOT EXISTS idx_gateway_memories_project_updated ON gateway_memories(project_ident,updated_at DESC);
	CREATE INDEX IF NOT EXISTS idx_gateway_memories_project_hash ON gateway_memories(project_ident,content_hash);
		CREATE INDEX IF NOT EXISTS idx_gateway_memories_project_type ON gateway_memories(project_ident,memory_type);
		CREATE TABLE IF NOT EXISTS gateway_memory_client_links (
			project_ident TEXT NOT NULL REFERENCES projects(ident) ON DELETE CASCADE,
			client_id TEXT NOT NULL,
			local_memory_id TEXT NOT NULL,
			gateway_memory_id TEXT NOT NULL REFERENCES gateway_memories(id) ON DELETE CASCADE,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY(project_ident,client_id,local_memory_id)
		);
		CREATE INDEX IF NOT EXISTS idx_gateway_memory_client_links_memory ON gateway_memory_client_links(project_ident,gateway_memory_id);`); err != nil {
		return fmt.Errorf("create gateway memories schema: %w", err)
	}
	columns := map[string]bool{}
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info(gateway_memories)")
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid, notnull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, column := range []string{"concept_hash", "okf_json"} {
		if !columns[column] {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE gateway_memories ADD COLUMN "+column+" TEXT"); err != nil {
				return fmt.Errorf("add gateway_memories.%s: %w", column, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)", gatewayMemoryMigration); err != nil {
		return fmt.Errorf("record gateway memory migration: %w", err)
	}
	return nil
}

const gatewayMemorySelect = `SELECT id,project_ident,server_revision,content,memory_type,tags,content_hash,
	concept_hash,okf_json,source_agent_id,source_hostname,source_client,source_client_version,
	provenance_json,client_created_at,client_updated_at,created_at,updated_at,tombstoned_at
	FROM gateway_memories`

func (s *Store) SearchGatewayMemories(ctx context.Context, projectIdent string, filters GatewayMemoryFilters) (*GatewayMemorySearchResponse, error) {
	projectIdent = projects.NormalizeIdent(projectIdent)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(server_revision),0) FROM gateway_memories WHERE project_ident=?`, projectIdent).Scan(&revision); err != nil {
		return nil, err
	}
	query := gatewayMemorySelect + ` WHERE project_ident=?`
	args := []any{projectIdent}
	if !filters.IncludeTombstones {
		query += ` AND tombstoned_at IS NULL`
	}
	if filters.SinceRevision != nil {
		query += ` AND server_revision>?`
		args = append(args, *filters.SinceRevision)
	}
	if memoryType := strings.ToLower(strings.TrimSpace(filters.MemoryType)); memoryType != "" {
		query += ` AND memory_type=?`
		args = append(args, memoryType)
	}
	if tag := strings.TrimSpace(filters.Tag); tag != "" {
		needle, err := json.Marshal(tag)
		if err != nil {
			return nil, err
		}
		query += ` AND tags LIKE ?`
		args = append(args, "%"+string(needle)+"%")
	}
	if q := strings.TrimSpace(filters.Query); q != "" {
		query += ` AND (id LIKE ? OR content LIKE ? OR memory_type LIKE ? OR tags LIKE ? OR content_hash LIKE ? OR COALESCE(source_agent_id,'') LIKE ? OR COALESCE(source_hostname,'') LIKE ? OR COALESCE(source_client,'') LIKE ? OR COALESCE(provenance_json,'') LIKE ?)`
		needle := "%" + q + "%"
		for range 9 {
			args = append(args, needle)
		}
	}
	limit := uint64(100)
	if filters.Limit != nil {
		limit = min(500, max(1, *filters.Limit))
	}
	query += ` ORDER BY updated_at DESC,server_revision DESC LIMIT ?`
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	response := &GatewayMemorySearchResponse{ProjectIdent: projectIdent, ServerRevision: revision, Memories: make([]GatewayMemory, 0)}
	for rows.Next() {
		memory, err := scanGatewayMemory(rows)
		if err != nil {
			return nil, err
		}
		response.Memories = append(response.Memories, memory)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return response, nil
}

func scanGatewayMemory(row interface{ Scan(...any) error }) (GatewayMemory, error) {
	var item GatewayMemory
	var tags, okf, provenance sql.NullString
	var tombstonedAt sql.NullInt64
	if err := row.Scan(&item.ID, &item.ProjectIdent, &item.ServerRevision, &item.Content, &item.MemoryType, &tags, &item.ContentHash,
		&item.ConceptHash, &okf, &item.SourceAgentID, &item.SourceHostname, &item.SourceClient, &item.SourceClientVersion,
		&provenance, &item.ClientCreatedAt, &item.ClientUpdatedAt, &item.CreatedAt, &item.UpdatedAt, &tombstonedAt); err != nil {
		return GatewayMemory{}, err
	}
	item.Tags = []string{}
	if tags.Valid {
		_ = json.Unmarshal([]byte(tags.String), &item.Tags)
		if item.Tags == nil {
			item.Tags = []string{}
		}
	}
	if okf.Valid {
		if !json.Valid([]byte(okf.String)) {
			return GatewayMemory{}, fmt.Errorf("decode memory OKF JSON: invalid JSON")
		}
		item.OKF = json.RawMessage(okf.String)
	}
	item.Provenance = any(json.RawMessage("null"))
	if provenance.Valid && json.Valid([]byte(provenance.String)) {
		item.Provenance = json.RawMessage(provenance.String)
	}
	if tombstonedAt.Valid {
		item.TombstonedAt = &tombstonedAt.Int64
		item.Tombstone = true
	}
	return item, nil
}

func (s *Store) DiscoverGatewayMemoryProjects(ctx context.Context) (*GatewayMemoryProjectsResponse, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project_ident,COUNT(*) FILTER (WHERE tombstoned_at IS NULL),COALESCE(MAX(server_revision),0)
		FROM gateway_memories GROUP BY project_ident ORDER BY project_ident ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	response := &GatewayMemoryProjectsResponse{Projects: make([]GatewayMemoryProject, 0)}
	for rows.Next() {
		var item GatewayMemoryProject
		if err := rows.Scan(&item.ProjectIdent, &item.MemoryCount, &item.ServerRevision); err != nil {
			return nil, err
		}
		response.Projects = append(response.Projects, item)
	}
	return response, rows.Err()
}

func (s *Store) PullGatewayMemories(ctx context.Context, projectIdent string, request GatewayMemoryPullRequest) (*GatewayMemoryPullResponse, error) {
	projectIdent = projects.NormalizeIdent(projectIdent)
	since := int64(0)
	if request.SinceRevision != nil && *request.SinceRevision > 0 {
		since = *request.SinceRevision
	}
	pageSize := uint64(100)
	if request.PageSize != nil {
		pageSize = *request.PageSize
	}
	if pageSize == 0 {
		pageSize = 1
	}
	if pageSize > 500 {
		pageSize = 500
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	query := gatewayMemorySelect + ` WHERE project_ident=? AND server_revision>?`
	if !request.IncludeTombstones {
		query += ` AND tombstoned_at IS NULL`
	}
	query += ` ORDER BY server_revision ASC LIMIT ?`
	rows, err := tx.QueryContext(ctx, query, projectIdent, since, pageSize+1)
	if err != nil {
		return nil, err
	}
	records := make([]GatewayMemory, 0)
	for rows.Next() {
		item, err := scanGatewayMemory(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		records = append(records, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	hasMore := uint64(len(records)) > pageSize
	if hasMore {
		records = records[:pageSize]
	}
	nextRevision := since
	if len(records) != 0 {
		nextRevision = records[len(records)-1].ServerRevision
	}
	response := &GatewayMemoryPullResponse{
		ProjectIdent: projectIdent, SinceRevision: since, NextSinceRevision: nextRevision,
		HasMore: hasMore, Memories: make([]GatewayMemory, 0), Tombstones: make([]GatewayMemoryTombstone, 0),
		Conflicts: make([]GatewayMemoryConflict, 0),
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(server_revision),0) FROM gateway_memories WHERE project_ident=?`, projectIdent).Scan(&response.ServerRevision); err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.TombstonedAt != nil {
			response.Tombstones = append(response.Tombstones, GatewayMemoryTombstone{
				GatewayMemoryID: record.ID, ServerRevision: record.ServerRevision, ProjectIdent: record.ProjectIdent,
				ContentHash: record.ContentHash, TombstonedAt: *record.TombstonedAt, UpdatedAt: record.UpdatedAt,
				SourceAgentID: record.SourceAgentID, SourceHostname: record.SourceHostname,
				SourceClient: record.SourceClient, SourceClientVersion: record.SourceClientVersion,
			})
		} else {
			response.Memories = append(response.Memories, record)
		}
	}
	for _, known := range request.Known {
		var current GatewayMemoryConflict
		current.GatewayMemoryID = known.GatewayMemoryID
		current.BaseGatewayRevision = known.ServerRevision
		err := tx.QueryRowContext(ctx, `SELECT server_revision,content_hash,updated_at FROM gateway_memories WHERE project_ident=? AND id=?`, projectIdent, known.GatewayMemoryID).Scan(
			&current.RemoteServerRevision, &current.RemoteContentHash, &current.RemoteUpdatedAt)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		if current.RemoteServerRevision > known.ServerRevision {
			current.Reason = "remote revision moved since base"
			response.Conflicts = append(response.Conflicts, current)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return response, nil
}
