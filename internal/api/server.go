// Package api exposes authenticated, versioned download management endpoints.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/myth815/xunlei-api/internal/store"
	"github.com/myth815/xunlei-api/internal/xunlei"
)

type Backend interface {
	Device(context.Context) (xunlei.Device, error)
	ListTasks(context.Context, xunlei.ListOptions) (xunlei.TaskPage, error)
	Task(context.Context, string) (xunlei.Task, error)
	Directories(context.Context, string, string, int) (xunlei.DirectoryPage, error)
	CreateDirectory(context.Context, string, string) (xunlei.Directory, error)
	Resolve(context.Context, string) (xunlei.Resolution, error)
	UploadTorrent(context.Context, string, io.Reader) (xunlei.Resolution, error)
	CreateTask(context.Context, xunlei.CreateRequest) (xunlei.Task, error)
	Control(context.Context, xunlei.Task, string) error
	DeleteRecord(context.Context, string) error
	PrepareRetry(context.Context, xunlei.Task) (xunlei.Task, error)
	RecreateTask(context.Context, xunlei.Task) (xunlei.Task, error)
}

type Config struct {
	APIKey           string
	AllowedOrigins   []string
	RequestTimeout   time.Duration
	OperationTimeout time.Duration
	PollInterval     time.Duration
	OpenAPI          []byte
}

type Server struct {
	cfg      Config
	backend  Backend
	store    *store.Store
	key      [32]byte
	origins  map[string]bool
	handler  http.Handler
	mutation sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func New(cfg Config, b Backend, st *store.Store) (*Server, error) {
	if len(cfg.APIKey) < 32 || len(cfg.APIKey) > 512 || strings.TrimSpace(cfg.APIKey) != cfg.APIKey {
		return nil, errors.New("API_KEY must contain 32 to 512 characters without surrounding whitespace")
	}
	if b == nil || st == nil {
		return nil, errors.New("backend and store are required")
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 45 * time.Second
	}
	if cfg.OperationTimeout <= 0 {
		cfg.OperationTimeout = 2 * time.Minute
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 3 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{cfg: cfg, backend: b, store: st, key: sha256.Sum256([]byte(cfg.APIKey)), origins: map[string]bool{}, ctx: ctx, cancel: cancel}
	for _, origin := range cfg.AllowedOrigins {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || strings.Contains(origin, "*") || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "chrome-extension") {
			cancel()
			return nil, errors.New("CORS_ALLOWED_ORIGINS must contain exact HTTP(S) or chrome-extension origins without paths or wildcards")
		}
		s.origins[origin] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(cfg.OpenAPI)
	})
	mux.HandleFunc("GET /v1/device", s.device)
	mux.HandleFunc("GET /v1/directories", s.directories)
	mux.HandleFunc("POST /v1/directories", s.createDirectory)
	mux.HandleFunc("POST /v1/resources/resolve", s.resolve)
	mux.HandleFunc("POST /v1/resources/torrent", s.torrent)
	mux.HandleFunc("GET /v1/tasks", s.tasks)
	mux.HandleFunc("GET /v1/tasks/{id}", s.task)
	mux.HandleFunc("POST /v1/tasks", s.createTask)
	mux.HandleFunc("POST /v1/tasks/{id}/pause", s.control)
	mux.HandleFunc("POST /v1/tasks/{id}/resume", s.control)
	mux.HandleFunc("POST /v1/tasks/{id}/retry", s.retry)
	mux.HandleFunc("DELETE /v1/tasks/{id}", s.deleteTask)
	mux.HandleFunc("GET /v1/operations/{id}", s.operation)
	s.handler = s.security(mux)
	// A persisted planned intent may have reached the upstream before a crash.
	// Never re-execute it automatically, even if its task ID is unknown.
	for _, op := range st.Pending() {
		if op.Status == "planned" {
			op.Status = "unknown_outcome"
			op.Message = "Service restarted during this operation; it will not be automatically repeated"
			if err := st.Update(op); err != nil {
				cancel()
				return nil, err
			}
		}
	}
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.handler }
func (s *Server) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(s.cfg.PollInterval)
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
				s.Reconcile(s.ctx)
			}
		}
	}()
}
func (s *Server) Close() { s.cancel(); s.wg.Wait() }

