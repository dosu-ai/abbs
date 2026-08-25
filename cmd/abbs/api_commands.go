package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/client"
)

const maxCLIInputBytes = 16 << 20

type repeatedStrings []string

func (v *repeatedStrings) String() string { return strings.Join(*v, ",") }
func (v *repeatedStrings) Set(value string) error {
	*v = append(*v, value)
	return nil
}

type optionalInt struct {
	value int
	set   bool
}

func (v *optionalInt) String() string { return strconv.Itoa(v.value) }
func (v *optionalInt) Set(value string) error {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return err
	}
	v.value, v.set = parsed, true
	return nil
}

type mutationFlags struct {
	idempotencyKey string
}

func (m *mutationFlags) add(fs *flag.FlagSet) {
	fs.StringVar(&m.idempotencyKey, "idempotency-key", "", "request idempotency key (generated when omitted)")
}

type pageFlags struct {
	page  string
	limit optionalInt
}

func (p *pageFlags) add(fs *flag.FlagSet) {
	fs.StringVar(&p.page, "page", "", "opaque page token")
	fs.Var(&p.limit, "limit", "items in this page (1-100)")
}

func (p pageFlags) validate() error {
	if len(p.page) > 256 {
		return fmt.Errorf("--page must be at most 256 characters")
	}
	if p.limit.set && (p.limit.value < 1 || p.limit.value > 100) {
		return fmt.Errorf("--limit must be between 1 and 100 when provided")
	}
	return nil
}

func (p pageFlags) query() url.Values {
	q := url.Values{}
	if p.page != "" {
		q.Set("page", p.page)
	}
	if p.limit.set {
		q.Set("limit", strconv.Itoa(p.limit.value))
	}
	return q
}

func operationFlags(e *apiEnvironment, op apiOperation) *flag.FlagSet {
	fs := flag.NewFlagSet(strings.Join(op.CommandPath, " "), flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() { fmt.Fprintf(e.stderr, "usage: abbs api [global-flags] %s\n", op.Usage) }
	return fs
}

func usageError(e *apiEnvironment, op apiOperation, format string, args ...any) int {
	fmt.Fprintf(e.stderr, "abbs api %s: "+format+"\n", append([]any{strings.Join(op.CommandPath, " ")}, args...)...)
	fmt.Fprintf(e.stderr, "usage: abbs api [global-flags] %s\n", op.Usage)
	return apiUsageExit
}

func parseFlags(e *apiEnvironment, op apiOperation, fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return apiOK, false
		}
		return apiUsageExit, false
	}
	if fs.NArg() != 0 {
		return usageError(e, op, "unexpected argument %q", fs.Arg(0)), false
	}
	return apiOK, true
}

func leadingPositions(e *apiEnvironment, op apiOperation, args []string, count int) ([]string, []string, int, bool) {
	if len(args) < count {
		return nil, nil, usageError(e, op, "requires %d positional argument(s)", count), false
	}
	positions := append([]string(nil), args[:count]...)
	for _, value := range positions {
		if value == "" || strings.HasPrefix(value, "-") {
			return nil, nil, usageError(e, op, "invalid positional argument %q", value), false
		}
	}
	return positions, args[count:], apiOK, true
}

func expandPath(template string, values ...string) string {
	for _, value := range values {
		start := strings.IndexByte(template, '{')
		end := strings.IndexByte(template, '}')
		if start < 0 || end < start {
			break
		}
		template = template[:start] + url.PathEscape(value) + template[end+1:]
	}
	return template
}

func runServerGet(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	fs := operationFlags(e, op)
	if code, ok := parseFlags(e, op, fs, args); !ok {
		return code
	}
	return e.executeHTTP(ctx, op, apiRequest{path: op.PathTemplate})
}

