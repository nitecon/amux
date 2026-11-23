package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestSkillReadRoutesPreserveLegacyRecordsAndOpaqueContent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-skills.sqlite")
	schema, err := os.ReadFile(filepath.Join("..", "storage", "testdata", "task_status_legacy.sql"))
	if err != nil {
		t.Fatal(err)
	}
	legacyDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacyDB.SetMaxOpenConns(1)
	if _, err := legacyDB.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	legacySchema := strings.Replace(string(schema), ", kind TEXT NOT NULL DEFAULT 'skill', content TEXT);", ");", 1)
	if legacySchema == string(schema) {
		t.Fatal("legacy fixture skills table shape changed")
	}
	if _, err := legacyDB.ExecContext(ctx, legacySchema); err != nil {
		t.Fatalf("load legacy profile schema: %v", err)
	}
	zipData := []byte{0x00, 0xff, 0x50, 0x4b, 0x00, 0x7f}
	if _, err := legacyDB.ExecContext(ctx, `INSERT INTO skills(name,zip_data,size,checksum,uploaded_at) VALUES('legacy-skill',?,?,?,100)`, zipData, len(zipData), "raw-checksum"); err != nil {
		t.Fatalf("seed synthetic legacy skills: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open legacy profile: %v", err)
	}
	defer store.Close()
	markdown := "# Opaque agent body\n\nbytes stay unchanged.\n"
	modernDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := modernDB.ExecContext(ctx, `INSERT INTO skills(name,zip_data,size,checksum,uploaded_at,kind,content) VALUES
('legacy-command',X'',0,'command-checksum',200,'command',NULL),
(?,?,?, ?,300,'agent',?)`, `quote"agent`, []byte{}, len(markdown), "agent-checksum", markdown); err != nil {
		_ = modernDB.Close()
		t.Fatalf("seed synthetic text skills: %v", err)
	}
	if err := modernDB.Close(); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(store, "skill-read-key")
	if err != nil {
		t.Fatal(err)
	}
	request := func(path, authorization string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	if unauthorized := request("/v1/skills", ""); unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("skills without bearer auth status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	const auth = "Bearer skill-read-key"
	list := request("/v1/skills", auth)
	if list.Code != http.StatusOK {
		t.Fatalf("list skills status=%d body=%s", list.Code, list.Body.String())
	}
	var all []storage.SkillMeta
	if err := json.Unmarshal(list.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Kind != "agent" || all[0].Name != `quote"agent` || all[1].Kind != "command" || all[2].Kind != "skill" {
		t.Fatalf("legacy skills were not retained in source ordering: %+v", all)
	}
	filtered := request("/v1/skills?kind=skill", auth)
	var onlySkills []storage.SkillMeta
	if filtered.Code != http.StatusOK || json.Unmarshal(filtered.Body.Bytes(), &onlySkills) != nil || len(onlySkills) != 1 || onlySkills[0].Name != "legacy-skill" {
		t.Fatalf("exact kind filter status=%d body=%s", filtered.Code, filtered.Body.String())
	}
	if emptyFilter := request("/v1/skills?kind=", auth); emptyFilter.Code != http.StatusOK || !bytes.Contains(emptyFilter.Body.Bytes(), []byte(`"legacy-command"`)) {
		t.Fatalf("empty kind should mean no filter: %d %s", emptyFilter.Code, emptyFilter.Body.String())
	}
	if wrongCase := request("/v1/skills?kind=COMMAND", auth); wrongCase.Code != http.StatusBadRequest || !bytes.Contains(wrongCase.Body.Bytes(), []byte("invalid kind 'COMMAND'")) {
		t.Fatalf("kind filter was not exact/case-sensitive: %d %s", wrongCase.Code, wrongCase.Body.String())
	}

	zip := request("/v1/skills/legacy-skill", auth)
	mediaType, _, err := mime.ParseMediaType(zip.Header().Get("Content-Type"))
	if err != nil || zip.Code != http.StatusOK || mediaType != "application/zip" || zip.Header().Get("X-Kind") != "skill" || !bytes.Equal(zip.Body.Bytes(), zipData) {
		t.Fatalf("zip download modified opaque bytes: status=%d headers=%v body=%v err=%v", zip.Code, zip.Header(), zip.Body.Bytes(), err)
	}
	_, zipParams, err := mime.ParseMediaType(zip.Header().Get("Content-Disposition"))
	if err != nil || zipParams["filename"] != "legacy-skill.zip" {
		t.Fatalf("zip filename header was not safely encoded: %q %v", zip.Header().Get("Content-Disposition"), err)
	}

	command := request("/v1/skills/legacy-command", auth)
	mediaType, params, err := mime.ParseMediaType(command.Header().Get("Content-Type"))
	if err != nil || command.Code != http.StatusOK || mediaType != "text/markdown" || params["charset"] != "utf-8" || command.Header().Get("X-Kind") != "command" || command.Body.Len() != 0 {
		t.Fatalf("NULL command download should return empty markdown bytes: status=%d headers=%v body=%q err=%v", command.Code, command.Header(), command.Body.String(), err)
	}
	_, commandParams, err := mime.ParseMediaType(command.Header().Get("Content-Disposition"))
	if err != nil || commandParams["filename"] != "legacy-command.md" {
		t.Fatalf("command filename header was not safely encoded: %q %v", command.Header().Get("Content-Disposition"), err)
	}

	namePath := "/v1/skills/" + url.PathEscape(`quote"agent`)
	agent := request(namePath, auth)
	if agent.Code != http.StatusOK || agent.Header().Get("X-Kind") != "agent" || agent.Body.String() != markdown {
		t.Fatalf("agent content bytes changed: status=%d headers=%v body=%q", agent.Code, agent.Header(), agent.Body.String())
	}
	_, agentParams, err := mime.ParseMediaType(agent.Header().Get("Content-Disposition"))
	if err != nil || agentParams["filename"] != `quote"agent.md` {
		t.Fatalf("unsafe name was not represented in a safe content-disposition: %q %v", agent.Header().Get("Content-Disposition"), err)
	}

	content := request("/v1/skills/legacy-command/content", auth)
	var contentJSON map[string]json.RawMessage
	if content.Code != http.StatusOK || json.Unmarshal(content.Body.Bytes(), &contentJSON) != nil || string(contentJSON["content"]) != "null" || string(contentJSON["name"]) != `"legacy-command"` || string(contentJSON["kind"]) != `"command"` {
		t.Fatalf("content endpoint must retain JSON null: status=%d body=%s", content.Code, content.Body.String())
	}
	if missing := request("/v1/skills/missing", auth); missing.Code != http.StatusNotFound {
		t.Fatalf("missing download status=%d body=%s", missing.Code, missing.Body.String())
	}
	if missing := request("/v1/skills/missing/content", auth); missing.Code != http.StatusNotFound {
		t.Fatalf("missing content status=%d body=%s", missing.Code, missing.Body.String())
	}

	replacement := httptest.NewRequest(http.MethodPost, "/v1/skills/legacy-skill", strings.NewReader(`{"kind":"command","content":"  # Replacement  "}`))
	replacement.Header.Set("Authorization", auth)
	replacement.Header.Set("Content-Type", "application/json")
	updated := httptest.NewRecorder()
	handler.ServeHTTP(updated, replacement)
	if updated.Code != http.StatusOK {
		t.Fatalf("replace migrated ZIP record: status=%d body=%s", updated.Code, updated.Body.String())
	}
	download := request("/v1/skills/legacy-skill", auth)
	if download.Code != http.StatusOK || download.Header().Get("X-Kind") != "command" || download.Body.String() != "  # Replacement  " {
		t.Fatalf("replacement download: status=%d headers=%v body=%q", download.Code, download.Header(), download.Body.String())
	}
	record, err := store.Skill(ctx, "legacy-skill")
	if err != nil || record == nil || len(record.ZipData) != 0 {
		t.Fatalf("replacement retained the old ZIP bytes or failed to load: record=%+v err=%v", record, err)
	}

}

func TestFreshProfileHasEmptyAuthenticatedSkillCatalog(t *testing.T) {
	store, handler := newArtifactWriteHTTPFixture(t, false)
	response := artifactWriteRequest(handler, http.MethodGet, "/v1/skills", nil, nil)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" {
		t.Fatalf("fresh skill catalog status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := store.ListSkills(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestSkillJSONUpsertAndDeleteThroughNewHandler(t *testing.T) {
	ctx := context.Background()
	store, handler := newArtifactWriteHTTPFixture(t, false)
	content := " \n# Opaque Markdown \nbody remains exact.\n\t"

	// Named JSON trims the name only. It hashes and persists the original
	// content bytes, including surrounding whitespace.
	created := artifactWriteRequest(handler, http.MethodPost, "/v1/skills", map[string]any{
		"name": "  shared-name  ", "kind": "command", "content": content,
	}, nil)
	if created.Code != http.StatusOK {
		t.Fatalf("named JSON upsert status=%d body=%s", created.Code, created.Body.String())
	}
	var receipt skillUploadResponse
	if err := json.Unmarshal(created.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	if receipt.Name != "shared-name" || receipt.Kind != "command" || receipt.Size != int64(len(content)) || receipt.Checksum != hex.EncodeToString(sum[:]) {
		t.Fatalf("receipt did not reflect trimmed name and original content bytes: %+v", receipt)
	}

	contentResponse := artifactWriteRequest(handler, http.MethodGet, "/v1/skills/shared-name/content", nil, nil)
	var stored skillContentResponse
	if contentResponse.Code != http.StatusOK || json.Unmarshal(contentResponse.Body.Bytes(), &stored) != nil || stored.Name != "shared-name" || stored.Kind != "command" || stored.Content == nil || *stored.Content != content {
		t.Fatalf("named write did not preserve exact content: status=%d body=%s decoded=%+v", contentResponse.Code, contentResponse.Body.String(), stored)
	}

	// The path address is authoritative, and replacing another kind reuses the
	// same row so the lookup, checksum and bytes all describe the new content.
	if err := store.UpsertSkill(ctx, storage.SkillRecord{Name: "replace-me", Kind: "skill", ZipData: []byte{0x50, 0x4b}, Size: 2, Checksum: "old", UploadedAt: 1}); err != nil {
		t.Fatal(err)
	}
	pathContent := "# Agent body\n\n  exact bytes  \n"
	updated := artifactWriteRequest(handler, http.MethodPost, "/v1/skills/replace-me", map[string]any{
		"name": "ignored-body-name", "kind": "agent", "content": pathContent,
	}, nil)
	if updated.Code != http.StatusOK {
		t.Fatalf("path JSON upsert status=%d body=%s", updated.Code, updated.Body.String())
	}
	if err := json.Unmarshal(updated.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	pathSum := sha256.Sum256([]byte(pathContent))
	if receipt.Name != "replace-me" || receipt.Kind != "agent" || receipt.Size != int64(len(pathContent)) || receipt.Checksum != hex.EncodeToString(pathSum[:]) {
		t.Fatalf("path receipt mismatch: %+v", receipt)
	}
	record, err := store.Skill(ctx, "replace-me")
	if err != nil || record == nil || record.Kind != "agent" || record.Content == nil || *record.Content != pathContent || len(record.ZipData) != 0 {
		t.Fatalf("cross-kind overwrite left stale data: record=%+v err=%v", record, err)
	}
	markdown := artifactWriteRequest(handler, http.MethodGet, "/v1/skills/replace-me", nil, nil)
	if markdown.Code != http.StatusOK || markdown.Body.String() != pathContent || markdown.Header().Get("X-Kind") != "agent" {
		t.Fatalf("download after path write status=%d kind=%q body=%q", markdown.Code, markdown.Header().Get("X-Kind"), markdown.Body.String())
	}

	for _, body := range []map[string]any{
		{"name": "valid", "kind": "COMMAND", "content": "x"},
		{"name": "valid", "kind": "skill", "content": "x"},
		{"name": "valid", "kind": "command", "content": " \n\t"},
		{"name": " \t ", "kind": "agent", "content": "x"},
	} {
		invalid := artifactWriteRequest(handler, http.MethodPost, "/v1/skills", body, nil)
		if invalid.Code != http.StatusBadRequest {
			t.Fatalf("invalid JSON skill accepted (%v): status=%d body=%s", body, invalid.Code, invalid.Body.String())
		}
	}

	unauthorizedWrite := artifactWriteRequest(handler, http.MethodPost, "/v1/skills", map[string]any{
		"name": "denied", "kind": "command", "content": "body",
	}, map[string]string{"Authorization": ""})
	if unauthorizedWrite.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized upsert status=%d body=%s", unauthorizedWrite.Code, unauthorizedWrite.Body.String())
	}
	unauthorized := artifactWriteRequest(handler, http.MethodDelete, "/v1/skills/shared-name", nil, map[string]string{"Authorization": ""})
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized delete status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	deleted := artifactWriteRequest(handler, http.MethodDelete, "/v1/skills/shared-name", nil, nil)
	if deleted.Code != http.StatusNoContent || deleted.Body.Len() != 0 {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	missing := artifactWriteRequest(handler, http.MethodDelete, "/v1/skills/shared-name", nil, nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("repeat delete status=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestSkillRawAndMultipartUploadsThroughNewHandler(t *testing.T) {
	_, handler := newArtifactWriteHTTPFixture(t, false)
	rawRequest := func(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer fixture-key")
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	assertReceipt := func(response *httptest.ResponseRecorder, name, kind string, payload []byte) {
		t.Helper()
		if response.Code != http.StatusOK {
			t.Fatalf("upload status=%d body=%s", response.Code, response.Body.String())
		}
		var receipt skillUploadResponse
		if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(payload)
		if receipt.Name != name || receipt.Kind != kind || receipt.Size != int64(len(payload)) || receipt.Checksum != hex.EncodeToString(sum[:]) {
			t.Fatalf("upload receipt mismatch: %+v", receipt)
		}
	}

	textBody := []byte("\n# Raw agent\n body with spaces \n")
	raw := rawRequest(http.MethodPut, "/v1/skills/raw-text", textBody, map[string]string{
		"Content-Type": "text/markdown; charset=utf-8", "X-Kind": "AGENT",
	})
	assertReceipt(raw, "raw-text", "agent", textBody)
	got := rawRequest(http.MethodGet, "/v1/skills/raw-text", nil, nil)
	if got.Code != http.StatusOK || got.Header().Get("X-Kind") != "agent" || !bytes.Equal(got.Body.Bytes(), textBody) {
		t.Fatalf("raw text readback changed: status=%d kind=%q body=%q", got.Code, got.Header().Get("X-Kind"), got.Body.Bytes())
	}

	zipBody := []byte{0, 0xff, 'P', 'K', 0x03, 0x04, '\n'}
	zip := rawRequest(http.MethodPut, "/v1/skills/raw-zip", zipBody, nil)
	assertReceipt(zip, "raw-zip", "skill", zipBody)
	zipReadback := rawRequest(http.MethodGet, "/v1/skills/raw-zip", nil, nil)
	if zipReadback.Code != http.StatusOK || zipReadback.Header().Get("X-Kind") != "skill" || !bytes.Equal(zipReadback.Body.Bytes(), zipBody) {
		t.Fatalf("raw zip bytes changed: status=%d kind=%q body=%v", zipReadback.Code, zipReadback.Header().Get("X-Kind"), zipReadback.Body.Bytes())
	}
	invalidKind := rawRequest(http.MethodPut, "/v1/skills/rejected", []byte("opaque"), map[string]string{"X-Kind": "zip"})
	if invalidKind.Code != http.StatusBadRequest {
		t.Fatalf("invalid raw X-Kind status=%d body=%s", invalidKind.Code, invalidKind.Body.String())
	}
	emptyKind := rawRequest(http.MethodPut, "/v1/skills/rejected", []byte("opaque"), map[string]string{"X-Kind": ""})
	if emptyKind.Code != http.StatusBadRequest {
		t.Fatalf("empty but present raw X-Kind status=%d body=%s", emptyKind.Code, emptyKind.Body.String())
	}
	invalidUTF8 := rawRequest(http.MethodPut, "/v1/skills/rejected", []byte{0xff}, map[string]string{"X-Kind": "command"})
	if invalidUTF8.Code != http.StatusBadRequest {
		t.Fatalf("invalid text UTF-8 status=%d body=%s", invalidUTF8.Code, invalidUTF8.Body.String())
	}
	emptyText := rawRequest(http.MethodPut, "/v1/skills/rejected", nil, map[string]string{"X-Kind": "command"})
	if emptyText.Code != http.StatusBadRequest {
		t.Fatalf("empty text status=%d body=%s", emptyText.Code, emptyText.Body.String())
	}
	tooLarge := rawRequest(http.MethodPut, "/v1/skills/rejected", bytes.Repeat([]byte{'x'}, skillUploadBodyLimit+1), nil)
	if tooLarge.Code != http.StatusBadRequest {
		t.Fatalf("oversized raw body status=%d body=%s", tooLarge.Code, tooLarge.Body.String())
	}

	buildForm := func(fields map[string]string, file []byte) ([]byte, string) {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for key, value := range fields {
			if err := writer.WriteField(key, value); err != nil {
				t.Fatal(err)
			}
		}
		if file != nil {
			part, err := writer.CreateFormFile("file", "synthetic.zip")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write(file); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return body.Bytes(), writer.FormDataContentType()
	}

	multipartText := []byte("\n# Multipart command\n  preserved  \n")
	form, formType := buildForm(map[string]string{"kind": " COMMAND ", "content": string(multipartText), "metadata": "ignored"}, nil)
	textUpload := rawRequest(http.MethodPost, "/v1/skills/form-command/multipart", form, map[string]string{"Content-Type": formType})
	assertReceipt(textUpload, "form-command", "command", multipartText)
	textReadback := rawRequest(http.MethodGet, "/v1/skills/form-command", nil, nil)
	if textReadback.Code != http.StatusOK || textReadback.Header().Get("X-Kind") != "command" || !bytes.Equal(textReadback.Body.Bytes(), multipartText) {
		t.Fatalf("multipart text readback changed: status=%d kind=%q body=%q", textReadback.Code, textReadback.Header().Get("X-Kind"), textReadback.Body.Bytes())
	}

	formZip := []byte{0x50, 0x4b, 0x00, 0xfe, 0x7f}
	form, formType = buildForm(map[string]string{"kind": "skill", "content": "ignored for zip"}, formZip)
	zipUpload := rawRequest(http.MethodPost, "/v1/skills/form-zip/multipart", form, map[string]string{"Content-Type": formType})
	assertReceipt(zipUpload, "form-zip", "skill", formZip)
	formZipReadback := rawRequest(http.MethodGet, "/v1/skills/form-zip", nil, nil)
	if formZipReadback.Code != http.StatusOK || !bytes.Equal(formZipReadback.Body.Bytes(), formZip) {
		t.Fatalf("multipart zip bytes changed: status=%d body=%v", formZipReadback.Code, formZipReadback.Body.Bytes())
	}

	for _, invalid := range []struct {
		fields map[string]string
		file   []byte
	}{
		{fields: map[string]string{"content": "body"}},
		{fields: map[string]string{"kind": "other"}},
		{fields: map[string]string{"kind": "skill"}},
		{fields: map[string]string{"kind": "skill"}, file: []byte{}},
		{fields: map[string]string{"kind": "agent", "content": " \t "}},
		{fields: map[string]string{"kind": "command"}},
	} {
		invalidBody, invalidType := buildForm(invalid.fields, invalid.file)
		response := rawRequest(http.MethodPost, "/v1/skills/invalid/multipart", invalidBody, map[string]string{"Content-Type": invalidType})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid multipart accepted (%v): status=%d body=%s", invalid, response.Code, response.Body.String())
		}
	}
	oversizedForm := fmt.Sprintf("--b\r\nContent-Disposition: form-data; name=\"kind\"\r\n\r\nskill\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x.zip\"\r\nContent-Type: application/zip\r\n\r\n%s\r\n--b--\r\n", strings.Repeat("x", skillUploadBodyLimit))
	oversized := rawRequest(http.MethodPost, "/v1/skills/too-large/multipart", []byte(oversizedForm), map[string]string{"Content-Type": "multipart/form-data; boundary=b"})
	if oversized.Code != http.StatusBadRequest {
		t.Fatalf("oversized multipart status=%d body=%s", oversized.Code, oversized.Body.String())
	}
}
