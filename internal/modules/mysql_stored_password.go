package modules

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
)

// How MySQL and MariaDB store an account's password, for mysql_user.
//
// The servers keep no password, only a value computed from it: the stored
// value. mysql_user computes the same value from the task's password and
// compares the two, so it never has to log in to learn whether a password is
// the one that the task names, and it sends the server only the stored value,
// so the password itself reaches no statement, no log of the server, and no
// command line.
//
// MMX knows the two authentication methods that the servers use for a
// password by default:
//
//   - mysql_native_password, MariaDB's method for a new account: an asterisk
//     and the 40 hexadecimal digits of SHA1(SHA1(password)). It has no salt,
//     so one password always gives one stored value.
//   - caching_sha2_password, MySQL's method for a new account: the text
//     $A$005$, 20 bytes of salt, and 43 characters of a digest. 005 is the
//     number of rounds divided by 1000, as three hexadecimal digits. The
//     digest is the one of crypt(3)'s SHA-256 method ($5$), computed with the
//     whole 20 bytes of salt; crypt(3) itself would use only 16 of them, so
//     MMX computes the digest itself.

const (
	mysqlNativePasswordMethod = "mysql_native_password"
	mysqlCachingSHA2Method    = "caching_sha2_password"
	// mysqlCachingSHA2SaltLength is the number of salt bytes in a stored
	// value of caching_sha2_password.
	mysqlCachingSHA2SaltLength = 20
	// mysqlCachingSHA2DefaultRounds is the number of rounds of a server that
	// has no setting for it.
	mysqlCachingSHA2DefaultRounds = 5000
	// The stored form holds the rounds divided by 1000 in three hexadecimal
	// digits.
	mysqlCachingSHA2RoundsUnit = 1000
	mysqlCachingSHA2MostRounds = 0xFFF * mysqlCachingSHA2RoundsUnit
	mysqlCachingSHA2DigestSize = 43
	// mysqlRedacted stands where a stored value was, as [redacted] stands
	// where the node task execution engine replaced a login.
	mysqlRedacted = "[redacted]"
)

// mysqlNativePassword is the stored value of password for the method
// mysql_native_password.
func mysqlNativePassword(password string) string {
	first := sha1.Sum([]byte(password))
	second := sha1.Sum(first[:])
	return fmt.Sprintf("*%X", second[:])
}

// mysqlCachingSHA2Password is the stored value of password for the method
// caching_sha2_password, with that salt and number of rounds.
func mysqlCachingSHA2Password(password string, salt []byte, rounds int) string {
	return fmt.Sprintf("$A$%03X$%s%s", rounds/mysqlCachingSHA2RoundsUnit, salt, sha256CryptDigest([]byte(password), salt, rounds))
}

// mysqlCachingSHA2Parts reads the salt and the number of rounds out of a
// stored value of caching_sha2_password. known is false for a value in a
// form that MMX does not know.
func mysqlCachingSHA2Parts(stored []byte) (salt []byte, rounds int, known bool) {
	const prefix = len("$A$005$")
	if len(stored) != prefix+mysqlCachingSHA2SaltLength+mysqlCachingSHA2DigestSize || !bytes.HasPrefix(stored, []byte("$A$")) || stored[prefix-1] != '$' {
		return nil, 0, false
	}
	thousands, err := strconv.ParseUint(string(stored[3:6]), 16, 16)
	if err != nil || thousands == 0 {
		return nil, 0, false
	}
	return stored[prefix : prefix+mysqlCachingSHA2SaltLength], int(thousands) * mysqlCachingSHA2RoundsUnit, true
}

// mysqlCachingSHA2Rounds is the number of rounds that a new stored value
// gets on a server whose setting caching_sha2_password_digest_rounds is
// setting; "" is a server without the setting. The stored form counts in
// thousands.
func mysqlCachingSHA2Rounds(setting string) int {
	rounds, err := strconv.Atoi(setting)
	if err != nil || rounds < mysqlCachingSHA2DefaultRounds {
		return mysqlCachingSHA2DefaultRounds
	}
	if rounds > mysqlCachingSHA2MostRounds {
		return mysqlCachingSHA2MostRounds
	}
	return rounds / mysqlCachingSHA2RoundsUnit * mysqlCachingSHA2RoundsUnit
}

// lettersAndDigits are the characters of a salt that MMX makes.
const lettersAndDigits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// newMySQLSalt makes the salt of a new stored value of
// caching_sha2_password from letters and digits. The servers' own salts hold
// any byte from 1 to 127, among them quotes, backslashes, and control
// characters. A stored value with a salt of letters and digits is one plain
// quoted string in a statement, which both products read the same in every
// SQL mode.
func newMySQLSalt() ([]byte, error) {
	salt := make([]byte, mysqlCachingSHA2SaltLength)
	random := make([]byte, 1)
	for i := 0; i < len(salt); {
		if _, err := rand.Read(random); err != nil {
			return nil, err
		}
		// 248 is the largest multiple of 62 below 256: a byte above it would
		// make the first characters more likely than the others.
		if int(random[0]) < 248 {
			salt[i] = lettersAndDigits[int(random[0])%len(lettersAndDigits)]
			i++
		}
	}
	return salt, nil
}

