// Package mcpserver exposes the contacts and calendars over the Model
// Context Protocol, so an assistant can search, read, create, change and
// delete them. Every write goes through the server's validated write path
// (validation, UID uniqueness, If-Match, a git commit naming the token,
// WebDAV-Push to the phones), so it is checked like a phone's write and can
// be undone from git history.
package mcpserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/1995parham-me/farvardin/internal/domain/calendar"
	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/domain/vcf"
	"github.com/1995parham-me/farvardin/internal/infra/store"
	"github.com/1995parham-me/farvardin/internal/infra/web"
)

// Errors returned to the client as tool errors.
var (
	ErrNotFound  = errors.New("not found")
	ErrBadInput  = errors.New("bad input")
	ErrNoBook    = errors.New("no address book")
	ErrNoCal     = errors.New("no calendar")
	errReadEvent = errors.New("event does not parse")
)

// Backend is what the tools need from the rest of the server.
type Backend struct {
	Store *store.Store
	// Parse returns cached (slim) objects; Full parses with photo bytes.
	Parse web.Parser
	Full  web.Parser
	// WriteCard and WriteEvent store through the validated write path;
	// ifMatch "" creates.
	WriteCard  func(book, name string, data []byte, ifMatch, requester string) (string, error)
	WriteEvent func(cal, name string, data []byte, ifMatch string, create bool, requester string) (string, error)
	Delete     func(kind store.Kind, col, name, ifMatch, requester string) error
	// PhoneRegion is the country of numbers written without a country
	// code: lint suggests their E.164 form, written phones are stored in
	// it, and search matches either form.
	PhoneRegion []string
}

// Version is reported to clients.
var Version = "dev"

// Handler returns the Streamable HTTP endpoint, guarded by bearer tokens.
// tokens maps a token name (written into commits) to the SHA-256 hex of
// the token.
func Handler(b Backend, tokens map[string]string) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "farvardin", Title: "farvardin contacts and calendars", Version: Version}, &mcp.ServerOptions{ //nolint:exhaustruct // defaults
		Instructions: "Contacts (vCards) and calendars (iCalendar) of one person, kept in git and synced to their phones. " +
			"Search before you create, to avoid duplicates. Writes are pushed to the phones at once and every change is a git commit, " +
			"so pass the etag you read to update or delete, and prefer field updates over raw vCards. " +
			"The owner's time zone is " + time.Local.String() + ": times are shown in it, and a time without an offset is read in it.", //nolint:gosmopolitan // the configured zone
	})

	t := tools{b: b}
	t.register(server)

	// Stateless: no sessions to keep. The SDK's DNS-rebinding guard only
	// lets localhost Host headers through when listening on loopback, but
	// farvardin sits behind a reverse proxy that passes the public name; a
	// rebinding page could reach the port but never present the bearer
	// token every request needs, so the guard adds nothing here.
	//nolint:exhaustruct // the two options that matter
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, DisableLocalhostProtection: true})

	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		sum := sha256.Sum256([]byte(token))
		got := hex.EncodeToString(sum[:])

		for name, want := range tokens {
			if subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(want))) == 1 {
				return &auth.TokenInfo{Scopes: nil, Expiration: time.Now().Add(time.Hour), UserID: name, Extra: nil}, nil
			}
		}

		return nil, auth.ErrInvalidToken
	}

	return auth.RequireBearerToken(verify, nil)(h)
}

// NewToken returns a random token and the SHA-256 hex to put in the config.
func NewToken() (string, string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)

	token := "fv_" + hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))

	return token, hex.EncodeToString(sum[:])
}

type tools struct{ b Backend }

func requester(req *mcp.CallToolRequest) string {
	name := "mcp"
	if req != nil && req.Extra != nil && req.Extra.TokenInfo != nil && req.Extra.TokenInfo.UserID != "" {
		name = req.Extra.TokenInfo.UserID
	}

	return "farvardin MCP (" + name + ")"
}

func readOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: nil, IdempotentHint: true, OpenWorldHint: new(false), Title: ""}
}

func writes(destructive bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: new(destructive), IdempotentHint: false, OpenWorldHint: new(false), Title: ""}
}

func (t tools) register(s *mcp.Server) {
	tool := func(name, desc string, ann *mcp.ToolAnnotations) *mcp.Tool {
		return &mcp.Tool{Name: name, Description: desc, Annotations: ann} //nolint:exhaustruct // schemas come from the Go types
	}

	mcp.AddTool(s, tool("search_contacts", "Find contacts by name, nickname, organization, email or phone (any part; phone digits match regardless of formatting). Returns summaries with ids.", readOnly()), t.searchContacts)
	mcp.AddTool(s, tool("get_contact", "Read one contact by id: every field, and optionally the raw vCard (3.0 as stored, or converted to 4.0) or JSContact.", readOnly()), t.getContact)
	mcp.AddTool(s, tool("create_contact", "Create a contact. Search first to avoid a duplicate.", writes(false)), t.createContact)
	mcp.AddTool(s, tool("update_contact", "Change fields of a contact. Only the fields you pass change; a list field you pass (phones, emails, urls) replaces the whole list. Pass the etag from get_contact to avoid overwriting a newer edit from a phone.", writes(false)), t.updateContact)
	mcp.AddTool(s, tool("delete_contact", "Delete a contact (it stays in git history). Pass the etag you read.", writes(true)), t.deleteContact)
	mcp.AddTool(s, tool("contact_history", "List the changes of a contact, newest first: when, which device, and the revision id of each.", readOnly()), t.contactHistory)
	mcp.AddTool(s, tool("restore_contact_photo", "Put back the photo a contact had at an earlier revision (from contact_history), for example a full photo a phone replaced with a thumbnail.", writes(false)), t.restorePhoto)
	mcp.AddTool(s, tool("fix_contact_phones", "Write a contact's phone numbers that have no country code in international (E.164) form, the fixes lint_contacts suggests. Labels and every other field stay as they are. Pass the etag you read.", writes(false)), t.fixPhones)
	t.registerTidy(s, tool)
	mcp.AddTool(s, tool("upcoming_birthdays", "Birthdays and anniversaries in the next N days, with ages where the year is known.", readOnly()), t.upcomingBirthdays)
	mcp.AddTool(s, tool("lint_contacts", "List contacts that fail validation (errors stop phones from saving them) or have warnings (phone numbers not in international form, repeated numbers, and more), and contacts with low-resolution photos. A warning with a fix can be applied with fix_contact_phones, or all at once with fix_all_contact_phones.", readOnly()), t.lintContacts)
	mcp.AddTool(s, tool("list_calendars", "List the calendars, including the generated read-only birthdays calendar.", readOnly()), t.listCalendars)
	mcp.AddTool(s, tool("list_events", "List events between two dates (default: the next 30 days), across all calendars or one. Repeating events are listed at their next occurrence in the range.", readOnly()), t.listEvents)
	mcp.AddTool(s, tool("get_event", "Read one event by calendar and id, with its raw iCalendar.", readOnly()), t.getEvent)
	mcp.AddTool(s, tool("create_event", "Create an event. Times are RFC 3339 (2026-11-20T18:30:00+03:30) or, for all-day events, dates (2026-11-20).", writes(false)), t.createEvent)
	mcp.AddTool(s, tool("update_event", "Change fields of an event; only the fields you pass change. Pass the etag you read.", writes(false)), t.updateEvent)
	mcp.AddTool(s, tool("delete_event", "Delete an event (it stays in git history). Pass the etag you read.", writes(true)), t.deleteEvent)
}

