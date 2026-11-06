package store

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// MetaFile is the per-collection metadata file. The name and format are
// the ones Xandikos uses, so both servers can share one store.
const MetaFile = ".xandikos"

// Meta is the collection metadata kept in MetaFile.
type Meta struct {
	sections []section
}

type section struct {
	name string
	keys []kv
}

type kv struct{ key, value string }

// ParseMeta reads an INI file in the Python configparser dialect that
// Xandikos writes.
func ParseMeta(data []byte) Meta {
	var (
		m   Meta
		cur *section
	)

	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())

		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			m.sections = append(m.sections, section{name: line[1 : len(line)-1], keys: nil})
			cur = &m.sections[len(m.sections)-1]
		default:
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				key, value, _ = strings.Cut(line, ":")
			}

			if cur == nil {
				m.sections = append(m.sections, section{name: "DEFAULT", keys: nil})
				cur = &m.sections[len(m.sections)-1]
			}

			cur.keys = append(cur.keys, kv{key: strings.TrimSpace(key), value: strings.TrimSpace(value)})
		}
	}

	return m
}

// Encode writes the metadata back in the same dialect.
func (m Meta) Encode() []byte {
	var b bytes.Buffer

	for i, s := range m.sections {
		if i > 0 {
			b.WriteString("\n")
		}

		b.WriteString("[" + s.name + "]\n")

		for _, k := range s.keys {
			b.WriteString(k.key + " = " + k.value + "\n")
		}
	}

	return b.Bytes()
}

// Get returns a value; section "" means DEFAULT.
func (m Meta) Get(sectionName, key string) string {
	if sectionName == "" {
		sectionName = "DEFAULT"
	}

	for _, s := range m.sections {
		if s.name != sectionName {
			continue
		}

		for _, k := range s.keys {
			if strings.EqualFold(k.key, key) {
				return k.value
			}
		}
	}

	return ""
}

// Set sets a value; an empty value removes the key.
func (m *Meta) Set(sectionName, key, value string) {
	if sectionName == "" {
		sectionName = "DEFAULT"
	}

	idx := -1

	for i, s := range m.sections {
		if s.name == sectionName {
			idx = i
		}
	}

	if idx < 0 {
		if value == "" {
			return
		}

		m.sections = append(m.sections, section{name: sectionName, keys: nil})
		idx = len(m.sections) - 1
	}

	s := &m.sections[idx]

	for i, k := range s.keys {
		if strings.EqualFold(k.key, key) {
			if value == "" {
				s.keys = append(s.keys[:i], s.keys[i+1:]...)
			} else {
				s.keys[i].value = value
			}

			return
		}
	}

	if value != "" {
		s.keys = append(s.keys, kv{key: key, value: value})
	}
}

// Type is "addressbook" or "calendar".
func (m Meta) Type() string { return m.Get("", "type") }

// DisplayName is the collection name shown by clients.
func (m Meta) DisplayName() string { return m.Get("", "displayname") }

// Description is the free-text description.
func (m Meta) Description() string { return m.Get("", "description") }

// Color is the calendar color, as #RRGGBB or #RRGGBBAA.
func (m Meta) Color() string { return m.Get("", "color") }

// Order is the calendar order shown by Apple clients.
func (m Meta) Order() int {
	n, _ := strconv.Atoi(m.Get("calendar", "order"))

	return n
}
