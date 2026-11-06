package dav

import (
	"encoding/xml"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/1995parham-me/farvardin/internal/infra/push"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

// pushCollection returns the git collection behind a resource when push
// is on; generated collections (birthdays) have no subscriptions.
func (h *Handler) pushCollection(res *resource) *store.Collection {
	if h.opts.Push == nil || res == nil || res.kind != resCollection {
		return nil
	}

	c, _ := res.col.(*store.Collection)

	return c
}

func (h *Handler) pushProp(res *resource, n xml.Name) *elem {
	col := h.pushCollection(res)
	if col == nil {
		return nil
	}

	switch n.Local {
	case "transports":
		key := el(NSPush, "vapid-public-key").withAttr("type", "p256ecdsa")
		key.text = h.opts.Push.PublicKey()

		return el(NSPush, "transports", el(NSPush, "web-push", key))
	case "topic":
		topic, err := h.opts.Push.Topic(col.Dir())
		if err != nil {
			return nil
		}

		return textEl(NSPush, "topic", topic)
	case "supported-triggers":
		return el(NSPush, "supported-triggers",
			el(NSPush, "content-update", textEl(NSDAV, "depth", "1")),
			el(NSPush, "property-update", textEl(NSDAV, "depth", "0")),
		)
	}

	return nil
}

func (h *Handler) change(r *http.Request) push.Change {
	skipAll, skip := push.ParseDontNotify(r.Header.Get("Push-Dont-Notify"))

	return push.Change{SyncToken: "", Properties: nil, SkipAll: skipAll, Skip: skip}
}

func (h *Handler) notifyContent(r *http.Request, col collection) {
	c, ok := col.(*store.Collection)
	if h.opts.Push == nil || !ok {
		return
	}

	snap, err := c.Snapshot()
	if err != nil {
		return
	}

	ch := h.change(r)
	ch.SyncToken = syncToken(snap)
	h.opts.Push.Notify(c.Dir(), ch)
}

func (h *Handler) notifyProperties(r *http.Request, col collection, names []string) {
	c, ok := col.(*store.Collection)
	if h.opts.Push == nil || !ok || len(names) == 0 {
		return
	}

	ch := h.change(r)
	ch.Properties = names
	h.opts.Push.Notify(c.Dir(), ch)
}

// servePost handles POST <collection> with a push-register body.
func (h *Handler) servePost(w http.ResponseWriter, r *http.Request) {
	res, err := h.resolve(r.URL.Path)
	if err != nil {
		h.fail(w, r, err)

		return
	}

	body, err := readXML(r)
	if err != nil || body == nil || !body.is(NSPush, "push-register") {
		http.Error(w, "POST takes a WebDAV-Push push-register body", http.StatusBadRequest)

		return
	}

	col := h.pushCollection(res)
	if col == nil {
		writeError(w, http.StatusForbidden, el(NSPush, "push-not-available"), "WebDAV-Push is not available on this resource")

		return
	}

	sub, err := parseRegister(body)
	if err != nil {
		pre := "invalid-subscription"
		if errors.Is(err, push.ErrNoTrigger) {
			pre = "no-supported-trigger"
		}

		writeError(w, http.StatusForbidden, el(NSPush, pre), err.Error())

		return
	}

	sub, created, err := h.opts.Push.Register(col.Dir(), h.collectionHref(res.home, col.Name()), sub)
	if err != nil {
		h.fail(w, r, err)

		return
	}

	h.log.Info("push: subscription registered", "collection", col.Name(), "id", sub.ID, "new", created, "agent", r.UserAgent())

	w.Header().Set("Location", h.opts.Prefix+"/"+push.Route+"/"+sub.ID)

	if exp, err := time.Parse(time.RFC3339, deref(sub.Expires)); err == nil {
		w.Header().Set("Expires", exp.UTC().Format(http.TimeFormat))
	}

	if created {
		w.WriteHeader(http.StatusCreated)

		return
	}

	w.WriteHeader(http.StatusOK)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

func parseRegister(body *node) (push.Subscription, error) {
	sub := push.Subscription{AuthSecret: "", ContentEncoding: "aes128gcm", Expires: nil, ID: "", P256DH: "", PushResource: "", Triggers: nil}

	s := body.child(NSPush, "subscription")
	if s == nil {
		return sub, errors.Join(push.ErrInvalid, errors.New("missing <subscription>")) //nolint:err113 // detail for the client
	}

	wp := s.child(NSPush, "web-push-subscription")
	if wp == nil {
		return sub, errors.Join(push.ErrInvalid, errors.New("only web-push subscriptions are supported")) //nolint:err113 // detail for the client
	}

	text := func(n *node, local string) string {
		if c := n.child(NSPush, local); c != nil {
			return strings.TrimSpace(c.Text)
		}

		return ""
	}

	sub.PushResource = text(wp, "push-resource")
	if enc := text(wp, "content-encoding"); enc != "" {
		sub.ContentEncoding = enc
	}

	if sub.ContentEncoding != "aes128gcm" {
		return sub, errors.Join(push.ErrInvalid, errors.New("only aes128gcm is supported")) //nolint:err113 // detail for the client
	}

	if pk := wp.child(NSPush, "subscription-public-key"); pk != nil && (pk.attr("type") == "" || pk.attr("type") == "p256dh") {
		sub.P256DH = strings.TrimSpace(pk.Text)
	}

	sub.AuthSecret = text(wp, "auth-secret")

	if sub.PushResource == "" || sub.P256DH == "" || sub.AuthSecret == "" {
		return sub, errors.Join(push.ErrInvalid, errors.New("push-resource, subscription-public-key and auth-secret are required")) //nolint:err113 // detail for the client
	}

	for _, v := range []string{sub.P256DH, sub.AuthSecret} {
		if _, err := push.ParseKey(v); err != nil {
			return sub, errors.Join(push.ErrInvalid, errors.New("keys must be base64url")) //nolint:err113 // detail for the client
		}
	}

	if t := body.child(NSPush, "trigger"); t != nil {
		for _, c := range t.Children {
			depth := "0"
			if d := c.child(NSDAV, "depth"); d != nil {
				depth = strings.TrimSpace(d.Text)
			}

			switch {
			case c.is(NSPush, "content-update"):
				sub.Triggers = append(sub.Triggers, push.Trigger{Depth: depth, Kind: push.ContentUpdate, Props: []string{}})
			case c.is(NSPush, "property-update"):
				props := []string{}

				if p := c.child(NSDAV, "prop"); p != nil {
					for _, pp := range p.Children {
						props = append(props, "{"+pp.XMLName.Space+"}"+pp.XMLName.Local)
					}
				}

				sub.Triggers = append(sub.Triggers, push.Trigger{Depth: depth, Kind: push.PropertyUpdate, Props: props})
			}
		}
	}

	if len(sub.Triggers) == 0 {
		return sub, push.ErrNoTrigger
	}

	if e := body.child(NSPush, "expires"); e != nil && strings.TrimSpace(e.Text) != "" {
		t, err := http.ParseTime(strings.TrimSpace(e.Text))
		if err != nil {
			return sub, errors.Join(push.ErrInvalid, errors.New("expires is not an HTTP date")) //nolint:err113 // detail for the client
		}

		exp := t.UTC().Format("2006-01-02T15:04:05-07:00")
		sub.Expires = &exp
	}

	return sub, nil
}

// serveSubscription handles DELETE <prefix>/.subscriptions/<id>.
func (h *Handler) serveSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "only DELETE is allowed on a subscription", http.StatusMethodNotAllowed)

		return
	}

	id := strings.TrimPrefix(r.URL.Path, h.opts.Prefix+"/"+push.Route+"/")

	href, err := h.opts.Push.Owner(id)
	if err != nil {
		http.NotFound(w, r)

		return
	}

	dir := ""
	if res, err := h.resolve(href); err == nil {
		if c := h.pushCollection(res); c != nil {
			dir = c.Dir()
		}
	}

	if err := h.opts.Push.Remove(dir, id); err != nil {
		http.NotFound(w, r)

		return
	}

	h.log.Info("push: subscription removed", "id", id, "agent", r.UserAgent())
	w.WriteHeader(http.StatusNoContent)
}
