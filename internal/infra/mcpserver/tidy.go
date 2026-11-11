package mcpserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/edits"
	"github.com/1995parham-me/farvardin/internal/infra/web"
)

// The tidy tools: duplicates and merge, deleted contacts and restore, and
// fixing every phone number at once. They share their code with the web
// UI (package edits).

func (t tools) editor() edits.Editor {
	return edits.Editor{
		Store: t.b.Store, Parse: edits.Parser(t.b.Parse), Full: edits.Parser(t.b.Full),
		Write: t.b.WriteCard, Delete: t.b.Delete, Regions: t.b.PhoneRegion,
	}
}

func (t tools) registerTidy(s *mcp.Server, tool func(name, desc string, ann *mcp.ToolAnnotations) *mcp.Tool) {
	mcp.AddTool(s, tool("find_duplicate_contacts", "List groups of contacts that look like the same one: they share a phone number (compared in international form), an email address or a name. A company and its people, or two people of one company, are not grouped by a shared number or email. A number on two cards makes SMS and call naming pick one of them; merge_contacts combines a group, mark_not_duplicates hides a group of different contacts.", readOnly()), t.findDuplicates)
	mcp.AddTool(s, tool("mark_not_duplicates", "Mark a group from find_duplicate_contacts as different contacts (pass all its ids), so it is no longer listed. It comes back only if another card joins the group. Nothing in the contacts changes; the web UI can show hidden groups again.", writes(false)), t.markNotDuplicates)
	mcp.AddTool(s, tool("merge_contacts", "Merge contacts into one: the kept contact gets what only the others have (phone numbers, emails, links, addresses, dates, a note, the larger photo), then the others are deleted. They stay in git history and list_deleted_contacts can bring them back. Pass the etag of every contact you read.", writes(true)), t.mergeContacts)
	mcp.AddTool(s, tool("list_deleted_contacts", "List contacts that were deleted (by a phone, the web UI or MCP), newest first, with who deleted them. restore_contact brings one back.", readOnly()), t.listDeleted)
	mcp.AddTool(s, tool("restore_contact", "Bring back a deleted contact (id from list_deleted_contacts) as it was before the delete, or put an existing contact back as it was at an earlier revision (from contact_history; pass its etag too). The restore is a new change on top; nothing is rewritten.", writes(false)), t.restoreContact)
	mcp.AddTool(s, tool("fix_all_contact_phones", "Write every phone number that has no country code in international (E.164) form, in every contact lint_contacts reports as fixable. One change per contact; labels and other fields stay as they are.", writes(false)), t.fixAllPhones)
}

// BookIn names an address book.
type BookIn struct {
	Book string `json:"book,omitempty"`
}

// DuplicatesOut lists duplicate groups.
type DuplicatesOut struct {
	Groups []contact.DuplicateGroup `json:"groups"`
	Hidden int                      `json:"hidden_groups" jsonschema:"groups marked as different contacts, not listed"`
}

func (t tools) findDuplicates(_ context.Context, _ *mcp.CallToolRequest, in BookIn) (*mcp.CallToolResult, DuplicatesOut, error) {
	book, err := t.book(in.Book)
	if err != nil {
		return nil, DuplicatesOut{}, err
	}

	groups, hidden, err := t.editor().Duplicates(book.Name())
	if err != nil {
		return nil, DuplicatesOut{}, err
	}

	return nil, DuplicatesOut{Groups: groups, Hidden: hidden}, nil
}

// NotDuplicatesIn is the input of mark_not_duplicates.
type NotDuplicatesIn struct {
	Book string   `json:"book,omitempty"`
	IDs  []string `json:"ids"            jsonschema:"every id of one group from find_duplicate_contacts"`
}

func (t tools) markNotDuplicates(_ context.Context, _ *mcp.CallToolRequest, in NotDuplicatesIn) (*mcp.CallToolResult, DuplicatesOut, error) {
	book, err := t.book(in.Book)
	if err != nil {
		return nil, DuplicatesOut{}, err
	}

	if err := t.editor().NotSame(book.Name(), in.IDs); errors.Is(err, edits.ErrNotGroup) {
		return nil, DuplicatesOut{}, fmt.Errorf("%w: %w", ErrBadInput, err)
	} else if err != nil {
		return nil, DuplicatesOut{}, err
	}

	groups, hidden, err := t.editor().Duplicates(book.Name())
	if err != nil {
		return nil, DuplicatesOut{}, err
	}

	return nil, DuplicatesOut{Groups: groups, Hidden: hidden}, nil
}

// CardRef names a contact with the etag it was read at.
type CardRef struct {
	ID   string `json:"id"`
	ETag string `json:"etag,omitempty" jsonschema:"the etag you read; the merge fails if the contact changed since"`
}

// MergeIn is the input of merge_contacts.
type MergeIn struct {
	Book   string    `json:"book,omitempty"`
	Keep   CardRef   `json:"keep"           jsonschema:"the contact to keep"`
	Others []CardRef `json:"others"         jsonschema:"the contacts to merge into it and then delete"`
}