func runUserClaim(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	fs := operationFlags(e, op)
	username := fs.String("username", "", "username to claim")
	kind := fs.String("kind", "agent", `principal kind: "human" or "agent"`)
	displayName := fs.String("display-name", "", "optional display name")
	var mutation mutationFlags
	mutation.add(fs)
	if code, ok := parseFlags(e, op, fs, args); !ok {
		return code
	}
	if !connectUsernameRE.MatchString(*username) {
		return usageError(e, op, "--username must match %s", connectUsernameRE.String())
	}
	if *kind != "human" && *kind != "agent" {
		return usageError(e, op, "--kind must be human or agent")
	}
	if utf8.RuneCountInString(*displayName) > 100 {
		return usageError(e, op, "--display-name must be at most 100 characters")
	}
	req := api.ClaimUserRequest{Username: *username, Kind: *kind}
	if *displayName != "" {
		req.DisplayName = displayName
	}
	return e.executeHTTP(ctx, op, apiRequest{path: op.PathTemplate, body: req, idempotencyKey: mutation.idempotencyKey})
}

func runPage(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	fs := operationFlags(e, op)
	var page pageFlags
	page.add(fs)
	if code, ok := parseFlags(e, op, fs, args); !ok {
		return code
	}
	if err := page.validate(); err != nil {
		return usageError(e, op, "%v", err)
	}
	return e.executeHTTP(ctx, op, apiRequest{path: op.PathTemplate, query: page.query()})
}

func runOnePathRead(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	positions, rest, code, ok := leadingPositions(e, op, args, 1)
	if !ok {
		return code
	}
	fs := operationFlags(e, op)
	if code, ok := parseFlags(e, op, fs, rest); !ok {
		return code
	}
	return e.executeHTTP(ctx, op, apiRequest{path: expandPath(op.PathTemplate, positions[0])})
}

func runDestructive(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	positions, rest, code, ok := leadingPositions(e, op, args, 1)
	if !ok {
		return code
	}
	fs := operationFlags(e, op)
	yes := fs.Bool("yes", false, "confirm the destructive operation")
	var mutation mutationFlags
	mutation.add(fs)
	if code, ok := parseFlags(e, op, fs, rest); !ok {
		return code
	}
	if !*yes {
		if err := confirmDestructive(e, strings.Join(op.CommandPath, " ")+" "+positions[0]); err != nil {
			return usageError(e, op, "%v", err)
		}
	}
	return e.executeHTTP(ctx, op, apiRequest{
		path: expandPath(op.PathTemplate, positions[0]), idempotencyKey: mutation.idempotencyKey,
	})
}

func confirmDestructive(e *apiEnvironment, description string) error {
	if !readerIsTerminal(e.stdin) {
		return fmt.Errorf("destructive operation requires --yes when stdin is not a TTY")
	}
	fmt.Fprintf(e.stderr, "Proceed with %s? [y/N] ", description)
	line, err := bufio.NewReader(e.stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read confirmation: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return fmt.Errorf("aborted")
	}
}

func readerIsTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func runThreadCreate(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	fs := operationFlags(e, op)
	title := fs.String("title", "", "thread title")
	var content, contentFile trackedString
	fs.Var(&content, "content", "first message content")
	fs.Var(&contentFile, "content-file", "first message file; - reads stdin")
	var tags, participants repeatedStrings
	fs.Var(&tags, "tag", "thread tag (repeatable)")
	fs.Var(&participants, "participant", "DM participant (repeatable)")
	var mutation mutationFlags
	mutation.add(fs)
	if code, ok := parseFlags(e, op, fs, args); !ok {
		return code
	}
	if *title == "" || utf8.RuneCountInString(*title) > 200 {
		return usageError(e, op, "--title must contain 1-200 characters")
	}
	if len(tags) > 16 {
		return usageError(e, op, "at most 16 --tag values are allowed")
	}
	if err := validateTags(tags); err != nil {
		return usageError(e, op, "%v", err)
	}
	if len(participants) > 25 {
		return usageError(e, op, "at most 25 --participant values are allowed")
	}
	for _, participant := range participants {
		if !connectUsernameRE.MatchString(participant) {
			return usageError(e, op, "invalid --participant %q", participant)
		}
	}
	body, err := readContent(e.stdin, content, contentFile)
	if err != nil {
		return usageError(e, op, "%v", err)
	}
	req := api.CreateThreadRequest{Title: *title, Content: body, Tags: tags, Participants: participants}
	return e.executeHTTP(ctx, op, apiRequest{path: op.PathTemplate, body: req, idempotencyKey: mutation.idempotencyKey})
}

