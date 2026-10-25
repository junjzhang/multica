// Package delivery is the transport-neutral core for durable agent delivery.
//
// It models a delivery request, its idempotency boundary and its receipt
// state. It deliberately does not import Multica's issue, comment, task or
// agent types: an adapter resolves business meaning (who may be addressed,
// which comment triggered the request) and hands the core stable identifiers.
// Issue and comment references are optional so background events that are not
// tied to an issue can use the same durable path.
package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound reports that no delivery exists in the requested scope.
var ErrNotFound = errors.New("delivery not found")

// ErrConflict reports a reused idempotency key whose immutable content differs
// from the stored delivery. The caller must not create a second request.
var ErrConflict = errors.New("delivery idempotency key already used with different content")

// Scope isolates deliveries by workspace. A delivery ID is unique globally,
// but every lookup is scoped so one workspace can never address another's row.
type Scope struct {
	WorkspaceID string
}

// Source identifies what produced the request. Type+Key is the idempotency
// key inside one scope and recipient; ActorType/ActorID/SessionID describe the
// sender when the source is a session and are part of the immutable content.
type Source struct {
	Type      string
	Key       string
	ActorType string
	ActorID   string
	SessionID string
}

// Recipient is one authorized delivery target. Version one supports agents;
// the type is stored so a later host adapter can add recipient kinds without a
// second storage path.
type Recipient struct {
	Type string
	ID   string
}

// Refs optionally links a delivery to the business objects that own the real
// conversation. The core treats them as opaque stable IDs.
//
// CommentID is the legacy comment projection and shares the table's
// (recipient, comment) unique key, so it is only valid for a "comment" source.
// A generic producer keeps any comment reference inside Payload instead: a
// generic row occupying that key would make a later comment delivery look like
// a replay of an unrelated request.
type Refs struct {
	IssueID   string
	CommentID string
}

// EnqueueRequest is the complete, immutable request stored by a Store.
type EnqueueRequest struct {
	Scope     Scope
	Source    Source
	Recipient Recipient
	Kind      string
	Payload   json.RawMessage
	Refs      Refs
}

// Delivery is a stored request plus its delivery and processing receipts.
// DeliveryStatus and ProcessingStatus never imply that the recipient did the
// requested work.
type Delivery struct {
	ID               string
	Scope            Scope
	Source           Source
	Recipient        Recipient
	Kind             string
	Payload          json.RawMessage
	Refs             Refs
	DeliveryStatus   string
	ProcessingStatus string
	Attempts         int32
	HostReceipt      string
	LastError        string
	CreatedAt        time.Time
	DeliveredAt      *time.Time
}

// Validate rejects a request the core cannot persist as an immutable delivery.
func (r EnqueueRequest) Validate() error {
	if strings.TrimSpace(r.Scope.WorkspaceID) == "" {
		return errors.New("delivery: workspace id is required")
	}
	if strings.TrimSpace(r.Source.Type) == "" {
		return errors.New("delivery: source type is required")
	}
	if strings.TrimSpace(r.Source.Key) == "" {
		return errors.New("delivery: source key is required")
	}
	if strings.TrimSpace(r.Recipient.Type) == "" {
		return errors.New("delivery: recipient type is required")
	}
	if strings.TrimSpace(r.Recipient.ID) == "" {
		return errors.New("delivery: recipient id is required")
	}
	if strings.TrimSpace(r.Kind) == "" {
		return errors.New("delivery: kind is required")
	}
	if r.Refs.CommentID != "" && r.Source.Type != "comment" {
		return fmt.Errorf("delivery: comment ref %s requires source type \"comment\"", r.Refs.CommentID)
	}
	if len(bytes.TrimSpace(r.Payload)) == 0 {
		return errors.New("delivery: payload is required")
	}
	var value any
	if err := json.Unmarshal(r.Payload, &value); err != nil {
		return fmt.Errorf("delivery: invalid payload: %w", err)
	}
	if value == nil {
		return errors.New("delivery: payload must not be null")
	}
	return nil
}

// Store persists deliveries. Enqueue must be atomic: either the delivery and
// its idempotency record exist, or neither does. Implementations return
// ErrConflict when the key is reused with different content.
type Store interface {
	Enqueue(ctx context.Context, req EnqueueRequest) (Delivery, error)
	Get(ctx context.Context, scope Scope, id string) (Delivery, error)
}

// sameContent reports whether an existing delivery holds exactly what req asks
// to enqueue. A legacy comment row may predate immutable payload snapshots; for
// those, matching kind and references are the available identity.
func (r EnqueueRequest) sameContent(existing Delivery) bool {
	if existing.Kind != r.Kind {
		return false
	}
	if existing.Refs != r.Refs {
		return false
	}
	if existing.Source.ActorType != r.Source.ActorType ||
		existing.Source.ActorID != r.Source.ActorID ||
		existing.Source.SessionID != r.Source.SessionID {
		return false
	}
	if len(existing.Payload) == 0 {
		return true
	}
	requestPayload, err := canonicalJSON(r.Payload)
	if err != nil {
		return false
	}
	existingPayload, err := canonicalJSON(existing.Payload)
	if err != nil {
		return false
	}
	return bytes.Equal(requestPayload, existingPayload)
}

// canonicalJSON normalizes key order and number spelling so logically equal
// payloads compare equal without depending on the producer's encoder.
func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
