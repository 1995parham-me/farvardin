# Roadmap

What is planned, roughly in order. Each item says why it matters.

## Next

- **Restore a photo from history.** The web UI already shows each card's history; a button that puts back the last full-size photo would repair cards that lost theirs to a thumbnail before the photo guard existed.
- **Editing in the web UI**, written through the same validated PUT path as clients, so the UI cannot store what a client could not.
- **Contact groups as first-class objects:** list `KIND:group` and `X-ADDRESSBOOKSERVER-KIND:group` cards with their members in the UI and in JSContact.

## Later

- **Scheduling** (RFC 6638) for invitations between principals on the same server, and iMIP for the rest.
- **Several principals** with their own stores and shared collections.
- **Full RRULE expansion** for `calendar-query` and the `expand` element of `calendar-data`. Today recurring events are matched as a safe superset, which every tested client accepts.
- **`free-busy-query`** and VAVAILABILITY (RFC 7953).
- **Accepting JSContact on PUT**, stored as vCard 4.0.

## Not planned

- **Contact Posters.** iOS keeps them in iCloud only; nothing about them reaches a CardDAV server except the avatar (`PHOTO`) and `X-SHARED-PHOTO-DISPLAY-PREF`.
- **Collection deletion over DAV.** A client that deletes a collection by mistake would empty every phone that syncs it. Remove the directory on the server instead.