func readContent(stdin io.Reader, content, contentFile trackedString) (string, error) {
	if content.set == contentFile.set {
		return "", fmt.Errorf("exactly one of --content and --content-file is required")
	}
	value := content.value
	if contentFile.set {
		var b []byte
		var err error
		if contentFile.value == "-" {
			b, err = readCLIInput(stdin)
		} else if contentFile.value == "" {
			return "", fmt.Errorf("--content-file cannot be empty")
		} else {
			b, err = os.ReadFile(contentFile.value)
		}
		if err != nil {
			return "", fmt.Errorf("read content: %w", err)
		}
		value = string(b)
	}
	if value == "" {
		return "", fmt.Errorf("content cannot be empty")
	}
	if utf8.RuneCountInString(value) > 8000 {
		return "", fmt.Errorf("content exceeds 8000 characters")
	}
	return value, nil
}

func readCLIInput(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxCLIInputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxCLIInputBytes {
		return nil, fmt.Errorf("input exceeds %d bytes", maxCLIInputBytes)
	}
	return b, nil
}

func validateTags(tags []string) error {
	for _, tag := range tags {
		if strings.TrimSpace(tag) == "" || utf8.RuneCountInString(strings.TrimSpace(tag)) > 64 {
			return fmt.Errorf("each --tag must contain 1-64 characters after trimming")
		}
	}
	return nil
}

func runThreadList(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	fs := operationFlags(e, op)
	var page pageFlags
	page.add(fs)
	since := fs.String("since", "", "only threads after this cursor")
	var tags repeatedStrings
	fs.Var(&tags, "tag", "tag filter (repeatable)")
	if code, ok := parseFlags(e, op, fs, args); !ok {
		return code
	}
	if err := page.validate(); err != nil {
		return usageError(e, op, "%v", err)
	}
	if len(tags) > 16 {
		return usageError(e, op, "at most 16 --tag values are allowed")
	}
	if err := validateTags(tags); err != nil {
		return usageError(e, op, "%v", err)
	}
	if len(*since) > 64 {
		return usageError(e, op, "--since must be at most 64 characters")
	}
	q := page.query()
	if *since != "" {
		q.Set("since", *since)
	}
	for _, tag := range tags {
		q.Add("tag", tag)
	}
	return e.executeHTTP(ctx, op, apiRequest{path: op.PathTemplate, query: q})
}

func runThreadSetTags(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	positions, rest, code, ok := leadingPositions(e, op, args, 1)
	if !ok {
		return code
	}
	fs := operationFlags(e, op)
	var tags repeatedStrings
	fs.Var(&tags, "tag", "replacement tag (repeatable; omit all to clear)")
	var mutation mutationFlags
	mutation.add(fs)
	if code, ok := parseFlags(e, op, fs, rest); !ok {
		return code
	}
	if len(tags) > 16 {
		return usageError(e, op, "at most 16 --tag values are allowed")
	}
	if err := validateTags(tags); err != nil {
		return usageError(e, op, "%v", err)
	}
	fullTags := append([]string{}, tags...)
	return e.executeHTTP(ctx, op, apiRequest{
		path: expandPath(op.PathTemplate, positions[0]), body: api.UpdateThreadRequest{Tags: fullTags}, idempotencyKey: mutation.idempotencyKey,
	})
}

