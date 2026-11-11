package mcpserver

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

// The group tools. A group is a card of its own (contact.IsGroup) whose
// members are named by UID; the phones turn each into a contact group.
// CATEGORIES on a contact is read as a group too, so a contact's groups
// are both kinds, but the tools write only group cards: that is what the
// phones here read.

func (t tools) registerGroups(s *mcp.Server, tool func(name, desc string, ann *mcp.ToolAnnotations) *mcp.Tool) {
	mcp.AddTool(s, tool("list_groups", "List the contact groups with their members. A group is a card of its own (iOS and DAVx5 turn it into a contact group on the phone). Categories written on contacts (CATEGORIES) are listed too, read-only.", readOnly()), t.listGroups)
	mcp.AddTool(s, tool("create_group", "Create a contact group, optionally with members (contact ids from search_contacts). List the groups first: a name that exists is refused.", writes(false)), t.createGroup)
	mcp.AddTool(s, tool("add_to_group", "Add contacts (ids from search_contacts) to a group, named by its id or its name. Contacts already in it are left as they are.", writes(false)), t.addToGroup)
	mcp.AddTool(s, tool("remove_from_group", "Remove contacts (ids from search_contacts) from a group, named by its id or its name. The contacts themselves are not changed.", writes(false)), t.removeFromGroup)
	mcp.AddTool(s, tool("rename_group", "Rename a group, named by its id or its current name.", writes(false)), t.renameGroup)
	mcp.AddTool(s, tool("delete_group", "Delete a group card (named by its id or its name). Its members stay; only the grouping goes. It remains in git history.", writes(true)), t.deleteGroup)
}

// groupIndex is every group of a book, read from one snapshot.
type groupIndex struct {
	book  *store.Collection
	snap  *store.Snapshot
	cards map[string]*contact.Card // object name -> parsed card
	uids  map[string]string        // UID -> object name
	// groups are the group cards, sorted by name.
	groups []groupCard
	// byUID lists the group names each contact UID belongs to, group
	// cards and CATEGORIES together, sorted.
	byUID map[string][]string
	// categories counts the contacts in each CATEGORIES value.
	categories map[string]int
}

type groupCard struct {
	id   string
	name string
	etag string
	card *contact.Card
}

func (t tools) groups(bookName string) (*groupIndex, error) {
	book, err := t.book(bookName)
	if err != nil {
		return nil, err
	}

	snap, err := book.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("reading the address book: %w", err)
	}

	gi := &groupIndex{
		book: book, snap: snap, cards: map[string]*contact.Card{}, uids: map[string]string{},
		groups: nil, byUID: map[string][]string{}, categories: map[string]int{},
	}

	for _, o := range snap.Objects {
		c, _, err := t.b.Parse(book, o.ETag)
		if err != nil || c == nil {
			continue
		}

		gi.cards[o.Name] = c
		if uid := c.UID(); uid != "" {
			gi.uids[uid] = o.Name
		}

		if contact.IsGroup(*c) {
			gi.groups = append(gi.groups, groupCard{id: o.Name, name: c.DisplayName(), etag: o.ETag, card: c})

			continue
		}

		for _, cat := range c.Categories() {
			gi.categories[cat]++
			gi.add(c.UID(), cat)
		}
	}

	sort.Slice(gi.groups, func(i, j int) bool { return strings.ToLower(gi.groups[i].name) < strings.ToLower(gi.groups[j].name) })

	for _, g := range gi.groups {
		for _, uid := range g.card.Members() {
			gi.add(uid, g.name)
		}
	}

	for uid := range gi.byUID {
		slices.SortFunc(gi.byUID[uid], func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	}

	return gi, nil
}

func (gi *groupIndex) add(uid, name string) {
	if uid == "" || slices.Contains(gi.byUID[uid], name) {
		return
	}

	gi.byUID[uid] = append(gi.byUID[uid], name)
}

// of returns the group names of one card.
func (gi *groupIndex) of(c contact.Card) []string { return gi.byUID[c.UID()] }

// find resolves a group by object id, or by name ignoring case.
func (gi *groupIndex) find(ref string) (groupCard, error) {
	ref = strings.TrimSpace(ref)
	for _, g := range gi.groups {
		if g.id == ref || g.id == ref+".vcf" {
			return g, nil
		}
	}

	var hits []groupCard

	for _, g := range gi.groups {
		if strings.EqualFold(g.name, ref) {
			hits = append(hits, g)
		}
	}

	switch len(hits) {
	case 0:
		return groupCard{}, fmt.Errorf("group %q: %w (list_groups names them)", ref, ErrNotFound)
	case 1:
		return hits[0], nil
	default:
		return groupCard{}, fmt.Errorf("%w: %d groups are named %q; pass the id", ErrBadInput, len(hits), ref)
	}
}

