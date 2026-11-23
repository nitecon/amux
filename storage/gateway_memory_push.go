package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/projects"
)

const GatewayMemoryBatchLimit = 500

const (
	GatewayMemoryOKFMaxEnvelopeBytes = 1 << 20
	GatewayMemoryOKFMaxDocumentBytes = 512 << 10
	GatewayMemoryOKFMaxDepth         = 16
	GatewayMemoryOKFMaxNodes         = 16384
)

type GatewayMemoryPushSource struct {
	AgentID, Hostname, ClientID, ClientVersion *string
}

type GatewayMemoryPushItem struct {
	LocalMemoryID   *string         `json:"local_memory_id"`
	ClientID        *string         `json:"client_id"`
	GatewayMemoryID *string         `json:"gateway_memory_id"`
	BaseRevision    *int64          `json:"base_gateway_revision"`
	ProjectIdent    *string         `json:"project_ident"`
	Scope           *string         `json:"scope"`
	Content         *string         `json:"content"`
	MemoryType      *string         `json:"memory_type"`
	Type            *string         `json:"type"`
	Tags            *[]string       `json:"tags"`
	ContentHash     *string         `json:"content_hash"`
	ConceptHash     *string         `json:"concept_hash"`
	OKF             json.RawMessage `json:"okf"`
	ClientCreatedAt *int64          `json:"created_at"`
	ClientUpdatedAt *int64          `json:"updated_at"`
	Provenance      json.RawMessage `json:"provenance"`
	Tombstone       json.RawMessage `json:"tombstone"`
	Deleted         bool            `json:"deleted"`
}

func (item GatewayMemoryPushItem) memoryType() *string {
	if item.MemoryType != nil {
		return item.MemoryType
	}
	return item.Type
}

func (item GatewayMemoryPushItem) okfPresent() bool {
	return len(item.OKF) != 0 && !strings.EqualFold(strings.TrimSpace(string(item.OKF)), "null")
}

func memoryOKFMatches(item GatewayMemoryPushItem, record GatewayMemory) bool {
	if !item.okfPresent() {
		return true
	}
	if len(record.OKF) == 0 {
		return false
	}
	decode := func(raw []byte) (any, error) {
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		var value any
		err := decoder.Decode(&value)
		return value, err
	}
	want, err := decode(item.OKF)
	if err != nil {
		return false
	}
	have, err := decode(record.OKF)
	return err == nil && reflect.DeepEqual(want, have)
}

func (item *GatewayMemoryPushItem) tombstoneRequested() bool {
	if item.Deleted {
		return true
	}
	var flag bool
	if json.Unmarshal(item.Tombstone, &flag) == nil && flag {
		return true
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(item.Tombstone, &object) == nil && object != nil {
		var deleted bool
		if raw, ok := object["deleted"]; ok && json.Unmarshal(raw, &deleted) == nil {
			return deleted
		}
		return true
	}
	return false
}

type GatewayMemoryPushConflict struct {
	GatewayMemoryID      string `json:"gateway_memory_id"`
	BaseGatewayRevision  *int64 `json:"base_gateway_revision"`
	RemoteServerRevision int64  `json:"remote_server_revision"`
	RemoteContentHash    string `json:"remote_content_hash"`
	RemoteUpdatedAt      int64  `json:"remote_updated_at"`
	Reason               string `json:"reason"`
}

type GatewayMemoryPushResult struct {
	Action          string                     `json:"action"`
	LocalMemoryID   *string                    `json:"local_memory_id,omitempty"`
	ClientID        *string                    `json:"client_id,omitempty"`
	GatewayMemoryID *string                    `json:"gateway_memory_id,omitempty"`
	ServerRevision  *int64                     `json:"server_revision,omitempty"`
	ContentHash     *string                    `json:"content_hash,omitempty"`
	Conflict        *GatewayMemoryPushConflict `json:"conflict,omitempty"`
	Error           *string                    `json:"error,omitempty"`
}

type GatewayMemoryPushResponse struct {
	ProjectIdent   string                    `json:"project_ident"`
	ServerRevision int64                     `json:"server_revision"`
	Results        []GatewayMemoryPushResult `json:"results"`
}

func cleanMemoryString(value *string) *string {
	if value == nil {
		return nil
	}
	clean := strings.TrimSpace(*value)
	if clean == "" {
		return nil
	}
	return &clean
}

func gatewayMemoryNow() int64 { return time.Now().UnixMilli() }

func memoryHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func normalizedGatewayMemoryType(value string, global bool) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", fmt.Errorf("memory_type is required")
	}
	if value == "working_context" {
		return "", fmt.Errorf("working_context memories are never synced to the gateway")
	}
	globalType := value == "global" || value == "preference" || value == "user_preference"
	if globalType && !global {
		return "", fmt.Errorf("only project-scoped durable memories may be pushed")
	}
	if !globalType && global {
		return "", fmt.Errorf("only global-scoped durable memories may be pushed under __global__")
	}
	return value, nil
}

