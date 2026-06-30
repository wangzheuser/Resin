package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Resinat/Resin/internal/proxy"
	"github.com/Resinat/Resin/internal/service"
)

type inheritLeaseRequest struct {
	ParentAccount string `json:"parent_account"`
	NewAccount    string `json:"new_account"`
}

type proxySessionResponse struct {
	ID           string `json:"id"`
	PlatformName string `json:"platform_name"`
	Account      string `json:"account"`
	ProxyURL     string `json:"proxy_url"`
	NodeHash     string `json:"node_hash"`
	EgressIP     string `json:"egress_ip"`
	TTL          string `json:"ttl"`
	ExpiresAt    string `json:"expires_at"`
}

// NewTokenActionHandler returns the handler for token-path actions.
func NewTokenActionHandler(proxyToken string, cp *service.ControlPlaneService, apiMaxBodyBytes int64) http.Handler {
	return NewTokenActionHandlerWithProxySessions(proxyToken, cp, nil, apiMaxBodyBytes)
}

// NewTokenActionHandlerWithProxySessions returns the token-path action handler,
// including optional browser proxy session endpoints.
func NewTokenActionHandlerWithProxySessions(
	proxyToken string,
	cp *service.ControlPlaneService,
	sessionManager *proxy.ProxySessionManager,
	apiMaxBodyBytes int64,
) http.Handler {
	if cp == nil {
		return http.NotFoundHandler()
	}

	mux := http.NewServeMux()
	mux.Handle("GET /{token}/api/v1/{platform}/proxy-sessions", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validateTokenPathProxyToken(w, r, proxyToken) {
			return
		}
		if sessionManager == nil {
			http.NotFound(w, r)
			return
		}

		platformName := strings.TrimSpace(PathParam(r, "platform"))
		if platformName == "" {
			writeInvalidArgument(w, "platform: must be non-empty")
			return
		}

		ttl, ok := parseProxySessionTTLOrWriteInvalid(w, r)
		if !ok {
			return
		}

		info, err := sessionManager.Create(proxy.ProxySessionCreateRequest{
			PlatformName: platformName,
			TTL:          ttl,
		})
		if err != nil {
			writeProxySessionError(w, err)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Query().Get("format") == "url" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(info.ProxyURL))
			return
		}

		WriteJSON(w, http.StatusOK, proxySessionToResponse(info))
	}))

	mux.Handle("DELETE /{token}/api/v1/{platform}/proxy-sessions/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validateTokenPathProxyToken(w, r, proxyToken) {
			return
		}
		if sessionManager == nil {
			http.NotFound(w, r)
			return
		}

		platformName := strings.TrimSpace(PathParam(r, "platform"))
		id := strings.TrimSpace(PathParam(r, "id"))
		if platformName == "" {
			writeInvalidArgument(w, "platform: must be non-empty")
			return
		}
		if id == "" {
			writeInvalidArgument(w, "id: must be non-empty")
			return
		}

		if err := sessionManager.Release(platformName, id); err != nil {
			writeProxySessionError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))

	mux.Handle("POST /{token}/api/v1/{platform}/actions/inherit-lease", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validateTokenPathProxyToken(w, r, proxyToken) {
			return
		}

		platformName := strings.TrimSpace(PathParam(r, "platform"))
		if platformName == "" {
			writeInvalidArgument(w, "platform: must be non-empty")
			return
		}

		var req inheritLeaseRequest
		if err := DecodeBody(r, &req); err != nil {
			writeDecodeBodyError(w, err)
			return
		}

		if err := cp.InheritLeaseByPlatformName(platformName, req.ParentAccount, req.NewAccount); err != nil {
			writeServiceError(w, err)
			return
		}

		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))

	return RequestBodyLimitMiddleware(apiMaxBodyBytes, mux)
}

func validateTokenPathProxyToken(w http.ResponseWriter, r *http.Request, proxyToken string) bool {
	token := PathParam(r, "token")
	if proxyToken != "" && token != proxyToken {
		http.NotFound(w, r)
		return false
	}
	return true
}

func parseProxySessionTTLOrWriteInvalid(w http.ResponseWriter, r *http.Request) (time.Duration, bool) {
	rawTTL := strings.TrimSpace(r.URL.Query().Get("ttl"))
	if rawTTL == "" {
		return 0, true
	}
	ttl, err := time.ParseDuration(rawTTL)
	if err != nil {
		writeInvalidArgument(w, "ttl: must be a valid duration")
		return 0, false
	}
	if ttl <= 0 {
		writeInvalidArgument(w, "ttl: must be positive")
		return 0, false
	}
	return ttl, true
}

func proxySessionToResponse(info proxy.ProxySessionInfo) proxySessionResponse {
	return proxySessionResponse{
		ID:           info.ID,
		PlatformName: info.PlatformName,
		Account:      info.Account,
		ProxyURL:     info.ProxyURL,
		NodeHash:     info.NodeHash,
		EgressIP:     info.EgressIP,
		TTL:          info.TTL.String(),
		ExpiresAt:    info.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}

func writeProxySessionError(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
	case errorsIsProxySessionPlatformNotFound(err):
		WriteError(w, http.StatusNotFound, "NOT_FOUND", "platform not found")
	case errorsIsProxySessionNotFound(err):
		WriteError(w, http.StatusNotFound, "NOT_FOUND", "proxy session not found")
	case errorsIsProxySessionConflict(err):
		WriteError(w, http.StatusConflict, "CONFLICT", err.Error())
	case errorsIsProxySessionNoAvailableNodes(err):
		WriteError(w, http.StatusServiceUnavailable, "NO_AVAILABLE_NODES", "no available nodes")
	default:
		WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
	}
}

func errorsIsProxySessionPlatformNotFound(err error) bool {
	return errors.Is(err, proxy.ErrProxySessionPlatformNotFound)
}

func errorsIsProxySessionNotFound(err error) bool {
	return errors.Is(err, proxy.ErrProxySessionNotFound)
}

func errorsIsProxySessionConflict(err error) bool {
	return errors.Is(err, proxy.ErrProxySessionLimitExceeded) || errors.Is(err, proxy.ErrProxySessionPortUnavailable)
}

func errorsIsProxySessionNoAvailableNodes(err error) bool {
	return errors.Is(err, proxy.ErrProxySessionNoAvailableNodes)
}