// ---- contacts ------------------------------------------------------------

// Labelled is a phone, email or link with its type or label.
type Labelled struct {
	Value string `json:"value"          jsonschema:"the phone number, email address or URL"`
	Type  string `json:"type,omitempty" jsonschema:"cell, home, work, main, fax, other, or a free label such as Vodafone (Spain)"`
}

// ContactSummary is a contact in a list.
type ContactSummary struct {
	ID           string   `json:"id"`
	Book         string   `json:"book"`
	Name         string   `json:"name"`
	Organization string   `json:"organization,omitempty"`
	Phones       []string `json:"phones,omitempty"`
	Emails       []string `json:"emails,omitempty"`
	Birthday     string   `json:"birthday,omitempty"`
	Photo        string   `json:"photo,omitempty"`
	ETag         string   `json:"etag"`
}

func (t tools) book(name string) (*store.Collection, error) {
	if name != "" {
		c, err := t.b.Store.Collection(store.AddressBook, name)
		if err != nil {
			return nil, fmt.Errorf("%w %q", ErrNoBook, name)
		}

		return c, nil
	}

	books := t.b.Store.Collections(store.AddressBook)
	if len(books) == 0 {
		return nil, ErrNoBook
	}

	return books[0], nil
}

func summarize(book string, id, etag string, c contact.Card) ContactSummary {
	s := ContactSummary{ID: id, Book: book, Name: c.DisplayName(), Organization: c.Organization(), Phones: nil, Emails: nil, Birthday: "", Photo: "", ETag: etag}

	for _, v := range c.Values("TEL") {
		s.Phones = append(s.Phones, v.Value)
	}

	for _, v := range c.Values("EMAIL") {
		s.Emails = append(s.Emails, v.Value)
	}

	for _, d := range c.Dates() {
		if d.Kind == contact.Birthday {
			s.Birthday = dateString(d)
		}
	}

	if p, ok := c.PhotoInfo(); ok {
		s.Photo = fmt.Sprintf("%dx%d", p.Width, p.Height)
	}

	return s
}

func dateString(d contact.Date) string {
	if d.Year > 0 {
		return fmt.Sprintf("%04d-%02d-%02d", d.Year, int(d.Month), d.Day)
	}

	return fmt.Sprintf("--%02d-%02d", int(d.Month), d.Day)
}

func digits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}

		return -1
	}, s)
}

// SearchIn is the input of search_contacts.
type SearchIn struct {
	Query string `json:"query"           jsonschema:"text to look for; empty lists everyone"`
	Limit int    `json:"limit,omitempty" jsonschema:"at most this many results (default 20)"`
	Book  string `json:"book,omitempty"  jsonschema:"address book name; default the first"`
}

// SearchOut lists contacts.
type SearchOut struct {
	Contacts []ContactSummary `json:"contacts"`
	Total    int              `json:"total"`
}

func (t tools) searchContacts(_ context.Context, _ *mcp.CallToolRequest, in SearchIn) (*mcp.CallToolResult, SearchOut, error) {
	book, err := t.book(in.Book)
	if err != nil {
		return nil, SearchOut{}, err
	}

	snap, err := book.Snapshot()
	if err != nil {
		return nil, SearchOut{}, fmt.Errorf("reading the address book: %w", err)
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}

	q := strings.ToLower(strings.TrimSpace(in.Query))
	qd := digits(q)
	out := SearchOut{Contacts: nil, Total: 0}

	for _, o := range snap.Objects {
		c, _, err := t.b.Parse(book, o.ETag)
		if err != nil || c == nil {
			continue
		}

		if q != "" {
			text := strings.ToLower(strings.Join([]string{c.DisplayName(), c.Value("NICKNAME"), c.Organization()}, " "))
			hit := strings.Contains(text, q)

			for _, v := range c.Values("EMAIL") {
				hit = hit || strings.Contains(strings.ToLower(v.Value), q)
			}

			for _, v := range c.Values("TEL") {
				for _, k := range contact.PhoneKeys(v.Value, t.b.PhoneRegion) {
					hit = hit || (len(qd) >= 4 && strings.Contains(k, qd))
				}
			}

			if !hit {
				continue
			}
		}

		out.Total++
		if len(out.Contacts) < limit {
			out.Contacts = append(out.Contacts, summarize(book.Name(), o.Name, o.ETag, *c))
		}
	}

	sort.Slice(out.Contacts, func(i, j int) bool {
		return strings.ToLower(out.Contacts[i].Name) < strings.ToLower(out.Contacts[j].Name)
	})

	return nil, out, nil
}

