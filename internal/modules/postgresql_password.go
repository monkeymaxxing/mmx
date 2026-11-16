package modules

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// How PostgreSQL stores a role's password, and how MMX computes that form
// itself.
//
// pg_authid.rolpassword holds nothing, a SCRAM-SHA-256 form, or the older
// md5 form. postgresql_role compares a task's password with the stored form
// without a login attempt, and it sets a password by sending the form, never
// the password: the server stores a valid form unchanged, so the clear
// password reaches no statement, no log, and no view of the server.
//
// The SCRAM form is
//
//	SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>
//
// with the salt and the two keys in Base64. The keys come from the password
// by RFC 5802, after the password went through SASLprep as PostgreSQL does
// it (postgresqlPreparedPassword). A client prepares the password in the
// same way when it logs in, so a form made from other bytes would refuse the
// login.

// postgresqlScramPrefix starts a SCRAM form.
const postgresqlScramPrefix = "SCRAM-SHA-256$"

// The iteration count and the salt length of the SCRAM forms that PostgreSQL
// itself makes. A server of version 16 or newer has the setting
// scram_iterations for the count; MMX uses that where it exists.
const (
	postgresqlScramIterations = 4096
	postgresqlScramSaltLength = 16
)

// postgresqlPreparedPassword is the bytes that PostgreSQL and its clients
// hash for a password: pg_saslprep in src/common/saslprep.c, which is the
// same in PostgreSQL 13 to 18, step by step.
//
//   - A password of ASCII bytes alone is taken as it is, also with control
//     characters.
//   - Any other password must be valid UTF-8. Each space that is not ASCII
//     becomes U+0020, and each character that RFC 3454 maps to nothing is
//     removed. What is left is normalized with NFKC.
//   - PostgreSQL then checks the text from before the normalization: it
//     must not be empty, must hold no prohibited character and no code point
//     that Unicode 3.2 does not assign, and must keep the rule for text of
//     both directions.
//   - Where a step refuses the text, the bytes as written are hashed,
//     unchanged: the server and its clients treat the outcome of SASLprep
//     as an attempt and never as an error.
func postgresqlPreparedPassword(password string) []byte {
	ascii := true
	for i := 0; i < len(password); i++ {
		if password[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii || !utf8.ValidString(password) {
		return []byte(password)
	}
	mapped := make([]rune, 0, len(password))
	for _, r := range password {
		switch {
		case saslprepNonASCIISpaces.has(r):
			mapped = append(mapped, ' ')
		case saslprepMappedToNothing.has(r):
		default:
			mapped = append(mapped, r)
		}
	}
	if len(mapped) == 0 {
		return []byte(password)
	}
	rightToLeft := false
	for _, r := range mapped {
		if saslprepProhibited.has(r) || saslprepUnassigned.has(r) {
			return []byte(password)
		}
		rightToLeft = rightToLeft || saslprepRightToLeft.has(r)
	}
	if rightToLeft {
		// Text with a right-to-left character holds no left-to-right
		// character, and starts and ends with a right-to-left character.
		for _, r := range mapped {
			if saslprepLeftToRight.has(r) {
				return []byte(password)
			}
		}
		if !saslprepRightToLeft.has(mapped[0]) || !saslprepRightToLeft.has(mapped[len(mapped)-1]) {
			return []byte(password)
		}
	}
	return norm.NFKC.Bytes([]byte(string(mapped)))
}

// postgresqlScramForm is the SCRAM-SHA-256 form of password with this salt
// and iteration count, as pg_authid.rolpassword holds it.
func postgresqlScramForm(password string, salt []byte, iterations int) (string, error) {
	salted, err := pbkdf2.Key(sha256.New, string(postgresqlPreparedPassword(password)), salt, iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	keyed := func(message string) []byte {
		mac := hmac.New(sha256.New, salted)
		mac.Write([]byte(message))
		return mac.Sum(nil)
	}
	storedKey := sha256.Sum256(keyed("Client Key"))
	text := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("%s%d:%s$%s:%s", postgresqlScramPrefix, iterations, text(salt), text(storedKey[:]), text(keyed("Server Key"))), nil
}

// newPostgresqlScramForm is a SCRAM-SHA-256 form of password with a new
// random salt, for MMX to send to the server. iterations is the server's
// scram_iterations, or 0 for a server that has no such setting.
func newPostgresqlScramForm(password string, iterations int) (string, error) {
	if iterations <= 0 {
		iterations = postgresqlScramIterations
	}
	salt := make([]byte, postgresqlScramSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return postgresqlScramForm(password, salt, iterations)
}

// postgresqlStoredFormHolds reports whether stored, a value of
// pg_authid.rolpassword, is a SCRAM-SHA-256 form of password: the form that
// MMX computes from the password with the stored salt and iteration count is
// the stored one. It is false for no stored password, and for an md5 form
// whatever password that holds, because MMX replaces an md5 form with a
// SCRAM form.
func postgresqlStoredFormHolds(stored, password string) bool {
	rest, found := strings.CutPrefix(stored, postgresqlScramPrefix)
	if !found {
		return false
	}
	salting, _, found := strings.Cut(rest, "$")
	if !found {
		return false
	}
	count, saltText, found := strings.Cut(salting, ":")
	if !found {
		return false
	}
	iterations, err := strconv.Atoi(count)
	if err != nil || iterations < 1 {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(saltText)
	if err != nil {
		return false
	}
	computed, err := postgresqlScramForm(password, salt, iterations)
	return err == nil && subtle.ConstantTimeCompare([]byte(computed), []byte(stored)) == 1
}

// postgresqlStoredFormIsMD5 reports whether stored is the older md5 form:
// "md5" and the 32 hexadecimal digits of MD5(password + role name).
func postgresqlStoredFormIsMD5(stored string) bool {
	digits, found := strings.CutPrefix(stored, "md5")
	return found && len(digits) == 32 && strings.Trim(digits, "0123456789abcdef") == ""
}
