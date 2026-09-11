package xunlei

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func scalar(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}
func int64Value(raw json.RawMessage) *int64 {
	s := scalar(raw)
	if s == "" {
		return nil
	}
	v, e := strconv.ParseInt(s, 10, 64)
	if e != nil || v < 0 {
		return nil
	}
	return &v
}
func floatValue(raw json.RawMessage) *float64 {
	s := scalar(raw)
	if s == "" {
		return nil
	}
	v, e := strconv.ParseFloat(s, 64)
	if e != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
		return nil
	}
	return &v
}
func paramsValue(raw json.RawMessage) map[string]string {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	p := map[string]string{}
	for k, v := range m {
		p[k] = scalar(v)
	}
	return p
}
func isDownload(typ string) bool { return typ == "user#download-url" || typ == "user#download" }
func normalizeTask(obj map[string]json.RawMessage) Task {
	p := paramsValue(obj["params"])
	t := Task{ID: scalar(obj["id"]), Name: scalar(obj["name"]), Type: scalar(obj["type"]), Phase: scalar(obj["phase"]), Message: scalar(obj["message"]), Params: p, Raw: obj, URL: p["url"], DestinationID: p["parent_folder_id"], SizeBytes: int64Value(obj["file_size"]), DownloadedBytes: int64Value(json.RawMessage(strconv.Quote(p["download_size"]))), Progress: floatValue(obj["progress"]), ObservedAt: time.Now().UTC()}
	statuses := map[string]string{"PHASE_TYPE_PENDING": "pending", "PHASE_TYPE_RUNNING": "running", "PHASE_TYPE_PAUSED": "paused", "PHASE_TYPE_COMPLETE": "complete", "PHASE_TYPE_ERROR": "error"}
	t.Status = statuses[t.Phase]
	if t.Status == "" {
		t.Status = "unknown"
	}
	speed, _ := strconv.ParseInt(p["speed"], 10, 64)
	if speed > 0 {
		t.SpeedBytesPerSecond = speed
	}
	if t.Status == "complete" {
		v := float64(100)
		t.Progress = &v
		if t.DownloadedBytes == nil && t.SizeBytes != nil {
			v := *t.SizeBytes
			t.DownloadedBytes = &v
		}
	} else if t.SizeBytes != nil && *t.SizeBytes > 0 && t.DownloadedBytes != nil && *t.DownloadedBytes == *t.SizeBytes {
		if checked := int64Value(json.RawMessage(strconv.Quote(p["checked_size"]))); checked != nil && *checked <= *t.SizeBytes {
			v := float64(*checked) / float64(*t.SizeBytes) * 100
			t.Progress = &v
		}
	}
	return t
}
func validateID(id string) error {
	if strings.TrimSpace(id) == "" || len(id) > 4096 || strings.ContainsAny(id, "\r\n\x00,") {
		return invalid("a valid nonempty resource ID is required")
	}
	return nil
}
func limitValue(limit, defaultLimit, max int) (int, error) {
	if limit == 0 {
		return defaultLimit, nil
	}
	if limit < 1 || limit > max {
		return 0, invalid("limit is outside the supported range")
	}
	return limit, nil
}
func encodeFilter(m map[string]any) string { data, _ := json.Marshal(m); return string(data) }

func (c *Client) ListTasks(ctx context.Context, options ListOptions) (TaskPage, error) {
	limit, err := limitValue(options.Limit, 100, 1000)
	if err != nil {
		return TaskPage{}, err
	}
	filters := map[string]any{"type": map[string]string{"in": "user#download-url,user#download"}}
	statuses := map[string]string{"pending": "PHASE_TYPE_PENDING", "running": "PHASE_TYPE_RUNNING", "paused": "PHASE_TYPE_PAUSED", "complete": "PHASE_TYPE_COMPLETE", "error": "PHASE_TYPE_ERROR", "active": "PHASE_TYPE_PENDING,PHASE_TYPE_RUNNING,PHASE_TYPE_PAUSED,PHASE_TYPE_ERROR"}
	if options.Status != "" && options.Status != "all" {
		phase, ok := statuses[options.Status]
		if !ok {
			return TaskPage{}, invalid("unsupported task status")
		}
		filters["phase"] = map[string]string{"in": phase}
	}
	return c.listTasks(ctx, filters, options.Cursor, limit)
}
func (c *Client) listTasks(ctx context.Context, filters map[string]any, cursor string, limit int) (TaskPage, error) {
	_, device, err := c.authenticated(ctx)
	if err != nil {
		return TaskPage{}, err
	}
	q := url.Values{"space": {device.ID}, "limit": {strconv.Itoa(limit)}, "page_token": {cursor}, "filters": {encodeFilter(filters)}}
	obj, err := c.call(ctx, http.MethodGet, "/drive/v1/tasks", q, nil, false)
	if err != nil {
		return TaskPage{}, err
	}
	var raw []map[string]json.RawMessage
	if v, ok := obj["tasks"]; ok {
		if json.Unmarshal(v, &raw) != nil {
			return TaskPage{}, malformed(false)
		}
	} else {
		// Engine 3.21.0 omits tasks when a successful query has no matches.
		// Require its status + polling envelope so arbitrary {} is not a 404.
		status := scalar(obj["HttpStatus"])
		if (status != "200" && status != "0") || int64Value(obj["expires_in"]) == nil {
			return TaskPage{}, malformed(false)
		}
	}
	page := TaskPage{Tasks: []Task{}, NextPageToken: scalar(obj["next_page_token"])}
	if n := int64Value(obj["expires_in"]); n != nil && *n <= 86400 {
		page.ExpiresIn = int(*n)
	}
	for _, v := range raw {
		t := normalizeTask(v)
		if !isDownload(t.Type) {
			continue
		}
		space := scalar(v["space"])
		if space != "" && space != device.ID {
			continue
		}
		if target := t.Params["target"]; target != "" && target != device.ID {
			continue
		}
		if t.ID == "" {
			return TaskPage{}, malformed(false)
		}
		page.Tasks = append(page.Tasks, t)
	}
	return page, nil
}
func (c *Client) Task(ctx context.Context, id string) (Task, error) {
	if err := validateID(id); err != nil {
		return Task{}, err
	}
	page, err := c.listTasks(ctx, map[string]any{"id": map[string]string{"in": id}, "type": map[string]string{"in": "user#download-url,user#download"}}, "", 100)
	if err != nil {
		return Task{}, err
	}
	for _, task := range page.Tasks {
		if task.ID == id {
			return task, nil
		}
	}
	return Task{}, &Error{Kind: "not_found", Status: 404, Message: "local download task was not found"}
}

