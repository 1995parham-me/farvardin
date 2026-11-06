package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/dav"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

func backend(t *testing.T) (Backend, *store.Store) {
	t.Helper()

	s, err := store.Open(t.TempDir(), "user", store.Author{Name: "T", Email: "t@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.EnsureDefaults(); err != nil {
		t.Fatal(err)
	}

	d := dav.New(s, dav.Options{ //nolint:exhaustruct // defaults
		Photo: contact.PhotoPolicy{MaxSize: 0, Guard: false, Stabilize: false, ThumbnailSize: 0, Quality: 0},
	}, slog.New(slog.DiscardHandler), nil)

	return Backend{Store: s, Parse: d.Cache(), Full: d.Full(), WriteCard: d.WriteCard, WriteEvent: d.WriteEvent, Delete: d.DeleteObject}, s
}

func call[T any](t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (T, bool, string) {
	t.Helper()

	var out T

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args}) //nolint:exhaustruct // name and arguments
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}

	text := ""
	var textSb49 strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			textSb49.WriteString(tc.Text)
		}
	}
	text += textSb49.String()

	if res.IsError {
		return out, true, text
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: decoding %s: %v", name, raw, err)
	}

	return out, false, text
}

func connect(t *testing.T, b Backend) *mcp.ClientSession {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "farvardin", Title: "", Version: "test"}, nil) //nolint:exhaustruct // test
	tools{b: b}.register(server)

	st, ct := mcp.NewInMemoryTransports()

	if _, err := server.Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Title: "", Version: "1"}, nil).Connect(t.Context(), ct, nil) //nolint:exhaustruct // test
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

func TestContactTools(t *testing.T) {
	t.Parallel()

	b, s := backend(t)
	cs := connect(t, b)

	created, isErr, msg := call[ContactDetail](t, cs, "create_contact", map[string]any{
		"given_name": "Sara", "family_name": "Ahmadi", "organization": "Nobitex", "birthday": "--03-16",
		"phones": []map[string]string{{"value": "+98 912 000 0000", "type": "mobile"}, {"value": "+34 600 000 000", "type": "Spain"}},
		"emails": []map[string]string{{"value": "sara@example.com", "type": "work"}},
	})
	if isErr {
		t.Fatalf("create: %s", msg)
	}

	if created.Name != "Sara Ahmadi" || created.Birthday != "--03-16" || len(created.PhonesFull) != 2 || created.PhonesFull[1].Type != "Spain" {
		t.Fatalf("created: %+v", created)
	}

	found, _, _ := call[SearchOut](t, cs, "search_contacts", map[string]any{"query": "9120000"})
	if found.Total != 1 || found.Contacts[0].ID != created.ID {
		t.Fatalf("search by phone digits: %+v", found)
	}

	// A stale etag is refused, so a phone's newer edit is never overwritten.
	if _, isErr, _ := call[ContactDetail](t, cs, "update_contact", map[string]any{"id": created.ID, "etag": "0000", "title": "x"}); !isErr {
		t.Fatal("update with a stale etag must fail")
	}

	updated, isErr, msg := call[ContactDetail](t, cs, "update_contact", map[string]any{"id": created.ID, "etag": created.ETag, "title": "Engineer", "note": "met at a meetup"})
	if isErr || updated.Title != "Engineer" || updated.Note != "met at a meetup" || len(updated.PhonesFull) != 2 {
		t.Fatalf("update: %s %+v", msg, updated)
	}

	got, _, _ := call[ContactDetail](t, cs, "get_contact", map[string]any{"id": created.ID, "format": "vcard"})
	if !strings.Contains(got.Data, "TITLE:Engineer") || !strings.Contains(got.Data, "X-ABLabel:Spain") {
		t.Fatalf("vcard:\n%s", got.Data)
	}

	hist, _, _ := call[HistoryOut](t, cs, "contact_history", map[string]any{"id": created.ID})
	if len(hist.Revisions) != 2 || hist.Revisions[0].Device != "MCP · mcp" {
		t.Fatalf("history: %+v", hist)
	}

	book, _ := s.Collection(store.AddressBook, "addressbook")
	if revs, _ := book.History(created.ID, 1); !strings.Contains(revs[0].Requester, "farvardin MCP") {
		t.Fatalf("requester %q", revs[0].Requester)
	}

	bdays, _, _ := call[BirthdaysOut](t, cs, "upcoming_birthdays", map[string]any{"days": 400})
	if len(bdays.Upcoming) != 1 || bdays.Upcoming[0].Name != "Sara Ahmadi" {
		t.Fatalf("birthdays: %+v", bdays)
	}

	done, isErr, msg := call[Done](t, cs, "delete_contact", map[string]any{"id": created.ID, "etag": updated.ETag})
	if isErr || !done.OK {
		t.Fatalf("delete: %s", msg)
	}

	if found, _, _ := call[SearchOut](t, cs, "search_contacts", map[string]any{"query": "Sara"}); found.Total != 0 {
		t.Fatal("deleted contact still found")
	}
}

