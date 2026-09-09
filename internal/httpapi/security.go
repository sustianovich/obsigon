package httpapi

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

var unsafeMethods = map[string]bool{http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true}

func Security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestHost, requestPort, ok := parseHostHeader(r.Host)
		if !ok || !isLoopback(requestHost) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "Cabecera Host no permitida."})
			return
		}
		if unsafeMethods[r.Method] {
			fetchSite := strings.ToLower(r.Header.Get("Sec-Fetch-Site"))
			if fetchSite != "" && fetchSite != "same-origin" && fetchSite != "none" {
				writeJSON(w, http.StatusForbidden, map[string]any{"detail": "Solicitud no permitida."})
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && !originMatches(origin, r, requestHost, requestPort) {
				writeJSON(w, http.StatusForbidden, map[string]any{"detail": "Solicitud no permitida."})
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func parseHostHeader(value string) (string, *int, bool) {
	raw := strings.TrimSpace(value)
	if raw == "" || strings.ContainsAny(raw, "@/?#\\") {
		return "", nil, false
	}
	host := raw
	var portText string
	if strings.HasPrefix(raw, "[") {
		closing := strings.Index(raw, "]")
		if closing < 0 {
			return "", nil, false
		}
		host = raw[1:closing]
		suffix := raw[closing+1:]
		if suffix != "" {
			if !strings.HasPrefix(suffix, ":") {
				return "", nil, false
			}
			portText = suffix[1:]
		}
	}
	if !strings.HasPrefix(raw, "[") {
		if strings.Count(raw, ":") == 1 {
			host, portText, _ = strings.Cut(raw, ":")
		} else if strings.Count(raw, ":") > 1 {
			return "", nil, false
		}
	}
	if host == "" {
		return "", nil, false
	}
	normalized := strings.ToLower(host)
	if ip := net.ParseIP(normalized); ip != nil {
		normalized = ip.String()
	}
	if portText == "" {
		return normalized, nil, true
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", nil, false
	}
	return normalized, &port, true
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func originMatches(origin string, r *http.Request, requestHost string, requestPort *int) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	originHost := strings.ToLower(parsed.Hostname())
	if ip := net.ParseIP(originHost); ip != nil {
		originHost = ip.String()
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	originPort := 80
	if parsed.Scheme == "https" {
		originPort = 443
	}
	if parsed.Port() != "" {
		value, err := strconv.Atoi(parsed.Port())
		if err != nil {
			return false
		}
		originPort = value
	}
	effectiveRequestPort := 80
	if scheme == "https" {
		effectiveRequestPort = 443
	}
	if requestPort != nil {
		effectiveRequestPort = *requestPort
	}
	return parsed.Scheme == scheme && originHost == requestHost && originPort == effectiveRequestPort
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
