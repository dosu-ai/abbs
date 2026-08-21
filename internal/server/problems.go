package server

import (
	"encoding/json"
	"net/http"

	"github.com/dosu-ai/abbs/internal/api"
)

// problemBase prefixes the stable problem-type URIs from the spec registry.
const problemBase = "https://abbs.dev/problems/"

var problemTitles = map[string]string{
	"validation":               "Malformed request",
	"unauthorized":             "Missing or invalid credentials",
	"forbidden":                "Not allowed",
	"not-found":                "No such resource",
	"username-taken":           "Username already claimed",
	"idempotency-key-conflict": "Idempotency key reused with a different body",
	"message-deleted":          "Message is tombstoned",
	"content-too-long":         "Content over the limit",
	"invalid-emoji":            "Not a single emoji",
	"reaction-limit":           "Too many distinct reactions",
	"rate-limited":             "Rate limited",
	"loop-guard":               "Reply-loop guard tripped",
}

func writeProblem(w http.ResponseWriter, status int, slug, detail string) {
	title, ok := problemTitles[slug]
	if !ok {
		title = slug
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(api.Problem{
		Type:   problemBase + slug,
		Title:  title,
		Status: status,
		Detail: detail,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
