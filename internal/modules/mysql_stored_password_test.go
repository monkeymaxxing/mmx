package modules

import (
	"encoding/hex"
	"strings"
	"testing"
)

// The digests of the specification's own examples ("Unix crypt using SHA-256
// and SHA-512"), which OpenSSL's passwd -5 gives too, and one password that
// is not ASCII.
func TestSHA256CryptDigest(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		password, salt string
		rounds         int
		want           string
	}{
		{"Hello world!", "saltstring", 5000, "5B8vYYiY.CVt1RlTTf8KbXBH3hsxY/GNooZaBBGWEc5"},
		{"Hello world!", "saltstringsaltst", 10000, "3xv.VbSHBb41AL9AvLeujZkZRBAwqFMz2.opqey6IcA"},
		{"This is just a test", "toolongsaltstrin", 5000, "Un/5jzAHMgOGZ5.mWJpuVolil07guHPvOW8mGRcvxa5"},
		{"a very much longer text to encrypt.  This one even stretches over morethan one line.", "anotherlongsalts", 1400, "Rx.j8H.h8HjEDGomFU8bDkXm3XIUnzyxf12oP84Bnq1"},
		{"we have a short salt string but not a short password", "short", 77777, "JiO1O3ZpDAxGJeaDIuqCoEFysAe1mZNJRs3pw0KQRd/"},
		{"pässwörd-日本", "Unicode0salt", 10000, "zhmH7LD9bgClNbphJ.vd85RgW9P688a16v7qYVcIF68"},
	} {
		if got := sha256CryptDigest([]byte(c.password), []byte(c.salt), c.rounds); got != c.want {
			t.Errorf("%q with the salt %q and %d rounds: %s, want %s", c.password, c.salt, c.rounds, got, c.want)
		}
	}
}

// The stored values that real servers made from these passwords: MariaDB
// 10.5 to 13.0 and MySQL 8.0 for mysql_native_password, MySQL 8.0 to 9.7 for
// caching_sha2_password with a salt of 20 bytes.
func TestMySQLStoredValues(t *testing.T) {
	t.Parallel()
	if got := mysqlNativePassword("Probe-pass-1x"); got != "*AFECD3821D5AD859BAF4C7243AFF2EED3703E9EC" {
		t.Fatal(got)
	}
	if got := mysqlNativePassword("password"); got != "*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E19" {
		t.Fatal(got)
	}
	// HEX(authentication_string) as MySQL 8.0.46 and 9.7.1 printed it for an
	// account that MMX's value made, after a login with the password.
	storedInHexadecimal := "244124303035244D6D7853616C74416263646566676830313233346D3679646A77452F2F4C4E4571493345596C6B446E674730496A73412E453876696B7558626D33524A5030"
	decoded, err := hex.DecodeString(storedInHexadecimal)
	if err != nil {
		t.Fatal(err)
	}
	stored := string(decoded)
	if got := mysqlCachingSHA2Password("Probe-pass-1x", []byte("MmxSaltAbcdefgh01234"), 5000); got != stored {
		t.Fatal(got)
	}
	salt, rounds, known := mysqlCachingSHA2Parts([]byte(stored))
	if !known || string(salt) != "MmxSaltAbcdefgh01234" || rounds != 5000 {
		t.Fatal(string(salt), rounds, known)
	}
	// MySQL 9.7 writes 10000 rounds as 00A, and a server's own salt holds
	// any byte from 1 to 127.
	ownSalt := []byte("\x4D\x13\x66\x08\x23\x15\x5E\x07\x1C\x76\x3B\x43\x1B\x25\x1D\x48\x78\x43\x6C\x27")
	value := mysqlCachingSHA2Password("pa'ss\\word\"x", ownSalt, 10000)
	if !strings.HasPrefix(value, "$A$00A$"+string(ownSalt)) || len(value) != 70 {
		t.Fatalf("%q", value)
	}
	salt, rounds, known = mysqlCachingSHA2Parts([]byte(value))
	if !known || string(salt) != string(ownSalt) || rounds != 10000 {
		t.Fatal(salt, rounds, known)
	}
	for _, unknown := range []string{"", "*AFECD3821D5AD859BAF4C7243AFF2EED3703E9EC", "$B$005$" + stored[7:], "$A$000$" + stored[7:], "$A$0x5$" + stored[7:], stored + "x", stored[:69], "$A$005x" + stored[7:]} {
		if _, _, known := mysqlCachingSHA2Parts([]byte(unknown)); known {
			t.Errorf("%q counts as a stored value of caching_sha2_password", unknown)
		}
	}
}

func TestMySQLCachingSHA2Rounds(t *testing.T) {
	t.Parallel()
	for setting, want := range map[string]int{"": 5000, "5000": 5000, "10000": 10000, "7999": 7000, "1": 5000, "many": 5000, "4095000": 4095000, "99999999": 4095000} {
		if got := mysqlCachingSHA2Rounds(setting); got != want {
			t.Errorf("the setting %q gives %d rounds, want %d", setting, got, want)
		}
	}
}

