package xunlei

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func directoryValue(raw map[string]json.RawMessage) (Directory, bool) {
	if scalar(raw["kind"]) != "drive#folder" {
		return Directory{}, false
	}
	params := paramsValue(raw["params"])
	d := Directory{ID: scalar(raw["id"]), Name: scalar(raw["name"]), Path: params["RealPath"], Writable: params["is_write"] == "true"}
	if d.Path == "" {
		d.Path = params["real_path"]
	}
	return d, d.ID != ""
}
func (c *Client) Directories(ctx context.Context, parent, cursor string, limit int) (DirectoryPage, error) {
	limit, err := limitValue(limit, 200, 1000)
	if err != nil {
		return DirectoryPage{}, err
	}
	_, d, err := c.authenticated(ctx)
	if err != nil {
		return DirectoryPage{}, err
	}
	q := url.Values{"space": {d.ID}, "parent_id": {parent}, "page_token": {cursor}, "limit": {strconv.Itoa(limit)}, "filters": {encodeFilter(map[string]any{"kind": map[string]string{"eq": "drive#folder"}})}, "with": {"withCategoryDiskMountPath", "withCategoryDownloadPath"}}
	obj, err := c.call(ctx, http.MethodGet, "/drive/v1/files", q, nil, false)
	if err != nil {
		return DirectoryPage{}, err
	}
	var files []map[string]json.RawMessage
	if v, ok := obj["files"]; !ok || json.Unmarshal(v, &files) != nil {
		return DirectoryPage{}, malformed(false)
	}
	page := DirectoryPage{Directories: []Directory{}, NextPageToken: scalar(obj["next_page_token"])}
	for _, raw := range files {
		if space := scalar(raw["space"]); space != "" && space != d.ID {
			continue
		}
		if entry, ok := directoryValue(raw); ok {
			page.Directories = append(page.Directories, entry)
		}
	}
	return page, nil
}
func (c *Client) requireDirectory(ctx context.Context, id string) (Directory, error) {
	if err := validateID(id); err != nil {
		return Directory{}, err
	}
	_, d, err := c.authenticated(ctx)
	if err != nil {
		return Directory{}, err
	}
	obj, err := c.call(ctx, http.MethodGet, "/drive/v1/files/"+url.PathEscape(id), url.Values{"space": {d.ID}, "with": {"withCategoryDiskMountPath", "withCategoryDownloadPath"}}, nil, false)
	if err != nil {
		return Directory{}, err
	}
	if file, ok := obj["file"]; ok {
		if json.Unmarshal(file, &obj) != nil {
			return Directory{}, malformed(false)
		}
	}
	if scalar(obj["id"]) != id {
		return Directory{}, invalid("destination does not identify a directory on this device")
	}
	if space := scalar(obj["space"]); space != "" && space != d.ID {
		return Directory{}, invalid("destination belongs to a different device")
	}
	if target := paramsValue(obj["params"])["target"]; target != "" && target != d.ID {
		return Directory{}, invalid("destination belongs to a different device")
	}
	dir, ok := directoryValue(obj)
	if !ok || !dir.Writable {
		return Directory{}, invalid("destination is not a writable directory")
	}
	return dir, nil
}
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 1024 && !strings.ContainsAny(name, "/\\\x00\r\n")
}
func (c *Client) CreateDirectory(ctx context.Context, parent, name string) (Directory, error) {
	if !validName(name) {
		return Directory{}, invalid("directory name must be a single nonempty path component")
	}
	if _, err := c.requireDirectory(ctx, parent); err != nil {
		return Directory{}, err
	}
	_, d, err := c.authenticated(ctx)
	if err != nil {
		return Directory{}, err
	}
	obj, err := c.call(ctx, http.MethodPost, "/drive/v1/files", nil, map[string]string{"parent_id": parent, "name": name, "space": d.ID, "kind": "drive#folder"}, true)
	if err != nil {
		return Directory{}, err
	}
	if file, ok := obj["file"]; ok {
		if json.Unmarshal(file, &obj) != nil {
			return Directory{}, malformed(true)
		}
	}
	dir, ok := directoryValue(obj)
	if !ok {
		return Directory{}, malformed(true)
	}
	return dir, nil
}