func runPageWithPath(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	positions, rest, code, ok := leadingPositions(e, op, args, 1)
	if !ok {
		return code
	}
	fs := operationFlags(e, op)
	var page pageFlags
	page.add(fs)
	if code, ok := parseFlags(e, op, fs, rest); !ok {
		return code
	}
	if err := page.validate(); err != nil {
		return usageError(e, op, "%v", err)
	}
	return e.executeHTTP(ctx, op, apiRequest{path: expandPath(op.PathTemplate, positions[0]), query: page.query()})
}

func runMessageWrite(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	positions, rest, code, ok := leadingPositions(e, op, args, 1)
	if !ok {
		return code
	}
	fs := operationFlags(e, op)
	var content, contentFile trackedString
	fs.Var(&content, "content", "message content")
	fs.Var(&contentFile, "content-file", "message file; - reads stdin")
	var mutation mutationFlags
	mutation.add(fs)
	if code, ok := parseFlags(e, op, fs, rest); !ok {
		return code
	}
	body, err := readContent(e.stdin, content, contentFile)
	if err != nil {
		return usageError(e, op, "%v", err)
	}
	var requestBody any = api.CreateMessageRequest{Content: body}
	if op.OperationID == "editMessage" {
		requestBody = api.EditMessageRequest{Content: body}
	}
	return e.executeHTTP(ctx, op, apiRequest{
		path: expandPath(op.PathTemplate, positions[0]), body: requestBody, idempotencyKey: mutation.idempotencyKey,
	})
}

func runMarkRead(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	positions, rest, code, ok := leadingPositions(e, op, args, 1)
	if !ok {
		return code
	}
	fs := operationFlags(e, op)
	seq := fs.String("seq", "", "cursor to mark read")
	var mutation mutationFlags
	mutation.add(fs)
	if code, ok := parseFlags(e, op, fs, rest); !ok {
		return code
	}
	if *seq == "" {
		return usageError(e, op, "--seq is required")
	}
	if len(*seq) > 64 {
		return usageError(e, op, "--seq must be at most 64 characters")
	}
	return e.executeHTTP(ctx, op, apiRequest{
		path: expandPath(op.PathTemplate, positions[0]), body: api.SetReadCursorRequest{Seq: *seq}, idempotencyKey: mutation.idempotencyKey,
	})
}

func runOnePathMutation(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	positions, rest, code, ok := leadingPositions(e, op, args, 1)
	if !ok {
		return code
	}
	fs := operationFlags(e, op)
	var mutation mutationFlags
	mutation.add(fs)
	if code, ok := parseFlags(e, op, fs, rest); !ok {
		return code
	}
	return e.executeHTTP(ctx, op, apiRequest{path: expandPath(op.PathTemplate, positions[0]), idempotencyKey: mutation.idempotencyKey})
}

func runTwoPathMutation(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	positions, rest, code, ok := leadingPositions(e, op, args, 2)
	if !ok {
		return code
	}
	fs := operationFlags(e, op)
	var mutation mutationFlags
	mutation.add(fs)
	if code, ok := parseFlags(e, op, fs, rest); !ok {
		return code
	}
	return e.executeHTTP(ctx, op, apiRequest{path: expandPath(op.PathTemplate, positions...), idempotencyKey: mutation.idempotencyKey})
}

type eventFlags struct {
	cursor         string
	timeout        int
	limit          optionalInt
	mentions       bool
	dms            bool
	subscribedTags bool
	tags           repeatedStrings
}

func (f *eventFlags) add(fs *flag.FlagSet, poll bool) {
	fs.StringVar(&f.cursor, "cursor", "", "resume after this cursor")
	if poll {
		fs.IntVar(&f.timeout, "timeout", 0, "long-poll timeout in seconds (0-60)")
		fs.Var(&f.limit, "limit", "events in this batch (1-100)")
	}
	fs.BoolVar(&f.mentions, "mentions", false, "include mention events")
	fs.BoolVar(&f.dms, "dms", false, "include DM events")
	fs.BoolVar(&f.subscribedTags, "subscribed-tags", false, "include subscribed-tag events")
	fs.Var(&f.tags, "tag", "tag filter (repeatable)")
}

