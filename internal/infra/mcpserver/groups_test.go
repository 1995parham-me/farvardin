package mcpserver

import (
	"strings"
	"testing"
)

func TestGroupTools(t *testing.T) {
	t.Parallel()

	b, _ := backend(t)
	cs := connect(t, b)

	mk := func(name string) ContactDetail {
		c, isErr, msg := call[ContactDetail](t, cs, "create_contact", map[string]any{"full_name": name, "phones": []map[string]string{{"value": "+98912" + strings.Repeat("1", 7), "type": "cell"}}})
		if isErr {
			t.Fatalf("create %s: %s", name, msg)
		}

		return c
	}

	hamid, amin, sara := mk("Hamid"), mk("Amin"), mk("Sara")

	g, isErr, msg := call[Group](t, cs, "create_group", map[string]any{"name": "Home Services", "members": []string{hamid.ID, amin.ID}})
	if isErr || g.Name != "Home Services" || len(g.Members) != 2 || g.Members[0].Name != "Amin" {
		t.Fatalf("create_group: %s %+v", msg, g)
	}

	if _, isErr, _ := call[Group](t, cs, "create_group", map[string]any{"name": "home services"}); !isErr {
		t.Fatal("a second group of the same name must be refused")
	}

	if _, isErr, _ := call[Group](t, cs, "add_to_group", map[string]any{"group": "Home Services", "contacts": []string{g.ID}}); !isErr {
		t.Fatal("a group cannot be a member")
	}

	// Members carry their groups; group cards stay out of contact search.
	all, _, _ := call[SearchOut](t, cs, "search_contacts", map[string]any{"query": ""})
	if all.Total != 3 {
		t.Fatalf("search lists the group card: %+v", all)
	}

	only, _, _ := call[SearchOut](t, cs, "search_contacts", map[string]any{"query": "", "group": "home services"})
	if only.Total != 2 || len(only.Contacts[0].Groups) != 1 || only.Contacts[0].Groups[0] != "Home Services" {
		t.Fatalf("group filter: %+v", only)
	}

	g, isErr, msg = call[Group](t, cs, "add_to_group", map[string]any{"group": g.ID, "contacts": []string{sara.ID, hamid.ID}, "etag": g.ETag})
	if isErr || len(g.Members) != 3 {
		t.Fatalf("add_to_group: %s %+v", msg, g)
	}

	g, isErr, msg = call[Group](t, cs, "remove_from_group", map[string]any{"group": "Home Services", "contacts": []string{hamid.ID}})
	if isErr || len(g.Members) != 2 {
		t.Fatalf("remove_from_group: %s %+v", msg, g)
	}

	got, _, _ := call[ContactDetail](t, cs, "get_contact", map[string]any{"id": sara.ID})
	if len(got.Groups) != 1 {
		t.Fatalf("get_contact groups: %+v", got.Groups)
	}

	g, isErr, msg = call[Group](t, cs, "rename_group", map[string]any{"group": "Home Services", "name": "House"})
	if isErr || g.Name != "House" {
		t.Fatalf("rename_group: %s %+v", msg, g)
	}

	raw, _, _ := call[ContactDetail](t, cs, "get_contact", map[string]any{"id": g.ID, "format": "vcard"})
	if !strings.Contains(raw.Data, "N:House;;;;") || !strings.Contains(raw.Data, "X-ADDRESSBOOKSERVER-KIND:group") {
		t.Fatalf("renamed card:\n%s", raw.Data)
	}

	list, _, _ := call[GroupsOut](t, cs, "list_groups", map[string]any{})
	if len(list.Groups) != 1 || list.Groups[0].Name != "House" || len(list.Groups[0].Members) != 2 {
		t.Fatalf("list_groups: %+v", list)
	}

	// A member deleted elsewhere is counted, not hidden.
	if _, isErr, msg := call[Done](t, cs, "delete_contact", map[string]any{"id": amin.ID}); isErr {
		t.Fatalf("delete: %s", msg)
	}

	list, _, _ = call[GroupsOut](t, cs, "list_groups", map[string]any{})
	if list.Groups[0].Missing != 1 {
		t.Fatalf("missing member: %+v", list.Groups[0])
	}

	done, isErr, msg := call[Done](t, cs, "delete_group", map[string]any{"group": "House"})
	if isErr || !done.OK {
		t.Fatalf("delete_group: %s", msg)
	}

	if got, _, _ := call[ContactDetail](t, cs, "get_contact", map[string]any{"id": sara.ID}); len(got.Groups) != 0 || got.Name != "Sara" {
		t.Fatalf("after delete_group: %+v", got)
	}
}
