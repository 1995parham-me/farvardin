package web

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/1995parham-me/farvardin/internal/domain/contact"
	"github.com/1995parham-me/farvardin/internal/infra/edits"
	"github.com/1995parham-me/farvardin/internal/infra/store"
)

// The tidy pages: duplicates (with merge), deleted contacts (with
// restore), fixing every phone number at once, and restoring a card's
// earlier version. Every write goes through h.edit, the same code MCP
// uses.

func (h *Handler) requester(r *http.Request) string {
	return uiRequester(userFrom(r.Context()))
}

// canPost is the guard of every tidy write: writes enabled and the form
// posted from this site.
func (h *Handler) canPost(w http.ResponseWriter, r *http.Request) bool {
	if h.write == nil || h.edit.Delete == nil || !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)

		return false
	}

	return true
}

// dupCard is one card of a duplicate group as the page shows it.
type dupCard struct {
	person

	Phones []string
	Emails []string
	Lines  int
	Best   bool // the suggested card to keep
}

type dupGroup struct {
	Book    string
	Reasons []string
	Cards   []dupCard
}

func (h *Handler) duplicates(w http.ResponseWriter, r *http.Request) {
	var groups []dupGroup

	hidden := 0

	for _, book := range h.store.Collections(store.AddressBook) {
		found, marked, err := h.edit.Duplicates(book.Name())
		if err != nil {
			continue
		}

		hidden += marked

		people, snap, err := h.people(book)
		if err != nil {
			continue
		}

		byFile := map[string]person{}
		for _, p := range people {
			byFile[p.File] = p
		}

		for _, g := range found {
			dg := dupGroup{Book: book.Name(), Reasons: g.Reasons, Cards: nil}
			best, bestScore := 0, -1

			for i, id := range g.IDs {
				dc := dupCard{person: byFile[id], Phones: nil, Emails: nil, Lines: 0, Best: false}

				if obj, ok := snap.Lookup(id); ok {
					if c, _, err := h.parse(book, obj.ETag); err == nil && c != nil {
						for _, v := range c.Values("TEL") {
							dc.Phones = append(dc.Phones, v.Value)
						}

						for _, v := range c.Values("EMAIL") {
							dc.Emails = append(dc.Emails, v.Value)
						}

						dc.Lines = len(c.Lines)
					}
				}

				// Suggest the card with a photo and the most on it.
				score := dc.Lines
				if dc.HasPhoto {
					score += 1000 + dc.PhotoW
				}

				if score > bestScore {
					best, bestScore = i, score
				}

				dg.Cards = append(dg.Cards, dc)
			}

			dg.Cards[best].Best = true
			groups = append(groups, dg)
		}
	}

	h.render(w, r, "duplicates.html", map[string]any{
		"Title": "Duplicates", "Groups": groups, "CanWrite": h.write != nil && h.edit.Delete != nil,
		"Merged": r.URL.Query().Get("merged"), "Failed": r.URL.Query().Get("error"),
		"Hidden": hidden, "NotSame": r.URL.Query().Get("notsame") != "",
	})
}

// notSame marks a duplicate group as different contacts.
func (h *Handler) notSame(w http.ResponseWriter, r *http.Request) {
	if !h.canPost(w, r) {
		return
	}

	back := h.prefix + "/duplicates"

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, back+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)

		return
	}

	if err := h.edit.NotSame(r.PostForm.Get("book"), r.PostForm["id"]); err != nil {
		http.Redirect(w, r, back+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)

		return
	}

	h.log.Info("duplicates marked not the same", "cards", r.PostForm["id"], "user", userFrom(r.Context()))
	http.Redirect(w, r, back+"?notsame=1", http.StatusSeeOther)
}

// showAllDuplicates lists the groups marked not the same again.
func (h *Handler) showAllDuplicates(w http.ResponseWriter, r *http.Request) {
	if !h.canPost(w, r) {
		return
	}

	for _, book := range h.store.Collections(store.AddressBook) {
		if err := h.edit.ShowAllDuplicates(book.Name()); err != nil {
			http.Redirect(w, r, h.prefix+"/duplicates?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)

			return
		}
	}

	http.Redirect(w, r, h.prefix+"/duplicates", http.StatusSeeOther)
}

func (h *Handler) merge(w http.ResponseWriter, r *http.Request) {
	if !h.canPost(w, r) {
		return
	}

	back := h.prefix + "/duplicates"

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, back+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)

		return
	}

	book, keepID := r.PostForm.Get("book"), r.PostForm.Get("keep")
	ids, tags := r.PostForm["id"], r.PostForm["etag"]

	var (
		keep   edits.Card
		others []edits.Card
	)

	for i, id := range ids {
		if i >= len(tags) {
			break
		}

		if id == keepID {
			keep = edits.Card{ID: id, ETag: tags[i]}
		} else {
			others = append(others, edits.Card{ID: id, ETag: tags[i]})
		}
	}

	res, err := h.edit.Merge(book, keep, others, h.requester(r))
	if err != nil {
		http.Redirect(w, r, back+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)

		return
	}

	h.log.Info("contacts merged", "kept", res.Kept, "merged", res.Merged, "failed", res.Failed, "user", userFrom(r.Context()))

	q := "?merged=" + strconv.Itoa(len(res.Merged))
	if len(res.Failed) > 0 {
		var msgs []string
		for id, msg := range res.Failed {
			msgs = append(msgs, id+": "+msg)
		}

		slices.Sort(msgs)
		q += "&error=" + url.QueryEscape("merged, but could not delete "+strings.Join(msgs, "; "))
	}

	http.Redirect(w, r, h.prefix+"/contacts/"+url.PathEscape(book)+"/"+url.PathEscape(res.Kept)+q, http.StatusSeeOther)
}

