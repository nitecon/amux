package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// ArtifactWriteLimits mirrors the artifact and version limits configured by
// the gateway. Values are loaded once, like the Rust operations envelope.
type ArtifactWriteLimits struct {
	VersionBodyMaxBytes       uint64
	StructuredPayloadMaxBytes uint64
	ContributionBodyMaxBytes  uint64
	CommentBodyMaxBytes       uint64
	ChunkTextMaxBytes         uint64
	ReadSetMaxRefs            uint64
	LabelsMaxCount            uint64
	LabelMaxBytes             uint64
	ArtifactSoft              uint64
	ArtifactHard              uint64
	VersionSoft               uint64
	VersionHard               uint64
	LinkSoft                  uint64
	LinkHard                  uint64
	ChunkSoft                 uint64
	ChunkHard                 uint64
}

func defaultArtifactWriteLimits() ArtifactWriteLimits {
	return ArtifactWriteLimits{
		VersionBodyMaxBytes: 1024 * 1024, StructuredPayloadMaxBytes: 512 * 1024,
		ContributionBodyMaxBytes: 256 * 1024, CommentBodyMaxBytes: 32 * 1024,
		ChunkTextMaxBytes: 8 * 1024, ReadSetMaxRefs: 256,
		LabelsMaxCount: 32, LabelMaxBytes: 64,
		ArtifactSoft: 5000, ArtifactHard: 10000, VersionSoft: 50000, VersionHard: 100000,
		LinkSoft: 200000, LinkHard: 400000,
		ChunkSoft: 100000, ChunkHard: 200000,
	}
}

// ArtifactSizeLimitError identifies the stable gateway rejection token.
type ArtifactSizeLimitError struct {
	Kind          string
	Limit, Actual uint64
}

func (e *ArtifactSizeLimitError) Error() string {
	return fmt.Sprintf("size limit exceeded for %s: %d > %d bytes", e.Kind, e.Actual, e.Limit)
}

func (e *ArtifactSizeLimitError) Token() string {
	switch e.Kind {
	case "labels_too_many", "label_too_long", "read_set_too_large":
		return e.Kind
	default:
		return e.Kind + "_too_large"
	}
}

// ArtifactQuotaError is returned before a write when the current count has
// reached its hard threshold.
type ArtifactQuotaError struct {
	Counter        string
	Limit, Current uint64
}

func (e *ArtifactQuotaError) Error() string {
	return fmt.Sprintf("hard quota exceeded for %s: %d >= %d", e.Counter, e.Current, e.Limit)
}

func (e *ArtifactQuotaError) Token() string { return "quota_" + e.Counter + "_exceeded" }

// ArtifactQuotaWarning accompanies a successful write at the soft threshold.
type ArtifactQuotaWarning struct {
	Counter       string `json:"counter"`
	SoftThreshold uint64 `json:"soft_threshold"`
	HardLimit     uint64 `json:"hard_limit"`
	Current       uint64 `json:"current"`
}

func (w ArtifactQuotaWarning) Token() string { return "quota_" + w.Counter + "_soft" }

func parseArtifactWriteLimits(get func(string) (string, bool)) (ArtifactWriteLimits, error) {
	limits := defaultArtifactWriteLimits()
	values := []struct {
		key   string
		field *uint64
	}{
		{"ARTIFACT_VERSION_BODY_MAX_BYTES", &limits.VersionBodyMaxBytes},
		{"ARTIFACT_VERSION_STRUCTURED_PAYLOAD_MAX_BYTES", &limits.StructuredPayloadMaxBytes},
		{"CONTRIBUTION_BODY_MAX_BYTES", &limits.ContributionBodyMaxBytes},
		{"COMMENT_BODY_MAX_BYTES", &limits.CommentBodyMaxBytes},
		{"CHUNK_TEXT_MAX_BYTES", &limits.ChunkTextMaxBytes},
		{"READ_SET_MAX_REFS", &limits.ReadSetMaxRefs},
		{"ARTIFACT_LABELS_MAX_COUNT", &limits.LabelsMaxCount},
		{"ARTIFACT_LABEL_MAX_BYTES", &limits.LabelMaxBytes},
		{"PROJECT_ARTIFACT_SOFT", &limits.ArtifactSoft},
		{"PROJECT_ARTIFACT_HARD", &limits.ArtifactHard},
		{"PROJECT_VERSION_SOFT", &limits.VersionSoft},
		{"PROJECT_VERSION_HARD", &limits.VersionHard},
		{"PROJECT_LINK_SOFT", &limits.LinkSoft},
		{"PROJECT_LINK_HARD", &limits.LinkHard},
		{"PROJECT_CHUNK_SOFT", &limits.ChunkSoft},
		{"PROJECT_CHUNK_HARD", &limits.ChunkHard},
	}
	for _, item := range values {
		raw, ok := get(item.key)
		if !ok {
			continue
		}
		value, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
		if err != nil || value == 0 {
			return ArtifactWriteLimits{}, fmt.Errorf("invalid value for env key %s: expected positive integer", item.key)
		}
		*item.field = value
	}
	if limits.ArtifactSoft > limits.ArtifactHard {
		return ArtifactWriteLimits{}, fmt.Errorf("PROJECT_ARTIFACT_SOFT (%d) must be <= PROJECT_ARTIFACT_HARD (%d)", limits.ArtifactSoft, limits.ArtifactHard)
	}
	if limits.VersionSoft > limits.VersionHard {
		return ArtifactWriteLimits{}, fmt.Errorf("PROJECT_VERSION_SOFT (%d) must be <= PROJECT_VERSION_HARD (%d)", limits.VersionSoft, limits.VersionHard)
	}
	if limits.LinkSoft > limits.LinkHard {
		return ArtifactWriteLimits{}, fmt.Errorf("PROJECT_LINK_SOFT (%d) must be <= PROJECT_LINK_HARD (%d)", limits.LinkSoft, limits.LinkHard)
	}
	if limits.ChunkSoft > limits.ChunkHard {
		return ArtifactWriteLimits{}, fmt.Errorf("PROJECT_CHUNK_SOFT (%d) must be <= PROJECT_CHUNK_HARD (%d)", limits.ChunkSoft, limits.ChunkHard)
	}
	return limits, nil
}