// A new salt is 20 letters and digits, so a stored value with it is one
// plain quoted string, and two salts differ.
func TestNewMySQLSalt(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 50 {
		salt, err := newMySQLSalt()
		if err != nil || len(salt) != 20 || strings.Trim(string(salt), lettersAndDigits) != "" {
			t.Fatalf("%q %v", salt, err)
		}
		seen[string(salt)] = true
	}
	if len(seen) != 50 {
		t.Fatal("two of 50 salts are equal")
	}
}

// What a client prints after a statement with a stored value failed must
// hold no part of the value. The texts are what MariaDB 10.5 and 11.8 and
// MySQL 8.0 and 9.7 printed for such statements.
func TestWithoutMySQLStoredValue(t *testing.T) {
	t.Parallel()
	const sha2 = "$A$005$MmxSaltAbcdefgh01234m6ydjwE//LNEqI3EYlkDngG0IjsA.E8vikuXbm3RJP0"
	const native = "*AFECD3821D5AD859BAF4C7243AFF2EED3703E9EC"
	for name, c := range map[string]struct{ text, stored, want string }{
		"MySQL quotes 80 characters of the statement, which end inside the value": {
			"ERROR 1064 (42000) at line 1: You have an error in your SQL syntax; check the manual that corresponds to your MySQL server version for the right syntax to use near 'USR `mmxq`@`%` IDENTIFIED WITH caching_sha2_password AS '$A$005$MmxSaltAbcdefgh0' at line 1",
			sha2,
			"ERROR 1064 (42000) at line 1: You have an error in your SQL syntax; check the manual that corresponds to your MySQL server version for the right syntax to use near 'USR `mmxq`@`%` IDENTIFIED WITH caching_sha2_password AS '[redacted]' at line 1",
		},
		"MariaDB's client prints the statement, and the server cuts it and adds dots": {
			"--------------\nCREATE USR `mmxq`@`%` IDENTIFIED WITH caching_sha2_password AS '" + sha2 + "'\n--------------\n\nERROR 1064 (42000) at line 1: You have an error in your SQL syntax; check the manual that corresponds to your MariaDB server version for the right syntax to use near 'USR `mmxq`@`%` IDENTIFIED WITH caching_sha2_password AS '$A$005$MmxSaltAbcdef...' at line 1",
			sha2,
			"--------------\nCREATE USR `mmxq`@`%` IDENTIFIED WITH caching_sha2_password AS '[redacted]'\n--------------\n\nERROR 1064 (42000) at line 1: You have an error in your SQL syntax; check the manual that corresponds to your MariaDB server version for the right syntax to use near 'USR `mmxq`@`%` IDENTIFIED WITH caching_sha2_password AS '[redacted]...' at line 1",
		},
		"a quote that ends after two characters of the value": {
			"for the right syntax to use near 'IDENTIFIED VIA mysql_native_password USING '*A' at line 1", native,
			"for the right syntax to use near 'IDENTIFIED VIA mysql_native_password USING '[redacted]' at line 1",
		},
		"a quote that ends with the value's first character": {
			"near 'USING '*' at line 1", native, "near 'USING '[redacted]' at line 1",
		},
		"the end and the middle of the value, without a quote before them": {
			"near '3703E9EC'' at line 1, and 59BAF4C7 again", native, "near '[redacted]'' at line 1, and [redacted] again",
		},
		"the whole value twice": {
			"ALTER USER `shop`@`%` IDENTIFIED VIA mysql_native_password USING '" + native + "'\nERROR 1396 (HY000) at line 1: Operation ALTER USER failed for 'shop'@'%' " + native,
			native,
			"ALTER USER `shop`@`%` IDENTIFIED VIA mysql_native_password USING '[redacted]'\nERROR 1396 (HY000) at line 1: Operation ALTER USER failed for 'shop'@'%' [redacted]",
		},
		"a text without the value stays": {
			"ERROR 1470 (HY000) at line 1: String 'uuuuuuuuuuuuuuuuuuuuuu' is too long for user name (should be no longer than 32)", native,
			"ERROR 1470 (HY000) at line 1: String 'uuuuuuuuuuuuuuuuuuuuuu' is too long for user name (should be no longer than 32)",
		},
		"no stored value": {"near 'x' at line 1", "", "near 'x' at line 1"},
	} {
		got := withoutMySQLStoredValue(c.text, c.stored)
		if got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", name, got, c.want)
		}
		// Whatever the text, no piece of the value of six characters is left.
		for from := 0; c.stored != "" && from+mysqlStoredValuePartLength <= len(c.stored); from++ {
			if piece := c.stored[from : from+mysqlStoredValuePartLength]; strings.Contains(got, piece) {
				t.Errorf("%s: the piece %q of the stored value is left in %s", name, piece, got)
			}
		}
	}
	// Every place where a server can cut the statement leaves nothing of
	// the value behind the quote.
	statement := "CREATE USER `shop`@`10.0.%` IDENTIFIED WITH caching_sha2_password AS '" + sha2 + "'"
	valueAt := strings.Index(statement, sha2)
	for end := valueAt + 1; end <= len(statement); end++ {
		quoted := "near 'password AS '" + statement[valueAt:end] + "' at line 1"
		if got := withoutMySQLStoredValue(quoted, sha2); got != "near 'password AS '[redacted]' at line 1" && got != "near 'password AS '[redacted]'' at line 1" {
			t.Fatalf("a statement cut after %d characters of the value: %s", end-valueAt, got)
		}
	}
}