func validateURL(source string) error {
	if len(source) > 65536 || strings.ContainsAny(source, "\r\n\x00") {
		return invalid("invalid source URL")
	}
	u, err := url.Parse(source)
	if err != nil {
		return invalid("invalid source URL")
	}
	switch u.Scheme {
	case "http", "https":
		if u.Host != "" {
			return nil
		}
	case "magnet":
		if len(u.Query()["xt"]) > 0 {
			return nil
		}
	}
	return invalid("source must be one HTTP(S) URL or magnet link")
}
func parseResource(raw map[string]json.RawMessage) (Resource, int, bool, error) {
	if _, err := decodeResponse(mustJSON(raw), false); err != nil {
		return Resource{}, 0, false, err
	}
	r := Resource{ID: scalar(raw["id"]), Name: scalar(raw["name"]), SizeBytes: int64Value(raw["file_size"]), Raw: raw}
	if v := int64Value(raw["file_count"]); v != nil && *v <= 10000000 {
		r.FileCount = int(*v)
	}
	if len(raw["file_index"]) > 0 {
		v, err := strconv.Atoi(scalar(raw["file_index"]))
		if err == nil {
			r.FileIndex = &v
		}
	}
	var dir map[string]json.RawMessage
	isDir := len(raw["dir"]) > 0 && string(raw["dir"]) != "null"
	if isDir && json.Unmarshal(raw["dir"], &dir) != nil {
		return Resource{}, 0, false, malformed(false)
	}
	if !isDir {
		if r.FileCount == 0 {
			r.FileCount = 1
		}
		// An advertised multi-file object without its tree is incomplete.
		return r, 1, r.FileCount == 1, nil
	}
	var children []map[string]json.RawMessage
	if err := json.Unmarshal(dir["resources"], &children); err != nil {
		return r, 0, false, nil
	}
	count, complete := 0, true
	for _, child := range children {
		node, n, done, err := parseResource(child)
		if err != nil {
			return Resource{}, 0, false, err
		}
		r.Resources = append(r.Resources, node)
		count += n
		complete = complete && done
	}
	if r.FileCount == 0 {
		r.FileCount = count
	}
	if count != r.FileCount || scalar(dir["next_page_token"]) != "" || scalar(dir["has_more"]) == "true" {
		complete = false
	}
	return r, count, complete, nil
}
func mustJSON(v any) []byte { data, _ := json.Marshal(v); return data }
func (c *Client) Resolve(ctx context.Context, source string) (Resolution, error) {
	if err := validateURL(source); err != nil {
		return Resolution{}, err
	}
	obj, err := c.call(ctx, http.MethodPost, "/drive/v1/resource/list", nil, map[string]any{"urls": source, "page_size": 1000}, false)
	if err != nil {
		return Resolution{}, err
	}
	var list map[string]json.RawMessage
	if json.Unmarshal(obj["list"], &list) != nil {
		return Resolution{}, malformed(false)
	}
	var resources []map[string]json.RawMessage
	if json.Unmarshal(list["resources"], &resources) != nil || len(resources) == 0 {
		return Resolution{}, &Error{Kind: "upstream", Status: 502, Message: "source resolution returned no downloadable resources"}
	}
	resolution := Resolution{ID: scalar(obj["list_id"]), URL: source, Resources: []Resource{}, Complete: true}
	for _, raw := range resources {
		r, _, complete, err := parseResource(raw)
		if err != nil {
			return Resolution{}, err
		}
		resolution.Resources = append(resolution.Resources, r)
		resolution.Complete = resolution.Complete && complete
	}
	if scalar(obj["next_page_token"]) != "" || scalar(list["next_page_token"]) != "" || scalar(list["has_more"]) == "true" {
		resolution.Complete = false
	}
	return resolution, nil
}
func (c *Client) UploadTorrent(ctx context.Context, filename string, data io.Reader) (Resolution, error) {
	if data == nil {
		return Resolution{}, invalid("torrent data is required")
	}
	name := filepath.Base(filename)
	if !validName(name) || !strings.EqualFold(filepath.Ext(name), ".torrent") {
		return Resolution{}, invalid("filename must end in .torrent")
	}
	torrent, err := io.ReadAll(io.LimitReader(data, maxTorrentBytes+1))
	if err != nil {
		return Resolution{}, invalid("torrent could not be read")
	}
	if len(torrent) == 0 || len(torrent) > maxTorrentBytes {
		return Resolution{}, invalid("torrent must contain between 1 byte and 16 MiB")
	}
	token, _, err := c.authenticated(ctx)
	if err != nil {
		return Resolution{}, err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		return Resolution{}, invalid("invalid torrent filename")
	}
	_, _ = part.Write(torrent)
	_ = writer.WriteField("pan-auth", token)
	_ = writer.Close()
	raw, err := c.exchange(ctx, http.MethodPost, "/device/btinfo", token, nil, body.Bytes(), writer.FormDataContentType(), true)
	if err != nil {
		return Resolution{}, err
	}
	obj, err := decodeResponse(raw, true)
	if err != nil {
		return Resolution{}, err
	}
	source := scalar(obj["url"])
	if source == "" {
		return Resolution{}, malformed(true)
	}
	return c.Resolve(ctx, source)
}

