// Package xunlei adapts the local Xunlei web API. It is not an official Xunlei SDK.
package xunlei

import (
	"encoding/json"
	"net/http"
	"time"
)

// Config configures a single local Xunlei instance (engine 3.21.0 or newer).
// BaseURL is the origin, optionally including a reverse-proxy path prefix.
type Config struct {
	BaseURL    string
	Username   string
	Password   string
	HTTPClient *http.Client
}

type Error struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Status  int    `json:"-"`
}

func (e *Error) Error() string { return e.Message }

type Volume struct {
	Path          string `json:"path"`
	CapacityBytes *int64 `json:"capacity_bytes"`
	UsedBytes     *int64 `json:"used_bytes"`
}
type Device struct {
	ID       string   `json:"id"`
	Version  string   `json:"version"`
	Online   bool     `json:"online"`
	LoggedIn bool     `json:"logged_in"`
	Volumes  []Volume `json:"volumes"`
}

type Task struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	Type                string            `json:"type"`
	Phase               string            `json:"phase"`
	Status              string            `json:"status"`
	Message             string            `json:"message,omitempty"`
	URL                 string            `json:"url,omitempty"`
	DestinationID       string            `json:"destination_id,omitempty"`
	Params              map[string]string `json:"params,omitempty"`
	SizeBytes           *int64            `json:"size_bytes"`
	DownloadedBytes     *int64            `json:"downloaded_bytes"`
	Progress            *float64          `json:"progress"`
	SpeedBytesPerSecond int64             `json:"speed_bytes_per_second"`
	ObservedAt          time.Time         `json:"observed_at"`
	// Raw retains unknown upstream metadata for protocol compatibility and retry.
	// It is never exposed by the public JSON representation.
	Raw      map[string]json.RawMessage `json:"-"`
	prepared *preparedRetry
}
type TaskPage struct {
	Tasks         []Task `json:"tasks"`
	NextPageToken string `json:"next_page_token"`
	ExpiresIn     int    `json:"expires_in"`
}
type ListOptions struct {
	Status, Cursor string
	Limit          int
}

type Directory struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	DisplayPath string `json:"display_path,omitempty"`
	Writable    bool   `json:"writable"`
}
type DirectoryPage struct {
	Directories   []Directory `json:"directories"`
	NextPageToken string      `json:"next_page_token"`
	ParentPath    string      `json:"parent_path,omitempty"`
}

type Resource struct {
	ID        string                     `json:"id,omitempty"`
	Name      string                     `json:"name"`
	FileIndex *int                       `json:"file_index"`
	SizeBytes *int64                     `json:"size_bytes"`
	FileCount int                        `json:"file_count"`
	Resources []Resource                 `json:"resources,omitempty"`
	Raw       map[string]json.RawMessage `json:"-"`
}
type Resolution struct {
	ID        string     `json:"id"`
	URL       string     `json:"url"`
	Resources []Resource `json:"resources"`
	// Complete means all advertised files were returned; partial selections
	// require a complete tree with unique, explicit upstream file indices.
	Complete bool `json:"complete"`
}
type CreateRequest struct {
	URL           string `json:"url"`
	DestinationID string `json:"destination_id,omitempty"`
	// DestinationPath is resolved by the public API before the Xunlei client is
	// called. It is a path made from names shown in Xunlei, not a host path.
	DestinationPath string `json:"destination_path,omitempty"`
	Name            string `json:"name,omitempty"`
	// A nil slice selects all files. An explicit empty slice is invalid.
	FileIndices []int `json:"file_indices,omitempty"`
}