// memberUIDs turns contact ids into their UIDs, refusing ids that do not
// exist, are groups, or have no UID.
func (gi *groupIndex) memberUIDs(ids []string) ([]string, error) {
	out := make([]string, 0, len(ids))

	for _, id := range ids {
		c, ok := gi.cards[id]
		if !ok {
			return nil, fmt.Errorf("contact %q: %w", id, ErrNotFound)
		}

		if contact.IsGroup(*c) {
			return nil, fmt.Errorf("%w: %q is a group, not a contact", ErrBadInput, id)
		}

		if c.UID() == "" {
			return nil, fmt.Errorf("%w: contact %q has no UID to refer to", ErrBadInput, id)
		}

		out = append(out, c.UID())
	}

	return out, nil
}

// Member is one member of a group.
type Member struct {
	ID   string `json:"id,omitempty" jsonschema:"the contact id; empty when no card has the UID"`
	Name string `json:"name"`
	UID  string `json:"uid"`
}

// Group is one group card.
type Group struct {
	ID      string   `json:"id"`
	Book    string   `json:"book"`
	Name    string   `json:"name"`
	ETag    string   `json:"etag"`
	Members []Member `json:"members"`
	// Missing counts members whose card no longer exists.
	Missing int `json:"missing,omitempty" jsonschema:"members whose contact no longer exists"`
}

// Category is a CATEGORIES value and how many contacts carry it.
type Category struct {
	Name     string `json:"name"`
	Contacts int    `json:"contacts"`
}

// GroupsOut lists the groups.
type GroupsOut struct {
	Groups     []Group    `json:"groups"`
	Categories []Category `json:"categories,omitempty"`
}

func (gi *groupIndex) describe(g groupCard) Group {
	out := Group{ID: g.id, Book: gi.book.Name(), Name: g.name, ETag: g.etag, Members: []Member{}, Missing: 0}

	for _, uid := range g.card.Members() {
		id, ok := gi.uids[uid]
		if !ok {
			out.Missing++
			out.Members = append(out.Members, Member{ID: "", Name: "", UID: uid})

			continue
		}

		out.Members = append(out.Members, Member{ID: id, Name: gi.cards[id].DisplayName(), UID: uid})
	}

	sort.SliceStable(out.Members, func(i, j int) bool {
		return strings.ToLower(out.Members[i].Name) < strings.ToLower(out.Members[j].Name)
	})

	return out
}

func (t tools) listGroups(_ context.Context, _ *mcp.CallToolRequest, in BookIn) (*mcp.CallToolResult, GroupsOut, error) {
	gi, err := t.groups(in.Book)
	if err != nil {
		return nil, GroupsOut{}, err
	}

	out := GroupsOut{Groups: []Group{}, Categories: nil}
	for _, g := range gi.groups {
		out.Groups = append(out.Groups, gi.describe(g))
	}

	for name, n := range gi.categories {
		out.Categories = append(out.Categories, Category{Name: name, Contacts: n})
	}

	sort.Slice(out.Categories, func(i, j int) bool {
		return strings.ToLower(out.Categories[i].Name) < strings.ToLower(out.Categories[j].Name)
	})

	return nil, out, nil
}

// CreateGroupIn is the input of create_group.
type CreateGroupIn struct {
	Name    string   `json:"name"`
	Members []string `json:"members,omitempty" jsonschema:"contact ids from search_contacts"`
	Book    string   `json:"book,omitempty"`
}

func (t tools) createGroup(_ context.Context, req *mcp.CallToolRequest, in CreateGroupIn) (*mcp.CallToolResult, Group, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, Group{}, fmt.Errorf("%w: a group needs a name", ErrBadInput)
	}

	gi, err := t.groups(in.Book)
	if err != nil {
		return nil, Group{}, err
	}

	for _, g := range gi.groups {
		if strings.EqualFold(g.name, name) {
			return nil, Group{}, fmt.Errorf("%w: group %q exists (id %s); use add_to_group", ErrBadInput, g.name, g.id)
		}
	}

	uids, err := gi.memberUIDs(in.Members)
	if err != nil {
		return nil, Group{}, err
	}

	uid := newUID()
	g := contact.NewGroup(uid, name)
	g.SetMembers(uids)
	g.Touch(time.Now())

	if _, err := t.b.WriteCard(gi.book.Name(), uid+".vcf", g.Encode(), "", requester(req)); err != nil {
		return nil, Group{}, err
	}

	return t.reread(gi.book.Name(), uid+".vcf")
}

// GroupMembersIn is the input of add_to_group and remove_from_group.
type GroupMembersIn struct {
	Group    string   `json:"group"          jsonschema:"the group's id or name"`
	Contacts []string `json:"contacts"       jsonschema:"contact ids from search_contacts"`
	ETag     string   `json:"etag,omitempty" jsonschema:"the group's etag from list_groups; the change fails if the group changed since"`
	Book     string   `json:"book,omitempty"`
}

