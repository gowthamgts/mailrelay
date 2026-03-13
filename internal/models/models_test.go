package models

import (
	"testing"
)

func TestFromDomain(t *testing.T) {
	tests := []struct {
		name         string
		envelopeFrom string
		want         string
	}{
		{"normal email", "user@Example.COM", "example.com"},
		{"no at sign", "localonly", ""},
		{"empty string", "", ""},
		{"multiple at signs", "user@sub@example.com", "sub@example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &ParsedEmail{EnvelopeFrom: tt.envelopeFrom}
			got := e.FromDomain()
			if got != tt.want {
				t.Errorf("FromDomain() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToDomains(t *testing.T) {
	tests := []struct {
		name       string
		envelopeTo []string
		want       []string
	}{
		{
			"multiple addresses different domains",
			[]string{"a@foo.com", "b@bar.com"},
			[]string{"foo.com", "bar.com"},
		},
		{
			"deduplication",
			[]string{"a@foo.com", "b@FOO.COM", "c@bar.com"},
			[]string{"foo.com", "bar.com"},
		},
		{
			"no at sign skipped",
			[]string{"localonly", "a@foo.com"},
			[]string{"foo.com"},
		},
		{
			"empty list",
			nil,
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &ParsedEmail{EnvelopeTo: tt.envelopeTo}
			got := e.ToDomains()
			if len(got) != len(tt.want) {
				t.Fatalf("ToDomains() returned %d domains, want %d: %v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ToDomains()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
