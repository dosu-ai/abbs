package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dosu-ai/abbs/internal/api"
	"github.com/dosu-ai/abbs/internal/emoji"
	"github.com/dosu-ai/abbs/internal/store"
)

// mapStoreError writes the problem for a store sentinel error; the caller
// handles nil itself.
func mapStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not-found", "no such resource")
	case errors.Is(err, store.ErrForbidden):
		writeProblem(w, http.StatusForbidden, "forbidden", "not allowed")
	case errors.Is(err, store.ErrMessageDeleted):
		writeProblem(w, http.StatusConflict, "message-deleted", "the message is tombstoned")
	case errors.Is(err, store.ErrReactionLimit):
		writeProblem(w, http.StatusUnprocessableEntity, "reaction-limit",
			"at most 10 distinct emoji per user per message")
	default:
		writeProblem(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authenticate(w, r); !ok {
		return
	}
	limit, ok := s.parseLimit(w, r, 50)
	if !ok {
		return
	}
	items, nextPage, asOf, err := s.store.ListUsers(r.URL.Query().Get("page"), limit)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.UserPage{Items: items, NextPage: nextPage, AsOf: asOf})
}

func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	_, authenticated, ok := s.conditionalReadViewer(w, r)
	if !ok {
		return
	}
	user, err := s.store.GetUser(r.PathValue("username"))
	if err != nil {
		mapStoreError(w, err)
		return
	}
	if authenticated == nil {
		writeJSON(w, http.StatusOK, api.PublicUser{
			Username: user.Username, Kind: user.Kind, DisplayName: user.DisplayName,
		})
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) handleDeactivateUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if !actor.Admin {
		writeProblem(w, http.StatusForbidden, "forbidden", "admin role required")
		return
	}
	user, err := s.store.DeactivateUser(r.PathValue("username"), time.Now())
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) handleUpdateThreadTags(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var req api.UpdateThreadRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tags := normalizeTags(req.Tags)
	if len(tags) > s.limits.ThreadMaxTags {
		writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("at most %d tags per thread", s.limits.ThreadMaxTags))
		return
	}
	for _, t := range tags {
		if utf8.RuneCountInString(t) > s.limits.TagMaxChars {
			writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("tag %q over %d characters", t, s.limits.TagMaxChars))
			return
		}
	}
	thread, err := s.store.UpdateThreadTags(r.PathValue("thread_id"), user.Username, tags, time.Now())
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, thread)
}

func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	msg, err := s.store.GetMessage(r.PathValue("message_id"), user.Username)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

func (s *Server) handleEditMessage(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var req api.EditMessageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.checkContent(w, req.Content) {
		return
	}
	msg, err := s.store.EditMessage(r.PathValue("message_id"), user.Username, req.Content, time.Now())
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	msg, err := s.store.DeleteMessage(r.PathValue("message_id"), user.Username, user.Admin, time.Now())
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

func (s *Server) handleListReactions(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	after, ok := parsePageAnchor(w, r)
	if !ok {
		return
	}
	limit, ok := s.parseLimit(w, r, 50)
	if !ok {
		return
	}
	items, nextPage, asOf, err := s.store.ListReactions(r.PathValue("message_id"), user.Username, after, limit)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.ReactionPage{Items: items, NextPage: nextPage, AsOf: asOf})
}

// pathEmoji validates and normalizes the {emoji} path parameter.
func pathEmoji(w http.ResponseWriter, r *http.Request) (string, bool) {
	key, err := emoji.Normalize(r.PathValue("emoji"))
	if err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid-emoji",
			"reactions must be a single Unicode emoji (one grapheme cluster with an emoji base)")
		return "", false
	}
	return key, true
}

func (s *Server) handleAddReaction(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	key, ok := pathEmoji(w, r)
	if !ok {
		return
	}
	if err := s.store.AddReaction(r.PathValue("message_id"), user.Username, key, time.Now()); err != nil {
		mapStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRemoveReaction(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	key, ok := pathEmoji(w, r)
	if !ok {
		return
	}
	if err := s.store.RemoveReaction(r.PathValue("message_id"), user.Username, key, time.Now()); err != nil {
		mapStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListTags(w http.ResponseWriter, r *http.Request) {
	viewer, _, ok := s.conditionalReadViewer(w, r)
	if !ok {
		return
	}
	limit, ok := s.parseLimit(w, r, 50)
	if !ok {
		return
	}
	items, nextPage, asOf, err := s.store.ListTags(viewer, r.URL.Query().Get("page"), limit)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.TagPage{Items: items, NextPage: nextPage, AsOf: asOf})
}

// pathTag normalizes the {tag} path parameter.
func (s *Server) pathTag(w http.ResponseWriter, r *http.Request) (string, bool) {
	tag := strings.ToLower(strings.TrimSpace(r.PathValue("tag")))
	if tag == "" || utf8.RuneCountInString(tag) > s.limits.TagMaxChars {
		writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("tag must be 1..%d characters", s.limits.TagMaxChars))
		return "", false
	}
	return tag, true
}

func (s *Server) handleSubscribeTag(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	tag, ok := s.pathTag(w, r)
	if !ok {
		return
	}
	if err := s.store.SubscribeTag(user.Username, tag); err != nil {
		mapStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUnsubscribeTag(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	tag, ok := s.pathTag(w, r)
	if !ok {
		return
	}
	if err := s.store.UnsubscribeTag(user.Username, tag); err != nil {
		mapStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListTagSubscriptions(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	limit, ok := s.parseLimit(w, r, 50)
	if !ok {
		return
	}
	items, nextPage, asOf, err := s.store.ListTagSubscriptions(user.Username, r.URL.Query().Get("page"), limit)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.TagSubscriptionPage{Items: items, NextPage: nextPage, AsOf: asOf})
}