// GetContactIn is the input of get_contact.
type GetContactIn struct {
	ID     string `json:"id"               jsonschema:"the contact id from search_contacts"`
	Book   string `json:"book,omitempty"`
	Format string `json:"format,omitempty" jsonschema:"also return the data as: vcard (as stored), vcard4, or jscontact"`
}

// ContactDetail is a whole contact.
type ContactDetail struct {
	ContactSummary

	GivenName  string     `json:"given_name,omitempty"`
	FamilyName string     `json:"family_name,omitempty"`
	Nickname   string     `json:"nickname,omitempty"`
	Title      string     `json:"title,omitempty"`
	Note       string     `json:"note,omitempty"`
	PhonesFull []Labelled `json:"phones_full,omitempty"`
	EmailsFull []Labelled `json:"emails_full,omitempty"`
	URLs       []Labelled `json:"urls,omitempty"`
	Addresses  []Labelled `json:"addresses,omitempty"`
	Dates      []Labelled `json:"dates,omitempty"`
	Issues     []string   `json:"issues,omitempty"`
	Data       string     `json:"data,omitempty"`
}

func labelled(vals []contact.Value) []Labelled {
	out := make([]Labelled, 0, len(vals))

	for _, v := range vals {
		lbl := v.Label
		if lbl == "" {
			lbl = strings.Join(slices.DeleteFunc(slices.Clone(v.Types), func(s string) bool { return s == "internet" || s == "voice" || s == "pref" }), ",")
		}

		out = append(out, Labelled{Value: v.Value, Type: lbl})
	}

	return out
}

func (t tools) load(bookName, id string) (*store.Collection, store.Object, *contact.Card, error) {
	book, err := t.book(bookName)
	if err != nil {
		return nil, store.Object{}, nil, err
	}

	snap, err := book.Snapshot()
	if err != nil {
		return nil, store.Object{}, nil, fmt.Errorf("reading the address book: %w", err)
	}

	obj, ok := snap.Lookup(id)
	if !ok {
		return nil, store.Object{}, nil, fmt.Errorf("contact %q: %w", id, ErrNotFound)
	}

	c, _, err := t.b.Full(book, obj.ETag)
	if err != nil || c == nil {
		return nil, store.Object{}, nil, fmt.Errorf("contact %q does not parse: %w", id, err)
	}

	return book, obj, c, nil
}

func detail(book string, obj store.Object, c contact.Card, region []string) ContactDetail {
	n := c.Name()
	d := ContactDetail{
		ContactSummary: summarize(book, obj.Name, obj.ETag, c),
		GivenName:      n.Given, FamilyName: n.Family, Nickname: c.Value("NICKNAME"), Title: c.Value("TITLE"), Note: c.Value("NOTE"),
		PhonesFull: labelled(c.Values("TEL")), EmailsFull: labelled(c.Values("EMAIL")), URLs: labelled(c.Values("URL")),
		Addresses: labelled(c.Values("ADR")), Dates: nil, Issues: nil, Data: "",
	}

	for _, dt := range c.Dates() {
		kind := string(dt.Kind)
		if dt.Label != "" {
			kind = dt.Label
		}

		d.Dates = append(d.Dates, Labelled{Value: dateString(dt), Type: kind})
	}

	for _, i := range contact.Lint(c, region) {
		d.Issues = append(d.Issues, i.String())
	}

	return d
}

func (t tools) getContact(_ context.Context, _ *mcp.CallToolRequest, in GetContactIn) (*mcp.CallToolResult, ContactDetail, error) {
	book, obj, c, err := t.load(in.Book, in.ID)
	if err != nil {
		return nil, ContactDetail{}, err
	}

	d := detail(book.Name(), obj, *c, t.b.PhoneRegion)

	switch strings.ToLower(in.Format) {
	case "":
	case "vcard":
		d.Data = string(c.Encode())
	case "vcard4":
		v4, err := contact.Convert(*c, contact.V4)
		if err != nil {
			return nil, ContactDetail{}, fmt.Errorf("converting: %w", err)
		}

		d.Data = string(v4.Encode())
	case "jscontact":
		js, err := json.MarshalIndent(contact.ToJSContact(*c, false), "", "  ")
		if err != nil {
			return nil, ContactDetail{}, fmt.Errorf("converting: %w", err)
		}

		d.Data = string(js)
	default:
		return nil, ContactDetail{}, fmt.Errorf("%w: format must be vcard, vcard4 or jscontact", ErrBadInput)
	}

	return nil, d, nil
}

// ContactFields are the settable fields; nil means "leave as it is".
type ContactFields struct {
	FullName     *string    `json:"full_name,omitempty"`
	GivenName    *string    `json:"given_name,omitempty"`
	FamilyName   *string    `json:"family_name,omitempty"`
	Nickname     *string    `json:"nickname,omitempty"`
	Organization *string    `json:"organization,omitempty"`
	Title        *string    `json:"title,omitempty"`
	Note         *string    `json:"note,omitempty"`
	Birthday     *string    `json:"birthday,omitempty"     jsonschema:"YYYY-MM-DD, or --MM-DD without a year; empty string removes it"`
	Phones       []Labelled `json:"phones,omitempty"       jsonschema:"replaces every phone"`
	Emails       []Labelled `json:"emails,omitempty"       jsonschema:"replaces every email"`
	URLs         []Labelled `json:"urls,omitempty"         jsonschema:"replaces every link"`
}

func toLabelled(in []Labelled) []contact.Labelled {
	out := make([]contact.Labelled, 0, len(in))
	for _, l := range in {
		out = append(out, contact.Labelled{Value: l.Value, Type: l.Type})
	}

	return out
}

