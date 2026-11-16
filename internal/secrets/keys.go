package secrets

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"mmx/internal/modules"

	"go.yaml.in/yaml/v3"
)

// The commands that work on the vault's keys (mmx secrets add-random, set,
// list-keys, and diff-keys) use the functions in this file. None of them
// returns a secret's value or puts one into an error.

// DefaultRandomValueLength is the length of a value that AddRandomKey
// generates when the user gives no length.
const DefaultRandomValueLength = 32

// randomValueCharacters are the 62 letters and digits that a generated value
// is made of.
const randomValueCharacters = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randomValue returns length characters. crypto/rand chooses each one, with
// the same probability for each of the 62 letters and digits.
func randomValue(length int) (string, error) {
	count := big.NewInt(int64(len(randomValueCharacters)))
	value := make([]byte, length)
	for i := range value {
		n, err := rand.Int(rand.Reader, count)
		if err != nil {
			return "", fmt.Errorf("generate a random value: %w", err)
		}
		value[i] = randomValueCharacters[n.Int64()]
	}
	return string(value), nil
}

// CheckKey accepts a name that a playbook can read as .secrets.KEY and that
// YAML reads as text when it stands unquoted before a colon.
func CheckKey(key string) error {
	if !modules.Name.MatchString(key) {
		return fmt.Errorf("%q cannot be a secret key: a key starts with a letter or an underscore and continues with letters, digits, underscores, or hyphens, so that a playbook can read it as .secrets.KEY", key)
	}
	if slices.Contains([]string{"true", "false", "null"}, strings.ToLower(key)) {
		return fmt.Errorf("secret key %s cannot be added: YAML reads %s as a value instead of a name; choose a different secret key", key, key)
	}
	return nil
}

// AddRandomKey sets key in the vault at path to a value of length random
// letters and digits, as SetKey sets a key: a key the vault does not have
// is added; an existing key's value is replaced only with replace, which
// rotates a password. It reports whether it replaced a value. A vault that
// does not exist is created.
func AddRandomKey(path, password, key string, length int, replace bool) (replaced bool, err error) {
	if err := CheckKey(key); err != nil {
		return false, err
	}
	if length < 1 {
		return false, fmt.Errorf("the length of a generated value must be a positive integer")
	}
	if length > MaxVaultBytes {
		return false, fmt.Errorf("a value of %d characters does not fit into the vault, which holds at most 4 MiB", length)
	}
	return setValue(path, password, key, replace, func() (string, error) { return randomValue(length) },
		"to give it a new random value, add --replace")
}

// SetKey sets key in the vault at path to the value that value returns. A
// key the vault does not have is added; an existing key's value is replaced
// only with replace. value is called only after the vault is open and the
// key may be set, so that a refusal comes before a prompt for the value. A
// vault that does not exist is created.
func SetKey(path, password, key string, replace bool, value func() (string, error)) error {
	if err := CheckKey(key); err != nil {
		return err
	}
	_, err := setValue(path, password, key, replace, value, "to replace its value, add --replace")
	return err
}

// setValue changes the vault's text with withValue and replaces the vault
// file as Edit replaces it: only after the new text is checked and
// encrypted, so a failure keeps the file as it was. An existing key without
// replace is refused, with toReplace saying what to do instead. It reports
// whether the key existed.
func setValue(path, password, key string, replace bool, value func() (string, error), toReplace string) (existed bool, err error) {
	// A project that has no vault yet gets one: its text starts empty.
	text, err := decrypt(path, password)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	before, err := decode(text)
	if err != nil {
		return false, err
	}
	_, existed = before[key]
	if existed && !replace {
		return true, fmt.Errorf("the vault already has the key %s; %s", key, toReplace)
	}
	newValue, err := value()
	if err != nil {
		return existed, err
	}
	if text, err = withValue(text, before, key, newValue); err != nil {
		return existed, err
	}
	return existed, Write(path, password, text)
}

