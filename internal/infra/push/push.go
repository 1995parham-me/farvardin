// Package push implements WebDAV-Push (draft-bitfire-webdav-push) over
// Web Push: RFC 8030 delivery, RFC 8291 message encryption and RFC 8292
// VAPID.
//
// Subscriptions are kept in the files and format Xandikos uses
// (push-subscriptions.json beside each collection, a server-wide
// push-subscription-index.json, the VAPID key as PEM), so a store moves
// between the two servers with its subscriptions intact and the phones
// never notice.
package push

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Namespace is the WebDAV-Push XML namespace.
const Namespace = "https://bitfire.at/webdav-push"

// File names, shared with Xandikos.
const (
	SubscriptionsFile = "push-subscriptions.json"
	IndexFile         = "push-subscription-index.json"
	// Route is the path segment under which registrations live.
	Route = ".subscriptions"
)

// DefaultExpiry is used when the client does not ask for one.
const DefaultExpiry = 30 * 24 * time.Hour

// Trigger kinds.
const (
	ContentUpdate  = "content-update"
	PropertyUpdate = "property-update"
)

// Errors returned by Register.
var (
	ErrInvalid   = errors.New("invalid subscription")
	ErrNoTrigger = errors.New("no supported trigger requested")
	ErrUnknown   = errors.New("unknown subscription")
)

// Trigger is one requested notification kind. Fields are in key order so
// the JSON matches Python's sort_keys output byte for byte.
type Trigger struct {
	Depth string   `json:"depth"`
	Kind  string   `json:"kind"`
	Props []string `json:"props"`
}

// Subscription is one registered Web Push subscription.
type Subscription struct {
	AuthSecret      string    `json:"auth_secret"`
	ContentEncoding string    `json:"content_encoding"`
	Expires         *string   `json:"expires"`
	ID              string    `json:"id"`
	P256DH          string    `json:"p256dh"`
	PushResource    string    `json:"push_resource"`
	Triggers        []Trigger `json:"triggers"`
}

// ExpiresAt parses Expires; zero means never.
func (s Subscription) ExpiresAt() time.Time {
	if s.Expires == nil {
		return time.Time{}
	}

	t, err := time.Parse(time.RFC3339, *s.Expires)
	if err != nil {
		return time.Time{}
	}

	return t
}

type collectionFile struct {
	Subscriptions map[string]Subscription `json:"subscriptions"`
	Topic         *string                 `json:"topic"`
}

// Service registers subscriptions and sends notifications.
type Service struct {
	keys     *Keys
	stateDir string
	log      *slog.Logger
	client   *http.Client

	mu sync.Mutex // guards every subscription file
}

// New returns a service that signs with keys and keeps its index in
// stateDir.
func New(keys *Keys, stateDir string, log *slog.Logger) *Service {
	//nolint:exhaustruct // a client with a timeout is all that is needed
	client := &http.Client{Timeout: 20 * time.Second}

	return &Service{keys: keys, stateDir: stateDir, log: log, client: client, mu: sync.Mutex{}}
}

// PublicKey is the VAPID public key for the transports property.
func (s *Service) PublicKey() string { return s.keys.Public }

func token() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)

	return b64(b)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path) //nolint:gosec // paths are built from the store root
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("push: reading %s: %w", path, err)
	}

	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("push: decoding %s: %w", path, err)
	}

	return nil
}

// writeJSON writes atomically with mode 0600, indented like Xandikos.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("push: encoding %s: %w", path, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".push-subscriptions.*.tmp")
	if err != nil {
		return fmt.Errorf("push: writing %s: %w", path, err)
	}

	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("push: writing %s: %w", path, err)
	}

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("push: writing %s: %w", path, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("push: writing %s: %w", path, err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("push: writing %s: %w", path, err)
	}

	return nil
}

func (s *Service) load(dir string) (collectionFile, error) {
	f := collectionFile{Subscriptions: map[string]Subscription{}, Topic: nil}
	if err := readJSON(filepath.Join(dir, SubscriptionsFile), &f); err != nil {
		return f, err
	}

	if f.Subscriptions == nil {
		f.Subscriptions = map[string]Subscription{}
	}

	return f, nil
}

// Topic returns the collection's stable topic, creating it once.
func (s *Service) Topic(dir string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.load(dir)
	if err != nil {
		return "", err
	}

	if f.Topic != nil && *f.Topic != "" {
		return *f.Topic, nil
	}

	t := token()
	f.Topic = &t

	return t, writeJSON(filepath.Join(dir, SubscriptionsFile), f)
}

// Register stores (or renews) a subscription for the collection in dir,
// whose URL path is href. It returns the subscription and whether it was
// new.
func (s *Service) Register(dir, href string, sub Subscription) (Subscription, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.load(dir)
	if err != nil {
		return sub, false, err
	}

	created := true
	sub.ID = token()

	for id, existing := range f.Subscriptions {
		if existing.PushResource == sub.PushResource {
			sub.ID, created = id, false
		}
	}

	if sub.Expires == nil {
		exp := time.Now().UTC().Truncate(time.Second).Add(DefaultExpiry).Format("2006-01-02T15:04:05-07:00")
		sub.Expires = &exp
	}

	f.Subscriptions[sub.ID] = sub
	if f.Topic == nil {
		t := token()
		f.Topic = &t
	}

	if err := writeJSON(filepath.Join(dir, SubscriptionsFile), f); err != nil {
		return sub, false, err
	}

	index := map[string]string{}
	if err := readJSON(filepath.Join(s.stateDir, IndexFile), &index); err != nil {
		return sub, false, err
	}

	index[sub.ID] = href

	return sub, created, writeJSON(filepath.Join(s.stateDir, IndexFile), index)
}

