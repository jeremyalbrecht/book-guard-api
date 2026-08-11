package books

import (
	"errors"
	"testing"
)

func TestCanonicalISBN(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"isbn-13 as scanned", "9782070368224", "9782070368224"},
		{"isbn-13 with hyphens", "978-2-07-036822-4", "9782070368224"},
		{"isbn-13 with spaces", "978 2 07 036822 4", "9782070368224"},
		{"isbn-10", "0140328726", "0140328726"},
		{"isbn-10 ending in x is upper-cased", " 080442957x ", "080442957X"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := canonicalISBN(tt.in)
			if err != nil {
				t.Fatalf("canonicalISBN(%q) returned error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("canonicalISBN(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCanonicalISBNRejectsBadShapes(t *testing.T) {
	// Every one of these would create a book nothing could ever enrich.
	bad := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"separators only", "- -"},
		{"too short", "123"},
		{"eleven digits", "97820703682"},
		{"fourteen digits", "97820703682245"},
		{"x in the middle of an isbn-13", "978207036822X"},
		{"letters", "not-an-isbn"},
	}

	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			got, err := canonicalISBN(tt.in)
			if !errors.Is(err, ErrInvalidISBN) {
				t.Fatalf("canonicalISBN(%q) = %q, %v; want ErrInvalidISBN", tt.in, got, err)
			}
		})
	}
}