// sha256CryptDigest is the digest of crypt(3)'s SHA-256 method as Ulrich
// Drepper specified it ("Unix crypt using SHA-256 and SHA-512"), with the
// whole salt, whatever its length, and without the method's prefix.
func sha256CryptDigest(password, salt []byte, rounds int) string {
	sum := func(parts ...[]byte) []byte {
		digest := sha256.New()
		for _, part := range parts {
			digest.Write(part)
		}
		return digest.Sum(nil)
	}
	// repeated is the first length bytes of block written again and again.
	repeated := func(block []byte, length int) []byte {
		out := make([]byte, 0, length)
		for len(out) < length {
			out = append(out, block[:min(len(block), length-len(out))]...)
		}
		return out
	}
	alternate := sum(password, salt, password)
	first := sha256.New()
	first.Write(password)
	first.Write(salt)
	first.Write(repeated(alternate, len(password)))
	for length := len(password); length > 0; length >>= 1 {
		if length&1 == 1 {
			first.Write(alternate)
		} else {
			first.Write(password)
		}
	}
	current := first.Sum(nil)
	passwordBlock := repeated(sum(bytes.Repeat(password, len(password))), len(password))
	saltBlock := repeated(sum(bytes.Repeat(salt, 16+int(current[0]))), len(salt))
	for round := 0; round < rounds; round++ {
		digest := sha256.New()
		if round&1 == 1 {
			digest.Write(passwordBlock)
		} else {
			digest.Write(current)
		}
		if round%3 != 0 {
			digest.Write(saltBlock)
		}
		if round%7 != 0 {
			digest.Write(passwordBlock)
		}
		if round&1 == 1 {
			digest.Write(current)
		} else {
			digest.Write(passwordBlock)
		}
		current = digest.Sum(nil)
	}
	// crypt(3) writes the 32 bytes in this order, three bytes as four
	// characters, lowest six bits first.
	const alphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var text strings.Builder
	write := func(high, middle, low byte, characters int) {
		bits := uint(high)<<16 | uint(middle)<<8 | uint(low)
		for ; characters > 0; characters-- {
			text.WriteByte(alphabet[bits&0x3f])
			bits >>= 6
		}
	}
	for _, group := range [][3]int{{0, 10, 20}, {21, 1, 11}, {12, 22, 2}, {3, 13, 23}, {24, 4, 14}, {15, 25, 5}, {6, 16, 26}, {27, 7, 17}, {18, 28, 8}, {9, 19, 29}} {
		write(current[group[0]], current[group[1]], current[group[2]], 4)
	}
	write(0, current[31], current[30], 3)
	return text.String()
}

// mysqlStoredValuePartLength is the shortest piece of a stored value that
// withoutMySQLStoredValue takes out of a text wherever it stands. A shorter
// piece is taken out where it follows a quote, which is where the servers
// cut a statement.
const mysqlStoredValuePartLength = 6

// withoutMySQLStoredValue returns text without the stored value, whole or in
// part. text is what the database's client wrote to standard error after a
// statement that carried the stored value failed.
//
// The whole value is there when MariaDB's client prints the statement that
// failed. A part is there when a server quotes the statement in a syntax
// error: it quotes at most 80 characters from the word that it did not
// understand, which can end inside the value, and MariaDB puts three dots
// behind what it cut. Replacing the exact value would leave such a part. So
// this function takes out:
//
//   - every piece of the value that is mysqlStoredValuePartLength characters
//     or longer, wherever it stands;
//   - every beginning of the value, however short, that follows a single
//     quote, because a statement of MMX holds the value as '<value>' and a
//     cut statement therefore ends with a quote and the value's beginning.
func withoutMySQLStoredValue(text, stored string) string {
	if stored == "" || text == "" {
		return text
	}
	covered := make([]bool, len(text))
	// sharedLength is the length of what text from at and stored from from
	// have in common.
	sharedLength := func(at, from int) int {
		length := 0
		for at+length < len(text) && from+length < len(stored) && text[at+length] == stored[from+length] {
			length++
		}
		return length
	}
	found := false
	cover := func(at, length int) {
		for i := at; i < at+length; i++ {
			covered[i] = true
		}
		found = found || length > 0
	}
	for at := range text {
		for from := range stored {
			if length := sharedLength(at, from); length >= mysqlStoredValuePartLength {
				cover(at, length)
			}
		}
		if text[at] == '\'' {
			cover(at+1, sharedLength(at+1, 0))
		}
	}
	if !found {
		return text
	}
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		if !covered[i] {
			out.WriteByte(text[i])
			continue
		}
		out.WriteString(mysqlRedacted)
		for i+1 < len(text) && covered[i+1] {
			i++
		}
	}
	return out.String()
}
