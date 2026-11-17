package secrets

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"os"
	"reflect"
	"regexp"
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

// A key path is a secret's place in the vault: a secret key, then one name
// for each mapping below it. mmx secrets set mmwatchdog_secret_files
// smtp-password has the key path [mmwatchdog_secret_files smtp-password],
// which a template reads as .secrets.mmwatchdog_secret_files and its entry
// smtp-password. A key path of one word is a top-level secret key.

// CheckKeyPath accepts a key path whose first word passes CheckKey and whose
// other words are names that a YAML mapping can hold, written in double
// quotes when they are not plain words.
func CheckKeyPath(keyPath []string) error {
	if len(keyPath) == 0 {
		return fmt.Errorf("a secret key is required, such as database_password")
	}
	if err := CheckKey(keyPath[0]); err != nil {
		return err
	}
	for _, name := range keyPath[1:] {
		switch {
		case name == "":
			return fmt.Errorf("an empty name cannot be a name inside the secret key %s; give each name below the secret key as a word that is not empty", keyPath[0])
		case !utf8.ValidString(name):
			return fmt.Errorf("a name inside the secret key %s is not UTF-8 text; give each name below the secret key as text", keyPath[0])
		case slices.Contains([]string{"true", "false", "null"}, strings.ToLower(name)):
			return fmt.Errorf("the name %s cannot be added: YAML reads %s as a value instead of a name; choose a different name", name, name)
		}
	}
	return nil
}

// KeyPathText names a key path in prompts and messages: its words, joined
// by spaces, as the command line gives them.
func KeyPathText(keyPath []string) string {
	return strings.Join(keyPath, " ")
}

// AddRandomKey sets the secret at keyPath in the vault at path to a value
// of length random letters and digits, as SetKey sets it: a secret the
// vault does not have is added; an existing secret's value is replaced only
// with replace, which rotates a password. It reports whether it replaced a
// value. A vault that does not exist is created.
func AddRandomKey(path, password string, keyPath []string, length int, replace bool) (replaced bool, err error) {
	if err := CheckKeyPath(keyPath); err != nil {
		return false, err
	}
	if length < 1 {
		return false, fmt.Errorf("the length of a generated value must be a positive integer")
	}
	if length > MaxVaultBytes {
		return false, fmt.Errorf("a value of %d characters does not fit into the vault, which holds at most 4 MiB", length)
	}
	return setValue(path, password, keyPath, replace, func() (string, error) { return randomValue(length) },
		"to give it a new random value, add --replace")
}

// SetKey sets the secret at keyPath in the vault at path to the value that
// value returns. A secret the vault does not have is added, with the
// mappings above it that the vault does not have yet; an existing secret's
// value is replaced only with replace. value is called only after the vault
// is open and the secret may be set, so that a refusal comes before a prompt
// for the value. A vault that does not exist is created.
func SetKey(path, password string, keyPath []string, replace bool, value func() (string, error)) error {
	if err := CheckKeyPath(keyPath); err != nil {
		return err
	}
	_, err := setValue(path, password, keyPath, replace, value, "to replace its value, add --replace")
	return err
}

// setValue changes the vault's text with withValue and replaces the vault
// file as Edit replaces it: only after the new text is checked and
// encrypted, so a failure keeps the file as it was. An existing secret
// without replace is refused, with toReplace saying what to do instead. It
// reports whether the secret existed.
func setValue(path, password string, keyPath []string, replace bool, value func() (string, error), toReplace string) (existed bool, err error) {
	// A project that has no vault yet gets one: its text starts empty.
	text, err := decrypt(path, password)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	before, err := decode(text)
	if err != nil {
		return false, err
	}
	place, err := placeOf(withLineEnd(text), keyPath)
	if err != nil {
		return false, err
	}
	if place.existed && !replace {
		return true, fmt.Errorf("the vault already has the key %s; %s", KeyPathText(keyPath), toReplace)
	}
	newValue, err := value()
	if err != nil {
		return place.existed, err
	}
	if text, err = withValue(text, before, keyPath, newValue); err != nil {
		return place.existed, err
	}
	return place.existed, Write(path, password, text)
}