type deletedRow struct {
	edits.Removed

	Book   string
	Device string
	Phone  string
}

func (h *Handler) deleted(w http.ResponseWriter, r *http.Request) {
	var rows []deletedRow

	for _, book := range h.store.Collections(store.AddressBook) {
		gone, err := h.edit.Deleted(book.Name())
		if err != nil {
			continue
		}

		for _, g := range gone {
			row := deletedRow{Removed: g, Book: book.Name(), Device: Device(g.Deleted.Requester), Phone: ""}

			if c, _, err := h.parse(book, g.Last); err == nil && c != nil {
				if t := c.Values("TEL"); len(t) > 0 {
					row.Phone = t[0].Value
				} else if org := c.Organization(); org != "" {
					row.Phone = org
				}
			}

			rows = append(rows, row)
		}
	}

	h.render(w, r, "deleted.html", map[string]any{
		"Title": "Deleted", "Rows": rows, "CanWrite": h.write != nil, "Failed": r.URL.Query().Get("error"),
	})
}

func (h *Handler) restoreDeleted(w http.ResponseWriter, r *http.Request) {
	if !h.canPost(w, r) {
		return
	}

	book, name := r.PostFormValue("book"), r.PostFormValue("name")

	if _, err := h.edit.RestoreDeleted(book, name, h.requester(r)); err != nil {
		http.Redirect(w, r, h.prefix+"/deleted?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)

		return
	}

	h.log.Info("deleted contact restored", "card", name, "user", userFrom(r.Context()))
	http.Redirect(w, r, h.prefix+"/contacts/"+url.PathEscape(book)+"/"+url.PathEscape(name)+"?back=1", http.StatusSeeOther)
}

// restoreVersion puts a card back as it was at an earlier revision.
func (h *Handler) restoreVersion(w http.ResponseWriter, r *http.Request) {
	if !h.canPost(w, r) {
		return
	}

	book, name := r.PathValue("book"), r.PathValue("name")
	back := h.prefix + "/contacts/" + url.PathEscape(book) + "/" + url.PathEscape(name)

	_, err := h.edit.RestoreVersion(book, name, r.PostFormValue("from"), r.PostFormValue("if_match"), h.requester(r))

	switch {
	case errors.Is(err, edits.ErrChanged):
		http.Redirect(w, r, back+"?error="+url.QueryEscape("the card changed since the page was loaded; look again and retry"), http.StatusSeeOther)
	case err != nil:
		http.Redirect(w, r, back+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
	default:
		h.log.Info("contact version restored", "card", name, "from", r.PostFormValue("from"), "user", userFrom(r.Context()))
		http.Redirect(w, r, back+"?version=1", http.StatusSeeOther)
	}
}

// fixAllPhones applies every phone fix lint suggests, in every address
// book.
func (h *Handler) fixAllPhones(w http.ResponseWriter, r *http.Request) {
	if !h.canPost(w, r) {
		return
	}

	cards, numbers := 0, 0

	var failed []string

	for _, book := range h.store.Collections(store.AddressBook) {
		res, err := h.edit.FixAllPhones(book.Name(), h.requester(r))
		if err != nil {
			failed = append(failed, err.Error())

			continue
		}

		for _, f := range res {
			if f.Error != "" {
				failed = append(failed, f.Name+": "+f.Error)

				continue
			}

			cards++
			numbers += len(f.Fixed)
		}
	}

	h.log.Info("phone numbers fixed", "cards", cards, "numbers", numbers, "failed", len(failed), "user", userFrom(r.Context()))

	q := "?phones=" + strconv.Itoa(numbers) + "&cards=" + strconv.Itoa(cards)
	if len(failed) > 0 {
		q += "&error=" + url.QueryEscape("not fixed: "+strings.Join(failed, "; "))
	}

	http.Redirect(w, r, h.prefix+"/lint"+q, http.StatusSeeOther)
}

// fixable counts the cards and numbers FixAllPhones would change.
func (h *Handler) fixable() (int, int) {
	cards, numbers := 0, 0

	for _, book := range h.store.Collections(store.AddressBook) {
		snap, err := book.Snapshot()
		if err != nil {
			continue
		}

		for _, o := range snap.Objects {
			if c, _, err := h.parse(book, o.ETag); err == nil && c != nil {
				if _, fixes := contact.FixPhones(*c, h.region); len(fixes) > 0 {
					cards++
					numbers += len(fixes)
				}
			}
		}
	}

	return cards, numbers
}
