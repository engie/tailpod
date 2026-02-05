package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Entry is a single line within an INI section: a key=value pair or a comment/blank line.
type Entry struct {
	Key   string // empty for comments and blank lines
	Value string
	Raw   string // original line text (used for comments/blanks)
}

// Section is a named group of entries in an INI file.
type Section struct {
	Name    string
	Entries []Entry
}

// INIFile represents a parsed INI file as an ordered list of sections.
// Lines before the first [Section] header go into a section with Name="".
type INIFile struct {
	Sections []Section
}

// ParseINI parses a Quadlet-style INI file from a reader.
// It handles [Section] headers, Key=Value lines, # comments, and blank lines.
func ParseINI(r io.Reader) (*INIFile, error) {
	f := &INIFile{}
	current := Section{Name: ""}
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Section header
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			f.Sections = append(f.Sections, current)
			current = Section{Name: trimmed[1 : len(trimmed)-1]}
			continue
		}

		// Comment or blank line
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			current.Entries = append(current.Entries, Entry{Raw: line})
			continue
		}

		// Key=Value (split on first =)
		if idx := strings.Index(line, "="); idx >= 0 {
			key := strings.TrimSpace(line[:idx])
			value := strings.TrimSpace(line[idx+1:])
			current.Entries = append(current.Entries, Entry{Key: key, Value: value})
		} else {
			// Bare line (no = sign), preserve as-is
			current.Entries = append(current.Entries, Entry{Raw: line})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parsing INI: %w", err)
	}
	f.Sections = append(f.Sections, current)
	return f, nil
}

// String renders the INI file back to text.
func (f *INIFile) String() string {
	var b strings.Builder
	wroteSection := false
	for _, sec := range f.Sections {
		// Skip empty preamble section
		if sec.Name == "" && len(sec.Entries) == 0 {
			continue
		}
		if sec.Name != "" {
			if wroteSection && !strings.HasSuffix(b.String(), "\n\n") {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "[%s]\n", sec.Name)
		}
		for _, e := range sec.Entries {
			if e.Key != "" {
				fmt.Fprintf(&b, "%s=%s\n", e.Key, e.Value)
			} else {
				b.WriteString(e.Raw)
				b.WriteString("\n")
			}
		}
		wroteSection = true
	}
	return b.String()
}

// GetSection returns the section with the given name, or nil if not found.
func (f *INIFile) GetSection(name string) *Section {
	for i := range f.Sections {
		if f.Sections[i].Name == name {
			return &f.Sections[i]
		}
	}
	return nil
}

// HasKey returns true if the section contains at least one entry with the given key.
func (s *Section) HasKey(key string) bool {
	for _, e := range s.Entries {
		if e.Key == key {
			return true
		}
	}
	return false
}