// withValue returns a vault's text, whose decoded values are before, with
// key set to value. A new key becomes a new last line, after the text's last
// line. An existing key's lines are replaced where they stand: from the
// key's line to the line before the next key, without the blank lines and
// comment lines at column 1 at its end, which belong to the next key or end
// the vault. Nothing else in the text changes, so the comments and the order
// of the other keys stay as they were written.
func withValue(text []byte, before map[string]any, key, value string) ([]byte, error) {
	entry, err := vaultEntry(key, value)
	if err != nil {
		return nil, err
	}
	keys, refused := len(before), cannotReplaceInPlace(key)
	if _, exists := before[key]; exists {
		if text, err = withReplacedLines(text, key, entry); err != nil {
			return nil, refused
		}
	} else {
		keys, refused = keys+1, cannotAddALine(key)
		if len(text) > 0 && !bytes.HasSuffix(text, []byte("\n")) {
			text = append(bytes.Clone(text), '\n')
		}
		text = append(bytes.Clone(text), entry...)
	}
	// The new lines must be the one changed key of the same mapping. A vault
	// whose text is written in another way is left as it is.
	after, err := decode(text)
	if err != nil || len(after) != keys || after[key] != value {
		return nil, refused
	}
	for name, written := range before {
		if name != key && !reflect.DeepEqual(after[name], written) {
			return nil, refused
		}
	}
	return text, nil
}

// vaultEntry returns the lines KEY: value. A value of one line is quoted:
// letters and digits need no escape, other characters get YAML's escapes,
// and 32 digits alone would otherwise be a number. A value of several lines,
// such as a certificate, is a literal block, as a person writes it in the
// editor, unless YAML can show it only quoted.
func vaultEntry(key, value string) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, fmt.Errorf("the value for %s is not UTF-8 text, and the vault holds text; encode other data first, for example with base64", key)
	}
	style := yaml.DoubleQuotedStyle
	if strings.Contains(value, "\n") {
		style = yaml.LiteralStyle
	}
	var entry bytes.Buffer
	encoder := yaml.NewEncoder(&entry)
	encoder.SetIndent(2)
	if err := encoder.Encode(&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: style},
	}}); err != nil {
		return nil, fmt.Errorf("cannot write the value for %s as YAML", key)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("cannot write the value for %s as YAML", key)
	}
	return entry.Bytes(), nil
}

// withReplacedLines returns text with the lines of key's entry replaced by
// entry, as withValue describes. It finds the lines from the positions that
// the YAML parser reports for the top-level keys.
func withReplacedLines(text []byte, key string, entry []byte) ([]byte, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(text, &document); err != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("the vault's text is not one mapping")
	}
	lines := bytes.SplitAfter(text, []byte("\n"))
	mapping := document.Content[0].Content
	at := 0 // mapping holds each key, then its value
	for at < len(mapping) && mapping[at].Value != key {
		at += 2
	}
	if at >= len(mapping) {
		return nil, fmt.Errorf("the vault's mapping has no key %s", key)
	}
	// The entry's lines are lines[first:end].
	first, end := mapping[at].Line-1, len(lines)
	if at+2 < len(mapping) {
		end = mapping[at+2].Line - 1
	}
	for end > first+1 {
		if line := bytes.TrimRight(lines[end-1], " \t\r\n"); len(line) == 0 || line[0] == '#' {
			end--
			continue
		}
		break
	}
	replaced := slices.Concat(lines[:first], [][]byte{entry}, lines[end:])
	return bytes.Join(replaced, nil), nil
}

func cannotAddALine(key string) error {
	return fmt.Errorf(`cannot append secret key %s: the vault does not end with separate "key: value" lines, for example because the keys are inside { }. Use mmx secrets edit to add the secret key`, key)
}

func cannotReplaceInPlace(key string) error {
	return fmt.Errorf(`cannot replace the value of secret key %s: the vault is not written as separate "key: value" lines, for example because the keys are inside { }. Use mmx secrets edit to change the secret value`, key)
}

// Keys returns the vault's top-level keys, sorted.
func Keys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// ReadEncrypted decrypts and decodes a vault from its encrypted bytes, such
// as the version of the vault file in the last Git commit.
func ReadEncrypted(encrypted []byte, password string) (map[string]any, error) {
	plaintext, err := decryptFrom(bytes.NewReader(encrypted), password)
	if err != nil {
		return nil, err
	}
	return decode(plaintext)
}

// CompareKeys compares the decoded values of two versions of a vault. It
// returns, each sorted, the keys that only current has (added), the keys
// that only committed has (removed), and the keys of both whose values
// differ (changed). It returns keys only.
func CompareKeys(committed, current map[string]any) (added, removed, changed []string) {
	added, removed, changed = []string{}, []string{}, []string{}
	for _, key := range Keys(current) {
		before, existed := committed[key]
		switch {
		case !existed:
			added = append(added, key)
		case !reflect.DeepEqual(before, current[key]):
			changed = append(changed, key)
		}
	}
	for _, key := range Keys(committed) {
		if _, exists := current[key]; !exists {
			removed = append(removed, key)
		}
	}
	return added, removed, changed
}
