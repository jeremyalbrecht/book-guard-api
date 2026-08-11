package books

import (
	"errors"
	"regexp"
	"strings"
)

// ErrInvalidISBN means the client sent something that cannot be an ISBN. It is a
// sentinel so the HTTP layer can map it to a 422 without matching on strings.
var ErrInvalidISBN = errors.New("isbn must be 10 or 13 characters once separators are removed")

// An ISBN-10 may end in a check character 'X'; an ISBN-13 is all digits.
var isbnShape = regexp.MustCompile(`^([0-9]{9}[0-9X]|[0-9]{13})$`)

// stripISBNSeparators removes what barcode scanners and people add: the hyphens
// printed on the back of the book, and stray whitespace from a paste.
func stripISBNSeparators(raw string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '-', ' ', '\t', ' ':
			return -1
		}
		return r
	}, raw)
}

// canonicalISBN turns client input into the form stored in books.isbn, which is
// also the key of the shared editions and book_covers rows. Two spellings of the
// same ISBN would otherwise create two editions and fetch Open Library twice.
//
// It checks the shape only — length and character set. It deliberately does NOT
// verify the check digit or convert ISBN-10 to ISBN-13.
//
// This is the single seam for ISBN handling: an ISBN-10 -> ISBN-13 normalisation
// belongs right here, between the shape check and the return, and nothing else
// in the codebase has to change when it lands.
func canonicalISBN(raw string) (string, error) {
	isbn := strings.ToUpper(stripISBNSeparators(raw))
	if !isbnShape.MatchString(isbn) {
		return "", ErrInvalidISBN
	}
	return isbn, nil
}
