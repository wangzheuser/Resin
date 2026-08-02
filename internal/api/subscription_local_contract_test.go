package api

import (
	"net/http"
	"testing"
)

func TestAPIContract_SubscriptionLocalCreateValidation(t *testing.T) {
	srv, _, _ := newControlPlaneTestServer(t)

	rec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions", map[string]any{
		"name":                    "sub-local",
		"source_type":             "local",
		"incremental_alive_nodes": true,
	}, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create local without content status: got %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	assertErrorCode(t, rec, "INVALID_ARGUMENT")

	rec = doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions", map[string]any{
		"name":        "sub-local",
		"source_type": "local",
		"content":     "vmess://example",
		"url":         "https://example.com/sub",
	}, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create local with url status: got %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	assertErrorCode(t, rec, "INVALID_ARGUMENT")
}

func TestAPIContract_SubscriptionSourceTypeReadOnlyOnPatch(t *testing.T) {
	srv, _, _ := newControlPlaneTestServer(t)

	createRec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions", map[string]any{
		"name": "sub-remote",
		"url":  "https://example.com/sub",
	}, true)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create remote subscription status: got %d, want %d, body=%s", createRec.Code, http.StatusCreated, createRec.Body.String())
	}
	body := decodeJSONMap(t, createRec)
	subID, _ := body["id"].(string)
	if subID == "" {
		t.Fatalf("create remote subscription missing id: body=%s", createRec.Body.String())
	}

	rec := doJSONRequest(t, srv, http.MethodPatch, "/api/v1/subscriptions/"+subID, map[string]any{
		"source_type": "local",
	}, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("patch source_type status: got %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	assertErrorCode(t, rec, "INVALID_ARGUMENT")
}

func TestAPIContract_SubscriptionListCanExcludeContent(t *testing.T) {
	srv, _, _ := newControlPlaneTestServer(t)

	createRec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions", map[string]any{
		"name":        "sub-local",
		"source_type": "local",
		"content":     "vmess://example",
	}, true)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create local subscription status: got %d, want %d, body=%s", createRec.Code, http.StatusCreated, createRec.Body.String())
	}
	subID, _ := decodeJSONMap(t, createRec)["id"].(string)

	listRec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/subscriptions?include_content=false", nil, true)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list subscriptions status: got %d, want %d, body=%s", listRec.Code, http.StatusOK, listRec.Body.String())
	}
	items, _ := decodeJSONMap(t, listRec)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["content"] != "" {
		t.Fatalf("list subscriptions should exclude content: body=%s", listRec.Body.String())
	}
	defaultListRec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/subscriptions", nil, true)
	defaultItems, _ := decodeJSONMap(t, defaultListRec)["items"].([]any)
	if content := defaultItems[0].(map[string]any)["content"]; content != "vmess://example" {
		t.Fatalf("default list subscription content: got %v, want %q", content, "vmess://example")
	}

	getRec := doJSONRequest(t, srv, http.MethodGet, "/api/v1/subscriptions/"+subID, nil, true)
	if content := decodeJSONMap(t, getRec)["content"]; content != "vmess://example" {
		t.Fatalf("get subscription content: got %v, want %q", content, "vmess://example")
	}
}