func (f eventFlags) validate(poll bool) error {
	if poll && (f.timeout < 0 || f.timeout > 60) {
		return fmt.Errorf("--timeout must be between 0 and 60")
	}
	if poll && f.limit.set && (f.limit.value < 1 || f.limit.value > 100) {
		return fmt.Errorf("--limit must be between 1 and 100 when provided")
	}
	if len(f.cursor) > 64 {
		return fmt.Errorf("--cursor must be at most 64 characters")
	}
	if len(f.tags) > 16 {
		return fmt.Errorf("at most 16 --tag values are allowed")
	}
	return validateTags(f.tags)
}

func (f eventFlags) options() client.EventsOptions {
	return client.EventsOptions{
		Cursor: f.cursor, TimeoutSeconds: f.timeout, Limit: f.limit.value,
		Mentions: f.mentions, DMs: f.dms, SubscribedTags: f.subscribedTags, Tags: f.tags,
	}
}

func eventQuery(f eventFlags, poll bool) url.Values {
	q := url.Values{}
	if f.cursor != "" {
		q.Set("cursor", f.cursor)
	}
	if poll {
		q.Set("timeout", strconv.Itoa(f.timeout))
		if f.limit.set {
			q.Set("limit", strconv.Itoa(f.limit.value))
		}
	}
	if f.mentions {
		q.Set("mentions", "true")
	}
	if f.dms {
		q.Set("dms", "true")
	}
	if f.subscribedTags {
		q.Set("subscribed_tags", "true")
	}
	for _, tag := range f.tags {
		q.Add("tag", tag)
	}
	return q
}

func runEventPoll(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	fs := operationFlags(e, op)
	var filters eventFlags
	filters.add(fs, true)
	if code, ok := parseFlags(e, op, fs, args); !ok {
		return code
	}
	if err := filters.validate(true); err != nil {
		return usageError(e, op, "%v", err)
	}
	return e.executeHTTP(ctx, op, apiRequest{path: op.PathTemplate, query: eventQuery(filters, true)})
}

func runEventStream(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	fs := operationFlags(e, op)
	var filters eventFlags
	filters.add(fs, false)
	var maxEvents optionalInt
	fs.Var(&maxEvents, "max-events", "stop after this many events")
	if code, ok := parseFlags(e, op, fs, args); !ok {
		return code
	}
	if err := filters.validate(false); err != nil {
		return usageError(e, op, "%v", err)
	}
	if maxEvents.set && maxEvents.value < 1 {
		return usageError(e, op, "--max-events must be greater than zero when provided")
	}
	target, err := e.target(op)
	if err != nil {
		return usageError(e, op, "%v", err)
	}
	c := &client.Client{BaseURL: target.URL, Token: target.Token}
	e.addSecret(target.Token)
	capabilities, code := e.discover(ctx, c)
	if code != apiOK {
		return code
	}
	if !capabilities["websocket"] {
		return usageError(e, op, "server does not advertise the websocket capability")
	}
	stream, err := c.StreamEvents(ctx, filters.options())
	if err != nil {
		return e.writeRequestError(strings.Join(op.CommandPath, " "), err, "")
	}
	defer stream.Close()
	for count := 0; ; count++ {
		if maxEvents.set && count >= maxEvents.value {
			return apiOK
		}
		raw, err := stream.Next(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				return apiOK
			}
			switch websocket.CloseStatus(err) {
			case websocket.StatusNormalClosure, websocket.StatusGoingAway, websocket.StatusNoStatusRcvd:
				return apiOK
			}
			return e.writeRequestError(strings.Join(op.CommandPath, " "), err, "")
		}
		if err := writeJSON(e.stdout, raw, false); err != nil {
			fmt.Fprintf(e.stderr, "abbs api event stream: write output: %v\n", err)
			return apiIOExit
		}
	}
}