func apply(c *contact.Card, f ContactFields, region []string) error {
	if f.FullName != nil || f.GivenName != nil || f.FamilyName != nil {
		c.SetName(deref(f.FullName), deref(f.GivenName), deref(f.FamilyName))
	}

	if f.Nickname != nil {
		c.SetText("NICKNAME", *f.Nickname)
	}

	if f.Organization != nil {
		if *f.Organization == "" {
			c.Replace("ORG", nil)
		} else {
			c.Replace("ORG", []vcf.Line{{Group: "", Name: "ORG", Params: nil, Value: vcf.Escape(*f.Organization), Spelling: ""}})
		}
	}

	if f.Title != nil {
		c.SetText("TITLE", *f.Title)
	}

	if f.Note != nil {
		c.SetText("NOTE", *f.Note)
	}

	if f.Birthday != nil {
		b := strings.TrimSpace(*f.Birthday)
		if b != "" {
			if _, ok := contact.ParseDate(b); !ok {
				return fmt.Errorf("%w: birthday %q is not YYYY-MM-DD or --MM-DD", ErrBadInput, b)
			}
		}

		c.Replace("BDAY", nil)

		if b != "" {
			if d, _ := contact.ParseDate(b); d.Year == 0 {
				// vCard 3.0 has no year-less date; write Apple's form.
				c.Replace("BDAY", []vcf.Line{{Group: "", Name: "BDAY", Params: []vcf.Param{{Name: "X-APPLE-OMIT-YEAR", Values: []string{"1604"}, Spelling: ""}}, Value: fmt.Sprintf("1604-%02d-%02d", int(d.Month), d.Day), Spelling: ""}})
			} else {
				c.Replace("BDAY", []vcf.Line{{Group: "", Name: "BDAY", Params: nil, Value: b, Spelling: ""}})
			}
		}
	}

	if f.Phones != nil {
		phones := toLabelled(f.Phones)
		for i := range phones {
			// The assistant's numbers go in as E.164 when they can be read;
			// a phone that wrote a number its own way keeps it.
			if e164, ok := contact.NormalizeTel(phones[i].Value, region); ok {
				phones[i].Value = e164
			}
		}

		c.SetLabelled("TEL", phones)
	}

	if f.Emails != nil {
		c.SetLabelled("EMAIL", toLabelled(f.Emails))
	}

	if f.URLs != nil {
		c.SetLabelled("URL", toLabelled(f.URLs))
	}

	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

// CreateContactIn is the input of create_contact.
type CreateContactIn struct {
	ContactFields

	Book string `json:"book,omitempty"`
}

func (t tools) createContact(_ context.Context, req *mcp.CallToolRequest, in CreateContactIn) (*mcp.CallToolResult, ContactDetail, error) {
	book, err := t.book(in.Book)
	if err != nil {
		return nil, ContactDetail{}, err
	}

	name := strings.TrimSpace(strings.TrimSpace(deref(in.GivenName)) + " " + strings.TrimSpace(deref(in.FamilyName)))
	if in.FullName != nil && strings.TrimSpace(*in.FullName) != "" {
		name = strings.TrimSpace(*in.FullName)
	}

	if name == "" {
		name = deref(in.Organization)
	}

	if name == "" {
		return nil, ContactDetail{}, fmt.Errorf("%w: a contact needs a name or an organization", ErrBadInput)
	}

	uid := newUID()
	c := contact.New(uid, name)

	if err := apply(&c, in.ContactFields, t.b.PhoneRegion); err != nil {
		return nil, ContactDetail{}, err
	}

	c.Touch(time.Now())

	if _, err := t.b.WriteCard(book.Name(), uid+".vcf", c.Encode(), "", requester(req)); err != nil {
		return nil, ContactDetail{}, err
	}

	_, obj, stored, err := t.load(book.Name(), uid+".vcf")
	if err != nil {
		return nil, ContactDetail{}, err
	}

	return nil, detail(book.Name(), obj, *stored, t.b.PhoneRegion), nil
}

func newUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)

	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// UpdateContactIn is the input of update_contact.
type UpdateContactIn struct {
	ContactFields

	ID    string `json:"id"`
	Book  string `json:"book,omitempty"`
	ETag  string `json:"etag,omitempty"  jsonschema:"the etag from get_contact; the update fails if the contact changed since"`
	VCard string `json:"vcard,omitempty" jsonschema:"replace the whole card with this vCard instead of setting fields (keep its UID)"`
}

func (t tools) updateContact(_ context.Context, req *mcp.CallToolRequest, in UpdateContactIn) (*mcp.CallToolResult, ContactDetail, error) {
	book, obj, c, err := t.load(in.Book, in.ID)
	if err != nil {
		return nil, ContactDetail{}, err
	}

	ifMatch := in.ETag
	if ifMatch == "" {
		ifMatch = obj.ETag
	}

	var data []byte

	if in.VCard != "" {
		data = []byte(in.VCard)
	} else {
		if err := apply(c, in.ContactFields, t.b.PhoneRegion); err != nil {
			return nil, ContactDetail{}, err
		}

		c.Touch(time.Now())
		data = c.Encode()
	}

	if _, err := t.b.WriteCard(book.Name(), in.ID, data, ifMatch, requester(req)); err != nil {
		return nil, ContactDetail{}, err
	}

	_, obj, stored, err := t.load(book.Name(), in.ID)
	if err != nil {
		return nil, ContactDetail{}, err
	}

	return nil, detail(book.Name(), obj, *stored, t.b.PhoneRegion), nil
}

// DeleteIn deletes one object.
type DeleteIn struct {
	ID   string `json:"id"`
	Book string `json:"book,omitempty"`
	ETag string `json:"etag,omitempty" jsonschema:"the etag you read; the delete fails if it changed since"`
}