// vaultPlace is where a secret's lines go in the vault's text, split into
// lines that each keep their line ending.
type vaultPlace struct {
	// lines[first:end] are replaced; first == end inserts before lines[first].
	first, end int
	// indent is the number of spaces before the first new line's name.
	indent int
	// written are the names that the new lines write, each one level below
	// the one before: the secret's own name, after the mappings above it
	// that the vault does not have yet.
	written []string
	// existed says that the vault has the secret.
	existed bool
	// emptyParentLine, when it is not nil, replaces the line of an empty
	// mapping written as {} or null, so that the new lines can go below it.
	emptyParentLine []byte
	// emptyParentAt is the index of that line in lines.
	emptyParentAt int
}

// placeOf finds where the secret at keyPath goes in text, which ends with a
// line ending, from the positions that the YAML parser reports. Each level
// is a block mapping, one name on each line:
//   - A secret the vault has: its lines, from its name's line to the line
//     before the next name of its mapping, or to the end of the mapping,
//     without the blank lines and comment lines at the end of that range
//     that are indented no further than the name.
//   - A top-level secret key the vault does not have: after the text's last
//     line, with the mappings below it.
//   - A name that a mapping below the top does not have: after the
//     mapping's last line, with the indentation of the mapping's names.
//   - A name inside an empty level, written as "parent:" alone, "parent: {}",
//     or "parent: null", as a vault often starts: below the level's line and
//     the comment lines indented below it, two spaces further in. {} and
//     null are removed from the level's line.
//
// A level that holds a value, such as text, a number, or a list, or that
// holds names written inside { }, is refused, since a name cannot go into
// it as a line.
func placeOf(text []byte, keyPath []string) (vaultPlace, error) {
	lines := bytes.SplitAfter(text, []byte("\n"))
	var document yaml.Node
	if err := yaml.Unmarshal(text, &document); err != nil {
		return vaultPlace{}, fmt.Errorf("vault must contain one YAML mapping with unique secret names")
	}
	if document.Kind == 0 {
		return vaultPlace{first: len(lines), end: len(lines), written: keyPath}, nil
	}
	mapping := document.Content[0]
	// The mapping's lines end before lines[limit]; its name is on
	// lines[nameLine].
	limit, nameLine, indent := len(lines), -1, 0
	for depth, name := range keyPath {
		at := 0 // mapping.Content holds each name, then its value
		for at < len(mapping.Content) && (mapping.Content[at].ShortTag() != "!!str" || mapping.Content[at].Value != name) {
			at += 2
		}
		if at >= len(mapping.Content) {
			insert := limit
			if depth > 0 {
				insert = withoutTrailingComments(lines, nameLine+1, limit, indent)
			}
			return vaultPlace{first: insert, end: insert, indent: indent, written: keyPath[depth:]}, nil
		}
		next := limit
		if at+2 < len(mapping.Content) {
			next = mapping.Content[at+2].Line - 1
		}
		nameLine = mapping.Content[at].Line - 1
		if depth == len(keyPath)-1 {
			end := withoutTrailingComments(lines, nameLine+1, next, indent)
			return vaultPlace{first: nameLine, end: end, indent: indent, written: keyPath[depth:], existed: true}, nil
		}
		value := mapping.Content[at+1]
		holder := KeyPathText(keyPath[:depth+1])
		if empty := value.Kind == yaml.MappingNode && len(value.Content) == 0 || value.Kind == yaml.ScalarNode && value.ShortTag() == "!!null"; empty {
			return emptyParentPlace(lines, nameLine, next, indent, value, holder, keyPath[depth+1:])
		}
		if value.Kind == yaml.SequenceNode {
			return vaultPlace{}, fmt.Errorf("the vault's %s holds a list, not names, so %s cannot go inside it. To keep the list, choose another secret key; to give %s names instead, run mmx secrets edit and replace the list with the names, one per line below %s, each indented by two spaces", holder, keyPath[depth+1], holder, holder)
		}
		if value.Kind != yaml.MappingNode {
			return vaultPlace{}, fmt.Errorf("the vault's %s holds a value, not names, so %s cannot go inside it. To keep the value, choose another name; to replace the value with names, run mmx secrets edit and remove the value of %s", holder, keyPath[depth+1], holder)
		}
		if value.Style&yaml.FlowStyle != 0 {
			return vaultPlace{}, fmt.Errorf("the vault writes the names of %s inside { }, and MMX adds a name only between names written one per line. Run mmx secrets edit and write the names of %s one per line below it, each indented by two spaces", holder, holder)
		}
		mapping, limit, indent = value, next, value.Content[0].Column-1
	}
	panic("unreachable: the loop returns at the last word of the key path")
}

