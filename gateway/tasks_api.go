package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxTaskBodyBytes caps a task submission body (413 beyond).
const maxTaskBodyBytes = 1 << 20

// Validation bounds for task submissions (plan §b).
const (
	minTimeoutS      = 1
	maxTimeoutS      = 86400
	minDeadlineS     = 60
	maxDeadlineS     = 604800
	minMaxAttempts   = 1
	maxMaxAttempts   = 5
	minPriority      = -100
	maxPriority      = 100
	defaultListLimit = 50
	maxListLimit     = 1000
)

// taskSubmitRequest is the POST /v1/tasks body. agent may alias as model;
// exactly one of input / request must be present. Pointer fields distinguish
// absent (config default) from explicit values.
type taskSubmitRequest struct {
	Agent          string          `json:"agent"`
	Model          string          `json:"model"`
	Input          *string         `json:"input"`
	Request        json.RawMessage `json:"request"`
	Priority       int             `json:"priority"`
	TimeoutS       *int            `json:"timeout_s"`
	DeadlineS      *int            `json:"deadline_s"`
	NotBefore      string          `json:"not_before"`
	MaxAttempts    *int            `json:"max_attempts"`
	RetryOnPartial *bool           `json:"retry_on_partial"`
	SessionID      string          `json:"session_id"`
}

// taskRequestShape validates the request field: it must carry messages.
type taskRequestShape struct {
	Messages []json.RawMessage `json:"messages"`
}

