# farvardin

A CardDAV and CalDAV server written in Go that keeps every contact and event in git. It reads and writes the same store layout as [Xandikos](https://github.com/jelmer/xandikos), so you can point it at an existing Xandikos store and your phones keep syncing without a full re-download.

The name: Xandikos is named after a spring month of the ancient Macedonian calendar. Its Persian counterpart is **Farvardin** (فروردین), the first month of the Iranian year.

## Features

**Protocols**

- WebDAV (RFC 4918), CardDAV (RFC 6352) and CalDAV (RFC 4791), with discovery through `/.well-known/carddav` and `/.well-known/caldav`.
- WebDAV sync (RFC 6578) `sync-collection` with real deltas, including deletions. The sync token is the git commit and the ETag is the git blob id. Both are identical to Xandikos, so clients moving between the two servers keep their ETags.
- `addressbook-multiget`, `addressbook-query` (text-match with the four match types and both case-insensitive collations), `calendar-multiget` and `calendar-query` (component filters and time ranges, including recurring events).
- `getctag`, `calendar-color`, `calendar-order`, `supported-calendar-component-set`, PROPPATCH (atomic), MKCOL and MKCALENDAR.
- `no-uid-conflict` and `valid-address-data` / `valid-calendar-data` preconditions, so a broken object is refused instead of stored.

**Contacts**

- **vCard 3.0 and 4.0.** Cards are stored as the client wrote them. A client that asks for the other version (through `address-data` in a report or `Accept: text/vcard; version=4.0`) gets a converted copy. The conversion maps the vCard 4.0 / RFC 9554 properties to the extensions Apple and DAVx5 use: `KIND`/`MEMBER` and `X-ADDRESSBOOKSERVER-*`, `SOCIALPROFILE` and `X-SOCIALPROFILE`, `ANNIVERSARY` and Apple's labelled `X-ABDATE`, partial dates (`--0316`) and Apple's `1604` year.
- **JSContact (RFC 9553 / RFC 9555).** `GET` with `Accept: application/jscontact+json` (or `?format=jscontact`) returns the card as JSContact, with anything unmapped kept in `vCardProps`.
- **Photo policy on every write:**
  - *Guard:* when a client sends a thumbnail (96×96, say) of a photo the server holds at full size, the server keeps the full one. Android's contact provider is known to hand back thumbnails.
  - *Stabilize:* when a client sends the same picture encoded again, the stored bytes stay. Two clients that re-encode one photo back and forth no longer make a commit a day.
  - *Scale:* photos larger than `photo_max_size` are scaled down once.
  
  When the server changes what a client sent, it omits the ETag in the reply, so the client fetches the stored version (RFC 6352 section 6.3.2.3).
- **Byte-exact storage.** Lines the server did not edit keep their exact bytes, folding and spelling, so git diffs show only real changes.
- **Validation** on every write and from the command line (`farvardin lint`).

**Calendars**

- A **birthdays calendar**, generated live from the address book: one yearly all-day event per birthday and anniversary, with an optional reminder. It updates the moment a card changes and syncs like any other calendar, read-only.
- `GET` on a calendar collection returns one `.ics` with every event: a webcal feed for clients that only subscribe.

**Around the server**

- A read-only **web UI** at `/ui/`: dashboard with the next 30 days of birthdays and recent changes, contact search, each contact with its photo, fields, git history and downloads (3.0, 4.0, JSContact), upcoming events, and a lint report. No JavaScript, no external assets, dark mode.
- **Prometheus metrics** at `/metrics`: requests, writes, refused writes by reason, photo policy actions, objects per collection.
- **Authentication**: none (behind a proxy), bcrypt basic auth, or a trusted proxy header.
- One static binary; commits go straight into the git object database, so a write costs the same with ten contacts or ten thousand.

## Quick start

```bash
go install github.com/1995parham-me/farvardin/cmd/farvardin@latest
farvardin serve --root ./data --listen 127.0.0.1:8000
```

Then add an account in your client with the server `http://127.0.0.1:8000/`. Discovery finds the principal at `/user/`, the address book at `/user/contacts/addressbook/` and the calendar at `/user/calendars/calendar/`. Open `http://127.0.0.1:8000/ui/` in a browser.

For a real deployment, copy [`configs/config.example.toml`](configs/config.example.toml) to `/etc/farvardin/config.toml` and use [`deployments/systemd/farvardin.service`](deployments/systemd/farvardin.service). Each tagged release publishes archives and `.deb`/`.rpm` packages for Linux and macOS on amd64 and arm64 (a Raspberry Pi works well), and container images at `ghcr.io/1995parham-me/farvardin`.

## Moving from Xandikos

1. Stop Xandikos.
2. Set `store.root` to the Xandikos root (the directory that holds `user/`) and keep `store.principal = "user"`. farvardin reads the `.xandikos` metadata files and the git history as they are.
3. Start farvardin on the same address.

Clients keep their ETags because both servers use the git blob id. Sync tokens differ in format, so each client does one full sync (a listing of ETags, not a download of every card) and then continues incrementally. Moving back works the same way.

## Command line

```text
farvardin serve          run the server (--listen, --root override the config)
farvardin lint DIR|FILE  check vCards; exits 1 when a card has errors (-w for warnings)
farvardin convert FILE   convert vCards: --to 3.0 | 4.0 | jscontact
farvardin hash-password  print a bcrypt hash for [auth.users]
```

Every config key can also be set from the environment: `FARVARDIN_SERVER__LISTEN=:8000`, `FARVARDIN_STORE__ROOT=/srv/dav`.

## Store layout

```text
<root>/<principal>/contacts/<address book>/   a git repository
    .xandikos                                 metadata (displayname, color, ...)
    <uid>.vcf
<root>/<principal>/calendars/<calendar>/      a git repository
    <uid>.ics
```

Each write is one commit on the collection's branch (`Added`, `Modified` or `Deleted <file>`, with the client's User-Agent as `Requester:`), moved with a compare-and-swap. The checked-out files and the index are kept in step, so `git status` stays clean and you can push the repositories anywhere as a backup.

## Development

```bash
just test   # go test -race
just lint   # golangci-lint
just dev    # build and serve ./data
```

See [docs/ROADMAP.md](docs/ROADMAP.md) for what is next.

## License

Apache-2.0.
