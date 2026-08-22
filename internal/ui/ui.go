// Package ui serves the read-only, multi-workspace ABBS development viewer.
// It consumes only the public /v1 API through internal/client; the browser
// never receives a workspace credential.
package ui

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/client"
	"github.com/dosu-ai/abbs/internal/workspace"
)

//go:embed templates/*.html static/*.css
var assets embed.FS

const contentSecurityPolicy = "default-src 'self'; base-uri 'none'; connect-src 'none'; font-src 'self'; form-action 'self'; frame-ancestors 'none'; img-src 'self'; object-src 'none'; script-src 'none'; style-src 'self'"

const pageLimit = 25

// Config identifies the profiles file and the single-workspace fallback.
// ConfigPath is re-read on every content request. HTTPClient is primarily a
// test seam; the default has a finite timeout so one dead workspace cannot
// hang the overview forever.
type Config struct {
	ConfigPath    string
	FallbackURL   string
	FallbackToken string
	HTTPClient    *http.Client
}

type Handler struct {
	config     Config
	httpClient *http.Client
	templates  map[string]*template.Template
	markdown   *markdownRenderer
}

type origin struct {
	ProfileName  string
	ServerLabel  string
	Description  string
	URL          string
	Capabilities []string
}

type workspaceRow struct {
	Origin    origin
	Reachable bool
	Error     string
}

type overviewPage struct {
	Title       string
	ConfigPath  string
	ConfigError string
	Workspaces  []workspaceRow
}

type threadsPage struct {
	Title     string
	Origin    origin
	Threads   []api.Thread
	Tags      []string
	NextURL   string
	FirstURL  string
	HasFilter bool
}

type threadPage struct {
	Title    string
	Origin   origin
	Thread   api.Thread
	Messages []api.Message
	NextURL  string
	FirstURL string
}

type tagsPage struct {
	Title    string
	Origin   origin
	Tags     []api.TagInfo
	NextURL  string
	FirstURL string
}

type errorPage struct {
	Title   string
	Origin  *origin
	Status  int
	Message string
}

// New constructs the UI handler. It deliberately does not read ConfigPath;
// starting the viewer before the file exists lets the user create it and
// refresh without restarting the process.
func New(cfg Config) (http.Handler, error) {
	if cfg.ConfigPath == "" {
		cfg.ConfigPath = workspace.DefaultConfigPath()
	}
	if cfg.FallbackURL == "" {
		cfg.FallbackURL = envOr("ABBS_URL", "http://127.0.0.1:8080")
	}
	if cfg.FallbackToken == "" {
		cfg.FallbackToken = os.Getenv("ABBS_TOKEN")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 8 * time.Second}
	}
	h := &Handler{config: cfg, httpClient: hc, markdown: newMarkdownRenderer()}
	templates, err := h.parseTemplates()
	if err != nil {
		return nil, err
	}
	h.templates = templates

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.overview)
	mux.HandleFunc("GET /static/app.css", h.stylesheet)
	mux.HandleFunc("GET /workspaces/{workspace}/threads", h.listThreads)
	mux.HandleFunc("GET /workspaces/{workspace}/threads/{thread}", h.showThread)
	mux.HandleFunc("GET /workspaces/{workspace}/tags", h.listTags)
	mux.HandleFunc("GET /", h.notFound)

	return securityHeaders(mux), nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) parseTemplates() (map[string]*template.Template, error) {
	funcs := template.FuncMap{
		"formatTime":       formatTime,
		"formatEditedTime": formatEditedTime,
		"markdown":         h.markdown.Render,
		"threadURL":        threadURL,
		"threadsURL":       threadsURL,
		"threadsHomeURL":   threadsHomeURL,
		"threadsForTagURL": threadsForTagURL,
		"tagsURL":          tagsURL,
	}
	pages := map[string]string{
		"overview": "overview.html",
		"threads":  "threads.html",
		"thread":   "thread.html",
		"tags":     "tags.html",
		"error":    "error.html",
	}
	parsed := make(map[string]*template.Template, len(pages))
	for name, file := range pages {
		t, err := template.New("base.html").Funcs(funcs).ParseFS(assets, "templates/base.html", "templates/"+file)
		if err != nil {
			return nil, fmt.Errorf("parse UI template %s: %w", file, err)
		}
		parsed[name] = t
	}
	return parsed, nil
}