func pushResult(action string, item GatewayMemoryPushItem, clientID *string) GatewayMemoryPushResult {
	return GatewayMemoryPushResult{Action: action, LocalMemoryID: cleanMemoryString(item.LocalMemoryID), ClientID: clientID, GatewayMemoryID: cleanMemoryString(item.GatewayMemoryID)}
}

func rejectedMemoryPush(item GatewayMemoryPushItem, clientID *string, message string) GatewayMemoryPushResult {
	result := pushResult("rejected", item, clientID)
	result.Error = &message
	return result
}

func (s *Store) PushGatewayMemories(ctx context.Context, projectIdent string, source GatewayMemoryPushSource, items []GatewayMemoryPushItem) (*GatewayMemoryPushResponse, error) {
	projectIdent = projects.NormalizeIdent(projectIdent)
	if len(items) > GatewayMemoryBatchLimit {
		return nil, fmt.Errorf("memory push batch exceeds %d records", GatewayMemoryBatchLimit)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	global := projectIdent == GatewayGlobalMemoryProjectIdent
	if global {
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects(ident,channel_name,room_id,created_at,kind) VALUES(?,?,?,?,'adhoc') ON CONFLICT(ident) DO NOTHING`, projectIdent, projectIdent, projectIdent, gatewayMemoryNow()); err != nil {
			return nil, err
		}
	} else {
		var exists int
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM projects WHERE ident=?)", projectIdent).Scan(&exists); err != nil {
			return nil, err
		}
		if exists == 0 {
			response := rejectMissingGatewayMemoryProject(projectIdent, source, items)
			return &response, nil
		}
	}
	response := &GatewayMemoryPushResponse{ProjectIdent: projectIdent, Results: make([]GatewayMemoryPushResult, 0, len(items))}
	for _, item := range items {
		result, err := pushGatewayMemoryItem(ctx, tx, projectIdent, source, item, global)
		if err != nil {
			return nil, err
		}
		response.Results = append(response.Results, result)
	}
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(server_revision),0) FROM gateway_memories WHERE project_ident=?", projectIdent).Scan(&response.ServerRevision); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return response, nil
}

func rejectMissingGatewayMemoryProject(project string, source GatewayMemoryPushSource, items []GatewayMemoryPushItem) GatewayMemoryPushResponse {
	message := fmt.Sprintf("project '%s' not found on gateway", project)
	response := GatewayMemoryPushResponse{ProjectIdent: project, Results: make([]GatewayMemoryPushResult, 0, len(items))}
	for _, item := range items {
		clientID := cleanMemoryString(item.ClientID)
		if clientID == nil {
			clientID = cleanMemoryString(source.ClientID)
		}
		response.Results = append(response.Results, rejectedMemoryPush(item, clientID, message))
	}
	return response
}

func pushGatewayMemoryItem(ctx context.Context, tx *sql.Tx, project string, source GatewayMemoryPushSource, item GatewayMemoryPushItem, global bool) (GatewayMemoryPushResult, error) {
	var err error
	clientID := cleanMemoryString(item.ClientID)
	if clientID == nil {
		clientID = cleanMemoryString(source.ClientID)
	}
	localID, gatewayID := cleanMemoryString(item.LocalMemoryID), cleanMemoryString(item.GatewayMemoryID)
	reject := func(message string) (GatewayMemoryPushResult, error) {
		return rejectedMemoryPush(item, clientID, message), nil
	}
	if item.Scope != nil {
		scope := strings.ToLower(strings.TrimSpace(*item.Scope))
		if scope != "" && ((global && scope != "global") || (!global && scope != "project")) {
			return reject(map[bool]string{true: "only global-scoped durable memories may be pushed under __global__", false: "only project-scoped durable memories may be pushed"}[global])
		}
	}
	if itemProject := cleanMemoryString(item.ProjectIdent); itemProject != nil && projects.NormalizeIdent(*itemProject) != project {
		return reject(fmt.Sprintf("memory project_ident '%s' does not match route project '%s'", projects.NormalizeIdent(*itemProject), project))
	}
	if err := validateGatewayMemoryOKF(item); err != nil {
		return reject(err.Error())
	}
	var record GatewayMemory
	exists := false
	if gatewayID != nil {
		record, exists, err = readGatewayMemory(ctx, tx, project, *gatewayID)
		if err != nil {
			return GatewayMemoryPushResult{}, err
		}
		if !exists {
			if item.tombstoneRequested() {
				revision, err := gatewayMemoryRevision(ctx, tx, project)
				if err != nil {
					return GatewayMemoryPushResult{}, err
				}
				hash := cleanMemoryString(item.ContentHash)
				result := pushResult("tombstoned", item, clientID)
				result.ServerRevision = &revision
				result.ContentHash = hash
				return result, nil
			}
			return reject("gateway_memory_id was not found for this project")
		}
	} else if clientID != nil && localID != nil {
		var linkedID string
		err = tx.QueryRowContext(ctx, `SELECT gateway_memory_id FROM gateway_memory_client_links WHERE project_ident=? AND client_id=? AND local_memory_id=?`, project, *clientID, *localID).Scan(&linkedID)
		if err == nil {
			record, exists, err = readGatewayMemory(ctx, tx, project, linkedID)
		}
		if err != nil && err != sql.ErrNoRows {
			return GatewayMemoryPushResult{}, err
		}
	}
	if item.tombstoneRequested() {
		if !exists {
			return reject("tombstone push requires gateway_memory_id or an existing client link")
		}
		if item.BaseRevision != nil && *item.BaseRevision != record.ServerRevision {
			return memoryConflictResult(item, clientID, record), nil
		}
		if record.TombstonedAt != nil {
			if err := upsertGatewayMemoryLink(ctx, tx, project, clientID, localID, record.ID); err != nil {
				return GatewayMemoryPushResult{}, err
			}
			return gatewayMemoryResult("linked", item, clientID, record), nil
		}
		record, err = tombstoneGatewayMemory(ctx, tx, project, record, source, clientID, localID)
		if err != nil {
			return GatewayMemoryPushResult{}, err
		}
		return gatewayMemoryResult("tombstoned", item, clientID, record), nil
	}
	if item.Content == nil {
		return reject("content is required for memory push")
	}
	content := *item.Content
	hash := memoryHash(content)
	if supplied := cleanMemoryString(item.ContentHash); supplied != nil && *supplied != hash {
		return reject("content_hash does not match content")
	}
	if exists {
		matches := (item.ConceptHash == nil || (record.ConceptHash != nil && *item.ConceptHash == *record.ConceptHash)) && memoryOKFMatches(item, record)
		if item.BaseRevision != nil && *item.BaseRevision != record.ServerRevision {
			if record.ContentHash == hash && record.TombstonedAt == nil && matches {
				if err := upsertGatewayMemoryLink(ctx, tx, project, clientID, localID, record.ID); err != nil {
					return GatewayMemoryPushResult{}, err
				}
				return gatewayMemoryResult("linked", item, clientID, record), nil
			}
			return memoryConflictResult(item, clientID, record), nil
		} else if item.BaseRevision == nil && (record.ContentHash != hash || record.TombstonedAt != nil || !matches) {
			return memoryConflictResult(item, clientID, record), nil
		}
		typ := record.MemoryType
		if memoryType := item.memoryType(); memoryType != nil {
			typ, err = normalizedGatewayMemoryType(*memoryType, global)
			if err != nil {
				return reject(err.Error())
			}
		}
		tags := record.Tags
		if item.Tags != nil {
			tags = cleanGatewayMemoryTags(*item.Tags)
		}
		oldTags, _ := json.Marshal(record.Tags)
		newTags, _ := json.Marshal(tags)
		if record.ContentHash == hash && record.MemoryType == typ && matches && string(oldTags) == string(newTags) && record.TombstonedAt == nil {
			if err := upsertGatewayMemoryLink(ctx, tx, project, clientID, localID, record.ID); err != nil {
				return GatewayMemoryPushResult{}, err
			}
			return gatewayMemoryResult("linked", item, clientID, record), nil
		}
		record, err = updateGatewayMemory(ctx, tx, project, record, item, source, clientID, localID, content, typ, tags, hash)
		if err != nil {
			return GatewayMemoryPushResult{}, err
		}
		return gatewayMemoryResult("updated", item, clientID, record), nil
	}
	var duplicate GatewayMemory
	duplicate, exists, err = readActiveMemoryByHash(ctx, tx, project, hash)
	if err != nil {
		return GatewayMemoryPushResult{}, err
	}
	if exists {
		matches := (item.ConceptHash == nil || (duplicate.ConceptHash != nil && *item.ConceptHash == *duplicate.ConceptHash)) && memoryOKFMatches(item, duplicate)
		if !matches {
			return memoryConflictResult(item, clientID, duplicate), nil
		}
		if err := upsertGatewayMemoryLink(ctx, tx, project, clientID, localID, duplicate.ID); err != nil {
			return GatewayMemoryPushResult{}, err
		}
		return gatewayMemoryResult("linked", item, clientID, duplicate), nil
	}
	memoryType := item.memoryType()
	if memoryType == nil {
		return reject("memory_type is required")
	}
	typ, err := normalizedGatewayMemoryType(*memoryType, global)
	if err != nil {
		return reject(err.Error())
	}
	tags := []string{}
	if item.Tags != nil {
		tags = cleanGatewayMemoryTags(*item.Tags)
	}
	record, err = createGatewayMemory(ctx, tx, project, item, source, clientID, localID, content, typ, tags, hash)
	if err != nil {
		return GatewayMemoryPushResult{}, err
	}
	return gatewayMemoryResult("created", item, clientID, record), nil
}

func validateGatewayMemoryOKF(item GatewayMemoryPushItem) error {
	if item.ConceptHash != nil && !validMemoryHash(*item.ConceptHash) {
		return fmt.Errorf("concept_hash must be a lowercase SHA-256 hex digest")
	}
	if !item.okfPresent() {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(item.OKF)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("invalid OKF memory envelope")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("invalid OKF memory envelope")
	}
	nodes := 0
	var visit func(any, int) error
	visit = func(current any, depth int) error {
		nodes++
		if depth > GatewayMemoryOKFMaxDepth || nodes > GatewayMemoryOKFMaxNodes {
			return fmt.Errorf("okf exceeds nesting or node limit")
		}
		switch v := current.(type) {
		case map[string]any:
			for key, child := range v {
				if strings.ContainsRune(key, '\x00') {
					return fmt.Errorf("okf keys must not contain NUL")
				}
				if err := visit(child, depth+1); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range v {
				if err := visit(child, depth+1); err != nil {
					return err
				}
			}
		case string:
			if strings.ContainsRune(v, '\x00') {
				return fmt.Errorf("okf strings must not contain NUL")
			}
		}
		return nil
	}
	if err := visit(value, 1); err != nil {
		return err
	}
	if len(item.OKF) > GatewayMemoryOKFMaxEnvelopeBytes {
		return fmt.Errorf("okf exceeds envelope byte limit")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("okf must be an object")
	}
	version, ok := object["version"].(json.Number)
	versionValue, versionErr := strconv.ParseUint(string(version), 10, 64)
	if !ok || versionErr != nil || versionValue != 1 {
		return fmt.Errorf("unsupported okf version; expected 1")
	}
	format, _ := object["format"].(string)
	if format != "okf-markdown" {
		return fmt.Errorf("unsupported okf format; expected okf-markdown")
	}
	revision, ok := object["revision"].(json.Number)
	revisionValue, revisionErr := strconv.ParseUint(string(revision), 10, 64)
	if !ok || revisionErr != nil || revisionValue == 0 {
		return fmt.Errorf("okf revision must be a positive integer")
	}
	semanticHash, _ := object["semantic_hash"].(string)
	if !validMemoryHash(semanticHash) {
		return fmt.Errorf("okf semantic_hash must be a lowercase SHA-256 hex digest")
	}
	if item.ConceptHash != nil && *item.ConceptHash != semanticHash {
		return fmt.Errorf("concept_hash does not match okf.semantic_hash")
	}
	document, ok := object["document"].(string)
	if !ok {
		return fmt.Errorf("okf document must be a string")
	}
	if len(document) > GatewayMemoryOKFMaxDocumentBytes {
		return fmt.Errorf("okf document exceeds byte limit")
	}
	return nil
}

func validMemoryHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func readGatewayMemory(ctx context.Context, q queryer, project, id string) (GatewayMemory, bool, error) {
	item, err := readGatewayMemoryByID(ctx, q, project, id)
	if err == sql.ErrNoRows {
		return GatewayMemory{}, false, nil
	}
	return item, err == nil, err
}
func readGatewayMemoryByID(ctx context.Context, q queryer, project, id string) (GatewayMemory, error) {
	return scanGatewayMemory(q.QueryRowContext(ctx, gatewayMemorySelect+` WHERE project_ident=? AND id=?`, project, id))
}
func readActiveMemoryByHash(ctx context.Context, q queryer, project, hash string) (GatewayMemory, bool, error) {
	item, err := scanGatewayMemory(q.QueryRowContext(ctx, gatewayMemorySelect+` WHERE project_ident=? AND content_hash=? AND tombstoned_at IS NULL ORDER BY server_revision LIMIT 1`, project, hash))
	if err == sql.ErrNoRows {
		return GatewayMemory{}, false, nil
	}
	return item, err == nil, err
}
func gatewayMemoryRevision(ctx context.Context, q queryer, project string) (int64, error) {
	var revision int64
	err := q.QueryRowContext(ctx, "SELECT COALESCE(MAX(server_revision),0) FROM gateway_memories WHERE project_ident=?", project).Scan(&revision)
	return revision, err
}
func nextGatewayMemoryRevision(ctx context.Context, q queryer, project string) (int64, error) {
	revision, err := gatewayMemoryRevision(ctx, q, project)
	return revision + 1, err
}
func cleanGatewayMemoryTags(tags []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag != "" && !seen[tag] {
			seen[tag] = true
			out = append(out, tag)
		}
	}
	return out
}
func upsertGatewayMemoryLink(ctx context.Context, tx *sql.Tx, project string, client, local *string, gatewayID string) error {
	if client == nil || local == nil {
		return nil
	}
	now := gatewayMemoryNow()
	_, err := tx.ExecContext(ctx, `INSERT INTO gateway_memory_client_links(project_ident,client_id,local_memory_id,gateway_memory_id,created_at,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(project_ident,client_id,local_memory_id) DO UPDATE SET gateway_memory_id=excluded.gateway_memory_id,updated_at=excluded.updated_at`, project, *client, *local, gatewayID, now, now)
	return err
}
func gatewayMemoryResult(action string, item GatewayMemoryPushItem, client *string, record GatewayMemory) GatewayMemoryPushResult {
	revision, hash, id := record.ServerRevision, record.ContentHash, record.ID
	return GatewayMemoryPushResult{Action: action, LocalMemoryID: cleanMemoryString(item.LocalMemoryID), ClientID: client, GatewayMemoryID: &id, ServerRevision: &revision, ContentHash: &hash}
}
func memoryConflictResult(item GatewayMemoryPushItem, client *string, record GatewayMemory) GatewayMemoryPushResult {
	result := gatewayMemoryResult("conflict", item, client, record)
	result.Conflict = &GatewayMemoryPushConflict{GatewayMemoryID: record.ID, BaseGatewayRevision: item.BaseRevision, RemoteServerRevision: record.ServerRevision, RemoteContentHash: record.ContentHash, RemoteUpdatedAt: record.UpdatedAt, Reason: "remote revision moved since base"}
	return result
}

func createGatewayMemory(ctx context.Context, tx *sql.Tx, project string, item GatewayMemoryPushItem, source GatewayMemoryPushSource, client, local *string, content, typ string, tags []string, hash string) (GatewayMemory, error) {
	revision, err := nextGatewayMemoryRevision(ctx, tx, project)
	if err != nil {
		return GatewayMemory{}, err
	}
	id := uuid.NewString()
	now := gatewayMemoryNow()
	tagsJSON, _ := json.Marshal(tags)
	var provenance any
	if len(item.Provenance) > 0 {
		provenance = string(item.Provenance)
	}
	var okf any
	if item.okfPresent() {
		okf = string(item.OKF)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO gateway_memories(id,project_ident,server_revision,content,memory_type,tags,content_hash,concept_hash,okf_json,source_agent_id,source_hostname,source_client,source_client_version,provenance_json,client_created_at,client_updated_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, project, revision, content, typ, string(tagsJSON), hash, item.ConceptHash, okf, source.AgentID, source.Hostname, client, source.ClientVersion, provenance, item.ClientCreatedAt, item.ClientUpdatedAt, now, now)
	if err != nil {
		return GatewayMemory{}, err
	}
	if err = upsertGatewayMemoryLink(ctx, tx, project, client, local, id); err != nil {
		return GatewayMemory{}, err
	}
	return readGatewayMemoryByID(ctx, tx, project, id)
}
func updateGatewayMemory(ctx context.Context, tx *sql.Tx, project string, record GatewayMemory, item GatewayMemoryPushItem, source GatewayMemoryPushSource, client, local *string, content, typ string, tags []string, hash string) (GatewayMemory, error) {
	revision, err := nextGatewayMemoryRevision(ctx, tx, project)
	if err != nil {
		return GatewayMemory{}, err
	}
	now := gatewayMemoryNow()
	tagsJSON, _ := json.Marshal(tags)
	var provenance any
	if len(item.Provenance) > 0 {
		provenance = string(item.Provenance)
	}
	concept := item.ConceptHash
	if concept == nil && hash == record.ContentHash && !item.okfPresent() {
		concept = record.ConceptHash
	}
	var okf any
	if item.okfPresent() {
		okf = string(item.OKF)
	} else if hash == record.ContentHash && item.ConceptHash == nil && len(record.OKF) > 0 {
		okf = string(record.OKF)
	}
	created := item.ClientCreatedAt
	if created == nil {
		created = record.ClientCreatedAt
	}
	_, err = tx.ExecContext(ctx, `UPDATE gateway_memories SET server_revision=?,content=?,memory_type=?,tags=?,content_hash=?,concept_hash=?,okf_json=?,source_agent_id=?,source_hostname=?,source_client=?,source_client_version=?,provenance_json=?,client_created_at=?,client_updated_at=?,updated_at=?,tombstoned_at=NULL WHERE project_ident=? AND id=?`, revision, content, typ, string(tagsJSON), hash, concept, okf, source.AgentID, source.Hostname, client, source.ClientVersion, provenance, created, item.ClientUpdatedAt, now, project, record.ID)
	if err != nil {
		return GatewayMemory{}, err
	}
	if err = upsertGatewayMemoryLink(ctx, tx, project, client, local, record.ID); err != nil {
		return GatewayMemory{}, err
	}
	return readGatewayMemoryByID(ctx, tx, project, record.ID)
}
func tombstoneGatewayMemory(ctx context.Context, tx *sql.Tx, project string, record GatewayMemory, source GatewayMemoryPushSource, client, local *string) (GatewayMemory, error) {
	revision, err := nextGatewayMemoryRevision(ctx, tx, project)
	if err != nil {
		return GatewayMemory{}, err
	}
	now := gatewayMemoryNow()
	_, err = tx.ExecContext(ctx, `UPDATE gateway_memories SET server_revision=?,source_agent_id=?,source_hostname=?,source_client=?,source_client_version=?,updated_at=?,tombstoned_at=? WHERE project_ident=? AND id=?`, revision, source.AgentID, source.Hostname, client, source.ClientVersion, now, now, project, record.ID)
	if err != nil {
		return GatewayMemory{}, err
	}
	if err = upsertGatewayMemoryLink(ctx, tx, project, client, local, record.ID); err != nil {
		return GatewayMemory{}, err
	}
	return readGatewayMemoryByID(ctx, tx, project, record.ID)
}