// taskSummary is the list-item shape of GET /v1/tasks (plan §b).
type taskSummary struct {
	ID              string     `json:"id"`
	Object          string     `json:"object"` // "task.summary"
	Agent           string     `json:"agent"`
	State           TaskState  `json:"state"`
	Priority        int        `json:"priority"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	Attempts        int        `json:"attempts"`
	CancelRequested bool       `json:"cancel_requested"`
	AgeS            int64      `json:"age_s"`
	Deadline        time.Time  `json:"deadline"`
	Error           *TaskError `json:"error"`
}

// taskListResponse is the GET /v1/tasks envelope.
type taskListResponse struct {
	Object  string        `json:"object"`
	Data    []taskSummary `json:"data"`
	HasMore bool          `json:"has_more"`
}

// taskCancelResponse is the task record plus the idempotency marker for
// cancels of already-terminal tasks.
type taskCancelResponse struct {
	Task
	AlreadyTerminal bool `json:"already_terminal,omitempty"`
}

// taskDeleteResponse acknowledges DELETE /v1/tasks/{id}.
type taskDeleteResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // "task.deleted"
	Deleted bool   `json:"deleted"`
}

// registerTasksAPI wires the /v1/tasks* routes onto mux. main.go calls it iff
// tasks.enabled — when disabled the paths are simply not registered, so they
// 404 exactly like any unknown path.
func registerTasksAPI(mux *http.ServeMux, s *server) {
	mux.HandleFunc("/v1/tasks", s.handleTasksCollection)
	mux.HandleFunc("/v1/tasks/", s.handleTaskItem)
}

// handleTasksCollection serves POST /v1/tasks (submit) and GET /v1/tasks (list).
func (s *server) handleTasksCollection(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.handleTaskSubmit(w, r, tok)
	case http.MethodGet:
		s.handleTaskList(w, r, tok)
	default:
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
	}
}

// handleTaskSubmit validates a submission (plan §b) and creates the task.
func (s *server) handleTaskSubmit(w http.ResponseWriter, r *http.Request, tok *Token) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTaskBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("Request body exceeds %d bytes", maxTaskBodyBytes), "invalid_request_error")
			return
		}
		writeError(w, http.StatusBadRequest, "Failed to read request body", "invalid_request_error")
		return
	}
	var req taskSubmitRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body", "invalid_request_error")
		return
	}

	agent := req.Agent
	if agent == "" {
		agent = req.Model // alias
	}
	if agent == "" {
		writeError(w, http.StatusBadRequest, "Missing required field: agent", "invalid_request_error")
		return
	}
	if _, ok := s.cfg.Agents[agent]; !ok {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("Unknown agent %s", agent), "invalid_request_error")
		return
	}
	ri := reqInfoFrom(r.Context())
	if ri != nil {
		ri.agent = agent
		ri.span.SetAttr("hermes.agent", agent)
		ri.span.SetAttr("hermes.mode", "task_api")
	}
	if !agentAllowed(tok.Agents, agent) {
		writeError(w, http.StatusForbidden,
			fmt.Sprintf("Token not authorized for agent %s", agent), "invalid_request_error")
		return
	}

	// input XOR request.
	hasInput := req.Input != nil && *req.Input != ""
	hasRequest := len(req.Request) > 0 && string(req.Request) != "null"
	if hasInput == hasRequest {
		writeError(w, http.StatusBadRequest,
			"Exactly one of input or request is required", "invalid_request_error")
		return
	}
	var taskReq json.RawMessage
	if hasInput {
		taskReq, err = json.Marshal(map[string]any{
			"messages": []map[string]string{{"role": "user", "content": *req.Input}},
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to build request", "internal_error")
			return
		}
	} else {
		var shape taskRequestShape
		if json.Unmarshal(req.Request, &shape) != nil || len(shape.Messages) == 0 {
			writeError(w, http.StatusBadRequest,
				"request must be an object with a non-empty messages array", "invalid_request_error")
			return
		}
		taskReq = req.Request // request.model, if any, is ignored by the dispatcher
	}

	// Range validation; absent (nil) fields take the config defaults in Submit.
	if req.TimeoutS != nil && (*req.TimeoutS < minTimeoutS || *req.TimeoutS > maxTimeoutS) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("timeout_s must be between %d and %d", minTimeoutS, maxTimeoutS), "invalid_request_error")
		return
	}
	if req.DeadlineS != nil && (*req.DeadlineS < minDeadlineS || *req.DeadlineS > maxDeadlineS) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("deadline_s must be between %d and %d", minDeadlineS, maxDeadlineS), "invalid_request_error")
		return
	}
	if req.MaxAttempts != nil && (*req.MaxAttempts < minMaxAttempts || *req.MaxAttempts > maxMaxAttempts) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("max_attempts must be between %d and %d", minMaxAttempts, maxMaxAttempts), "invalid_request_error")
		return
	}
	var notBefore *time.Time
	if req.NotBefore != "" {
		t, perr := time.Parse(time.RFC3339, req.NotBefore)
		if perr != nil {
			writeError(w, http.StatusBadRequest, "not_before must be RFC3339", "invalid_request_error")
			return
		}
		notBefore = &t
	}
	// A not_before past the deadline (always created_at + deadline_s) would
	// create a task guaranteed to expire without ever running.
	if notBefore != nil {
		deadlineS := s.cfg.Tasks.DefaultDeadlineS
		if req.DeadlineS != nil {
			deadlineS = *req.DeadlineS
		}
		if notBefore.After(time.Now().Add(time.Duration(deadlineS) * time.Second)) {
			writeError(w, http.StatusBadRequest,
				"not_before must not be later than the task deadline", "invalid_request_error")
			return
		}
	}
	priority := req.Priority
	if priority < minPriority {
		priority = minPriority
	}
	if priority > maxPriority {
		priority = maxPriority
	}

	spec := SubmitSpec{
		Agent:          agent,
		Request:        taskReq,
		Priority:       priority,
		NotBefore:      notBefore,
		RetryOnPartial: req.RetryOnPartial,
		SessionID:      req.SessionID,
		SubmittedBy:    tok.Name,
	}
	// Persist the submit span context so task attempts can link back to it
	// across restarts (the traceparent is generated even when export is off).
	if ri != nil && ri.span != nil {
		sc := ri.span.Context()
		spec.SubmitTrace = &TraceRef{TraceID: sc.TraceID, SpanID: sc.SpanID}
	}
	if req.TimeoutS != nil {
		spec.TimeoutS = *req.TimeoutS
	}
	if req.DeadlineS != nil {
		spec.DeadlineS = *req.DeadlineS
	}
	if req.MaxAttempts != nil {
		spec.MaxAttempts = *req.MaxAttempts
	}

	task, err := s.store.Submit(spec)
	switch {
	case err == nil:
		if ri != nil {
			ri.span.SetAttr("hermes.task_id", task.ID)
		}
		writeJSON(w, http.StatusCreated, task)
	case errors.Is(err, ErrTooManyPending):
		w.Header().Set("Retry-After", strconv.Itoa(s.cfg.Scheduler.RetryAfterS))
		writeError(w, http.StatusTooManyRequests,
			fmt.Sprintf("Agent %s has too many pending tasks", agent), "rate_limit_error")
	default:
		var span *Span
		if ri != nil {
			span = ri.span
		}
		logError("store_error", append([]any{"agent", agent, "detail", "submit persist failed",
			"err", err.Error()}, spanLogAttrs(span)...)...)
		writeError(w, http.StatusInternalServerError, "Failed to persist task", "server_error")
	}
}

// handleTaskList serves GET /v1/tasks?agent=&state=&limit=&after= — summaries
// sorted created_at desc with a keyset cursor, filtered to the token's scope.
func (s *server) handleTaskList(w http.ResponseWriter, r *http.Request, tok *Token) {
	q := r.URL.Query()
	filter := TaskFilter{
		Agent: q.Get("agent"),
		Scope: tok.Agents,
		Limit: defaultListLimit,
		After: q.Get("after"),
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer", "invalid_request_error")
			return
		}
		if n > maxListLimit {
			n = maxListLimit
		}
		filter.Limit = n
	}
	for _, v := range q["state"] {
		switch TaskState(v) {
		case TaskPending, TaskRunning, TaskSucceeded, TaskFailed, TaskCancelled, TaskExpired:
			filter.States = append(filter.States, TaskState(v))
		default:
			if v == "active" {
				filter.States = append(filter.States, TaskPending, TaskRunning)
			} else {
				writeError(w, http.StatusBadRequest,
					fmt.Sprintf("Unknown state %s", v), "invalid_request_error")
				return
			}
		}
	}

	items, hasMore := s.store.List(filter)
	now := time.Now().UTC()
	data := make([]taskSummary, 0, len(items))
	for i := range items {
		t := &items[i]
		data = append(data, taskSummary{
			ID:              t.ID,
			Object:          "task.summary",
			Agent:           t.Agent,
			State:           t.State,
			Priority:        t.Priority,
			CreatedAt:       t.CreatedAt,
			UpdatedAt:       t.UpdatedAt,
			Attempts:        t.Attempts,
			CancelRequested: t.CancelRequested,
			AgeS:            int64(now.Sub(t.CreatedAt) / time.Second),
			Deadline:        t.Deadline,
			Error:           t.Error,
		})
	}
	writeJSON(w, http.StatusOK, taskListResponse{Object: "list", Data: data, HasMore: hasMore})
}

// handleTaskItem serves the suffix-routed item endpoints (go1.21-safe manual
// routing): GET/DELETE /v1/tasks/{id}, GET /v1/tasks/{id}/output and
// POST /v1/tasks/{id}/cancel.
func (s *server) handleTaskItem(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/tasks/")
	id, action := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		id, action = rest[:i], rest[i+1:]
	}
	if id == "" {
		writeError(w, http.StatusNotFound, "No such task", "invalid_request_error")
		return
	}

	task, found := s.store.Get(id)
	if !found {
		writeError(w, http.StatusNotFound, "No such task", "invalid_request_error")
		return
	}
	if ri := reqInfoFrom(r.Context()); ri != nil {
		ri.agent = task.Agent
		ri.span.SetAttr("hermes.agent", task.Agent)
		ri.span.SetAttr("hermes.mode", "task_api")
		ri.span.SetAttr("hermes.task_id", task.ID)
	}
	if !agentAllowed(tok.Agents, task.Agent) {
		writeError(w, http.StatusForbidden,
			fmt.Sprintf("Token not authorized for agent %s", task.Agent), "invalid_request_error")
		return
	}

	switch action {
	case "":
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, task)
		case http.MethodDelete:
			s.handleTaskDelete(w, r, id)
		default:
			writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
		}
	case "output":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
			return
		}
		s.handleTaskOutput(w, id)
	case "cancel":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "invalid_request_error")
			return
		}
		s.handleTaskCancel(w, r, id)
	default:
		writeError(w, http.StatusNotFound, "No such task", "invalid_request_error")
	}
}

// handleTaskOutput streams the whole output spool snapshot as text/plain.
func (s *server) handleTaskOutput(w http.ResponseWriter, id string) {
	data, err := os.ReadFile(s.store.OutputPath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "Failed to read task output", "server_error")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleTaskCancel requests cancellation; idempotent on terminal tasks.
func (s *server) handleTaskCancel(w http.ResponseWriter, r *http.Request, id string) {
	task, alreadyTerminal, err := s.store.Cancel(id)
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			writeError(w, http.StatusNotFound, "No such task", "invalid_request_error")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to cancel task", "server_error")
		return
	}
	writeJSON(w, http.StatusOK, taskCancelResponse{Task: task, AlreadyTerminal: alreadyTerminal})
}

// handleTaskDelete removes a terminal task (409 otherwise).
func (s *server) handleTaskDelete(w http.ResponseWriter, r *http.Request, id string) {
	err := s.store.Delete(id)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, taskDeleteResponse{ID: id, Object: "task.deleted", Deleted: true})
	case errors.Is(err, ErrTaskNotFound):
		writeError(w, http.StatusNotFound, "No such task", "invalid_request_error")
	case errors.Is(err, ErrNotTerminal):
		writeError(w, http.StatusConflict, "Task is not terminal", "invalid_request_error")
	default:
		writeError(w, http.StatusInternalServerError, "Failed to delete task", "server_error")
	}
}
