package main

import (
	"strings"
)

// MergeTransform merges a transform INI file into a spec INI file.
//
// Rules:
//   - Key=Value in transform: set only if the spec hasn't set this key (spec takes precedence)
//   - +Key=Value in transform: prepend this value before the spec's values (for multi-value keys)
//
// The result is a new INIFile. The originals are not modified.
func MergeTransform(spec, transform *INIFile) *INIFile {
	result := &INIFile{}

	// Track which transform sections we've already handled
	handledSections := map[string]bool{}

	// Process each section in the spec
	for _, specSec := range spec.Sections {
		if specSec.Name == "" && len(specSec.Entries) == 0 {
			continue
		}

		transSec := transform.GetSection(specSec.Name)
		if transSec == nil {
			// No transform for this section, keep spec as-is
			result.Sections = append(result.Sections, cloneSection(specSec))
			handledSections[specSec.Name] = true
			continue
		}
		handledSections[specSec.Name] = true

		merged := mergeSection(specSec, *transSec)
		result.Sections = append(result.Sections, merged)
	}

	// Add transform sections that aren't in the spec
	for _, transSec := range transform.Sections {
		if transSec.Name == "" && len(transSec.Entries) == 0 {
			continue
		}
		if handledSections[transSec.Name] {
			continue
		}
		// Strip + prefixes from keys
		cleaned := Section{Name: transSec.Name}
		for _, e := range transSec.Entries {
			if e.Key != "" && strings.HasPrefix(e.Key, "+") {
				cleaned.Entries = append(cleaned.Entries, Entry{Key: e.Key[1:], Value: e.Value})
			} else {
				cleaned.Entries = append(cleaned.Entries, e)
			}
		}
		result.Sections = append(result.Sections, cleaned)
	}

	return result
}

// mergeSection merges a single section from spec and transform.
func mergeSection(spec, transform Section) Section {
	merged := Section{Name: spec.Name}

	// Collect prepend entries (+Key=Value) from transform
	var prepends []Entry
	for _, e := range transform.Entries {
		if e.Key != "" && strings.HasPrefix(e.Key, "+") {
			prepends = append(prepends, Entry{Key: e.Key[1:], Value: e.Value})
		}
	}

	// Add prepend entries first
	merged.Entries = append(merged.Entries, prepends...)

	// Add all spec entries
	merged.Entries = append(merged.Entries, spec.Entries...)

	// Add transform defaults (non-prepend entries where spec doesn't have the key)
	for _, e := range transform.Entries {
		if e.Key == "" || strings.HasPrefix(e.Key, "+") {
			continue // skip comments/blanks and prepend entries
		}
		if !sectionHasKey(merged, e.Key) {
			merged.Entries = append(merged.Entries, e)
		}
	}

	return merged
}

func sectionHasKey(sec Section, key string) bool {
	for _, e := range sec.Entries {
		if e.Key == key {
			return true
		}
	}
	return false
}

func cloneSection(s Section) Section {
	c := Section{Name: s.Name, Entries: make([]Entry, len(s.Entries))}
	copy(c.Entries, s.Entries)
	return c
}
