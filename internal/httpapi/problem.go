package httpapi

import (
	"encoding/json"
	"net/http"
)

type FieldError struct {
	Pointer string `json:"pointer,omitempty"`
	Code    string `json:"code"`
	Detail  string `json:"detail"`
}
type Problem struct {
	Type      string       `json:"type"`
	Title     string       `json:"title"`
	Status    int          `json:"status"`
	Detail    string       `json:"detail"`
	Instance  string       `json:"instance,omitempty"`
	Code      string       `json:"code"`
	RequestID string       `json:"requestId"`
	Retryable bool         `json:"retryable"`
	Errors    []FieldError `json:"errors,omitempty"`
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, title, detail string, errors ...FieldError) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{Type: "https://docs.kuberploy.io/problems/" + code, Title: title, Status: status, Detail: detail, Instance: r.URL.Path, Code: code, RequestID: requestID(r.Context()), Retryable: status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests, Errors: errors})
}
