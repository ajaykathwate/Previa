package pullrequests_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDescriptionPersistsEditsAndIsEscapedOnDetail(t *testing.T) {
	h := newActionsHarness(t)
	var generation int64
	if err := h.pool.QueryRow(t.Context(), "SELECT generation FROM pull_requests WHERE id=$1", h.prID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	for n, body := range []string{"Summary\n\n<script>alert('unsafe')</script> & <b>details</b>", "Updated details", ""} {
		var data map[string]any
		if err := json.Unmarshal([]byte(payload("edited", "sha-a", "open", "2026-10-03T10:00:00Z")), &data); err != nil {
			t.Fatal(err)
		}
		data["pull_request"].(map[string]any)["body"] = body
		encoded, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		if code := send(h.hook, string(rune('a'+n))+"-description", "pull_request", string(encoded)); code != 204 {
			t.Fatalf("description observation: %d", code)
		}
		var currentGeneration int64
		if err := h.pool.QueryRow(t.Context(), "SELECT generation FROM pull_requests WHERE id=$1", h.prID).Scan(&currentGeneration); err != nil || currentGeneration != generation {
			t.Fatalf("description edit advanced code generation: %d %v", currentGeneration, err)
		}
		var saved string
		if err := h.pool.QueryRow(t.Context(), "SELECT description FROM pull_requests WHERE id=$1", h.prID).Scan(&saved); err != nil || saved != body {
			t.Fatalf("saved description %q %v", saved, err)
		}
		result := h.request(t, http.MethodGet, detailPath, "")
		html := result.Body.String()
		if result.Code != 200 {
			t.Fatalf("description page: %d", result.Code)
		}
		if strings.Contains(html, "<script>alert(") || strings.Contains(html, "<b>details</b>") {
			t.Fatal("untrusted PR HTML rendered")
		}
		if (body != "") != strings.Contains(html, "Pull request description") {
			t.Fatal("empty or missing description panel")
		}
	}
}
func TestExecutionFailureAndRetryAreVisibleOnPR(t *testing.T) {
	h := newActionsHarness(t)
	h.exec(t, `INSERT INTO build_intents(pull_request_id,revision,base_revision,generation,head_sha,source,request_action,idempotency_key,state,available_at,started_at,execution_error_code)
 SELECT id,1,0,generation,head_sha,'automatic','automatic','execution-health','running',now(),now(),'worker_retrying' FROM pull_requests WHERE id=$1`, h.prID)
	if html := h.request(t, "GET", detailPath, "").Body.String(); !strings.Contains(html, "Build attempt failed; retrying") {
		t.Fatal("retry feedback is missing")
	}
	h.exec(t, "UPDATE build_intents SET state='failed',completed_at=now(),execution_error_code='worker_failed' WHERE pull_request_id=$1", h.prID)
	html := h.request(t, "GET", detailPath, "").Body.String()
	if !strings.Contains(html, "Build failed") || !strings.Contains(html, "retries have ended") {
		t.Fatal("terminal worker failure is not explained")
	}
	h.exec(t, "UPDATE build_intents SET state='running',completed_at=NULL,execution_error_code='' WHERE pull_request_id=$1", h.prID)
	h.caps.execution = false
	if html := h.request(t, "GET", detailPath, "").Body.String(); !strings.Contains(html, "Build waiting for a worker") {
		t.Fatal("missing worker still appears to build")
	}
}

func TestPublicationRetryIsExplainedWithoutActiveProgress(t *testing.T) {
	h := newActionsHarness(t)
	h.exec(t, `INSERT INTO build_intents(pull_request_id,revision,base_revision,generation,head_sha,source,request_action,idempotency_key,state,available_at,started_at,completed_at)
 SELECT id,1,0,generation,head_sha,'automatic','automatic','publication-health','succeeded',now(),now(),now() FROM pull_requests WHERE id=$1`, h.prID)
	h.exec(t, "INSERT INTO environments(pull_request_id,subdomain,status) VALUES($1,'publication-health','publishing')", h.prID)
	h.exec(t, `INSERT INTO environment_operations(pull_request_id,generation,revision,kind,idempotency_key,payload,state,last_error_code)
 SELECT id,generation,1,'activate_route','publication-health','{}','retryable','router_unavailable' FROM pull_requests WHERE id=$1`, h.prID)
	html := h.request(t, "GET", detailPath, "").Body.String()
	if !strings.Contains(html, "Publishing failed; retrying") || strings.Contains(html, `data-progress-events=`) {
		t.Fatal("publication retries still look like active publishing")
	}
}