// emptyParentPlace is the place of written inside the empty level holder,
// whose name is on lines[nameLine] with indent spaces before it; the level's
// lines end before lines[next]. The new lines go below the comment lines
// that are indented below the name.
func emptyParentPlace(lines [][]byte, nameLine, next, indent int, value *yaml.Node, holder string, written []string) (vaultPlace, error) {
	place := vaultPlace{indent: indent + 2, written: written}
	if value.Value != "" || value.Kind == yaml.MappingNode {
		// {} or null: the line keeps the name and a comment after the value.
		if value.Line-1 != nameLine {
			return vaultPlace{}, fmt.Errorf("the vault writes the empty value of %s on a line of its own; run mmx secrets edit, remove that line, and write the names one per line below %s, each indented by two spaces", holder, holder)
		}
		line := lines[nameLine]
		rewritten := bytes.TrimRight(bytes.Clone(line[:value.Column-1]), " \t")
		if comment := bytes.IndexByte(line[value.Column-1:], '#'); comment >= 0 {
			rewritten = append(append(rewritten, ' '), bytes.TrimRight(line[value.Column-1+comment:], "\r\n")...)
		}
		place.emptyParentLine, place.emptyParentAt = append(rewritten, '\n'), nameLine
	}
	at := nameLine + 1
	for at < next {
		line := bytes.TrimRight(lines[at], " \t\r\n")
		text := bytes.TrimLeft(line, " ")
		if len(text) == 0 || text[0] != '#' || len(line)-len(text) <= indent {
			break
		}
		at++
	}
	place.first, place.end = at, at
	return place, nil
}

// withoutTrailingComments returns end, moved up past the blank lines and the
// comment lines indented by at most indent spaces at the end of
// lines[from:end]. Those lines belong to what follows.
func withoutTrailingComments(lines [][]byte, from, end, indent int) int {
	for end > from {
		line := bytes.TrimRight(lines[end-1], " \t\r\n")
		text := bytes.TrimLeft(line, " ")
		if len(line) == 0 || (text[0] == '#' && len(line)-len(text) <= indent) {
			end--
			continue
		}
		break
	}
	return end
}

// withLineEnd returns text with a line ending after its last line.
func withLineEnd(text []byte) []byte {
	if len(text) > 0 && !bytes.HasSuffix(text, []byte("\n")) {
		return append(bytes.Clone(text), '\n')
	}
	return text
}

// withValue returns a vault's text, whose decoded values are before, with
// the secret at keyPath set to value, at the place that placeOf finds.
// Nothing else in the text changes, so the comments and the order of the
// other names stay as they were written.
func withValue(text []byte, before map[string]any, keyPath []string, value string) ([]byte, error) {
	text = withLineEnd(text)
	place, err := placeOf(text, keyPath)
	if err != nil {
		return nil, err
	}
	entry, err := vaultEntry(place.written, value, place.indent)
	if err != nil {
		return nil, err
	}
	refused := cannotAddALine(keyPath)
	if place.existed {
		refused = cannotReplaceInPlace(keyPath)
	}
	lines := bytes.SplitAfter(text, []byte("\n"))
	if place.emptyParentLine != nil {
		lines[place.emptyParentAt] = place.emptyParentLine
	}
	text = bytes.Join(slices.Concat(lines[:place.first], [][]byte{entry}, lines[place.end:]), nil)
	// The new lines must change only the one secret. A vault whose text is
	// written in another way is left as it is.
	after, err := decode(text)
	want, ok := withDecodedValue(before, keyPath, value)
	if err != nil || !ok || !reflect.DeepEqual(after, want) {
		return nil, refused
	}
	return text, nil
}

// withDecodedValue returns a copy of values with the secret at keyPath set
// to value, adding the mappings above it that values does not have. Only
// the mappings on the key path are copied. It reports false when a level of
// the key path is not a mapping of names.
func withDecodedValue(values map[string]any, keyPath []string, value string) (map[string]any, bool) {
	root := maps.Clone(values)
	if root == nil {
		root = map[string]any{}
	}
	mapping := root
	for _, name := range keyPath[:len(keyPath)-1] {
		below := map[string]any{}
		if existing, exists := mapping[name]; exists && existing != nil {
			names, ok := existing.(map[string]any)
			if !ok {
				return nil, false
			}
			below = maps.Clone(names)
		}
		mapping[name], mapping = below, below
	}
	mapping[keyPath[len(keyPath)-1]] = value
	return root, true
}