func (c *Client) ownedTask(ctx context.Context, task Task) (Device, error) {
	if err := validateID(task.ID); err != nil {
		return Device{}, err
	}
	if !isDownload(task.Type) {
		return Device{}, invalid("only local download tasks can be controlled")
	}
	_, d, err := c.authenticated(ctx)
	if err != nil {
		return Device{}, err
	}
	if space := scalar(task.Raw["space"]); space != "" && space != d.ID {
		return Device{}, invalid("task belongs to a different device")
	}
	if target := task.Params["target"]; target != "" && target != d.ID {
		return Device{}, invalid("task belongs to a different device")
	}
	// Task is normally obtained through Task/ListTasks. Verify again if callers
	// construct one themselves without upstream ownership metadata.
	if scalar(task.Raw["space"]) == "" && task.Params["target"] == "" {
		actual, err := c.Task(ctx, task.ID)
		if err != nil {
			return Device{}, err
		}
		if actual.Type != task.Type {
			return Device{}, invalid("task type does not match the upstream task")
		}
	}
	return d, nil
}
func (c *Client) Control(ctx context.Context, task Task, action string) error {
	phase := ""
	switch action {
	case "pause":
		phase = "pause"
	case "resume", "start", "running":
		phase = "running"
	case "delete", "delete_files":
		phase = "delete"
	default:
		return invalid("unsupported task action")
	}
	if phase == "running" && task.Status == "error" {
		return invalid("failed tasks require an explicit retry")
	}
	d, err := c.ownedTask(ctx, task)
	if err != nil {
		return err
	}
	spec, _ := json.Marshal(map[string]string{"phase": phase})
	_, err = c.call(ctx, http.MethodPost, "/method/patch/drive/v1/task", nil, map[string]any{"space": d.ID, "type": task.Type, "id": task.ID, "set_params": map[string]string{"spec": string(spec)}}, true)
	return err
}
func (c *Client) DeleteRecord(ctx context.Context, id string) error {
	task, err := c.Task(ctx, id)
	if err != nil {
		return err
	}
	d, err := c.ownedTask(ctx, task)
	if err != nil {
		return err
	}
	_, err = c.call(ctx, http.MethodPost, "/method/delete/drive/v1/tasks", url.Values{"space": {d.ID}, "task_ids": {id}}, map[string]any{}, true)
	return err
}

// preparedRetry is an immutable, private copy of the actual creation request.
// Returning Task metadata to a caller cannot change a prepared request's source,
// selection, directory, or device through shared map/pointer references.
type preparedRetry struct {
	deviceID string
	body     map[string]any
}

func validSavedSelection(value string) bool {
	// Preserve the known single-file sentinels used by older providers.
	if value == "" || value == "-1" || value == "--1," {
		return true
	}
	if len(value) > 1<<20 {
		return false
	}
	for _, part := range strings.Split(value, ",") {
		bounds := strings.Split(part, "-")
		if len(bounds) < 1 || len(bounds) > 2 {
			return false
		}
		first, err := strconv.ParseUint(bounds[0], 10, 31)
		if err != nil || bounds[0] == "" {
			return false
		}
		if len(bounds) == 2 {
			last, err := strconv.ParseUint(bounds[1], 10, 31)
			if err != nil || last < first {
				return false
			}
		}
	}
	return true
}