func (h *Handler) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := h.templates[name].ExecuteTemplate(w, "base", data); err != nil {
		// Headers may already be committed; keep the failure out of the page so
		// a template bug cannot disclose request or credential data.
		return
	}
}

func (h *Handler) renderError(w http.ResponseWriter, status int, o *origin, message string) {
	h.render(w, status, "error", errorPage{
		Title: http.StatusText(status), Origin: o, Status: status, Message: message,
	})
}

func (h *Handler) stylesheet(w http.ResponseWriter, _ *http.Request) {
	b, err := assets.ReadFile("static/app.css")
	if err != nil {
		http.Error(w, "stylesheet unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

func (h *Handler) notFound(w http.ResponseWriter, _ *http.Request) {
	h.renderError(w, http.StatusNotFound, nil, "No such UI page.")
}

func (h *Handler) loadProfiles() (map[string]workspace.Profile, []string, error) {
	_, err := os.Stat(h.config.ConfigPath)
	switch {
	case err == nil:
		return workspace.Load(h.config.ConfigPath)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, nil, fmt.Errorf("workspace config %s: %w", h.config.ConfigPath, err)
	case h.config.FallbackToken == "":
		return nil, nil, fmt.Errorf("workspace config %s does not exist and ABBS_TOKEN is empty", h.config.ConfigPath)
	default:
		return map[string]workspace.Profile{
			"default": {URL: h.config.FallbackURL, Token: h.config.FallbackToken},
		}, []string{"default"}, nil
	}
}

func (h *Handler) clientFor(name string, profiles map[string]workspace.Profile) (*client.Client, origin, error) {
	p, ok := profiles[name]
	if !ok {
		return nil, origin{}, fmt.Errorf("workspace %q is not configured", name)
	}
	o := origin{ProfileName: name, URL: strings.TrimRight(p.URL, "/")}
	token, err := p.ResolveToken()
	if err != nil {
		return nil, o, fmt.Errorf("resolve credential: %w", err)
	}
	c := &client.Client{BaseURL: o.URL, Token: token, HTTP: h.httpClient}
	return c, o, nil
}

func (h *Handler) discover(r *http.Request, name string, profiles map[string]workspace.Profile) (*client.Client, origin, error) {
	c, o, err := h.clientFor(name, profiles)
	if err != nil {
		return nil, o, err
	}
	info, err := c.ServerInfo(r.Context())
	if err != nil {
		return nil, o, fmt.Errorf("GET /v1/server: %w", err)
	}
	o.ServerLabel = info.Workspace.Name
	o.Description = info.Workspace.Description
	o.Capabilities = append([]string(nil), info.Capabilities...)
	sort.Strings(o.Capabilities)
	return c, o, nil
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	profiles, names, err := h.loadProfiles()
	page := overviewPage{Title: "Workspaces", ConfigPath: h.config.ConfigPath}
	if err != nil {
		page.ConfigError = err.Error()
		h.render(w, http.StatusInternalServerError, "overview", page)
		return
	}

	page.Workspaces = make([]workspaceRow, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		i, name := i, name
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, o, err := h.discover(r, name, profiles)
			row := workspaceRow{Origin: o, Reachable: err == nil}
			if err != nil {
				row.Error = err.Error()
			}
			page.Workspaces[i] = row
		}()
	}
	wg.Wait()
	h.render(w, http.StatusOK, "overview", page)
}

func (h *Handler) selectedWorkspace(w http.ResponseWriter, r *http.Request) (*client.Client, origin, bool) {
	profiles, _, err := h.loadProfiles()
	if err != nil {
		h.renderError(w, http.StatusInternalServerError, nil, err.Error())
		return nil, origin{}, false
	}
	name := r.PathValue("workspace")
	if _, ok := profiles[name]; !ok {
		h.renderError(w, http.StatusNotFound, nil, fmt.Sprintf("Workspace %q is not configured.", name))
		return nil, origin{}, false
	}
	c, o, err := h.discover(r, name, profiles)
	if err != nil {
		h.renderError(w, http.StatusBadGateway, &o, err.Error())
		return nil, origin{}, false
	}
	return c, o, true
}

func (h *Handler) listThreads(w http.ResponseWriter, r *http.Request) {
	c, o, ok := h.selectedWorkspace(w, r)
	if !ok {
		return
	}
	tags := nonempty(r.URL.Query()["tag"])
	pageToken := r.URL.Query().Get("page")
	page, err := c.ListThreads(r.Context(), client.ListThreadsOptions{Tags: tags, Page: pageToken, Limit: pageLimit})
	if err != nil {
		h.renderError(w, upstreamStatus(err), &o, fmt.Sprintf("List threads: %v", err))
		return
	}
	data := threadsPage{
		Title: "Threads", Origin: o, Threads: page.Items, Tags: tags,
		HasFilter: len(tags) > 0,
	}
	if page.NextPage != nil {
		data.NextURL = threadsURL(o.ProfileName, tags, *page.NextPage)
	}
	if pageToken != "" {
		data.FirstURL = threadsURL(o.ProfileName, tags, "")
	}
	h.render(w, http.StatusOK, "threads", data)
}

func (h *Handler) showThread(w http.ResponseWriter, r *http.Request) {
	c, o, ok := h.selectedWorkspace(w, r)
	if !ok {
		return
	}
	threadID := r.PathValue("thread")
	t, err := c.GetThread(r.Context(), threadID)
	if err != nil {
		h.renderError(w, upstreamStatus(err), &o, fmt.Sprintf("Read thread: %v", err))
		return
	}
	pageToken := r.URL.Query().Get("page")
	page, err := c.ListMessages(r.Context(), threadID, pageToken, pageLimit)
	if err != nil {
		h.renderError(w, upstreamStatus(err), &o, fmt.Sprintf("List messages: %v", err))
		return
	}
	data := threadPage{Title: t.Title, Origin: o, Thread: t, Messages: page.Items}
	if page.NextPage != nil {
		data.NextURL = threadURL(o.ProfileName, threadID, *page.NextPage)
	}
	if pageToken != "" {
		data.FirstURL = threadURL(o.ProfileName, threadID, "")
	}
	h.render(w, http.StatusOK, "thread", data)
}

func (h *Handler) listTags(w http.ResponseWriter, r *http.Request) {
	c, o, ok := h.selectedWorkspace(w, r)
	if !ok {
		return
	}
	pageToken := r.URL.Query().Get("page")
	page, err := c.ListTags(r.Context(), pageToken, pageLimit)
	if err != nil {
		h.renderError(w, upstreamStatus(err), &o, fmt.Sprintf("List tags: %v", err))
		return
	}
	data := tagsPage{Title: "Tags", Origin: o, Tags: page.Items}
	if page.NextPage != nil {
		data.NextURL = tagsURL(o.ProfileName, *page.NextPage)
	}
	if pageToken != "" {
		data.FirstURL = tagsURL(o.ProfileName, "")
	}
	h.render(w, http.StatusOK, "tags", data)
}

func upstreamStatus(err error) int {
	var apiErr *client.Error
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusBadRequest || apiErr.StatusCode == http.StatusNotFound) {
		return apiErr.StatusCode
	}
	return http.StatusBadGateway
}

func nonempty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func workspacePath(name string) string {
	return "/workspaces/" + url.PathEscape(name)
}

func threadsURL(name string, tags []string, page string) string {
	q := url.Values{}
	for _, tag := range tags {
		q.Add("tag", tag)
	}
	if page != "" {
		q.Set("page", page)
	}
	return withQuery(workspacePath(name)+"/threads", q)
}

func threadsForTagURL(name, tag string) string {
	return threadsURL(name, []string{tag}, "")
}

func threadsHomeURL(name string) string {
	return threadsURL(name, nil, "")
}

func threadURL(name, threadID, page string) string {
	q := url.Values{}
	if page != "" {
		q.Set("page", page)
	}
	return withQuery(workspacePath(name)+"/threads/"+url.PathEscape(threadID), q)
}

func tagsURL(name, page string) string {
	q := url.Values{}
	if page != "" {
		q.Set("page", page)
	}
	return withQuery(workspacePath(name)+"/tags", q)
}

func withQuery(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

func formatTime(value string) string {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	return t.Local().Format("Jan 2, 2006 15:04:05 MST")
}

func formatEditedTime(value *string) string {
	if value == nil {
		return ""
	}
	return formatTime(*value)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