// Owner returns the collection path that holds a subscription id.
func (s *Service) Owner(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	index := map[string]string{}
	if err := readJSON(filepath.Join(s.stateDir, IndexFile), &index); err != nil {
		return "", err
	}

	href, ok := index[id]
	if !ok {
		return "", ErrUnknown
	}

	return href, nil
}

// Remove deletes a subscription from the collection in dir and the index.
func (s *Service) Remove(dir, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.removeLocked(dir, id)
}

func (s *Service) removeLocked(dir, id string) error {
	index := map[string]string{}
	if err := readJSON(filepath.Join(s.stateDir, IndexFile), &index); err == nil {
		if _, ok := index[id]; ok {
			delete(index, id)

			if err := writeJSON(filepath.Join(s.stateDir, IndexFile), index); err != nil {
				return err
			}
		}
	}

	if dir == "" {
		return nil
	}

	f, err := s.load(dir)
	if err != nil {
		return err
	}

	if _, ok := f.Subscriptions[id]; !ok {
		return ErrUnknown
	}

	delete(f.Subscriptions, id)

	return writeJSON(filepath.Join(dir, SubscriptionsFile), f)
}

// ParseDontNotify reads the Push-Dont-Notify header: "*" or a list of
// registration URLs or ids.
func ParseDontNotify(header string) (bool, map[string]bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return false, nil
	}

	if header == "*" {
		return true, nil
	}

	ids := map[string]bool{}

	for part := range strings.SplitSeq(header, ",") {
		part = strings.Trim(strings.TrimSpace(part), `"`)
		if part == "" {
			continue
		}

		ids[part[strings.LastIndexByte(part, '/')+1:]] = true
	}

	return false, ids
}

// Change describes what happened to a collection.
type Change struct {
	// SyncToken is set for a content change.
	SyncToken string
	// Properties are the changed property names ("{ns}local") for a
	// property change.
	Properties []string
	// SkipAll and Skip come from Push-Dont-Notify.
	SkipAll bool
	Skip    map[string]bool
}

// Notify sends a push message to every subscriber of the collection in
// dir that asked for this kind of change. It runs in the background and
// never fails the request that caused it.
func (s *Service) Notify(dir string, c Change) {
	if c.SkipAll {
		return
	}

	go s.notify(dir, c)
}

func (s *Service) notify(dir string, c Change) {
	s.mu.Lock()
	f, err := s.load(dir)
	s.mu.Unlock()

	if err != nil || len(f.Subscriptions) == 0 {
		return
	}

	topic := ""
	if f.Topic != nil {
		topic = *f.Topic
	}

	payload := Message(topic, c)
	now := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	for id, sub := range f.Subscriptions {
		if exp := sub.ExpiresAt(); !exp.IsZero() && !exp.After(now) {
			s.log.Info("push: removing expired subscription", "id", id)
			_ = s.Remove(dir, id)

			continue
		}

		if c.Skip[id] || !wants(sub, c) {
			continue
		}

		status, err := s.keys.send(ctx, s.client, sub, payload, topic)
		if err != nil {
			s.log.Warn("push: delivery failed", "id", id, "error", err)

			continue
		}

		switch {
		case status == http.StatusNotFound || status == http.StatusGone:
			s.log.Info("push: the push service dropped the subscription; removing it", "id", id, "status", status)
			_ = s.Remove(dir, id)
		case status >= 300:
			s.log.Warn("push: push service refused the message", "id", id, "status", status)
		default:
			s.log.Debug("push: delivered", "id", id, "status", status)
		}
	}
}

func wants(sub Subscription, c Change) bool {
	for _, t := range sub.Triggers {
		if c.SyncToken != "" && t.Kind == ContentUpdate {
			return true
		}

		if len(c.Properties) > 0 && t.Kind == PropertyUpdate {
			if len(t.Props) == 0 {
				return true
			}

			for _, p := range t.Props {
				if slices.Contains(c.Properties, p) {
					return true
				}
			}
		}
	}

	return false
}

// Message builds the <push-message> body.
func Message(topic string, c Change) []byte {
	var b strings.Builder

	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<P:push-message xmlns:D="DAV:" xmlns:P="` + Namespace + `"><P:topic>` + xmlEscape(topic) + `</P:topic>`)

	if c.SyncToken != "" {
		b.WriteString(`<P:content-update><D:sync-token>` + xmlEscape(c.SyncToken) + `</D:sync-token></P:content-update>`)
	}

	if len(c.Properties) > 0 && c.SyncToken == "" {
		b.WriteString(`<P:property-update><D:prop>`)

		for i, p := range c.Properties {
			ns, local := splitClark(p)
			fmt.Fprintf(&b, `<x%d:%s xmlns:x%d="%s"/>`, i, local, i, xmlEscape(ns))
		}

		b.WriteString(`</D:prop></P:property-update>`)
	}

	b.WriteString(`</P:push-message>`)

	return []byte(b.String())
}

// splitClark splits "{ns}local".
func splitClark(s string) (string, string) {
	if strings.HasPrefix(s, "{") {
		if i := strings.IndexByte(s, '}'); i > 0 {
			return s[1:i], s[i+1:]
		}
	}

	return "", s
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// ParseKey decodes a base64url subscription key or secret.
func ParseKey(s string) ([]byte, error) { return unb64(strings.TrimSpace(s)) }
