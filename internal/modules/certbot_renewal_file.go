package modules

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// certbot keeps the settings of one certificate in its renewal file,
// /etc/letsencrypt/renewal/<certificate_name>.conf: the paths of the
// certificate's files, and in the section [renewalparams] what certbot needs
// for the next renewal. certbot reads and writes the file with Python's
// ConfigObj:
//
//	# renew_before_expiry = 30 days
//	version = 4.0.0
//	cert = /etc/letsencrypt/live/shop.example.com/cert.pem
//	[renewalparams]
//	account = 00d1f2dd2e9cfc561d10d8808c07a80a
//	renew_hook = "'/usr/local/lib/shop site/reload'"
//	server = https://acme-v02.api.letsencrypt.org/directory
//	authenticator = webroot
//	webroot_path = /var/www/certbot-challenges,
//	[[webroot_map]]
//	shop.example.com = /var/www/certbot-challenges
//
// MMX reads the file by section and key, never by the position of a line:
// the order of the keys, the comments, and the sections differ between
// certbot's versions, and certbot writes the file again at every renewal.
// MMX changes one setting at a time and keeps every other line as it is.
//
// The reader accepts less than ConfigObj does: what certbot itself writes,
// and the plain edits of a person. A file that it cannot read fails the task
// before certbot starts. That is the safe side: when certbot cannot read a
// renewal file, it gets a new certificate, puts it into another directory
// (<certificate_name>-0001), and reports success.

// certbotRenewalFile is one renewal file as its lines.
type certbotRenewalFile struct {
	lines []certbotRenewalLine
}

// certbotRenewalLine is one line of a renewal file.
type certbotRenewalLine struct {
	// text is the line as the file has it, without its line end.
	text string
	// kind is "setting" for key = value, "section" for a line that starts a
	// section, and "" for a blank line or a comment.
	kind string
	// section is the section that the line is in or starts: "" before the
	// first section, a name such as "renewalparams", or two names with a
	// slash for a section inside a section, "renewalparams/webroot_map".
	section string
	// key and values are a setting's. list says that the value is written as
	// a list (with a comma), as certbot writes webroot_path.
	key    string
	values []string
	list   bool
}

var (
	// A line that starts a section: [name] or, inside a section, [[name]].
	certbotRenewalSectionLine = regexp.MustCompile(`^\s*(\[+)\s*([^\[\]'"#=,]+?)\s*(\]+)\s*$`)
	// A setting: a key without quotes, an equals sign, and the value.
	certbotRenewalSettingLine = regexp.MustCompile(`^(\s*)([^\s=\[\]'"#][^=]*?)\s*=\s*(.*?)\s*$`)
)

// parseCertbotRenewalFile reads the text of a renewal file. Its error names
// the line that it cannot read.
func parseCertbotRenewalFile(text string) (*certbotRenewalFile, error) {
	file := &certbotRenewalFile{}
	if text == "" {
		return nil, errors.New("certbot's renewal file is empty")
	}
	section, top := "", ""
	sections := map[string]bool{}
	keys := map[string]bool{}
	for number, raw := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		line := certbotRenewalLine{text: raw, section: section}
		content := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		fail := func(format string, values ...any) (*certbotRenewalFile, error) {
			return nil, fmt.Errorf("line %d (%s): %s", number+1, shortenedLine(content), fmt.Sprintf(format, values...))
		}
		switch {
		case content == "" || strings.HasPrefix(content, "#"):
		case strings.HasPrefix(content, "["):
			match := certbotRenewalSectionLine.FindStringSubmatch(content)
			if match == nil || len(match[1]) != len(match[3]) {
				return fail("the line starts with [ and is not a section such as [renewalparams]")
			}
			switch len(match[1]) {
			case 1:
				top, section = match[2], match[2]
			case 2:
				if top == "" {
					return fail("a section in double brackets must be inside another section")
				}
				section = top + "/" + match[2]
			default:
				return fail("the section is nested deeper than certbot writes")
			}
			if sections[section] {
				return fail("the section is in the file twice")
			}
			sections[section] = true
			line.kind, line.section = "section", section
		default:
			match := certbotRenewalSettingLine.FindStringSubmatch(strings.TrimSuffix(raw, "\r"))
			if match == nil {
				return fail("the line is neither a setting (key = value), a section, nor a comment")
			}
			values, list, err := parseCertbotRenewalValue(match[3])
			if err != nil {
				return fail("%v", err)
			}
			if keys[section+"\x00"+match[2]] {
				return fail("the key %s is in its section twice", match[2])
			}
			keys[section+"\x00"+match[2]] = true
			line.kind, line.key, line.values, line.list = "setting", match[2], values, list
		}
		file.lines = append(file.lines, line)
	}
	return file, nil
}

// shortenedLine is a line for an error message.
func shortenedLine(line string) string {
	if len(line) > 60 {
		return line[:60] + "..."
	}
	return line
}