// Done reports a finished write.
type Done struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func (t tools) deleteContact(_ context.Context, req *mcp.CallToolRequest, in DeleteIn) (*mcp.CallToolResult, Done, error) {
	book, obj, c, err := t.load(in.Book, in.ID)
	if err != nil {
		return nil, Done{}, err
	}

	ifMatch := in.ETag
	if ifMatch == "" {
		ifMatch = obj.ETag
	}

	if err := t.b.Delete(store.AddressBook, book.Name(), in.ID, ifMatch, requester(req)); err != nil {
		return nil, Done{}, err
	}

	return nil, Done{OK: true, Message: "deleted " + c.DisplayName() + "; it remains in git history"}, nil
}

// HistoryIn is the input of contact_history.
type HistoryIn struct {
	ID    string `json:"id"`
	Book  string `json:"book,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// Revision is one change.
type Revision struct {
	Revision string `json:"revision"        jsonschema:"the object's blob id at that change; pass it to restore_contact_photo"`
	When     string `json:"when"`
	Device   string `json:"device"`
	Message  string `json:"message"`
	Photo    string `json:"photo,omitempty"`
}

// HistoryOut lists changes.
type HistoryOut struct {
	Revisions []Revision `json:"revisions"`
}

func (t tools) contactHistory(_ context.Context, _ *mcp.CallToolRequest, in HistoryIn) (*mcp.CallToolResult, HistoryOut, error) {
	book, _, _, err := t.load(in.Book, in.ID)
	if err != nil {
		return nil, HistoryOut{}, err
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}

	revs, err := book.History(in.ID, limit)
	if err != nil {
		return nil, HistoryOut{}, fmt.Errorf("reading history: %w", err)
	}

	out := HistoryOut{Revisions: nil}

	for _, r := range revs {
		rev := Revision{Revision: r.ETag, When: localTime(r.When), Device: web.Device(r.Requester), Message: r.Message, Photo: ""}

		if r.ETag != "" {
			if c, _, err := t.b.Parse(book, r.ETag); err == nil && c != nil {
				if p, ok := c.PhotoInfo(); ok {
					rev.Photo = fmt.Sprintf("%dx%d", p.Width, p.Height)
				}
			}
		}

		out.Revisions = append(out.Revisions, rev)
	}

	return nil, out, nil
}

// RestoreIn is the input of restore_contact_photo.
type RestoreIn struct {
	ID       string `json:"id"`
	Book     string `json:"book,omitempty"`
	Revision string `json:"revision"       jsonschema:"a revision from contact_history whose photo to restore"`
}

func (t tools) restorePhoto(_ context.Context, req *mcp.CallToolRequest, in RestoreIn) (*mcp.CallToolResult, ContactDetail, error) {
	book, obj, cur, err := t.load(in.Book, in.ID)
	if err != nil {
		return nil, ContactDetail{}, err
	}

	revs, err := book.History(in.ID, 200)
	if err != nil || !slices.ContainsFunc(revs, func(r store.Revision) bool { return r.ETag == in.Revision && in.Revision != "" }) {
		return nil, ContactDetail{}, fmt.Errorf("%w: revision %q is not in this contact's history", ErrBadInput, in.Revision)
	}

	old, _, err := t.b.Full(book, in.Revision)
	if err != nil || old == nil {
		return nil, ContactDetail{}, fmt.Errorf("reading revision %s: %w", in.Revision, err)
	}

	if _, ok := old.PhotoInfo(); !ok {
		return nil, ContactDetail{}, fmt.Errorf("%w: that revision has no photo", ErrBadInput)
	}

	if _, err := t.b.WriteCard(book.Name(), in.ID, contact.WithPhotoOf(*cur, *old).Encode(), obj.ETag, requester(req)); err != nil {
		return nil, ContactDetail{}, err
	}

	_, obj, stored, err := t.load(book.Name(), in.ID)
	if err != nil {
		return nil, ContactDetail{}, err
	}

	return nil, detail(book.Name(), obj, *stored, t.b.PhoneRegion), nil
}

// FixPhonesIn is the input of fix_contact_phones.
type FixPhonesIn struct {
	ID   string `json:"id"`
	Book string `json:"book,omitempty"`
	ETag string `json:"etag,omitempty" jsonschema:"the etag you read; the fix fails if the contact changed since"`
}

// FixPhonesOut is the contact after the fix and the numbers changed.
type FixPhonesOut struct {
	ContactDetail

	Fixed []contact.PhoneFix `json:"fixed"`
}

var errNoRegion = errors.New("contacts.phone_region is not set in the server configuration, so numbers without a country code cannot be read")

func (t tools) fixPhones(_ context.Context, req *mcp.CallToolRequest, in FixPhonesIn) (*mcp.CallToolResult, FixPhonesOut, error) {
	if len(t.b.PhoneRegion) == 0 {
		return nil, FixPhonesOut{}, errNoRegion
	}

	book, obj, cur, err := t.load(in.Book, in.ID)
	if err != nil {
		return nil, FixPhonesOut{}, err
	}

	ifMatch := in.ETag
	if ifMatch == "" {
		ifMatch = obj.ETag
	}

	fixed, fixes := contact.FixPhones(*cur, t.b.PhoneRegion)
	if len(fixes) == 0 {
		return nil, FixPhonesOut{ContactDetail: detail(book.Name(), obj, *cur, t.b.PhoneRegion), Fixed: nil}, nil
	}

	if _, err := t.b.WriteCard(book.Name(), in.ID, fixed.Encode(), ifMatch, requester(req)); err != nil {
		return nil, FixPhonesOut{}, err
	}

	_, obj, stored, err := t.load(book.Name(), in.ID)
	if err != nil {
		return nil, FixPhonesOut{}, err
	}

	return nil, FixPhonesOut{ContactDetail: detail(book.Name(), obj, *stored, t.b.PhoneRegion), Fixed: fixes}, nil
}

// BirthdaysIn is the input of upcoming_birthdays.
type BirthdaysIn struct {
	Days int `json:"days,omitempty" jsonschema:"look this many days ahead (default 30)"`
}

// Upcoming is one birthday or anniversary.
type Upcoming struct {
	Date string `json:"date"`
	In   int    `json:"in_days"`
	Name string `json:"name"`
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Age  int    `json:"age,omitempty"`
}

// BirthdaysOut lists upcoming dates.
type BirthdaysOut struct {
	Upcoming []Upcoming `json:"upcoming"`
}

func (t tools) upcomingBirthdays(_ context.Context, _ *mcp.CallToolRequest, in BirthdaysIn) (*mcp.CallToolResult, BirthdaysOut, error) {
	days := in.Days
	if days <= 0 {
		days = 30
	}

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	out := BirthdaysOut{Upcoming: nil}

	for _, book := range t.b.Store.Collections(store.AddressBook) {
		snap, err := book.Snapshot()
		if err != nil {
			continue
		}

		for _, o := range snap.Objects {
			c, _, err := t.b.Parse(book, o.ETag)
			if err != nil || c == nil {
				continue
			}

			for _, d := range c.Dates() {
				if d.Kind == contact.OtherDate {
					continue
				}

				next := d.Next(now)

				n := int(next.Sub(today).Hours() / 24)
				if n > days {
					continue
				}

				u := Upcoming{Date: next.Format("2006-01-02"), In: n, Name: c.DisplayName(), ID: o.Name, Kind: string(d.Kind), Age: 0}
				if d.Year > 0 {
					u.Age = next.Year() - d.Year
				}

				out.Upcoming = append(out.Upcoming, u)
			}
		}
	}

	sort.Slice(out.Upcoming, func(i, j int) bool { return out.Upcoming[i].Date < out.Upcoming[j].Date })

	return nil, out, nil
}

// LintIn is the input of lint_contacts.
type LintIn struct {
	Warnings bool `json:"warnings,omitempty" jsonschema:"include warnings, not only errors"`
}

// LintFinding is one contact with problems.
type LintFinding struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Issues []string `json:"issues"`
}

// LintOut lists findings.
type LintOut struct {
	Findings      []LintFinding `json:"findings"`
	LowResPhotos  []string      `json:"low_res_photos,omitempty" jsonschema:"ids of contacts whose photo is 128 px or smaller"`
	CheckedCount  int           `json:"checked"`
	ErrorsCount   int           `json:"with_errors"`
	LowResCount   int           `json:"low_res"`
	FixableCount  int           `json:"fixable_phones"           jsonschema:"contacts with phone numbers fix_contact_phones can fix"`
	LowResPreview string        `json:"note,omitempty"`
}

func (t tools) lintContacts(_ context.Context, _ *mcp.CallToolRequest, in LintIn) (*mcp.CallToolResult, LintOut, error) {
	out := LintOut{Findings: nil, LowResPhotos: nil, CheckedCount: 0, ErrorsCount: 0, LowResCount: 0, FixableCount: 0, LowResPreview: ""}

	for _, book := range t.b.Store.Collections(store.AddressBook) {
		snap, err := book.Snapshot()
		if err != nil {
			continue
		}

		for _, o := range snap.Objects {
			out.CheckedCount++

			c, _, err := t.b.Parse(book, o.ETag)
			if err != nil || c == nil {
				out.Findings = append(out.Findings, LintFinding{ID: o.Name, Name: o.Name, Issues: []string{fmt.Sprint(err)}})
				out.ErrorsCount++

				continue
			}

			if p, ok := c.PhotoInfo(); ok && max(p.Width, p.Height) > 0 && max(p.Width, p.Height) <= 128 {
				out.LowResCount++
				if len(out.LowResPhotos) < 200 {
					out.LowResPhotos = append(out.LowResPhotos, o.Name)
				}
			}

			var (
				issues  []string
				fixable bool
			)

			for _, i := range contact.Lint(*c, t.b.PhoneRegion) {
				if i.Fix != "" {
					fixable = true
				}

				if i.Severity == contact.Error || in.Warnings {
					issues = append(issues, i.String())
				}
			}

			if contact.HasErrors(contact.Validate(*c)) {
				out.ErrorsCount++
			}

			if len(issues) > 0 {
				out.Findings = append(out.Findings, LintFinding{ID: o.Name, Name: c.DisplayName(), Issues: issues})
			}

			if fixable {
				out.FixableCount++
			}
		}
	}

	if out.LowResCount > 0 {
		out.LowResPreview = "contact_history shows when a full photo was replaced; restore_contact_photo puts it back"
	}

	return nil, out, nil
}

// ---- calendars -------------------------------------------------------------

// CalendarInfo describes a calendar.
type CalendarInfo struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Events      int    `json:"events"`
	ReadOnly    bool   `json:"read_only"`
}

// CalendarsOut lists calendars.
type CalendarsOut struct {
	Calendars []CalendarInfo `json:"calendars"`
}

// Empty is a tool input without fields.
type Empty struct{}

func (t tools) listCalendars(_ context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, CalendarsOut, error) {
	out := CalendarsOut{Calendars: nil}

	for _, c := range t.b.Store.Collections(store.Calendar) {
		snap, err := c.Snapshot()
		if err != nil {
			continue
		}

		out.Calendars = append(out.Calendars, CalendarInfo{Name: c.Name(), DisplayName: snap.Meta().DisplayName(), Events: len(snap.Objects), ReadOnly: false})
	}

	out.Calendars = append(out.Calendars, CalendarInfo{Name: "birthdays", DisplayName: "Birthdays", Events: 0, ReadOnly: true})

	return nil, out, nil
}

func (t tools) calendar(name string) (*store.Collection, error) {
	if name != "" {
		c, err := t.b.Store.Collection(store.Calendar, name)
		if err != nil {
			return nil, fmt.Errorf("%w %q (birthdays is read-only; use upcoming_birthdays)", ErrNoCal, name)
		}

		return c, nil
	}

	cals := t.b.Store.Collections(store.Calendar)
	if len(cals) == 0 {
		return nil, ErrNoCal
	}

	return cals[0], nil
}

// EventSummary is an event in a list.
type EventSummary struct {
	Calendar    string `json:"calendar"`
	ID          string `json:"id"`
	Title       string `json:"title"`
	Start       string `json:"start"`
	End         string `json:"end,omitempty"`
	AllDay      bool   `json:"all_day"`
	Location    string `json:"location,omitempty"`
	Description string `json:"description,omitempty"`
	Repeats     bool   `json:"repeats,omitempty"`
	ETag        string `json:"etag"`
}

func timeString(tm time.Time, allDay bool) string {
	if tm.IsZero() {
		return ""
	}

	if allDay {
		return tm.Format("2006-01-02")
	}

	return tm.Local().Format(time.RFC3339) //nolint:gosmopolitan // times are shown in the owner's zone (server.timezone), with the offset
}

func eventSummary(cal string, o store.Object, c *calendar.Component, at time.Time) EventSummary {
	s, _ := calendar.Summarize(c)
	start, end := s.Start, s.End

	if !at.IsZero() && s.Repeats {
		end = at.Add(end.Sub(start))
		start = at
	}

	desc := ""
	if m := calendar.Master(c); m != nil {
		desc = m.Value("DESCRIPTION")
	}

	return EventSummary{
		Calendar: cal, ID: o.Name, Title: s.Title, Start: timeString(start, s.AllDay), End: timeString(end, s.AllDay),
		AllDay: s.AllDay, Location: s.Location, Description: desc, Repeats: s.Repeats, ETag: o.ETag,
	}
}

// ListEventsIn is the input of list_events.
type ListEventsIn struct {
	Calendar string `json:"calendar,omitempty" jsonschema:"one calendar; default all"`
	From     string `json:"from,omitempty"     jsonschema:"start date YYYY-MM-DD (default today)"`
	To       string `json:"to,omitempty"       jsonschema:"end date YYYY-MM-DD, exclusive (default 30 days after from)"`
	Query    string `json:"query,omitempty"    jsonschema:"only events whose title, location or description contains this"`
}

// EventsOut lists events.
type EventsOut struct {
	Events []EventSummary `json:"events"`
}

func parseDay(s string, def time.Time) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return def, nil
	}

	t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(s), time.Local) //nolint:gosmopolitan // dates are days in the owner's zone
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %q is not YYYY-MM-DD", ErrBadInput, s)
	}

	return t, nil
}

func (t tools) listEvents(_ context.Context, _ *mcp.CallToolRequest, in ListEventsIn) (*mcp.CallToolResult, EventsOut, error) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	from, err := parseDay(in.From, today)
	if err != nil {
		return nil, EventsOut{}, err
	}

	to, err := parseDay(in.To, from.AddDate(0, 0, 30))
	if err != nil {
		return nil, EventsOut{}, err
	}

	cals := t.b.Store.Collections(store.Calendar)
	if in.Calendar != "" {
		c, err := t.calendar(in.Calendar)
		if err != nil {
			return nil, EventsOut{}, err
		}

		cals = []*store.Collection{c}
	}

	q := strings.ToLower(in.Query)
	out := EventsOut{Events: nil}

	for _, col := range cals {
		snap, err := col.Snapshot()
		if err != nil {
			continue
		}

		for _, o := range snap.Objects {
			_, c, err := t.b.Parse(col, o.ETag)
			if err != nil || c == nil || !calendar.Overlaps(c, from, to) {
				continue
			}

			next, _ := calendar.NextOccurrence(c, from)
			if !next.IsZero() && (next.Before(from.Add(-24*time.Hour)) || !next.Before(to)) {
				continue
			}

			ev := eventSummary(col.Name(), o, c, next)
			if q != "" && !strings.Contains(strings.ToLower(ev.Title+" "+ev.Location+" "+ev.Description), q) {
				continue
			}

			out.Events = append(out.Events, ev)
		}
	}

	sort.Slice(out.Events, func(i, j int) bool { return out.Events[i].Start < out.Events[j].Start })

	return nil, out, nil
}

// GetEventIn is the input of get_event.
type GetEventIn struct {
	Calendar string `json:"calendar,omitempty"`
	ID       string `json:"id"`
}

// EventDetail is an event with its iCalendar.
type EventDetail struct {
	EventSummary

	Data string `json:"data"`
}

func (t tools) loadEvent(calName, id string) (*store.Collection, store.Object, *calendar.Component, []byte, error) {
	col, err := t.calendar(calName)
	if err != nil {
		return nil, store.Object{}, nil, nil, err
	}

	snap, err := col.Snapshot()
	if err != nil {
		return nil, store.Object{}, nil, nil, fmt.Errorf("reading the calendar: %w", err)
	}

	obj, ok := snap.Lookup(id)
	if !ok {
		return nil, store.Object{}, nil, nil, fmt.Errorf("event %q: %w", id, ErrNotFound)
	}

	data, err := col.ReadBlob(obj.ETag)
	if err != nil {
		return nil, store.Object{}, nil, nil, fmt.Errorf("reading %s: %w", id, err)
	}

	c, err := calendar.Parse(data)
	if err != nil {
		return nil, store.Object{}, nil, nil, fmt.Errorf("%w: %w", errReadEvent, err)
	}

	return col, obj, c, data, nil
}

func (t tools) getEvent(_ context.Context, _ *mcp.CallToolRequest, in GetEventIn) (*mcp.CallToolResult, EventDetail, error) {
	col, obj, c, data, err := t.loadEvent(in.Calendar, in.ID)
	if err != nil {
		return nil, EventDetail{}, err
	}

	return nil, EventDetail{EventSummary: eventSummary(col.Name(), obj, c, time.Time{}), Data: string(data)}, nil
}

// EventIn holds settable event fields; nil means "leave as it is".
type EventIn struct {
	Summary     *string `json:"summary,omitempty"`
	Start       *string `json:"start,omitempty"       jsonschema:"RFC 3339 time (2026-11-20T18:30:00+03:30) or a date for all-day events"`
	End         *string `json:"end,omitempty"         jsonschema:"same form as start; for all-day events the day AFTER the last day"`
	AllDay      *bool   `json:"all_day,omitempty"`
	Location    *string `json:"location,omitempty"`
	Description *string `json:"description,omitempty"`
	RRule       *string `json:"rrule,omitempty"       jsonschema:"RFC 5545 rule, for example FREQ=WEEKLY;BYDAY=MO; empty string removes it"`
}

func parseWhen(s string) (time.Time, bool, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, false, nil
	}

	if t, err := time.ParseInLocation("2006-01-02T15:04", s, time.Local); err == nil { //nolint:gosmopolitan // a time without offset is in the owner's zone
		return t, false, nil
	}

	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true, nil
	}

	return time.Time{}, false, fmt.Errorf("%w: %q is not RFC 3339 or YYYY-MM-DD", ErrBadInput, s)
}

func (in EventIn) fields() (calendar.EventFields, error) {
	f := calendar.EventFields{Summary: in.Summary, Description: in.Description, Location: in.Location, Start: nil, End: nil, AllDay: in.AllDay, RRule: in.RRule}

	for _, x := range []struct {
		src *string
		dst **time.Time
	}{{in.Start, &f.Start}, {in.End, &f.End}} {
		if x.src == nil {
			continue
		}

		tm, dateOnly, err := parseWhen(*x.src)
		if err != nil {
			return f, err
		}

		*x.dst = &tm

		if dateOnly && f.AllDay == nil {
			f.AllDay = new(true)
		}
	}

	return f, nil
}

// CreateEventIn is the input of create_event.
type CreateEventIn struct {
	EventIn

	Calendar string `json:"calendar,omitempty" jsonschema:"default the first calendar"`
}

func (t tools) createEvent(ctx context.Context, req *mcp.CallToolRequest, in CreateEventIn) (*mcp.CallToolResult, EventDetail, error) {
	col, err := t.calendar(in.Calendar)
	if err != nil {
		return nil, EventDetail{}, err
	}

	if in.Summary == nil || strings.TrimSpace(*in.Summary) == "" || in.Start == nil {
		return nil, EventDetail{}, fmt.Errorf("%w: an event needs a summary and a start", ErrBadInput)
	}

	f, err := in.fields()
	if err != nil {
		return nil, EventDetail{}, err
	}

	if f.End == nil {
		end := f.Start.Add(time.Hour)
		if f.AllDay != nil && *f.AllDay {
			end = f.Start.AddDate(0, 0, 1)
		}

		f.End = &end
	}

	uid := newUID()
	name := uid + ".ics"

	if _, err := t.b.WriteEvent(col.Name(), name, calendar.NewEvent(uid, time.Now(), f).Encode(), "", true, requester(req)); err != nil {
		return nil, EventDetail{}, err
	}

	return t.getEvent(ctx, req, GetEventIn{Calendar: col.Name(), ID: name})
}

// UpdateEventIn is the input of update_event.
type UpdateEventIn struct {
	EventIn

	Calendar string `json:"calendar,omitempty"`
	ID       string `json:"id"`
	ETag     string `json:"etag,omitempty"     jsonschema:"the etag you read; the update fails if it changed since"`
}

func (t tools) updateEvent(ctx context.Context, req *mcp.CallToolRequest, in UpdateEventIn) (*mcp.CallToolResult, EventDetail, error) {
	col, obj, c, _, err := t.loadEvent(in.Calendar, in.ID)
	if err != nil {
		return nil, EventDetail{}, err
	}

	f, err := in.fields()
	if err != nil {
		return nil, EventDetail{}, err
	}

	m := calendar.Master(c)
	if m == nil {
		return nil, EventDetail{}, fmt.Errorf("%w: the object holds no event", ErrBadInput)
	}

	calendar.ApplyEvent(m, time.Now(), f)

	ifMatch := in.ETag
	if ifMatch == "" {
		ifMatch = obj.ETag
	}

	if _, err := t.b.WriteEvent(col.Name(), in.ID, c.Encode(), ifMatch, false, requester(req)); err != nil {
		return nil, EventDetail{}, err
	}

	return t.getEvent(ctx, req, GetEventIn{Calendar: col.Name(), ID: in.ID})
}

// DeleteEventIn is the input of delete_event.
type DeleteEventIn struct {
	Calendar string `json:"calendar,omitempty"`
	ID       string `json:"id"`
	ETag     string `json:"etag,omitempty"`
}

func (t tools) deleteEvent(_ context.Context, req *mcp.CallToolRequest, in DeleteEventIn) (*mcp.CallToolResult, Done, error) {
	col, obj, c, _, err := t.loadEvent(in.Calendar, in.ID)
	if err != nil {
		return nil, Done{}, err
	}

	ifMatch := in.ETag
	if ifMatch == "" {
		ifMatch = obj.ETag
	}

	if err := t.b.Delete(store.Calendar, col.Name(), in.ID, ifMatch, requester(req)); err != nil {
		return nil, Done{}, err
	}

	title := in.ID
	if s, ok := calendar.Summarize(c); ok && s.Title != "" {
		title = s.Title
	}

	return nil, Done{OK: true, Message: "deleted " + title + "; it remains in git history"}, nil
}

// localTime is a moment in the owner's zone (server.timezone), with the
// offset: commits keep the zone of the machine that wrote them.
func localTime(t time.Time) string {
	return t.Local().Format(time.RFC3339) //nolint:gosmopolitan // times are shown in the owner's zone
}
