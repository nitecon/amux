package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nitecon/amux/storage"
)

type skillContentResponse struct {
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`
	Content *string `json:"content"`
}

type skillJSONUpsertRequest struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

type skillUploadResponse struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
}

const skillUploadBodyLimit = 2 << 20

func (a *api) registerSkillRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/skills", a.listSkills)
	mux.HandleFunc("POST /v1/skills", a.upsertNamedSkillJSON)
	mux.HandleFunc("GET /v1/skills/{name}", a.downloadSkill)
	mux.HandleFunc("POST /v1/skills/{name}", a.upsertSkillJSON)
	mux.HandleFunc("PUT /v1/skills/{name}", a.uploadSkill)
	mux.HandleFunc("POST /v1/skills/{name}/multipart", a.uploadSkillMultipart)
	mux.HandleFunc("DELETE /v1/skills/{name}", a.deleteSkill)
	mux.HandleFunc("GET /v1/skills/{name}/content", a.getSkillContent)
}

func (a *api) upsertNamedSkillJSON(w http.ResponseWriter, r *http.Request) {
	a.upsertSkillJSONWithName(w, r, "")
}

func (a *api) upsertSkillJSON(w http.ResponseWriter, r *http.Request) {
	a.upsertSkillJSONWithName(w, r, r.PathValue("name"))
}

func (a *api) upsertSkillJSONWithName(w http.ResponseWriter, r *http.Request, pathName string) {
	var body skillJSONUpsertRequest
	if !readJSONRequest(w, r, &body, skillUploadBodyLimit) {
		return
	}
	name := body.Name
	if pathName != "" {
		name = pathName
	}
	name = strings.TrimSpace(name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "'name' must be non-empty")
		return
	}
	if body.Kind != "command" && body.Kind != "agent" {
		writeError(w, http.StatusBadRequest, "invalid 'kind': '"+body.Kind+"' (must be 'command' or 'agent'; zip skills use PUT /v1/skills/:name)")
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		writeError(w, http.StatusBadRequest, "'content' must be non-empty")
		return
	}
	content := body.Content
	a.persistSkillRecord(w, r, name, body.Kind, []byte{}, &content)
}

func (a *api) uploadSkill(w http.ResponseWriter, r *http.Request) {
	body, ok := readSkillUploadBody(w, r)
	if !ok {
		return
	}
	contentType := "application/zip"
	if values, present := r.Header[http.CanonicalHeaderKey("Content-Type")]; present && len(values) > 0 {
		contentType = values[0]
	}
	kind := "skill"
	if values, present := r.Header[http.CanonicalHeaderKey("X-Kind")]; present && len(values) > 0 {
		headerKind := values[0]
		kind = strings.ToLower(headerKind)
		if kind != "skill" && kind != "command" && kind != "agent" {
			writeError(w, http.StatusBadRequest, "invalid X-Kind: '"+kind+"'")
			return
		}
	} else if strings.HasPrefix(contentType, "text/markdown") {
		kind = "command"
	}
	if kind == "command" || kind == "agent" {
		if !utf8.Valid(body) {
			writeError(w, http.StatusBadRequest, "body is not valid UTF-8")
			return
		}
		if len(body) == 0 {
			writeError(w, http.StatusBadRequest, "body is empty")
			return
		}
		content := string(body)
		a.persistSkillRecord(w, r, r.PathValue("name"), kind, []byte{}, &content)
		return
	}
	a.persistSkillRecord(w, r, r.PathValue("name"), kind, body, nil)
}

func (a *api) uploadSkillMultipart(w http.ResponseWriter, r *http.Request) {
	body, ok := readSkillUploadBody(w, r)
	if !ok {
		return
	}
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") || params["boundary"] == "" {
		writeError(w, http.StatusBadRequest, "failed to parse multipart body")
		return
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var kindField, contentField *string
	var fileBytes []byte
	fileSeen := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to parse multipart body")
			return
		}
		fieldName := part.FormName()
		partBytes, readErr := io.ReadAll(part)
		_ = part.Close()
		if readErr != nil {
			writeError(w, http.StatusBadRequest, "failed to read multipart field")
			return
		}
		switch fieldName {
		case "kind":
			if !utf8.Valid(partBytes) {
				writeError(w, http.StatusBadRequest, "failed to read 'kind' field")
				return
			}
			value := strings.ToLower(strings.TrimSpace(string(partBytes)))
			kindField = &value
		case "content":
			if !utf8.Valid(partBytes) {
				writeError(w, http.StatusBadRequest, "failed to read 'content' field")
				return
			}
			value := string(partBytes)
			contentField = &value
		case "file":
			fileBytes, fileSeen = partBytes, true
		}
	}
	kind := ""
	if kindField != nil {
		kind = *kindField
	}
	switch kind {
	case "skill", "command", "agent":
	default:
		if kindField == nil {
			writeError(w, http.StatusBadRequest, "'kind' field is required")
		} else {
			writeError(w, http.StatusBadRequest, "invalid 'kind': '"+kind+"' (must be skill|command|agent)")
		}
		return
	}
	name := r.PathValue("name")
	if kind == "skill" {
		if !fileSeen {
			writeError(w, http.StatusBadRequest, "'file' field is required when kind is 'skill'")
			return
		}
		if len(fileBytes) == 0 {
			writeError(w, http.StatusBadRequest, "'file' must be non-empty")
			return
		}
		a.persistSkillRecord(w, r, name, kind, fileBytes, nil)
		return
	}
	if contentField == nil {
		writeError(w, http.StatusBadRequest, "'content' field is required when kind is '"+kind+"'")
		return
	}
	if strings.TrimSpace(*contentField) == "" {
		writeError(w, http.StatusBadRequest, "'content' must be non-empty")
		return
	}
	a.persistSkillRecord(w, r, name, kind, []byte{}, contentField)
}

func readSkillUploadBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, skillUploadBodyLimit))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body too large or unreadable")
		return nil, false
	}
	return body, true
}

func (a *api) persistSkillRecord(w http.ResponseWriter, r *http.Request, name, kind string, zipData []byte, content *string) {
	var payload []byte
	if content != nil {
		payload = []byte(*content)
	} else {
		payload = zipData
	}
	sum := sha256.Sum256(payload)
	record := storage.SkillRecord{
		Name: name, Kind: kind, ZipData: zipData, Content: content,
		Size: int64(len(payload)), Checksum: hex.EncodeToString(sum[:]), UploadedAt: time.Now().UnixMilli(),
	}
	if err := a.store.UpsertSkill(r.Context(), record); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot save skill")
		return
	}
	writeJSON(w, skillUploadResponse{Name: name, Kind: kind, Size: record.Size, Checksum: record.Checksum})
}

func (a *api) deleteSkill(w http.ResponseWriter, r *http.Request) {
	existed, err := a.store.DeleteSkill(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot delete skill")
		return
	}
	if !existed {
		writeError(w, http.StatusNotFound, "skill not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) listSkills(w http.ResponseWriter, r *http.Request) {
	var kind *string
	if values, present := r.URL.Query()["kind"]; present && len(values) > 0 && values[0] != "" {
		value := values[0]
		switch value {
		case "skill", "command", "agent":
			kind = &value
		default:
			writeError(w, http.StatusBadRequest, "invalid kind '"+value+"': must be skill|command|agent")
			return
		}
	}
	items, err := a.store.ListSkills(r.Context(), kind)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot list skills")
		return
	}
	writeJSON(w, items)
}

func (a *api) downloadSkill(w http.ResponseWriter, r *http.Request) {
	record, ok := a.readSkill(w, r)
	if !ok {
		return
	}
	filename, contentType, body := record.Name+".zip", "application/zip", record.ZipData
	if record.Kind == "command" || record.Kind == "agent" {
		filename, contentType = record.Name+".md", "text/markdown; charset=utf-8"
		body = []byte{}
		if record.Content != nil {
			body = []byte(*record.Content)
		}
	}
	contentDisposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if contentDisposition == "" {
		contentDisposition = "attachment"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Kind", record.Kind)
	w.Header().Set("Content-Disposition", contentDisposition)
	_, _ = w.Write(body)
}

func (a *api) getSkillContent(w http.ResponseWriter, r *http.Request) {
	record, ok := a.readSkill(w, r)
	if !ok {
		return
	}
	writeJSON(w, skillContentResponse{Name: record.Name, Kind: record.Kind, Content: record.Content})
}

func (a *api) readSkill(w http.ResponseWriter, r *http.Request) (*storage.SkillRecord, bool) {
	record, err := a.store.Skill(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read skill")
		return nil, false
	}
	if record == nil {
		writeError(w, http.StatusNotFound, "not found")
		return nil, false
	}
	return record, true
}