// MergeOut is the kept contact after the merge.
type MergeOut struct {
	ContactDetail

	Merged []string          `json:"merged"`
	Failed map[string]string `json:"not_deleted,omitempty" jsonschema:"contacts merged but not deleted, with the reason; their content is in the kept contact"`
}

func (t tools) mergeContacts(_ context.Context, req *mcp.CallToolRequest, in MergeIn) (*mcp.CallToolResult, MergeOut, error) {
	book, err := t.book(in.Book)
	if err != nil {
		return nil, MergeOut{}, err
	}

	// A missing etag means "as it is now", as in the other write tools.
	snap, err := book.Snapshot()
	if err != nil {
		return nil, MergeOut{}, fmt.Errorf("reading the address book: %w", err)
	}

	ref := func(r CardRef) edits.Card {
		if r.ETag == "" {
			if obj, ok := snap.Lookup(r.ID); ok {
				r.ETag = obj.ETag
			}
		}

		return edits.Card{ID: r.ID, ETag: r.ETag}
	}

	others := make([]edits.Card, 0, len(in.Others))
	for _, o := range in.Others {
		others = append(others, ref(o))
	}

	res, err := t.editor().Merge(book.Name(), ref(in.Keep), others, requester(req))
	if err != nil {
		return nil, MergeOut{}, err
	}

	_, obj, stored, err := t.load(book.Name(), res.Kept)
	if err != nil {
		return nil, MergeOut{}, err
	}

	return nil, MergeOut{ContactDetail: t.detail(book.Name(), obj, *stored), Merged: res.Merged, Failed: res.Failed}, nil
}

// DeletedContact is one deleted contact.
type DeletedContact struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	DeletedAt string `json:"deleted_at"`
	DeletedBy string `json:"deleted_by"`
}

// DeletedOut lists deleted contacts.
type DeletedOut struct {
	Deleted []DeletedContact `json:"deleted"`
}

func (t tools) listDeleted(_ context.Context, _ *mcp.CallToolRequest, in BookIn) (*mcp.CallToolResult, DeletedOut, error) {
	book, err := t.book(in.Book)
	if err != nil {
		return nil, DeletedOut{}, err
	}

	gone, err := t.editor().Deleted(book.Name())
	if err != nil {
		return nil, DeletedOut{}, err
	}

	out := DeletedOut{Deleted: make([]DeletedContact, 0, len(gone))}

	for _, g := range gone {
		out.Deleted = append(out.Deleted, DeletedContact{
			ID: g.Name, Name: g.Display, DeletedAt: localTime(g.Deleted.When), DeletedBy: web.Device(g.Deleted.Requester),
		})
	}

	return nil, out, nil
}

// RestoreContactIn is the input of restore_contact.
type RestoreContactIn struct {
	ID       string `json:"id"`
	Book     string `json:"book,omitempty"`
	Revision string `json:"revision,omitempty" jsonschema:"for a contact that exists: the revision from contact_history to go back to"`
	ETag     string `json:"etag,omitempty"     jsonschema:"for a contact that exists: the etag you read"`
}

func (t tools) restoreContact(_ context.Context, req *mcp.CallToolRequest, in RestoreContactIn) (*mcp.CallToolResult, ContactDetail, error) {
	book, err := t.book(in.Book)
	if err != nil {
		return nil, ContactDetail{}, err
	}

	snap, err := book.Snapshot()
	if err != nil {
		return nil, ContactDetail{}, fmt.Errorf("reading the address book: %w", err)
	}

	if obj, exists := snap.Lookup(in.ID); exists {
		if in.Revision == "" {
			return nil, ContactDetail{}, fmt.Errorf("%w: %s exists; pass the revision (from contact_history) to go back to", ErrBadInput, in.ID)
		}

		ifMatch := in.ETag
		if ifMatch == "" {
			ifMatch = obj.ETag
		}

		_, err = t.editor().RestoreVersion(book.Name(), in.ID, in.Revision, ifMatch, requester(req))
	} else {
		_, err = t.editor().RestoreDeleted(book.Name(), in.ID, requester(req))
	}

	if err != nil {
		return nil, ContactDetail{}, err
	}

	_, obj, stored, err := t.load(book.Name(), in.ID)
	if err != nil {
		return nil, ContactDetail{}, err
	}

	return nil, t.detail(book.Name(), obj, *stored), nil
}

// FixAllOut reports fix_all_contact_phones.
type FixAllOut struct {
	Contacts []edits.PhoneFixes `json:"contacts"`
	Fixed    int                `json:"fixed_numbers"`
}

func (t tools) fixAllPhones(_ context.Context, req *mcp.CallToolRequest, in BookIn) (*mcp.CallToolResult, FixAllOut, error) {
	if len(t.b.PhoneRegion) == 0 {
		return nil, FixAllOut{}, errNoRegion
	}

	book, err := t.book(in.Book)
	if err != nil {
		return nil, FixAllOut{}, err
	}

	res, err := t.editor().FixAllPhones(book.Name(), requester(req))
	if err != nil {
		return nil, FixAllOut{}, err
	}

	out := FixAllOut{Contacts: res, Fixed: 0}
	for _, r := range res {
		out.Fixed += len(r.Fixed)
	}

	return nil, out, nil
}
