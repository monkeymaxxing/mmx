package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"filippo.io/age"
)

// quickVault writes a vault whose passphrase takes little work to derive a
// key from, so that a test that only reads a vault does not wait.
func quickVault(t *testing.T, path, password, plaintext string) []byte {
	t.Helper()
	recipient, err := age.NewScryptRecipient(password)
	if err != nil {
		t.Fatal(err)
	}
	recipient.SetWorkFactor(10)
	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, recipient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(plaintext)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encrypted.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return encrypted.Bytes()
}

// withValueOf sets the secret at a key path in a vault's text, as SetKey and
// AddRandomKey do once the vault is open. keyPath is the key path's words
// joined by spaces, as on the command line.
func withValueOf(t *testing.T, text, keyPath, value string) ([]byte, error) {
	t.Helper()
	before, err := decode([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return withValue([]byte(text), before, strings.Fields(keyPath), value)
}

func TestANewKeyIsANewLastLineAndNothingElseChanges(t *testing.T) {
	const value = "Zx9generatedValue"
	for name, tc := range map[string]struct{ text, want string }{
		"comments, order, and a nested value stay": {
			"# Shop secrets\nzeta: sentinel-one\n\n# The database\nalpha:\n  user: app\n  password: sentinel-two\n",
			"# Shop secrets\nzeta: sentinel-one\n\n# The database\nalpha:\n  user: app\n  password: sentinel-two\nnew_key: \"Zx9generatedValue\"\n"},
		"a last line without a line end gets one": {"zeta: sentinel-one", "zeta: sentinel-one\nnew_key: \"Zx9generatedValue\"\n"},
		"a text block as the last value":          {"certificate: |\n  line one\n  line two\n", "certificate: |\n  line one\n  line two\nnew_key: \"Zx9generatedValue\"\n"},
		"a vault that does not exist yet":         {"", "new_key: \"Zx9generatedValue\"\n"},
		"the new vault of mmx secrets edit":       {newVaultText, newVaultText + "new_key: \"Zx9generatedValue\"\n"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := withValueOf(t, tc.text, "new_key", value)
			if err != nil || string(got) != tc.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

// A value is written so that the vault reads it back as the same text: one
// line quoted with YAML's escapes, several lines as a literal block, as a
// person writes a certificate in the editor.
func TestAValueReadsBackAsTheSameText(t *testing.T) {
	for value, line := range map[string]string{
		"0123456789":                  `pin: "0123456789"` + "\n", // digits alone stay text
		`quote " and \ backslash`:     `pin: "quote \" and \\ backslash"` + "\n",
		"tab\tand\x00nul":             `pin: "tab\tand\0nul"` + "\n",
		"# not a comment":             `pin: "# not a comment"` + "\n",
		"ünïcode":                     `pin: "ünïcode"` + "\n",
		"line one\nline two":          "pin: |-\n  line one\n  line two\n",
		"line one\nline two\n":        "pin: |\n  line one\n  line two\n",
		"  indented first\nline two":  "pin: |2-\n    indented first\n  line two\n",
		"trailing space \nline two":   `pin: "trailing space \nline two"` + "\n",
		"carriage return\r\nline two": `pin: "carriage return\r\nline two"` + "\n",
	} {
		text, err := withValueOf(t, "", "pin", value)
		if err != nil || string(text) != line {
			t.Fatalf("%q: got %q, %v", value, text, err)
		}
		if values, err := decode(text); err != nil || values["pin"] != value {
			t.Fatalf("%q: read back %q, %v", value, values["pin"], err)
		}
	}
	if _, err := withValueOf(t, "", "pin", "sentinel-value\xff"); err == nil || !strings.Contains(err.Error(), "the value for pin is not UTF-8 text") || strings.Contains(err.Error(), "sentinel-value") {
		t.Fatal(err)
	}
}

// An existing key's lines are replaced where they stand. Its comments above
// and the comments at column 1 below it stay; a comment on the key's own
// line belongs to the value it replaces.
func TestAReplacedValueKeepsItsPlaceAndNothingElseChanges(t *testing.T) {
	const vault = "# Shop secrets\nzeta: sentinel-one # rotated in spring\n\n# The database\nalpha:\n  user: app\n  password: sentinel-two\n\n# The certificate\ncertificate: |\n  sentinel line one\n  sentinel line two\n# Nothing below\n"
	for name, tc := range map[string]struct{ key, value, want string }{
		"the first key": {"zeta", "Zx9new",
			"# Shop secrets\nzeta: \"Zx9new\"\n\n# The database\nalpha:\n  user: app\n  password: sentinel-two\n\n# The certificate\ncertificate: |\n  sentinel line one\n  sentinel line two\n# Nothing below\n"},
		"a nested value becomes one line": {"alpha", "Zx9new",
			"# Shop secrets\nzeta: sentinel-one # rotated in spring\n\n# The database\nalpha: \"Zx9new\"\n\n# The certificate\ncertificate: |\n  sentinel line one\n  sentinel line two\n# Nothing below\n"},
		"the last key, a text block, becomes another text block": {"certificate", "new line one\nnew line two\nnew line three\n",
			"# Shop secrets\nzeta: sentinel-one # rotated in spring\n\n# The database\nalpha:\n  user: app\n  password: sentinel-two\n\n# The certificate\ncertificate: |\n  new line one\n  new line two\n  new line three\n# Nothing below\n"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := withValueOf(t, vault, tc.key, tc.value)
			if err != nil || string(got) != tc.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
	got, err := withValueOf(t, "zeta: sentinel-one\nalpha: sentinel-two", "alpha", "Zx9new")
	if err != nil || string(got) != "zeta: sentinel-one\nalpha: \"Zx9new\"\n" {
		t.Fatalf("a last line without a line end: got %q, %v", got, err)
	}
}

func TestAChangedTextThatIsNotAListOfKeysIsRefusedWithoutAValueInTheMessage(t *testing.T) {
	for name, tc := range map[string]struct{ text, key, want string }{
		"a new key, keys written inside braces":         {"{database_password: sentinel-value}\n", "new_key", `cannot append secret key new_key: the vault does not end with separate "key: value" lines`},
		"a new key, a document end marker":              {"database_password: sentinel-value\n...\n", "new_key", "cannot append secret key new_key"},
		"a new key, keys that do not start at column 1": {"  database_password: sentinel-value\n", "new_key", "cannot append secret key new_key"},
		"a replaced key, keys written inside braces":    {"{database_password: sentinel-value, other: sentinel-value}\n", "database_password", "cannot replace the value of secret key database_password"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := withValueOf(t, tc.text, tc.key, "Zx9generatedValue")
			if err == nil || got != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %q, %v", got, err)
			}
			for _, secret := range []string{"sentinel-value", "Zx9generatedValue"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("the message holds a value: %v", err)
				}
			}
		})
	}
}

func TestKeyNames(t *testing.T) {
	for _, key := range []string{"database_password", "_private", "api-token-2", "Token"} {
		if err := CheckKey(key); err != nil {
			t.Fatal(key, err)
		}
	}
	for _, key := range []string{"", "9lives", "two words", "a.b", "a:b", "-dash", "true", "False", "NULL"} {
		if err := CheckKey(key); err == nil {
			t.Fatalf("accepted the key %q", key)
		}
	}
}

func TestRandomValueIsLettersAndDigitsEachAsLikelyAsAnother(t *testing.T) {
	for _, length := range []int{1, 32, 48} {
		value, err := randomValue(length)
		if err != nil || !regexp.MustCompile(`^[A-Za-z0-9]+$`).MatchString(value) || len(value) != length {
			t.Fatal(length, len(value), err)
		}
	}
	// 4000 expected for each of the 62 characters. A choice by remainder of a
	// random byte would give eight of them 25% more than the others.
	const expected = 4000
	value, err := randomValue(expected * len(randomValueCharacters))
	if err != nil {
		t.Fatal(err)
	}
	for _, character := range randomValueCharacters {
		if count := strings.Count(value, string(character)); count < expected*85/100 || count > expected*115/100 {
			t.Fatalf("%c appears %d times, expected about %d", character, count, expected)
		}
	}
	if again, _ := randomValue(32); again == value[:32] {
		t.Fatal("two generated values are equal")
	}
}

func TestAddRandomKeyReplacesTheVaultOnlyAfterTheNewTextIsChecked(t *testing.T) {
	cleanEnvironment(t)
	const password = "add random test passphrase"
	path := filepath.Join(t.TempDir(), "secrets.age")
	// A project that has no vault yet gets one, readable only by its owner.
	if _, err := AddRandomKey(path, password, []string{"session_cookie_secret"}, 48, false); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	created, err := decrypt(path, password)
	if err != nil || !regexp.MustCompile(`^session_cookie_secret: "[A-Za-z0-9]{48}"\n$`).Match(created) {
		t.Fatalf("unexpected text of %d bytes: %v", len(created), err)
	}
	// Every refusal keeps the file as it was, byte for byte.
	original := quickVault(t, path, password, "# The shop\ndatabase_password: sentinel-value\n")
	add := func(path, password, key string, length int) error {
		_, err := AddRandomKey(path, password, []string{key}, length, false)
		return err
	}
	for name, refused := range map[string]func() error{
		"the key exists":     func() error { return add(path, password, "database_password", 32) },
		"a wrong passphrase": func() error { return add(path, "another passphrase", "new_key", 32) },
		"an invalid key":     func() error { return add(path, password, "9lives", 32) },
		"a length of zero":   func() error { return add(path, password, "new_key", 0) },
		"a negative length":  func() error { return add(path, password, "new_key", -5) },
		"a vault that is no vault": func() error {
			broken := filepath.Join(t.TempDir(), "broken.age")
			quickVault(t, broken, password, "database_password: [sentinel-value\n")
			return add(broken, password, "new_key", 32)
		},
	} {
		err := refused()
		if err == nil || strings.Contains(err.Error(), "sentinel-value") {
			t.Fatalf("%s: %v", name, err)
		}
		if now, _ := os.ReadFile(path); !bytes.Equal(now, original) {
			t.Fatalf("%s: the refused command changed the vault", name)
		}
	}
	// A second key follows the text that is there; the first value stays.
	if _, err := AddRandomKey(path, password, []string{"api_token"}, DefaultRandomValueLength, false); err != nil {
		t.Fatal(err)
	}
	text, err := decrypt(path, password)
	if err != nil || !regexp.MustCompile(`^# The shop\ndatabase_password: sentinel-value\napi_token: "[A-Za-z0-9]{32}"\n$`).Match(text) {
		t.Fatalf("unexpected text of %d bytes: %v", len(text), err)
	}
	// With replace, an existing key gets a new random value in its place,
	// and a key the vault does not have is added.
	if replaced, err := AddRandomKey(path, password, []string{"database_password"}, 40, true); err != nil || !replaced {
		t.Fatal(replaced, err)
	}
	if replaced, err := AddRandomKey(path, password, []string{"session_cookie_secret"}, 48, true); err != nil || replaced {
		t.Fatal(replaced, err)
	}
	text, err = decrypt(path, password)
	if err != nil || !regexp.MustCompile(`^# The shop\ndatabase_password: "[A-Za-z0-9]{40}"\napi_token: "[A-Za-z0-9]{32}"\nsession_cookie_secret: "[A-Za-z0-9]{48}"\n$`).Match(text) {
		t.Fatalf("unexpected text of %d bytes: %v", len(text), err)
	}
}

// SetKey asks for the value only once the vault is open and the key may be
// set, and every refusal keeps the file as it was.
func TestSetKeyAddsOrReplacesAKeyAndAsksForTheValueLast(t *testing.T) {
	cleanEnvironment(t)
	const password = "set key test passphrase"
	path := filepath.Join(t.TempDir(), "secrets.age")
	asked := 0
	value := func(v string) func() (string, error) {
		return func() (string, error) { asked++; return v, nil }
	}
	// A project that has no vault yet gets one, readable only by its owner.
	if err := SetKey(path, password, []string{"api_token"}, false, value("Zx9first")); err != nil {
		t.Fatal(err)
	}
	if text, err := decrypt(path, password); err != nil || string(text) != "api_token: \"Zx9first\"\n" {
		t.Fatalf("%q %v", text, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	original := quickVault(t, path, password, "# The shop\ndatabase_password: sentinel-value\nlast: sentinel-value\n")
	asked = 0
	for name, refused := range map[string]func() error{
		"the key exists, without replace": func() error { return SetKey(path, password, []string{"database_password"}, false, value("Zx9new")) },
		"a wrong passphrase": func() error {
			return SetKey(path, "another passphrase", []string{"database_password"}, true, value("Zx9new"))
		},
		"an invalid key": func() error { return SetKey(path, password, []string{"9lives"}, true, value("Zx9new")) },
	} {
		if err := refused(); err == nil || strings.Contains(err.Error(), "sentinel-value") {
			t.Fatalf("%s: %v", name, err)
		}
		if now, _ := os.ReadFile(path); !bytes.Equal(now, original) {
			t.Fatalf("%s: the refused command changed the vault", name)
		}
	}
	if asked != 0 {
		t.Fatalf("a refused command asked for the value %d times", asked)
	}
	err := SetKey(path, password, []string{"database_password"}, false, value("Zx9new"))
	if err == nil || err.Error() != "the vault already has the key database_password; to replace its value, add --replace" {
		t.Fatal(err)
	}
	// A value that cannot be read leaves the vault as it was.
	if err := SetKey(path, password, []string{"new_key"}, false, func() (string, error) { return "", errors.New("no value") }); err == nil || err.Error() != "no value" {
		t.Fatal(err)
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, original) {
		t.Fatal("a value that could not be read changed the vault")
	}
	// With replace, an existing key keeps its place and a missing key is added.
	if err := SetKey(path, password, []string{"database_password"}, true, value("Zx9replaced")); err != nil {
		t.Fatal(err)
	}
	if err := SetKey(path, password, []string{"api_token"}, true, value("Zx9added")); err != nil {
		t.Fatal(err)
	}
	if text, err := decrypt(path, password); err != nil || string(text) != "# The shop\ndatabase_password: \"Zx9replaced\"\nlast: sentinel-value\napi_token: \"Zx9added\"\n" {
		t.Fatalf("%q %v", text, err)
	}
}

func TestCompareKeysComparesDecodedValuesAndReturnsKeysOnly(t *testing.T) {
	committed := map[string]any{
		"unchanged":      "same",
		"unchanged_list": []any{"a", map[string]any{"b": 1}},
		"changed":        "old sentinel",
		"changed_inside": map[string]any{"user": "app", "password": "old sentinel"},
		"changed_type":   42,
		"removed_b":      "sentinel",
		"removed_a":      "sentinel",
	}
	current := map[string]any{
		"unchanged":      "same",
		"unchanged_list": []any{"a", map[string]any{"b": 1}},
		"changed":        "new sentinel",
		"changed_inside": map[string]any{"user": "app", "password": "new sentinel"},
		"changed_type":   "42",
		"added_b":        "sentinel",
		"added_a":        "sentinel",
	}
	added, removed, changed := CompareKeys(committed, current)
	if !reflect.DeepEqual(added, []string{"added_a", "added_b"}) || !reflect.DeepEqual(removed, []string{"removed_a", "removed_b"}) ||
		!reflect.DeepEqual(changed, []string{"changed", "changed_inside", "changed_type"}) {
		t.Fatal(added, removed, changed)
	}
	// Lists without a key are empty lists, which JSON prints as [].
	added, removed, changed = CompareKeys(current, current)
	if added == nil || removed == nil || changed == nil || len(added)+len(removed)+len(changed) != 0 {
		t.Fatal(added, removed, changed)
	}
}

func TestReadEncryptedReadsAVaultFromItsBytes(t *testing.T) {
	const password = "committed version passphrase"
	encrypted := quickVault(t, filepath.Join(t.TempDir(), "secrets.age"), password, "zeta: sentinel-value\nalpha: 1\n")
	values, err := ReadEncrypted(encrypted, password)
	if err != nil || !reflect.DeepEqual(Keys(values), []string{"alpha", "zeta"}) || values["zeta"] != "sentinel-value" {
		t.Fatal(Keys(values), err)
	}
	if _, err := ReadEncrypted(encrypted, "another passphrase"); err == nil {
		t.Fatal("another passphrase opened the vault")
	}
	if _, err := ReadEncrypted([]byte("no vault at all"), password); err == nil || strings.Contains(err.Error(), "no vault at all") {
		t.Fatal(err)
	}
}

// A secret below a top-level secret key goes into its mapping: a missing
// mapping is written at the end of its level, a new name after the last
// line of its mapping with the mapping's indentation, and nothing else in
// the text changes.
func TestANestedSecretGoesIntoItsMappingAndNothingElseChanges(t *testing.T) {
	for name, tc := range map[string]struct{ text, keyPath, want string }{
		"a new vault": {"", "mmwatchdog_secret_files smtp-password",
			"mmwatchdog_secret_files:\n  smtp-password: \"Zx9new\"\n"},
		"a new mapping after the other keys": {"# Shop\nzeta: sentinel-one\n", "mmwatchdog_secret_files smtp-password",
			"# Shop\nzeta: sentinel-one\nmmwatchdog_secret_files:\n  smtp-password: \"Zx9new\"\n"},
		"a new name in a mapping followed by another key": {
			"# Shop\nmmwatchdog_secret_files:\n  # SMTP\n  smtp-user: sentinel-one\n\n# Next\nzeta: sentinel-two\n", "mmwatchdog_secret_files smtp-password",
			"# Shop\nmmwatchdog_secret_files:\n  # SMTP\n  smtp-user: sentinel-one\n  smtp-password: \"Zx9new\"\n\n# Next\nzeta: sentinel-two\n"},
		"a new name in the last mapping, before a comment that ends the vault": {"files:\n  user: sentinel-one\n# The end\n", "files password",
			"files:\n  user: sentinel-one\n  password: \"Zx9new\"\n# The end\n"},
		"the mapping's own indentation": {"files:\n    user: sentinel-one\nzeta: sentinel-two\n", "files password",
			"files:\n    user: sentinel-one\n    password: \"Zx9new\"\nzeta: sentinel-two\n"},
		"after a text block in the mapping": {"files:\n  certificate: |\n    sentinel line one\n    # sentinel line two\nzeta: sentinel-two\n", "files password",
			"files:\n  certificate: |\n    sentinel line one\n    # sentinel line two\n  password: \"Zx9new\"\nzeta: sentinel-two\n"},
		"a name with a dot": {"files:\n  user: sentinel-one\n", "files ca.pem",
			"files:\n  user: sentinel-one\n  ca.pem: \"Zx9new\"\n"},
		"a name of digits is quoted, so that it stays text": {"", "files 2026",
			"files:\n  \"2026\": \"Zx9new\"\n"},
		"depth 3, all new": {"", "service tls key",
			"service:\n  tls:\n    key: \"Zx9new\"\n"},
		"depth 3, a new mapping inside a mapping": {"service:\n  user: sentinel-one\nzeta: sentinel-two\n", "service tls key",
			"service:\n  user: sentinel-one\n  tls:\n    key: \"Zx9new\"\nzeta: sentinel-two\n"},
		"depth 3, a new name in the deepest mapping": {"service:\n  tls:\n    certificate: sentinel-one\n  user: sentinel-two\n", "service tls key",
			"service:\n  tls:\n    certificate: sentinel-one\n    key: \"Zx9new\"\n  user: sentinel-two\n"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := withValueOf(t, tc.text, tc.keyPath, "Zx9new")
			if err != nil || string(got) != tc.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
	// A name that is not a plain word is written in double quotes.
	before, _ := decode([]byte("files:\n  user: sentinel-one\n"))
	got, err := withValue([]byte("files:\n  user: sentinel-one\n"), before, []string{"files", "smtp password: #1"}, "Zx9new")
	if err != nil || string(got) != "files:\n  user: sentinel-one\n  \"smtp password: #1\": \"Zx9new\"\n" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// A replaced name's lines are its own line down to the next name of its
// mapping or the mapping's end; comments and the other names stay.
func TestAReplacedNestedValueKeepsCommentsAndOtherNames(t *testing.T) {
	const vault = "# Shop\nfiles:\n  # The user\n  smtp-user: sentinel-one\n  # The password\n  smtp-password: sentinel-old # rotated\n  # The certificate\n  tls: |\n    sentinel line one\n    sentinel line two\n\n# Next\nzeta: sentinel-two\n"
	for name, tc := range map[string]struct{ keyPath, value, want string }{
		"a name between names": {"files smtp-password", "Zx9new",
			"# Shop\nfiles:\n  # The user\n  smtp-user: sentinel-one\n  # The password\n  smtp-password: \"Zx9new\"\n  # The certificate\n  tls: |\n    sentinel line one\n    sentinel line two\n\n# Next\nzeta: sentinel-two\n"},
		"the last name, a text block, before the next key": {"files tls", "new line one\nnew line two\n",
			"# Shop\nfiles:\n  # The user\n  smtp-user: sentinel-one\n  # The password\n  smtp-password: sentinel-old # rotated\n  # The certificate\n  tls: |\n    new line one\n    new line two\n\n# Next\nzeta: sentinel-two\n"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := withValueOf(t, vault, tc.keyPath, tc.value)
			if err != nil || string(got) != tc.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

// A certificate below a key is a literal block indented below its name, and
// reads back as the same text, also with an empty line inside.
func TestANestedValueOfSeveralLinesReadsBackAsTheSameText(t *testing.T) {
	const certificate = "-----BEGIN CERTIFICATE-----\nMIIBszCCAVmgAwIBAgIU\n\nAAAA==\n-----END CERTIFICATE-----\n"
	got, err := withValueOf(t, "files:\n  user: sentinel-one\nzeta: sentinel-two\n", "files tls ca.pem", certificate)
	want := "files:\n  user: sentinel-one\n  tls:\n    ca.pem: |\n      -----BEGIN CERTIFICATE-----\n      MIIBszCCAVmgAwIBAgIU\n\n      AAAA==\n      -----END CERTIFICATE-----\nzeta: sentinel-two\n"
	if err != nil || string(got) != want {
		t.Fatalf("got %q, %v", got, err)
	}
	values, err := decode(got)
	if err != nil {
		t.Fatal(err)
	}
	if read := values["files"].(map[string]any)["tls"].(map[string]any)["ca.pem"]; read != certificate {
		t.Fatalf("read back %q", read)
	}
}

// Each refusal names the place, says what to do next, asks for no value,
// keeps the vault file as it was, and shows no value.
func TestNestedSetRefusalsSayWhatToDoAndKeepTheVault(t *testing.T) {
	cleanEnvironment(t)
	const password = "nested set test passphrase"
	path := filepath.Join(t.TempDir(), "secrets.age")
	original := quickVault(t, path, password, "token: sentinel-value\nports:\n  - sentinel-value\nflow: {user: sentinel-value}\nfiles:\n  user: sentinel-value\n  deeper:\n    leaf: sentinel-value\n")
	asked := 0
	value := func() (string, error) { asked++; return "Zx9new", nil }
	for name, tc := range map[string]struct {
		keyPath []string
		replace bool
		want    string
	}{
		"a parent that holds a value": {[]string{"token", "smtp-password"}, false,
			"the vault's token holds a value, not names, so smtp-password cannot go inside it. To keep the value, choose another name; to replace the value with names, run mmx secrets edit and remove the value of token"},
		"a parent that holds a list": {[]string{"ports", "first"}, true, "the vault's ports holds a value, not names"},
		"a parent at depth 2 that holds a value": {[]string{"files", "user", "first"}, true,
			"the vault's files user holds a value, not names, so first cannot go inside it. To keep the value, choose another name; to replace the value with names, run mmx secrets edit and remove the value of files user"},
		"a parent written inside braces": {[]string{"flow", "password"}, true,
			"the vault writes the names of flow inside { }, and MMX adds a name only between names written one per line. Run mmx secrets edit and write the names of flow one per line below it, each indented by two spaces"},
		"an existing name without replace": {[]string{"files", "user"}, false,
			"the vault already has the key files user; to replace its value, add --replace"},
		"an existing name at depth 3 without replace": {[]string{"files", "deeper", "leaf"}, false,
			"the vault already has the key files deeper leaf; to replace its value, add --replace"},
		"an empty name":     {[]string{"files", ""}, true, "an empty name cannot be a name inside the secret key files; give each name below the secret key as a word that is not empty"},
		"a name YAML reads": {[]string{"files", "Null"}, true, "the name Null cannot be added: YAML reads Null as a value instead of a name; choose a different name"},
	} {
		t.Run(name, func(t *testing.T) {
			err := SetKey(path, password, tc.keyPath, tc.replace, value)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
			if strings.Contains(err.Error(), "sentinel-value") {
				t.Fatalf("the message holds a value: %v", err)
			}
			if now, _ := os.ReadFile(path); !bytes.Equal(now, original) {
				t.Fatal("the refused command changed the vault")
			}
		})
	}
	if asked != 0 {
		t.Fatalf("a refused command asked for the value %d times", asked)
	}
	// With --replace, an existing name gets the new value.
	if err := SetKey(path, password, []string{"files", "deeper", "leaf"}, true, value); err != nil {
		t.Fatal(err)
	}
	values, err := Read(path, password)
	if err != nil || values["files"].(map[string]any)["deeper"].(map[string]any)["leaf"] != "Zx9new" || values["files"].(map[string]any)["user"] != "sentinel-value" {
		t.Fatal(err)
	}
}

func TestAddRandomKeySetsANestedName(t *testing.T) {
	cleanEnvironment(t)
	const password = "nested random test passphrase"
	path := filepath.Join(t.TempDir(), "secrets.age")
	quickVault(t, path, password, "# The shop\nservice:\n  user: sentinel-value\n")
	if replaced, err := AddRandomKey(path, password, []string{"service", "session_cookie_secret"}, 48, false); err != nil || replaced {
		t.Fatal(replaced, err)
	}
	text, err := decrypt(path, password)
	if err != nil || !regexp.MustCompile(`^# The shop\nservice:\n  user: sentinel-value\n  session_cookie_secret: "[A-Za-z0-9]{48}"\n$`).Match(text) {
		t.Fatalf("unexpected text of %d bytes: %v", len(text), err)
	}
	if _, err := AddRandomKey(path, password, []string{"service", "session_cookie_secret"}, 48, false); err == nil || err.Error() != "the vault already has the key service session_cookie_secret; to give it a new random value, add --replace" {
		t.Fatal(err)
	}
}

func TestKeyPathNames(t *testing.T) {
	for _, keyPath := range [][]string{{"files"}, {"files", "ca.pem"}, {"files", "smtp password"}, {"files", "ünïcode", "9lives"}, {"files", "a:b"}} {
		if err := CheckKeyPath(keyPath); err != nil {
			t.Fatal(keyPath, err)
		}
	}
	for _, keyPath := range [][]string{{}, {"9lives", "name"}, {"a.b"}, {"files", ""}, {"files", "true"}, {"files", "FALSE"}, {"files", "bad\xff"}} {
		if err := CheckKeyPath(keyPath); err == nil {
			t.Fatalf("accepted the key path %q", keyPath)
		}
	}
}
