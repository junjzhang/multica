package backgroundwait

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ReadDelivery reads receipts without consuming or acknowledging a result.
type ReadDelivery func(context.Context, []protocol.WaitLookup) (protocol.WaitDeliveryResponse, error)

func (m *Manager) ConfigureList(read ReadDelivery) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readDelivery = read
}

// ServeSessionList is called only with a server-authenticated frozen address.
// It does not grant command execution or access to another session's records.
func (m *Manager) ServeSessionList(w http.ResponseWriter, r *http.Request, address protocol.WaitAddress) {
	if r.Method != http.MethodGet || r.URL.Path != "/waits" || address.SessionID == "" || address.WorkspaceID == "" || address.AgentID == "" || address.DaemonID != m.daemonID || address.TaskID != "" {
		http.Error(w, "session scope required", 403)
		return
	}
	m.list(w, r, address)
}

func (m *Manager) list(w http.ResponseWriter, r *http.Request, scope protocol.WaitAddress) {
	q := r.URL.Query()
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			http.Error(w, "limit must be 1..200", 400)
			return
		}
		limit = n
	}
	if !validFilter(q.Get("state"), "running,cancelling,succeeded,failed,timed_out,cancelled,lost") || !validFilter(q.Get("delivery_status"), "unknown,not_enqueued,pending,leased,delivered,failed") || !validFilter(q.Get("processing_status"), "unknown,unread,read,handled") {
		http.Error(w, "invalid status filter", 400)
		return
	}
	// Query parameters narrow an authenticated scope; they never select one.
	if (scope.TaskID != "" && (q.Get("session") != "" || (q.Get("task") != "" && q.Get("task") != scope.TaskID))) || (scope.SessionID != "" && ((q.Get("session") != "" && q.Get("session") != scope.SessionID) || q.Get("task") != "" || (q.Get("workspace") != "" && q.Get("workspace") != scope.WorkspaceID))) {
		http.Error(w, "filter outside credential scope", 403)
		return
	}
	m.mu.Lock()
	read := m.readDelivery
	items := make([]protocol.WaitListItem, 0)
	for _, row := range m.rows {
		a := row.Address
		if a.DaemonID != m.daemonID || (scope.TaskID != "" && a.TaskID != scope.TaskID) || (scope.SessionID != "" && (a.TaskID != "" || a.WorkspaceID != scope.WorkspaceID || a.AgentID != scope.AgentID || a.SessionID != scope.SessionID)) {
			continue
		}
		if (q.Get("session") != "" && a.SessionID != q.Get("session")) || (q.Get("workspace") != "" && a.WorkspaceID != q.Get("workspace")) || (q.Get("task") != "" && a.TaskID != q.Get("task")) {
			continue
		}
		a.WorkDir = ""
		item := protocol.WaitListItem{ID: row.ID, Address: a, State: row.State, Description: row.Description, StartedAt: row.StartedAt, Deadline: row.Deadline, ExitCode: row.ExitCode, NotifyError: row.NotifyError}
		if !row.FinishedAt.IsZero() {
			finished := row.FinishedAt
			item.FinishedAt = &finished
		}
		// Local durable receipts remain useful while the server is unreachable,
		// but Known=false prevents them masquerading as a fresh observation.
		item.Delivery.DeliveryID = row.DeliveryID
		if row.Handled {
			item.Delivery.ProcessingStatus = "handled"
		}
		items = append(items, item)
	}
	m.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	var deliveryObservedAt *time.Time
	if read != nil && len(items) > 0 {
		lookups := make([]protocol.WaitLookup, len(items))
		for i, item := range items {
			lookups[i] = protocol.WaitLookup{ID: item.ID, Address: item.Address}
		}
		result, err := read(r.Context(), lookups)
		if err == nil {
			if !result.ObservedAt.IsZero() {
				deliveryObservedAt = &result.ObservedAt
			}
			for i := range items {
				if status, ok := result.Statuses[items[i].ID]; ok {
					items[i].Delivery = status
				}
			}
		}
	}
	filtered := make([]protocol.WaitListItem, 0, len(items))
	for _, item := range items {
		delivery, processing := "unknown", "unknown"
		if item.Delivery.Known {
			if item.Delivery.Enqueued {
				delivery = item.Delivery.DeliveryStatus
				processing = item.Delivery.ProcessingStatus
			} else {
				delivery = "not_enqueued"
			}
		}
		if matchesFilter(q.Get("state"), item.State) && matchesFilter(q.Get("delivery_status"), delivery) && matchesFilter(q.Get("processing_status"), processing) {
			filtered = append(filtered, item)
		}
	}
	// Content addressing survives restart, covers receipts and rejects pagination
	// across mutations. Observation timestamps are deliberately outside the hash.
	data, _ := json.Marshal(struct {
		Scope protocol.WaitAddress
		Query string
		Items []protocol.WaitListItem
	}{scope, q.Get("workspace") + "|" + q.Get("session") + "|" + q.Get("task") + "|" + q.Get("state") + "|" + q.Get("delivery_status") + "|" + q.Get("processing_status"), filtered})
	sum := sha256.Sum256(data)
	version := hex.EncodeToString(sum[:])
	after := ""
	if cursor := q.Get("cursor"); cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		parts := strings.SplitN(string(raw), ":", 2)
		if err != nil || len(parts) != 2 {
			http.Error(w, "invalid cursor", 400)
			return
		}
		if parts[0] != version {
			http.Error(w, "snapshot changed; restart pagination", 409)
			return
		}
		after = parts[1]
	}
	start := sort.Search(len(filtered), func(i int) bool { return filtered[i].ID > after })
	end := min(start+limit, len(filtered))
	response := protocol.WaitList{Items: filtered[start:end], SnapshotVersion: version, ObservedAt: time.Now().UTC(), DeliveryObservedAt: deliveryObservedAt}
	if end < len(filtered) {
		response.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(version + ":" + filtered[end-1].ID))
	}
	writeJSON(w, response)
}

func validFilter(value, allowed string) bool {
	if value == "" {
		return true
	}
	for _, part := range strings.Split(value, ",") {
		if !strings.Contains(","+allowed+",", ","+part+",") || part == "" {
			return false
		}
	}
	return true
}
func matchesFilter(filter, value string) bool {
	return filter == "" || strings.Contains(","+filter+",", ","+value+",")
}