var artifactLimitsOnce sync.Once
var artifactLimitsValue ArtifactWriteLimits
var artifactLimitsErr error

func artifactWriteLimits() (ArtifactWriteLimits, error) {
	artifactLimitsOnce.Do(func() {
		artifactLimitsValue, artifactLimitsErr = parseArtifactWriteLimits(os.LookupEnv)
	})
	return artifactLimitsValue, artifactLimitsErr
}

func checkArtifactLabels(labels []string, limits ArtifactWriteLimits) error {
	if uint64(len(labels)) > limits.LabelsMaxCount {
		return &ArtifactSizeLimitError{Kind: "labels_too_many", Limit: limits.LabelsMaxCount, Actual: uint64(len(labels))}
	}
	for _, label := range labels {
		if uint64(len(label)) > limits.LabelMaxBytes {
			return &ArtifactSizeLimitError{Kind: "label_too_long", Limit: limits.LabelMaxBytes, Actual: uint64(len(label))}
		}
	}
	return nil
}

func checkArtifactVersionSize(body *string, structuredPayload json.RawMessage, limits ArtifactWriteLimits) error {
	if body != nil {
		if err := checkArtifactTextSize("artifact_version_body", *body, limits.VersionBodyMaxBytes); err != nil {
			return err
		}
	}
	if len(structuredPayload) > 0 {
		var compact bytes.Buffer
		if err := json.Compact(&compact, structuredPayload); err != nil {
			return err
		}
		if uint64(compact.Len()) > limits.StructuredPayloadMaxBytes {
			return &ArtifactSizeLimitError{Kind: "structured_payload", Limit: limits.StructuredPayloadMaxBytes, Actual: uint64(compact.Len())}
		}
	}
	return nil
}

func checkArtifactTextSize(kind, body string, limit uint64) error {
	if uint64(len(body)) > limit {
		return &ArtifactSizeLimitError{Kind: kind, Limit: limit, Actual: uint64(len(body))}
	}
	return nil
}

func checkArtifactReadSetSize(raw json.RawMessage, limit uint64) error {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	count := countArtifactJSONRefs(value)
	if uint64(count) > limit {
		return &ArtifactSizeLimitError{Kind: "read_set_too_large", Limit: limit, Actual: uint64(count)}
	}
	return nil
}

func countArtifactJSONRefs(value any) int {
	switch value := value.(type) {
	case []any:
		n := 0
		for _, child := range value {
			n += countArtifactJSONRefs(child)
		}
		return n
	case map[string]any:
		n := 0
		for _, child := range value {
			n += countArtifactJSONRefs(child)
		}
		return n
	case string:
		return 1
	default:
		return 0
	}
}

func checkArtifactQuota(ctx context.Context, tx *sql.Tx, projectIdent, counter string, limits ArtifactWriteLimits) ([]ArtifactQuotaWarning, error) {
	var query string
	var soft, hard uint64
	switch counter {
	case "artifact":
		query = `SELECT COUNT(*) FROM artifacts WHERE project_ident=? AND lifecycle_state!='archived'`
		soft, hard = limits.ArtifactSoft, limits.ArtifactHard
	case "version":
		query = `SELECT COUNT(*) FROM artifact_versions v JOIN artifacts a ON a.artifact_id=v.artifact_id WHERE a.project_ident=?`
		soft, hard = limits.VersionSoft, limits.VersionHard
	case "link":
		soft, hard = limits.LinkSoft, limits.LinkHard
	case "chunk":
		query = `SELECT COUNT(*) FROM artifact_chunks c JOIN artifacts a ON a.artifact_id=c.artifact_id WHERE a.project_ident=? AND c.superseded_by_chunk_id IS NULL`
		soft, hard = limits.ChunkSoft, limits.ChunkHard
	default:
		return nil, fmt.Errorf("unknown artifact quota counter %q", counter)
	}
	var current uint64
	if counter == "link" {
		count, err := countArtifactLinks(ctx, tx, projectIdent)
		if err != nil {
			return nil, err
		}
		current = uint64(count)
	} else if err := tx.QueryRowContext(ctx, query, projectIdent).Scan(&current); err != nil {
		return nil, err
	}
	if current >= hard {
		return nil, &ArtifactQuotaError{Counter: counter, Limit: hard, Current: current}
	}
	if current >= soft {
		return []ArtifactQuotaWarning{{Counter: counter, SoftThreshold: soft, HardLimit: hard, Current: current}}, nil
	}
	return nil, nil
}
