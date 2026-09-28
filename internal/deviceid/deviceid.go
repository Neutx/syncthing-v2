// Package deviceid computes and parses Syncthing device IDs.
//
// A device ID is the SHA-256 of the device's TLS certificate (DER), encoded as
// base32 without padding (52 characters), split into four groups of 13 that
// each get a Luhn mod-32 check character (56 characters), and finally written
// as eight groups of seven separated by dashes. This matches upstream
// Syncthing's lib/protocol DeviceID.String.
package deviceid

import (
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
)

// alphabet is the RFC 4648 base32 alphabet used by Syncthing.
const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

const (
	rawLen    = 52 // base32 of 32 bytes, unpadded
	luhnLen   = 56 // rawLen plus four check characters
	groupSize = 13 // characters per Luhn group
	chunkSize = 7  // characters per dash-separated chunk
)

var enc = base32.StdEncoding.WithPadding(base32.NoPadding)

// ErrInvalid is returned (wrapped) by Parse for strings that are not device IDs.
var ErrInvalid = errors.New("invalid device ID")

// FromCert returns the canonical device ID of a DER-encoded certificate.
func FromCert(der []byte) string {
	sum := sha256.Sum256(der)
	return format(sum[:])
}

// Short returns the first seven characters of id, the short form Syncthing
// shows in its UI. Shorter input is returned unchanged.
func Short(id string) string {
	if len(id) <= chunkSize {
		return id
	}
	return id[:chunkSize]
}

// Parse accepts a device ID in any form upstream Syncthing accepts (with or
// without dashes or spaces, any case, with or without the Luhn characters,
// and with the look-alike digits 0, 1 and 8 typed for O, I and B) and returns
// the canonical form. Luhn check characters, when present, must be correct.
func Parse(s string) (string, error) {
	norm := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(s)))
	norm = strings.NewReplacer("0", "O", "1", "I", "8", "B").Replace(norm)
	switch len(norm) {
	case luhnLen:
		raw, err := unluhnify(norm)
		if err != nil {
			return "", err
		}
		norm = raw
	case rawLen:
	default:
		return "", fmt.Errorf("%w: length %d", ErrInvalid, len(norm))
	}
	b, err := enc.DecodeString(norm)
	if err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("%w: not base32", ErrInvalid)
	}
	return format(b), nil
}

// Valid reports whether s is a device ID in canonical form.
func Valid(s string) bool {
	c, err := Parse(s)
	return err == nil && c == s
}

func format(sum []byte) string {
	return chunkify(luhnify(enc.EncodeToString(sum)))
}

// luhnify appends a check character after every group of 13 characters.
func luhnify(s string) string {
	var b strings.Builder
	b.Grow(luhnLen)
	for i := 0; i < rawLen; i += groupSize {
		g := s[i : i+groupSize]
		b.WriteString(g)
		b.WriteByte(luhn32(g))
	}
	return b.String()
}

// unluhnify verifies and strips the four check characters.
func unluhnify(s string) (string, error) {
	var b strings.Builder
	b.Grow(rawLen)
	for i := 0; i < luhnLen; i += groupSize + 1 {
		g := s[i : i+groupSize]
		for j := 0; j < len(g); j++ {
			if strings.IndexByte(alphabet, g[j]) < 0 {
				return "", fmt.Errorf("%w: character %q", ErrInvalid, g[j])
			}
		}
		if luhn32(g) != s[i+groupSize] {
			return "", fmt.Errorf("%w: check character mismatch", ErrInvalid)
		}
		b.WriteString(g)
	}
	return b.String(), nil
}

func chunkify(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/chunkSize)
	for i := 0; i < len(s); i += chunkSize {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(s[i : i+chunkSize])
	}
	return b.String()
}

// luhn32 is upstream Syncthing's Luhn mod-32 check over the base32 alphabet.
// Every character of s must be in alphabet.
func luhn32(s string) byte {
	const n = 32
	factor, sum := 1, 0
	for i := 0; i < len(s); i++ {
		addend := factor * strings.IndexByte(alphabet, s[i])
		if factor == 2 {
			factor = 1
		} else {
			factor = 2
		}
		sum += addend/n + addend%n
	}
	return alphabet[(n-sum%n)%n]
}