func selectedIndices(res Resolution, indices []int) (string, error) {
	if indices == nil {
		return "", nil
	}
	if len(indices) == 0 {
		return "", invalid("file_indices cannot be an empty array")
	}
	if !res.Complete {
		return "", invalid("file selection requires a complete resource tree; upstream pagination or missing metadata prevents safe selection")
	}
	available := map[int]bool{}
	valid := true
	var walk func(Resource)
	walk = func(r Resource) {
		if len(r.Resources) > 0 {
			for _, child := range r.Resources {
				walk(child)
			}
			return
		}
		if r.FileIndex == nil || *r.FileIndex < 0 || available[*r.FileIndex] {
			valid = false
			return
		}
		available[*r.FileIndex] = true
	}
	for _, r := range res.Resources {
		walk(r)
	}
	if !valid {
		return "", invalid("file selection requires unique, explicit upstream file indices")
	}
	selected := append([]int(nil), indices...)
	sort.Ints(selected)
	result := make([]string, 0, len(selected))
	for i, index := range selected {
		if index < 0 || !available[index] {
			return "", invalid("file_indices contains an index absent from the resolved resource")
		}
		if i > 0 && selected[i-1] == index {
			return "", invalid("file_indices contains duplicate indices")
		}
		result = append(result, strconv.Itoa(index))
	}
	return strings.Join(result, ","), nil
}
func (c *Client) CreateTask(ctx context.Context, request CreateRequest) (Task, error) {
	if request.FileIndices != nil && len(request.FileIndices) == 0 {
		return Task{}, invalid("file_indices cannot be an empty array")
	}
	if request.Name != "" && !validName(request.Name) {
		return Task{}, invalid("task name must be a single path component")
	}
	if err := validateURL(request.URL); err != nil {
		return Task{}, err
	}
	if _, err := c.requireDirectory(ctx, request.DestinationID); err != nil {
		return Task{}, err
	}
	res, err := c.Resolve(ctx, request.URL)
	if err != nil {
		return Task{}, err
	}
	if len(res.Resources) != 1 {
		return Task{}, invalid("a task must resolve to exactly one top-level resource")
	}
	indices, err := selectedIndices(res, request.FileIndices)
	if err != nil {
		return Task{}, err
	}
	r := res.Resources[0]
	name := request.Name
	if name == "" {
		name = r.Name
	}
	if !validName(name) {
		return Task{}, invalid("resolved resource needs an explicit safe task name")
	}
	_, d, err := c.authenticated(ctx)
	if err != nil {
		return Task{}, err
	}
	size := "0"
	if r.SizeBytes != nil {
		size = strconv.FormatInt(*r.SizeBytes, 10)
	}
	count := r.FileCount
	if count < 1 {
		count = 1
	}
	params := map[string]string{"target": d.ID, "url": request.URL, "total_file_count": strconv.Itoa(count), "parent_folder_id": request.DestinationID, "file_id": ""}
	if indices != "" {
		params["sub_file_index"] = indices
	}
	return c.submit(ctx, map[string]any{"type": "user#download-url", "name": name, "file_name": name, "file_size": size, "space": d.ID, "params": params})
}