func TestEventTools(t *testing.T) {
	t.Parallel()

	b, _ := backend(t)
	cs := connect(t, b)

	ev, isErr, msg := call[EventDetail](t, cs, "create_event", map[string]any{
		"summary": "Dentist", "start": "2030-01-10T10:00:00Z", "location": "Tehran",
	})
	if isErr || ev.Title != "Dentist" || !strings.Contains(ev.Data, "DTSTART:20300110T100000Z") || !strings.Contains(ev.Data, "DTEND:20300110T110000Z") {
		t.Fatalf("create: %s %+v", msg, ev)
	}

	list, _, _ := call[EventsOut](t, cs, "list_events", map[string]any{"from": "2030-01-01", "to": "2030-02-01"})
	if len(list.Events) != 1 || list.Events[0].ID != ev.ID {
		t.Fatalf("list: %+v", list)
	}

	upd, isErr, msg := call[EventDetail](t, cs, "update_event", map[string]any{"id": ev.ID, "etag": ev.ETag, "summary": "Dentist (moved)", "start": "2030-01-11", "end": "2030-01-12"})
	if isErr || upd.Title != "Dentist (moved)" || !upd.AllDay || !strings.Contains(upd.Data, "SEQUENCE:1") {
		t.Fatalf("update: %s %+v", msg, upd)
	}

	if _, isErr, _ := call[Done](t, cs, "delete_event", map[string]any{"id": ev.ID, "etag": ev.ETag}); !isErr {
		t.Fatal("delete with the pre-update etag must fail")
	}

	if done, isErr, msg := call[Done](t, cs, "delete_event", map[string]any{"id": ev.ID, "etag": upd.ETag}); isErr || !done.OK {
		t.Fatalf("delete: %s", msg)
	}
}

func TestHandlerNeedsToken(t *testing.T) {
	t.Parallel()

	b, _ := backend(t)
	token, hash := NewToken()

	srv := httptest.NewServer(Handler(b, map[string]string{"claude": hash}))
	t.Cleanup(srv.Close)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`

	for _, tc := range []struct {
		auth string
		want int
	}{{"", http.StatusUnauthorized}, {"Bearer fv_wrong", http.StatusUnauthorized}, {"Bearer " + token, http.StatusOK}} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader(body))
		// As behind nginx: the public name, on a loopback listener.
		req.Host = "davx.example.com"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")

		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		_ = resp.Body.Close()

		if resp.StatusCode != tc.want {
			t.Fatalf("auth %q: status %d, want %d", tc.auth, resp.StatusCode, tc.want)
		}
	}
}

func TestPhoneTools(t *testing.T) {
	t.Parallel()

	b, s := backend(t)
	cs := connect(t, b)

	book, _ := s.Collection(store.AddressBook, "addressbook")

	const vcard = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:n\r\nFN:Nima\r\nN:;Nima;;;\r\nTEL;type=CELL:0912 345 6789\r\nEND:VCARD\r\n"
	if _, _, err := book.Put("n.vcf", []byte(vcard), store.Condition{}, "test"); err != nil {
		t.Fatal(err)
	}

	// Without a region the national number cannot be fixed.
	if _, isErr, msg := call[FixPhonesOut](t, cs, "fix_contact_phones", map[string]any{"id": "n.vcf"}); !isErr || !strings.Contains(msg, "phone_region") {
		t.Fatalf("fix without region: %v %s", isErr, msg)
	}

	b.PhoneRegion = "IR"
	cs = connect(t, b)

	lint, _, _ := call[LintOut](t, cs, "lint_contacts", map[string]any{"warnings": true})
	if lint.FixableCount != 1 || len(lint.Findings) != 1 || !strings.Contains(lint.Findings[0].Issues[0], "(fix: +989123456789)") {
		t.Fatalf("lint: %+v", lint)
	}

	found, _, _ := call[SearchOut](t, cs, "search_contacts", map[string]any{"query": "+98 912 345"})
	if found.Total != 1 {
		t.Fatalf("search by international form: %+v", found)
	}

	fixed, isErr, msg := call[FixPhonesOut](t, cs, "fix_contact_phones", map[string]any{"id": "n.vcf"})
	if isErr || len(fixed.Fixed) != 1 || fixed.Phones[0] != "+989123456789" || len(fixed.Issues) != 0 {
		t.Fatalf("fix: %s %+v", msg, fixed)
	}

	// Numbers the assistant writes are stored in E.164.
	created, isErr, msg := call[ContactDetail](t, cs, "create_contact", map[string]any{
		"full_name": "Shirin", "phones": []map[string]string{{"value": "0935 111 2233", "type": "mobile"}, {"value": "*141#"}},
	})
	if isErr || created.Phones[0] != "+989351112233" || created.Phones[1] != "*141#" {
		t.Fatalf("create: %s %+v", msg, created)
	}
}