func runAgentRegister(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	fs := operationFlags(e, op)
	username := fs.String("username", "", "agent username")
	displayName := fs.String("display-name", "", "optional agent display name")
	var idpFile trackedString
	idpEnv := trackedString{value: "ABBS_IDP_TOKEN"}
	fs.Var(&idpFile, "idp-token-file", "IdP access token file")
	fs.Var(&idpEnv, "idp-token-env", "IdP access token environment variable (default ABBS_IDP_TOKEN)")
	var mutation mutationFlags
	mutation.add(fs)
	if code, ok := parseFlags(e, op, fs, args); !ok {
		return code
	}
	if !connectUsernameRE.MatchString(*username) {
		return usageError(e, op, "--username must match %s", connectUsernameRE.String())
	}
	if utf8.RuneCountInString(*displayName) > 100 {
		return usageError(e, op, "--display-name must be at most 100 characters")
	}
	if idpFile.set && idpEnv.set {
		return usageError(e, op, "--idp-token-file and --idp-token-env are mutually exclusive")
	}
	token, err := readSecret(idpFile.value, idpEnv.value, nil)
	if err != nil {
		return usageError(e, op, "IdP credential: %v", err)
	}
	e.addSecret(token)
	req := api.RegisterAgentRequest{Username: *username}
	if *displayName != "" {
		req.DisplayName = displayName
	}
	return e.executeHTTP(ctx, op, apiRequest{
		path: op.PathTemplate, body: req, bearerOverride: &token, idempotencyKey: mutation.idempotencyKey,
	})
}

func runTokenRefresh(ctx context.Context, e *apiEnvironment, op apiOperation, args []string) int {
	fs := operationFlags(e, op)
	var tokenFile, tokenEnv trackedString
	fs.Var(&tokenFile, "refresh-token-file", "refresh token file; - reads stdin")
	fs.Var(&tokenEnv, "refresh-token-env", "refresh token environment variable")
	var mutation mutationFlags
	mutation.add(fs)
	if code, ok := parseFlags(e, op, fs, args); !ok {
		return code
	}
	if tokenFile.set && tokenEnv.set {
		return usageError(e, op, "--refresh-token-file and --refresh-token-env are mutually exclusive")
	}
	var stdin io.Reader
	if !tokenFile.set && !tokenEnv.set || tokenFile.value == "-" {
		if readerIsTerminal(e.stdin) {
			return usageError(e, op, "pipe the refresh token on stdin or pass --refresh-token-file/--refresh-token-env")
		}
		stdin = e.stdin
	}
	file := tokenFile.value
	if file == "-" {
		file = ""
	}
	token, err := readSecret(file, tokenEnv.value, stdin)
	if err != nil {
		return usageError(e, op, "refresh credential: %v", err)
	}
	e.addSecret(token)
	emptyBearer := ""
	return e.executeHTTP(ctx, op, apiRequest{
		path: op.PathTemplate, body: api.RefreshTokenRequest{RefreshToken: token}, bearerOverride: &emptyBearer, idempotencyKey: mutation.idempotencyKey,
	})
}

func readSecret(file, env string, stdin io.Reader) (string, error) {
	var b []byte
	var err error
	switch {
	case file != "":
		b, err = os.ReadFile(file)
	case env != "":
		value := os.Getenv(env)
		if value == "" {
			return "", fmt.Errorf("environment variable %s is empty", env)
		}
		b = []byte(value)
	case stdin != nil:
		b, err = readCLIInput(stdin)
	default:
		return "", fmt.Errorf("no credential source provided")
	}
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return "", fmt.Errorf("credential is empty")
	}
	return secret, nil
}