// PrepareRetry validates every known precondition before the caller removes the
// old record. The returned snapshot is bound to this device, and contains a
// private copy of the exact request needed by RecreateTask.
func (c *Client) PrepareRetry(ctx context.Context, task Task) (Task, error) {
	d, err := c.ownedTask(ctx, task)
	if err != nil {
		return Task{}, err
	}
	if task.DestinationID == "" {
		return Task{}, invalid("task has no saved destination")
	}
	if _, err = c.requireDirectory(ctx, task.DestinationID); err != nil {
		return Task{}, err
	}
	name := task.Name
	fileName := scalar(task.Raw["file_name"])
	if fileName == "" {
		fileName = name
	}
	if !validName(name) || !validName(fileName) {
		return Task{}, invalid("saved task names cannot safely be recreated")
	}
	if task.SizeBytes != nil && *task.SizeBytes < 0 {
		return Task{}, invalid("saved task size is invalid")
	}
	params := map[string]string{"target": d.ID, "parent_folder_id": task.DestinationID, "file_id": ""}
	switch task.Type {
	case "user#download-url":
		if err := validateURL(task.URL); err != nil {
			return Task{}, invalid("saved task source URL cannot be recreated")
		}
		params["url"] = task.URL
	case "user#download":
		if err := validateID(task.Params["file_id"]); err != nil {
			return Task{}, invalid("saved cloud file ID cannot be recreated")
		}
		params["file_id"] = task.Params["file_id"]
	default:
		return Task{}, invalid("saved task type cannot be recreated")
	}
	count := task.Params["total_file_count"]
	if count == "" {
		count = "1"
	}
	fileCount, err := strconv.ParseInt(count, 10, 64)
	if err != nil || fileCount < 1 || fileCount > 10000000 {
		return Task{}, invalid("saved task file count is invalid")
	}
	params["total_file_count"] = count
	selection := task.Params["sub_file_index"]
	if !validSavedSelection(selection) {
		return Task{}, invalid("saved file selection cannot safely be recreated")
	}
	if selection != "" {
		params["sub_file_index"] = selection
	}
	params["mime_type"] = task.Params["mime_type"]
	size := "0"
	if task.SizeBytes != nil {
		size = strconv.FormatInt(*task.SizeBytes, 10)
	}
	_, current, err := c.authenticated(ctx)
	if err != nil {
		return Task{}, err
	}
	if current.ID != d.ID {
		return Task{}, &Error{Kind: "unavailable", Status: 503, Message: "the upstream device changed while preparing the retry; the old record must be retained"}
	}
	body := map[string]any{"type": task.Type, "name": name, "file_name": fileName, "file_size": size, "space": d.ID, "params": params}
	// Avoid modifying maps/pointers in the caller's original observation.
	copied := task
	copied.Params = map[string]string{}
	for key, value := range task.Params {
		copied.Params[key] = value
	}
	copied.Params["target"] = d.ID
	copied.Raw = map[string]json.RawMessage{}
	for key, value := range task.Raw {
		copied.Raw[key] = append(json.RawMessage(nil), value...)
	}
	if task.SizeBytes != nil {
		value := *task.SizeBytes
		copied.SizeBytes = &value
	}
	if task.DownloadedBytes != nil {
		value := *task.DownloadedBytes
		copied.DownloadedBytes = &value
	}
	if task.Progress != nil {
		value := *task.Progress
		copied.Progress = &value
	}
	copied.prepared = &preparedRetry{deviceID: d.ID, body: body}
	return copied, nil
}

// RecreateTask creates a new task from a PrepareRetry snapshot. It never removes
// the old record or queries that old ID after preparation. For callers that have
// not prepared a snapshot, validation happens first and the old record must
// still exist. Deleting the old record is always the caller's responsibility.
func (c *Client) RecreateTask(ctx context.Context, task Task) (Task, error) {
	if task.prepared == nil {
		var err error
		task, err = c.PrepareRetry(ctx, task)
		if err != nil {
			return Task{}, err
		}
	}
	_, d, err := c.authenticated(ctx)
	if err != nil {
		return Task{}, err
	}
	if d.ID != task.prepared.deviceID {
		return Task{}, &Error{Kind: "unavailable", Status: 503, Message: "the upstream device changed after retry preparation; no replacement was created"}
	}
	return c.submit(ctx, task.prepared.body)
}

func (c *Client) submit(ctx context.Context, body map[string]any) (Task, error) {
	obj, err := c.call(ctx, http.MethodPost, "/drive/v1/task", nil, body, true)
	if err != nil {
		return Task{}, err
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(obj["task"], &raw) != nil || scalar(raw["id"]) == "" {
		return Task{}, malformed(true)
	}
	// Creation replies may contain only an ID. Preserve the known submitted fields
	// without turning a follow-up read failure into an accidental duplicate create.
	for _, key := range []string{"type", "name", "file_name", "file_size", "space", "params"} {
		if _, ok := raw[key]; !ok {
			raw[key], _ = json.Marshal(body[key])
		}
	}
	return normalizeTask(raw), nil
}