func (s *Server) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if origin := r.Header.Get("Origin"); origin != "" {
			if !s.origins[origin] {
				fail(w, 403, "origin_not_allowed", "This browser origin is not allowed")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				method := r.Header.Get("Access-Control-Request-Method")
				if method != "GET" && method != "POST" && method != "DELETE" {
					fail(w, 403, "method_not_allowed", "Preflight method is not allowed")
					return
				}
				for _, header := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
					switch strings.ToLower(strings.TrimSpace(header)) {
					case "", "authorization", "content-type", "idempotency-key":
					default:
						fail(w, 403, "header_not_allowed", "Preflight header is not allowed")
						return
					}
				}
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(204)
				return
			}
		}
		if r.URL.Path == "/healthz" && (r.Method == "GET" || r.Method == "HEAD") {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(auth, "Bearer ")
		sum := sha256.Sum256([]byte(token))
		if !ok || len(token) > 512 || subtle.ConstantTimeCompare(sum[:], s.key[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="xunlei-api"`)
			fail(w, 401, "unauthorized", "A valid Bearer API key is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func backendError(w http.ResponseWriter, err error) {
	var e *xunlei.Error
	if errors.As(err, &e) {
		status := 502
		switch e.Kind {
		case "invalid":
			status = 400
		case "not_found":
			status = 404
		case "unauthorized":
			status = 502
		case "unavailable":
			status = 503
		case "unknown_outcome":
			status = 504
		}
		fail(w, status, e.Kind, e.Message)
		return
	}
	fail(w, 502, "upstream_error", "The upstream request could not be completed")
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		fail(w, 415, "unsupported_media_type", "Use application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "invalid_json", "Invalid JSON request body")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		fail(w, 400, "invalid_json", "Exactly one JSON value is required")
		return false
	}
	return true
}
func limit(r *http.Request) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 100, nil
	}
	n, e := strconv.Atoi(v)
	if e != nil || n < 1 || n > 200 {
		return 0, errors.New("limit must be between 1 and 200")
	}
	return n, nil
}
func (s *Server) readContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
}
func (s *Server) device(w http.ResponseWriter, r *http.Request) {
	ctx, c := s.readContext(r)
	defer c()
	v, e := s.backend.Device(ctx)
	if e != nil {
		backendError(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) directories(w http.ResponseWriter, r *http.Request) {
	n, e := limit(r)
	if e != nil {
		fail(w, 400, "invalid_limit", e.Error())
		return
	}
	ctx, c := s.readContext(r)
	defer c()
	v, e := s.backend.Directories(ctx, r.URL.Query().Get("parent_id"), r.URL.Query().Get("cursor"), n)
	if e != nil {
		backendError(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	n, e := limit(r)
	if e != nil {
		fail(w, 400, "invalid_limit", e.Error())
		return
	}
	ctx, c := s.readContext(r)
	defer c()
	v, e := s.backend.ListTasks(ctx, xunlei.ListOptions{Status: r.URL.Query().Get("status"), Cursor: r.URL.Query().Get("cursor"), Limit: n})
	if e != nil {
		backendError(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) task(w http.ResponseWriter, r *http.Request) {
	ctx, c := s.readContext(r)
	defer c()
	v, e := s.lookupTask(ctx, r.PathValue("id"))
	if e != nil {
		backendError(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &body) {
		return
	}
	ctx, c := s.readContext(r)
	defer c()
	v, e := s.backend.Resolve(ctx, body.URL)
	if e != nil {
		backendError(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) torrent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 17<<20)
	if e := r.ParseMultipartForm(1 << 20); e != nil {
		fail(w, 400, "invalid_torrent", "A multipart torrent file of at most 16 MiB is required")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if r.MultipartForm == nil || len(r.MultipartForm.File["file"]) != 1 || len(r.MultipartForm.File) != 1 {
		fail(w, 400, "invalid_torrent", "Supply exactly one file field")
		return
	}
	f, h, e := r.FormFile("file")
	if e != nil {
		fail(w, 400, "invalid_torrent", "The file field is required")
		return
	}
	defer f.Close()
	if h.Size > 16<<20 || h.Size == 0 || !strings.HasSuffix(strings.ToLower(h.Filename), ".torrent") {
		fail(w, 400, "invalid_torrent", "Supply a .torrent file of at most 16 MiB")
		return
	}
	ctx, c := s.readContext(r)
	defer c()
	v, e := s.backend.UploadTorrent(ctx, h.Filename, f)
	if e != nil {
		backendError(w, e)
		return
	}
	writeJSON(w, 200, v)
}

func publicOperation(op store.Operation) store.Operation { op.Details = nil; return op }
func operationResponse(w http.ResponseWriter, status int, op store.Operation) {
	writeJSON(w, status, map[string]any{"operation": publicOperation(op)})
}
func (s *Server) operation(w http.ResponseWriter, r *http.Request) {
	op, e := s.store.Find(r.PathValue("id"))
	if errors.Is(e, store.ErrNotFound) {
		fail(w, 404, "not_found", "Operation not found")
		return
	}
	if e != nil {
		fail(w, 503, "storage_error", "Operation storage is unavailable")
		return
	}
	operationResponse(w, 200, op)
}

// The global mutation lock prevents overlapping commands and a retry/delete
// interleaving. Reads remain concurrent. An intent is synced before any write.
func (s *Server) begin(w http.ResponseWriter, r *http.Request, kind string, body any) (store.Operation, bool) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 1 || len(key) > 200 || strings.TrimSpace(key) != key {
		fail(w, 400, "idempotency_key_required", "Use an Idempotency-Key of 1 to 200 characters")
		return store.Operation{}, false
	}
	data, e := json.Marshal(body)
	if e != nil {
		fail(w, 400, "invalid_request", "Invalid request")
		return store.Operation{}, false
	}
	sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.EscapedPath()+"?"+r.URL.Query().Encode()+"\n"), data...))
	op, exists, e := s.store.Begin(key, hex.EncodeToString(sum[:]), store.Operation{Kind: kind, TaskID: r.PathValue("id"), Status: "planned", Details: data})
	if errors.Is(e, store.ErrConflict) {
		fail(w, 409, "idempotency_conflict", "This Idempotency-Key was used for a different request")
		return op, false
	}
	if e != nil {
		fail(w, 503, "storage_error", "Could not persist the operation; no upstream write was attempted")
		return op, false
	}
	if exists {
		operationResponse(w, 200, op)
		return op, false
	}
	return op, true
}
func (s *Server) save(w http.ResponseWriter, op store.Operation) bool {
	if e := s.store.Update(op); e != nil {
		writeJSON(w, 503, map[string]any{"error": map[string]string{"code": "storage_error", "message": "Could not persist the result; inspect this operation or replay the same Idempotency-Key before issuing another request", "operation_id": op.ID}})
		return false
	}
	return true
}
func (s *Server) failure(w http.ResponseWriter, op store.Operation, err error) {
	status := 502
	op.Status = "failed"
	op.Message = "The operation could not be completed"
	var e *xunlei.Error
	if errors.As(err, &e) {
		op.Message = e.Message
		switch e.Kind {
		case "invalid":
			status = 400
		case "not_found":
			status = 404
		case "unavailable":
			status = 503
		case "unknown_outcome":
			status = 504
		}
		if e.Kind == "unknown_outcome" {
			op.Status = "unknown_outcome"
		}
	}
	if op.Kind == "retry" && op.PreviousTaskID != "" {
		op.Message = "The retry did not finish; inspect the operation before trying again. " + op.Message
	}
	if !s.save(w, op) {
		return
	}
	operationResponse(w, status, op)
}
func (s *Server) createDirectory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ParentID string `json:"parent_id"`
		Name     string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	op, ok := s.begin(w, r, "create_directory", body)
	if !ok {
		return
	}
	ctx, c := context.WithTimeout(s.ctx, s.cfg.RequestTimeout)
	defer c()
	v, e := s.backend.CreateDirectory(ctx, body.ParentID, body.Name)
	if e != nil {
		s.failure(w, op, e)
		return
	}
	op.Status = "confirmed"
	op.Result, _ = json.Marshal(v)
	if s.save(w, op) {
		operationResponse(w, 201, op)
	}
}
func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var body xunlei.CreateRequest
	if !decode(w, r, &body) {
		return
	}
	if body.FileIndices != nil && len(body.FileIndices) == 0 {
		fail(w, 400, "invalid_file_indices", "file_indices cannot be an empty array; omit it to select all files")
		return
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	op, ok := s.begin(w, r, "create_task", body)
	if !ok {
		return
	}
	ctx, c := context.WithTimeout(s.ctx, s.cfg.RequestTimeout)
	defer c()
	v, e := s.backend.CreateTask(ctx, body)
	if e != nil {
		s.failure(w, op, e)
		return
	}
	op.TaskID = v.ID
	op.Status = "accepted"
	op.Result, _ = json.Marshal(v)
	if s.save(w, op) {
		operationResponse(w, 201, op)
	}
}
func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	action := "pause"
	if strings.HasSuffix(r.URL.Path, "/resume") {
		action = "resume"
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	op, ok := s.begin(w, r, action, nil)
	if !ok {
		return
	}
	ctx, c := context.WithTimeout(s.ctx, s.cfg.RequestTimeout)
	defer c()
	task, e := s.lookupTask(ctx, op.TaskID)
	if e != nil {
		s.failure(w, op, e)
		return
	}
	if task.Status == "error" || task.Status == "complete" {
		op.Status = "failed"
		op.Message = "This task cannot be resumed or paused; use retry for a failed task"
		if s.save(w, op) {
			operationResponse(w, 409, op)
		}
		return
	}
	if (action == "pause" && task.Status == "paused") || (action == "resume" && (task.Status == "running" || task.Status == "pending")) {
		op.Status = "confirmed"
		op.Result, _ = json.Marshal(task)
		if s.save(w, op) {
			operationResponse(w, 200, op)
		}
		return
	}
	if task.Status != "paused" && task.Status != "running" && task.Status != "pending" {
		op.Status = "failed"
		op.Message = "Task state is unknown; no control command was sent"
		if s.save(w, op) {
			operationResponse(w, 409, op)
		}
		return
	}
	if e = s.backend.Control(ctx, task, action); e != nil {
		s.failure(w, op, e)
		return
	}
	op.Status = "accepted"
	if s.save(w, op) {
		operationResponse(w, 202, op)
	}
}
func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	files := false
	if values, ok := r.URL.Query()["delete_files"]; ok {
		if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
			fail(w, 400, "invalid_delete_files", "delete_files must be true or false")
			return
		}
		files = values[0] == "true"
	}
	kind := "delete_record"
	if files {
		kind = "delete_files"
	}
	s.mutation.Lock()
	defer s.mutation.Unlock()
	op, ok := s.begin(w, r, kind, nil)
	if !ok {
		return
	}
	ctx, c := context.WithTimeout(s.ctx, s.cfg.RequestTimeout)
	defer c()
	task, e := s.lookupTask(ctx, op.TaskID)
	if e != nil {
		s.failure(w, op, e)
		return
	}
	if files {
		e = s.backend.Control(ctx, task, "delete")
	} else {
		e = s.backend.DeleteRecord(ctx, task.ID)
	}
	if e != nil {
		s.failure(w, op, e)
		return
	}
	op.Status = "accepted"
	if files {
		op.Message = "Deletion requested; this API cannot independently verify physical filesystem deletion"
	}
	if s.save(w, op) {
		operationResponse(w, 202, op)
	}
}
func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	op, ok := s.begin(w, r, "retry", nil)
	if !ok {
		return
	}
	ctx, c := context.WithTimeout(s.ctx, 2*s.cfg.RequestTimeout)
	defer c()
	task, e := s.lookupTask(ctx, op.TaskID)
	if e != nil {
		s.failure(w, op, e)
		return
	}
	if task.Status != "error" {
		op.Status = "failed"
		op.Message = "Only failed tasks can be retried"
		if s.save(w, op) {
			operationResponse(w, 409, op)
		}
		return
	}
	if task.DestinationID == "" || (task.URL == "" && task.Params["file_id"] == "") {
		op.Status = "failed"
		op.Message = "Original source or destination is missing; old task was preserved"
		if s.save(w, op) {
			operationResponse(w, 409, op)
		}
		return
	}
	// Preflight the device before deleting a recoverable record.
	device, e := s.backend.Device(ctx)
	if e != nil {
		s.failure(w, op, e)
		return
	}
	if !device.Online || !device.LoggedIn {
		op.Status = "failed"
		op.Message = "Device must be online and logged in; old task was preserved"
		if s.save(w, op) {
			operationResponse(w, 409, op)
		}
		return
	}
	task, e = s.backend.PrepareRetry(ctx, task)
	if e != nil {
		s.failure(w, op, e)
		return
	}
	if task.ID != op.TaskID {
		s.failure(w, op, errors.New("retry snapshot returned a different task ID"))
		return
	}
	op.PreviousTaskID = task.ID
	op.Details, _ = json.Marshal(task)
	if !s.save(w, op) {
		return
	}
	if e = s.backend.DeleteRecord(ctx, task.ID); e != nil {
		s.failure(w, op, e)
		return
	}
	op.TaskID = ""
	op.Message = "Old task record removed; files kept; replacement pending"
	if !s.save(w, op) {
		return
	}
	replacement, e := s.backend.RecreateTask(ctx, task)
	if e != nil {
		s.failure(w, op, e)
		return
	}
	op.TaskID = replacement.ID
	op.Status = "accepted"
	op.Message = "Replacement created; old task files were kept"
	op.Result, _ = json.Marshal(replacement)
	if s.save(w, op) {
		operationResponse(w, 201, op)
	}
}

// Reconcile observes known IDs only. It never repeats a write, or infers that
// an unidentifiable create/retry succeeded from some other task's presence.
func (s *Server) Reconcile(ctx context.Context) {
	s.mutation.Lock()
	defer s.mutation.Unlock()
	for _, op := range s.store.Pending() {
		if op.TaskID == "" || op.Status == "planned" {
			continue
		}
		if op.Status == "unknown_outcome" && time.Since(op.CreatedAt) > s.cfg.OperationTimeout {
			continue
		}
		call, c := context.WithTimeout(ctx, s.cfg.RequestTimeout)
		task, e := s.lookupTask(call, op.TaskID)
		c()
		var xe *xunlei.Error
		absent := errors.As(e, &xe) && xe.Kind == "not_found"
		switch {
		case e == nil && (op.Kind == "create_task" || (op.Kind == "retry" && op.TaskID != op.PreviousTaskID)):
			op.Status = "confirmed"
			op.Result, _ = json.Marshal(task)
			op.Message = "Task creation confirmed; download completion is tracked on the task"
		case e == nil && ((op.Kind == "pause" && task.Status == "paused") || (op.Kind == "resume" && (task.Status == "pending" || task.Status == "running" || task.Status == "complete"))):
			op.Status = "confirmed"
			op.Result, _ = json.Marshal(task)
		case absent && op.Kind == "delete_record":
			op.Status = "confirmed"
			op.Result = json.RawMessage(`{"task_record_absent":true,"files_deleted":false}`)
		case absent && op.Kind == "delete_files":
			op.Result = json.RawMessage(`{"task_record_absent":true,"files_deleted":null}`)
		}
		if op.Status != "confirmed" && time.Since(op.CreatedAt) > s.cfg.OperationTimeout {
			op.Status = "unknown_outcome"
			op.Message = "The command was sent but its full result could not be confirmed; it will not be automatically repeated"
		}
		if err := s.store.Update(op); err != nil {
			return
		}
	}
}

func (s *Server) lookupTask(ctx context.Context, id string) (xunlei.Task, error) {
	task, err := s.backend.Task(ctx, id)
	if err == nil && task.ID != id {
		return xunlei.Task{}, &xunlei.Error{Kind: "upstream", Message: "The upstream returned a different task than requested", Status: 502}
	}
	return task, err
}
