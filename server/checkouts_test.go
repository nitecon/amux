package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckoutRoutesRequireBearerAndReplaceHostReport(t *testing.T) {
	_, handler := taskAPI(t)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		req := httptest.NewRequest(method, "/v1/checkouts", strings.NewReader(`{"hostname":"Defcon1","checkouts":[]}`))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s status=%d", method, response.Code)
		}
	}
	put := taskRequest(handler, http.MethodPut, "/v1/checkouts", `{"hostname":" Defcon1 ","checkouts":[{"project":"TASKS","origin":"https://token@github.com/team/tasks.git"}]}`, nil)
	if put.Code != http.StatusOK || put.Body.String() != "{\"ok\":true}\n" {
		t.Fatalf("PUT checkout report status=%d body=%s", put.Code, put.Body.String())
	}
	response := taskRequest(handler, http.MethodGet, "/v1/checkouts", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET checkout overview status=%d body=%s", response.Code, response.Body.String())
	}
	var overview []map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &overview); err != nil || len(overview) != 1 {
		t.Fatalf("checkout overview=%s err=%v", response.Body.String(), err)
	}
	var machines []map[string]any
	if err := json.Unmarshal(overview[0]["machines"], &machines); err != nil || len(machines) != 1 || machines[0]["hostname"] != "Defcon1" || machines[0]["origin"] != nil {
		t.Fatalf("machine origin should be omitted when unsafe: %#v err=%v", machines, err)
	}
	if strings.Contains(response.Body.String(), "token") {
		t.Fatalf("checkout response leaked embedded credentials: %s", response.Body.String())
	}
	var agentCount, active int
	if err := json.Unmarshal(overview[0]["agents"], &agentCount); err != nil || agentCount != 0 {
		t.Fatalf("agents=%d err=%v", agentCount, err)
	}
	if err := json.Unmarshal(overview[0]["active"], &active); err != nil || active != 0 {
		t.Fatalf("active=%d err=%v", active, err)
	}
	legacyInventory := taskRequest(handler, http.MethodGet, "/v1/projects", "", nil)
	var projectRows []map[string]json.RawMessage
	if legacyInventory.Code != http.StatusOK || json.Unmarshal(legacyInventory.Body.Bytes(), &projectRows) != nil || len(projectRows) != 1 {
		t.Fatalf("project inventory status=%d body=%s", legacyInventory.Code, legacyInventory.Body.String())
	}
	if _, exists := projectRows[0]["machines"]; exists {
		t.Fatalf("checkout port changed /v1/projects response: %s", legacyInventory.Body.String())
	}
	empty := taskRequest(handler, http.MethodPut, "/v1/checkouts", `{"hostname":"Defcon1","checkouts":[]}`, nil)
	if empty.Code != http.StatusOK {
		t.Fatalf("empty replacement status=%d body=%s", empty.Code, empty.Body.String())
	}
	response = taskRequest(handler, http.MethodGet, "/v1/checkouts", "", nil)
	if err := json.Unmarshal(response.Body.Bytes(), &overview); err != nil || len(overview) != 1 {
		t.Fatalf("after replacement overview=%s err=%v", response.Body.String(), err)
	}
	if err := json.Unmarshal(overview[0]["machines"], &machines); err != nil || len(machines) != 0 {
		t.Fatalf("empty report did not remove host checkout: %#v err=%v", machines, err)
	}
}

func TestCheckoutRoutesStrictReportShapeAndQuery(t *testing.T) {
	_, handler := taskAPI(t)
	for _, body := range []string{`null`, `{}`, `{"hostname":"host"}`, `{"hostname":"host","checkouts":null}`, `{"hostname":"host","checkouts":[],"extra":true}`, `{"hostname":" ","checkouts":[]}`} {
		response := taskRequest(handler, http.MethodPut, "/v1/checkouts", body, nil)
		if response.Code != http.StatusBadRequest {
			t.Errorf("PUT %s status=%d body=%s", body, response.Code, response.Body.String())
		}
	}
	response := taskRequest(handler, http.MethodGet, "/v1/checkouts?unexpected=1", "", nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("GET query status=%d body=%s", response.Code, response.Body.String())
	}
}
