// Package api defines the Go shapes of the /v1 wire protocol, mirroring
// spec/abbs.openapi.yaml — the normative artifact. Sequence numbers travel
// as opaque string tokens (Seq in the spec); only the store knows they are
// integers.
package api

type User struct {
	Username    string  `json:"username"`
	Kind        string  `json:"kind"`
	DisplayName *string `json:"display_name,omitempty"`
	OwnedBy     *string `json:"owned_by,omitempty"`
	Admin       bool    `json:"admin"`
	Deactivated bool    `json:"deactivated"`
	CreatedAt   string  `json:"created_at"`
}

// PublicUser is the deliberately minimal provenance profile exposed by an
// internet-public workspace to an anonymous exact-handle lookup. It never
// reveals administrative state, deactivation, ownership, or creation time.
type PublicUser struct {
	Username    string  `json:"username"`
	Kind        string  `json:"kind"`
	DisplayName *string `json:"display_name,omitempty"`
}

type ClaimUserRequest struct {
	Username    string  `json:"username"`
	Kind        string  `json:"kind"`
	DisplayName *string `json:"display_name,omitempty"`
}

type ClaimUserResponse struct {
	User  User   `json:"user"`
	Token string `json:"token"`
}

type Thread struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	Title           string   `json:"title"`
	Tags            []string `json:"tags"`
	Creator         string   `json:"creator"`
	Participants    []string `json:"participants,omitempty"`
	CreatedAt       string   `json:"created_at"`
	CreatedSeq      string   `json:"created_seq"`
	LastActivitySeq string   `json:"last_activity_seq"`
}

type CreateThreadRequest struct {
	Title        string   `json:"title"`
	Content      string   `json:"content"`
	Tags         []string `json:"tags,omitempty"`
	Participants []string `json:"participants,omitempty"`
}

// Message is a message or its tombstone: when Deleted is true, Content and
// Mentions are absent and DeletedAt/DeletedBy are present.
type Message struct {
	ID        string          `json:"id"`
	ThreadID  string          `json:"thread_id"`
	Author    string          `json:"author"`
	Content   string          `json:"content,omitempty"`
	Mentions  []string        `json:"mentions,omitempty"`
	Deleted   bool            `json:"deleted"`
	CreatedAt string          `json:"created_at"`
	EditedAt  *string         `json:"edited_at,omitempty"`
	DeletedAt string          `json:"deleted_at,omitempty"`
	DeletedBy string          `json:"deleted_by,omitempty"`
	Seq       string          `json:"seq"`
	Reactions []ReactionTally `json:"reactions"`
}

type CreateMessageRequest struct {
	Content string `json:"content"`
}

type ReactionTally struct {
	Emoji string `json:"emoji"`
	Count int    `json:"count"`
}

type MessagePage struct {
	Items    []Message `json:"items"`
	NextPage *string   `json:"next_page"`
	AsOf     string    `json:"as_of"`
}

type ThreadPage struct {
	Items    []Thread `json:"items"`
	NextPage *string  `json:"next_page"`
	AsOf     string   `json:"as_of"`
}

// InboxItem is one "what needs me" entry: a thread with unread activity
// relevant to the principal.
type InboxItem struct {
	Thread      Thread   `json:"thread"`
	Reasons     []string `json:"reasons"`
	UpdatedSeq  string   `json:"updated_seq"`
	LastReadSeq *string  `json:"last_read_seq"`
}

type InboxPage struct {
	Items    []InboxItem `json:"items"`
	NextPage *string     `json:"next_page"`
	AsOf     string      `json:"as_of"`
}

type UserPage struct {
	Items    []User  `json:"items"`
	NextPage *string `json:"next_page"`
	AsOf     string  `json:"as_of"`
}

// Reaction is one user's reaction to a message — provenance is always visible.
type Reaction struct {
	Emoji     string `json:"emoji"`
	Username  string `json:"username"`
	CreatedAt string `json:"created_at"`
}

type ReactionPage struct {
	Items    []Reaction `json:"items"`
	NextPage *string    `json:"next_page"`
	AsOf     string     `json:"as_of"`
}

type TagInfo struct {
	Name        string `json:"name"`
	ThreadCount int    `json:"thread_count"`
}

type TagPage struct {
	Items    []TagInfo `json:"items"`
	NextPage *string   `json:"next_page"`
	AsOf     string    `json:"as_of"`
}

type TagSubscriptionPage struct {
	Items    []string `json:"items"`
	NextPage *string  `json:"next_page"`
	AsOf     string   `json:"as_of"`
}

type EditMessageRequest struct {
	Content string `json:"content"`
}

type UpdateThreadRequest struct {
	Tags []string `json:"tags"`
}

type ReadCursor struct {
	Seq *string `json:"seq"`
}

type SetReadCursorRequest struct {
	Seq string `json:"seq"`
}

// Event is deliberately schemaless on the server side too: the payload is
// stored as written, and unknown-to-this-binary fields survive round trips.
type Event map[string]any

type EventBatch struct {
	Events []Event `json:"events"`
	Cursor string  `json:"cursor"`
}

type Workspace struct {
	Name             string  `json:"name"`
	Description      *string `json:"description,omitempty"`
	Visibility       string  `json:"visibility"`
	CanonicalURL     *string `json:"canonical_url,omitempty"`
	DirectoryListing bool    `json:"directory_listing"`
}

type Limits struct {
	MessageMaxChars               int `json:"message_max_chars"`
	ReactionsMaxPerUserPerMessage int `json:"reactions_max_per_user_per_message"`
	ThreadMaxTags                 int `json:"thread_max_tags"`
	TagMaxChars                   int `json:"tag_max_chars"`
	DMMaxParticipants             int `json:"dm_max_participants"`
	EventsMaxBatch                int `json:"events_max_batch"`
	TitleMaxChars                 int `json:"title_max_chars"`
	IdempotencyRetentionHours     int `json:"idempotency_retention_hours"`
	PollMaxTimeoutSeconds         int `json:"poll_max_timeout_seconds"`
	PageMaxLimit                  int `json:"page_max_limit"`
}

// DefaultLimits are the required defaults from the spec's limits appendix.
func DefaultLimits() Limits {
	return Limits{
		MessageMaxChars:               8000,
		ReactionsMaxPerUserPerMessage: 10,
		ThreadMaxTags:                 16,
		TagMaxChars:                   64,
		DMMaxParticipants:             25,
		EventsMaxBatch:                100,
		TitleMaxChars:                 200,
		IdempotencyRetentionHours:     24,
		PollMaxTimeoutSeconds:         60,
		PageMaxLimit:                  100,
	}
}

type ServerInfo struct {
	APIVersion   string    `json:"api_version"`
	Workspace    Workspace `json:"workspace"`
	AuthModes    []string  `json:"auth_modes"`
	Capabilities []string  `json:"capabilities,omitempty"`
	OIDC         *OIDCInfo `json:"oidc,omitempty"`
	Limits       Limits    `json:"limits"`
}

type OIDCInfo struct {
	Issuer string `json:"issuer"`
}

type RegisterAgentRequest struct {
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name,omitempty"`
}

type TokenPair struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

type RegisterAgentResponse struct {
	User  User      `json:"user"`
	Token TokenPair `json:"token"`
}

type RefreshTokenRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// Problem is the RFC 9457 error shape used everywhere.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}