// parseCertbotRenewalValue reads the value of one setting as ConfigObj does:
// plain text, text in single or double quotes, or a list of such texts with
// commas, where a comma at the end makes a list of one. Text after a # is a
// comment. ConfigObj knows no escape character: text in quotes ends at the
// next quote of the same kind.
func parseCertbotRenewalValue(raw string) (values []string, list bool, err error) {
	raw = strings.TrimSpace(raw)
	for _, quote := range []string{`"""`, `'''`} {
		if !strings.HasPrefix(raw, quote) {
			continue
		}
		rest := raw[3:]
		end := strings.LastIndex(rest, quote)
		if end < 0 {
			return nil, false, errors.New("the renewal setting value opens triple quotes and does not close them on the same line; MMX reads only single-line renewal setting values")
		}
		if after := strings.TrimSpace(rest[end+3:]); after != "" && !strings.HasPrefix(after, "#") {
			return nil, false, errors.New("text follows the value's closing quotes")
		}
		return []string{rest[:end]}, false, nil
	}
	position := 0
	skipSpaces := func() {
		for position < len(raw) && (raw[position] == ' ' || raw[position] == '\t') {
			position++
		}
	}
	for {
		skipSpaces()
		if position == len(raw) || raw[position] == '#' {
			// Nothing after the equals sign is an empty text; nothing after a
			// comma ends a list.
			if !list {
				values = []string{""}
			}
			return values, list, nil
		}
		switch raw[position] {
		case ',':
			if len(values) > 0 || list {
				return nil, false, errors.New("the value has two commas with nothing between them")
			}
			list = true
			position++
			continue
		case '"', '\'':
			quote := raw[position]
			end := strings.IndexByte(raw[position+1:], quote)
			if end < 0 {
				return nil, false, errors.New("the renewal setting value has an unclosed quote")
			}
			values = append(values, raw[position+1:position+1+end])
			position += end + 2
		default:
			end := position
			for end < len(raw) && raw[end] != ',' && raw[end] != '#' {
				end++
			}
			values = append(values, strings.TrimSpace(raw[position:end]))
			position = end
		}
		skipSpaces()
		switch {
		case position == len(raw) || raw[position] == '#':
			return values, list, nil
		case raw[position] == ',':
			list = true
			position++
		default:
			return nil, false, errors.New("text follows a value's closing quote")
		}
	}
}

// certbotRenewalWritten writes one text as ConfigObj writes it into a
// renewal file: as it is, or in quotes when it starts or ends with a space
// or a quote or holds a comma or a #. ConfigObj has no escape character, so
// a text with both kinds of quotes can only be a value of its own, in three
// quotes, and never part of a list.
func certbotRenewalWritten(text string, inList bool) (string, error) {
	const startsOrEndsQuoted = " \t\r\n\v'\""
	switch {
	case text == "":
		return `""`, nil
	case strings.ContainsAny(text, "\r\n"):
		return "", errors.New("certbot's renewal file cannot hold a value with a line break")
	case !strings.ContainsAny(text[:1], startsOrEndsQuoted) && !strings.ContainsAny(text[len(text)-1:], startsOrEndsQuoted) && !strings.ContainsAny(text, ",#"):
		return text, nil
	case !strings.Contains(text, `"`):
		return `"` + text + `"`, nil
	case !strings.Contains(text, `'`):
		return `'` + text + `'`, nil
	case !inList && !strings.Contains(text, `"""`) && !strings.HasSuffix(text, `"`):
		return `"""` + text + `"""`, nil
	case !inList && !strings.Contains(text, `'''`) && !strings.HasSuffix(text, `'`):
		return `'''` + text + `'''`, nil
	}
	return "", fmt.Errorf("certbot's renewal file cannot hold the value %s because the value contains single and double quotes", text)
}

// text is the file's text.
func (file *certbotRenewalFile) text() string {
	var b strings.Builder
	for _, line := range file.lines {
		b.WriteString(line.text)
		b.WriteByte('\n')
	}
	return b.String()
}

// hasSection reports whether the file has the section.
func (file *certbotRenewalFile) hasSection(section string) bool {
	return section == "" || slices.ContainsFunc(file.lines, func(line certbotRenewalLine) bool {
		return line.kind == "section" && line.section == section
	})
}

// settings lists the settings of one section in the file's order.
func (file *certbotRenewalFile) settings(section string) []certbotRenewalLine {
	var found []certbotRenewalLine
	for _, line := range file.lines {
		if line.kind == "setting" && line.section == section {
			found = append(found, line)
		}
	}
	return found
}

// value reads one setting; found is false when the section has no such key.
func (file *certbotRenewalFile) value(section, key string) (values []string, found bool) {
	for _, line := range file.lines {
		if line.kind == "setting" && line.section == section && line.key == key {
			return line.values, true
		}
	}
	return nil, false
}

// one reads a setting that holds one text; found is false when the section
// has no such key or the key holds a list of another length.
func (file *certbotRenewalFile) one(section, key string) (text string, found bool) {
	values, found := file.value(section, key)
	if !found || len(values) != 1 {
		return "", false
	}
	return values[0], true
}

// set makes key in section hold written, which is the value as the file
// writes it. It changes the key's own line, or adds a line after the
// section's last setting: ConfigObj reads a setting below a section inside
// the section as that inner section's.
func (file *certbotRenewalFile) set(section, key, written string) error {
	line := certbotRenewalLine{text: key + " = " + written, kind: "setting", section: section, key: key}
	after := -1
	for i, existing := range file.lines {
		if existing.section != section {
			continue
		}
		if existing.kind == "setting" && existing.key == key {
			indent := existing.text[:len(existing.text)-len(strings.TrimLeft(existing.text, " \t"))]
			line.text = indent + line.text
			file.lines[i] = line
			return nil
		}
		if existing.kind != "" {
			after = i
		}
	}
	if after < 0 && section != "" {
		return fmt.Errorf("the file has no section [%s]", section)
	}
	file.lines = slices.Insert(file.lines, after+1, line)
	return nil
}

// remove takes key out of section.
func (file *certbotRenewalFile) remove(section, key string) {
	file.lines = slices.DeleteFunc(file.lines, func(line certbotRenewalLine) bool {
		return line.kind == "setting" && line.section == section && line.key == key
	})
}

// removeSection takes a section's own line and its settings out of the
// file. Comments stay.
func (file *certbotRenewalFile) removeSection(section string) {
	file.lines = slices.DeleteFunc(file.lines, func(line certbotRenewalLine) bool {
		return line.kind != "" && line.section == section
	})
}