func (t tools) addToGroup(_ context.Context, req *mcp.CallToolRequest, in GroupMembersIn) (*mcp.CallToolResult, Group, error) {
	return t.editMembers(req, in, func(members, uids []string) []string {
		return append(members, uids...)
	})
}

func (t tools) removeFromGroup(_ context.Context, req *mcp.CallToolRequest, in GroupMembersIn) (*mcp.CallToolResult, Group, error) {
	return t.editMembers(req, in, func(members, uids []string) []string {
		return slices.DeleteFunc(members, func(m string) bool { return slices.Contains(uids, m) })
	})
}

func (t tools) editMembers(req *mcp.CallToolRequest, in GroupMembersIn, edit func(members, uids []string) []string) (*mcp.CallToolResult, Group, error) {
	if len(in.Contacts) == 0 {
		return nil, Group{}, fmt.Errorf("%w: name at least one contact", ErrBadInput)
	}

	gi, err := t.groups(in.Book)
	if err != nil {
		return nil, Group{}, err
	}

	g, err := gi.find(in.Group)
	if err != nil {
		return nil, Group{}, err
	}

	uids, err := gi.memberUIDs(in.Contacts)
	if err != nil {
		return nil, Group{}, err
	}

	return t.writeGroup(req, gi.book.Name(), g, in.ETag, func(c *contact.Card) {
		c.SetMembers(edit(c.Members(), uids))
	})
}

// writeGroup reads the full group card, changes it and writes it back
// under If-Match.
func (t tools) writeGroup(req *mcp.CallToolRequest, book string, g groupCard, etag string, change func(*contact.Card)) (*mcp.CallToolResult, Group, error) {
	_, obj, c, err := t.load(book, g.id)
	if err != nil {
		return nil, Group{}, err
	}

	ifMatch := etag
	if ifMatch == "" {
		ifMatch = obj.ETag
	}

	change(c)
	c.Touch(time.Now())

	if _, err := t.b.WriteCard(book, g.id, c.Encode(), ifMatch, requester(req)); err != nil {
		return nil, Group{}, err
	}

	return t.reread(book, g.id)
}

func (t tools) reread(book, id string) (*mcp.CallToolResult, Group, error) {
	gi, err := t.groups(book)
	if err != nil {
		return nil, Group{}, err
	}

	g, err := gi.find(id)
	if err != nil {
		return nil, Group{}, err
	}

	return nil, gi.describe(g), nil
}

// RenameGroupIn is the input of rename_group.
type RenameGroupIn struct {
	Group string `json:"group"          jsonschema:"the group's id or current name"`
	Name  string `json:"name"           jsonschema:"the new name"`
	ETag  string `json:"etag,omitempty"`
	Book  string `json:"book,omitempty"`
}

func (t tools) renameGroup(_ context.Context, req *mcp.CallToolRequest, in RenameGroupIn) (*mcp.CallToolResult, Group, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, Group{}, fmt.Errorf("%w: a group needs a name", ErrBadInput)
	}

	gi, err := t.groups(in.Book)
	if err != nil {
		return nil, Group{}, err
	}

	g, err := gi.find(in.Group)
	if err != nil {
		return nil, Group{}, err
	}

	for _, other := range gi.groups {
		if other.id != g.id && strings.EqualFold(other.name, name) {
			return nil, Group{}, fmt.Errorf("%w: group %q exists (id %s)", ErrBadInput, other.name, other.id)
		}
	}

	return t.writeGroup(req, gi.book.Name(), g, in.ETag, func(c *contact.Card) {
		c.SetGroupName(name)
	})
}

// DeleteGroupIn is the input of delete_group.
type DeleteGroupIn struct {
	Group string `json:"group"          jsonschema:"the group's id or name"`
	ETag  string `json:"etag,omitempty"`
	Book  string `json:"book,omitempty"`
}

func (t tools) deleteGroup(_ context.Context, req *mcp.CallToolRequest, in DeleteGroupIn) (*mcp.CallToolResult, Done, error) {
	gi, err := t.groups(in.Book)
	if err != nil {
		return nil, Done{}, err
	}

	g, err := gi.find(in.Group)
	if err != nil {
		return nil, Done{}, err
	}

	ifMatch := in.ETag
	if ifMatch == "" {
		ifMatch = g.etag
	}

	if err := t.b.Delete(store.AddressBook, gi.book.Name(), g.id, ifMatch, requester(req)); err != nil {
		return nil, Done{}, err
	}

	return nil, Done{OK: true, Message: fmt.Sprintf("deleted group %s (%d members kept); it remains in git history", g.name, len(g.card.Members()))}, nil
}