// plainName is a name that the vault's text shows without quotes, when YAML
// reads it as text.
var plainName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// vaultEntry returns the lines that write value below the names written,
// each name one level below the one before, such as
//
//	mmwatchdog_secret_files:
//	  smtp-password: "value"
//
// with indent spaces before the first name. A plain name is written
// without quotes when YAML reads it as text; any other name in double
// quotes. A value of one line is quoted: letters and digits need no escape,
// other characters get YAML's escapes, and 32 digits alone would otherwise
// be a number. A value of several lines, such as a certificate, is a
// literal block, as a person writes it in the editor, unless YAML can show
// it only quoted.
func vaultEntry(written []string, value string, indent int) ([]byte, error) {
	name := KeyPathText(written)
	if !utf8.ValidString(value) {
		return nil, fmt.Errorf("the value for %s is not UTF-8 text, and the vault holds text; encode other data first, for example with base64", name)
	}
	style := yaml.DoubleQuotedStyle
	if strings.Contains(value, "\n") {
		style = yaml.LiteralStyle
	}
	node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: style}
	for i := len(written) - 1; i >= 0; i-- {
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: written[i]}
		if !plainName.MatchString(written[i]) {
			key.Style = yaml.DoubleQuotedStyle
		}
		node = &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{key, node}}
	}
	var entry bytes.Buffer
	encoder := yaml.NewEncoder(&entry)
	encoder.SetIndent(2)
	if err := encoder.Encode(node); err != nil {
		return nil, fmt.Errorf("cannot write the value for %s as YAML", name)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("cannot write the value for %s as YAML", name)
	}
	if indent == 0 {
		return entry.Bytes(), nil
	}
	// Each line moves right by the same amount, which keeps a literal
	// block's text: YAML reads a block's lines relative to their name. An
	// empty line stays empty.
	var indented bytes.Buffer
	for _, line := range bytes.SplitAfter(entry.Bytes(), []byte("\n")) {
		if len(bytes.TrimRight(line, "\n")) > 0 {
			indented.WriteString(strings.Repeat(" ", indent))
		}
		indented.Write(line)
	}
	return indented.Bytes(), nil
}

func cannotAddALine(keyPath []string) error {
	return fmt.Errorf(`cannot append secret key %s: the vault does not end with separate "key: value" lines, for example because the keys are inside { }. Use mmx secrets edit to add the secret key`, KeyPathText(keyPath))
}

func cannotReplaceInPlace(keyPath []string) error {
	return fmt.Errorf(`cannot replace the value of secret key %s: the vault is not written as separate "key: value" lines, for example because the keys are inside { }. Use mmx secrets edit to change the secret value`, KeyPathText(keyPath))
}

// ValueAt returns the value of the secret at keyPath in a vault's decoded
// values, for a command that hands one value to a person or a program. A
// value that is not text, such as a number or a list, is its YAML text.
// When the key path does not end at one value, the error says what the
// vault has there instead, so the person can pick the right words; it
// names keys and names, never a value.
func ValueAt(values map[string]any, keyPath []string) (string, error) {
	var found any = values
	for depth, name := range keyPath {
		place := KeyPathText(keyPath[:depth])
		mapping, ok := found.(map[string]any)
		if !ok {
			return "", fmt.Errorf("the vault's %s holds a value, not names; to read the value, give only %s", place, place)
		}
		if found, ok = mapping[name]; !ok {
			if depth == 0 {
				return "", fmt.Errorf("the vault has no secret key %s; mmx secrets list-keys lists the secret keys", name)
			}
			return "", fmt.Errorf("the vault's %s has no name %s; its names are %s", place, name, strings.Join(Keys(mapping), ", "))
		}
	}
	switch found := found.(type) {
	case string:
		return found, nil
	case map[string]any:
		place := KeyPathText(keyPath)
		return "", fmt.Errorf("the vault's %s holds names, not one value: %s. Give one more word for the name to read, such as %s %s", place, strings.Join(Keys(found), ", "), place, Keys(found)[0])
	}
	text, err := yaml.Marshal(found)
	if err != nil {
		return "", fmt.Errorf("cannot write the value of %s as YAML text", KeyPathText(keyPath))
	}
	return string(text), nil
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
