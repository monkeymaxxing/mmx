package modules

import (
	"reflect"
	"strings"
	"testing"
)

// The renewal file of certbot 1.21.0, older than the oldest certbot that a
// supported distribution packages (2.1.0 on Debian 12), and of a newer
// certbot than any of them packages, 5.8.0, as each wrote it for one
// certificate with a script.
const (
	renewalFileOfCertbot121 = `# renew_before_expiry = 30 days
version = 1.21.0
archive_dir = /etc/letsencrypt/archive/shop.example.com
cert = /etc/letsencrypt/live/shop.example.com/cert.pem
privkey = /etc/letsencrypt/live/shop.example.com/privkey.pem
chain = /etc/letsencrypt/live/shop.example.com/chain.pem
fullchain = /etc/letsencrypt/live/shop.example.com/fullchain.pem

# Options used in the renewal process
[renewalparams]
account = 762d877a83b2fd2af95882f75a1b87e4
renew_hook = /usr/local/bin/reload-after-certificate-renewal
server = https://localhost:14000/dir
authenticator = webroot
webroot_path = /var/www/certbot-challenges,
[[webroot_map]]
www.shop.example.com = /var/www/certbot-challenges
shop.example.com = /var/www/certbot-challenges
`
	renewalFileOfCertbot58 = `version = 5.8.0
archive_dir = /etc/letsencrypt/archive/shop.example.com
cert = /etc/letsencrypt/live/shop.example.com/cert.pem
privkey = /etc/letsencrypt/live/shop.example.com/privkey.pem
chain = /etc/letsencrypt/live/shop.example.com/chain.pem
fullchain = /etc/letsencrypt/live/shop.example.com/fullchain.pem
[renewalparams]
account = 1c60c001a83b34d641790d6bd10ee686
server = https://localhost:14000/dir
authenticator = webroot
webroot_path = /var/www/certbot-challenges,
key_type = ecdsa
renew_hook = /usr/local/bin/reload-after-certificate-renewal
[[webroot_map]]
shop.example.com = /var/www/certbot-challenges
www.shop.example.com = /var/www/certbot-challenges
[acme_renewal_info]
ari_retry_after = 2026-10-09T22:08:58
`
)

func mustParseRenewalFile(t *testing.T, text string) *certbotRenewalFile {
	t.Helper()
	file, err := parseCertbotRenewalFile(text)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// The files of both versions read the same by section and key, whatever the
// order of their lines, and an unread file is written back as it was.
func TestRenewalFileIsReadBySectionAndKeyOnEveryVersion(t *testing.T) {
	t.Parallel()
	for version, text := range map[string]string{"1.21.0": renewalFileOfCertbot121, "5.8.0": renewalFileOfCertbot58} {
		file := mustParseRenewalFile(t, text)
		if file.text() != text {
			t.Errorf("%s: the file does not come back as it was:\n%s", version, file.text())
		}
		for key, want := range map[string]string{
			"server": "https://localhost:14000/dir", "authenticator": "webroot", "renew_hook": "/usr/local/bin/reload-after-certificate-renewal",
		} {
			if got, found := file.one("renewalparams", key); !found || got != want {
				t.Errorf("%s: %s is %q, %v", version, key, got, found)
			}
		}
		if got, found := file.one("", "cert"); !found || got != "/etc/letsencrypt/live/shop.example.com/cert.pem" {
			t.Errorf("%s: cert is %q", version, got)
		}
		// webroot_path is a list of one, which certbot writes with a comma.
		if paths, _ := file.value("renewalparams", "webroot_path"); !reflect.DeepEqual(paths, []string{"/var/www/certbot-challenges"}) {
			t.Errorf("%s: webroot_path is %q", version, paths)
		}
		var names []string
		for _, entry := range file.settings("renewalparams/webroot_map") {
			names = append(names, entry.key+"="+strings.Join(entry.values, "|"))
		}
		if len(names) != 2 || !strings.Contains(strings.Join(names, " "), "www.shop.example.com=/var/www/certbot-challenges") {
			t.Errorf("%s: the map is %q", version, names)
		}
		// A key of a section is not found in another section.
		if _, found := file.one("", "server"); found {
			t.Errorf("%s: server was found outside its section", version)
		}
	}
	// The section that only the newest certbot writes ends the map above it.
	file := mustParseRenewalFile(t, renewalFileOfCertbot58)
	if got, found := file.one("acme_renewal_info", "ari_retry_after"); !found || got != "2026-10-09T22:08:58" || len(file.settings("renewalparams/webroot_map")) != 2 {
		t.Errorf("ari_retry_after is %q, %v", got, found)
	}
}

// A value is plain text, text in quotes, or a list with commas, as Python's
// ConfigObj reads it; and MMX writes a text so that ConfigObj reads the same
// text back.
func TestRenewalFileValuesReadAndWriteAsConfigObjDoes(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string][]string{
		"/var/www/certbot-challenges,":           {"/var/www/certbot-challenges"},
		"/var/www/one, /var/www/two":             {"/var/www/one", "/var/www/two"},
		`"'/usr/local/lib/mmx test/hook, one'"`:  {"'/usr/local/lib/mmx test/hook, one'"},
		`'say "hello"'`:                          {`say "hello"`},
		`"/var/www/a,b",`:                        {"/var/www/a,b"},
		"webroot   # the challenge":              {"webroot"},
		"":                                       {""},
		`""`:                                     {""},
		`"""'/srv/a"b c'"""`:                     {`'/srv/a"b c'`},
		"True":                                   {"True"},
		"/usr/local/bin/it's-a-script":           {"/usr/local/bin/it's-a-script"},
		`"/var/www/one" , '/var/www/two' # both`: {"/var/www/one", "/var/www/two"},
		`'/usr/local/bin/reload' # after renewal`: {"/usr/local/bin/reload"},
	} {
		got, _, err := parseCertbotRenewalValue(raw)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q reads as %q, %v; want %q", raw, got, err, want)
		}
	}
	if values, list, err := parseCertbotRenewalValue(","); err != nil || !list || len(values) != 0 {
		t.Errorf("a comma alone is an empty list: %q %v %v", values, list, err)
	}
	for raw, want := range map[string]string{
		`"/var/www/open`:        "has an unclosed quote",
		`"/var/www/one" two`:    "text follows a value's closing quote",
		"/var/www/one,, /two":   "two commas with nothing between them",
		`"""/var/www/one`:       "opens triple quotes and does not close them",
		`"""/var/www/one""" x`:  "text follows the value's closing quotes",
		`'/var/www/one' "/two"`: "text follows a value's closing quote",
	} {
		if _, _, err := parseCertbotRenewalValue(raw); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v; want %s", raw, err, want)
		}
	}
	for text, want := range map[string]string{
		"/usr/local/bin/reload":               "/usr/local/bin/reload",
		"'/usr/local/lib/mmx test/hook, one'": `"'/usr/local/lib/mmx test/hook, one'"`,
		"/var/www/a,b":                        `"/var/www/a,b"`,
		"/var/www/a#b":                        `"/var/www/a#b"`,
		`/srv/say "hello", twice`:             `'/srv/say "hello", twice'`,
		`'/srv/a"b c'`:                        `"""'/srv/a"b c'"""`,
		"":                                    `""`,
		" /leading/space":                     `" /leading/space"`,
	} {
		written, err := certbotRenewalWritten(text, false)
		if err != nil || written != want {
			t.Errorf("%q is written as %s, %v; want %s", text, written, err, want)
			continue
		}
		if back, _, err := parseCertbotRenewalValue(written); err != nil || len(back) != 1 || back[0] != text {
			t.Errorf("%q, written as %s, reads back as %q, %v", text, written, back, err)
		}
	}
	// ConfigObj has no escape character: a part of a list cannot hold both
	// kinds of quotes, and no value holds a line break.
	if _, err := certbotRenewalWritten(`/srv/it's "here", twice`, true); err == nil || !strings.Contains(err.Error(), "single and double quotes") {
		t.Error(err)
	}
	if _, err := certbotRenewalWritten("/srv/two\nlines", false); err == nil || !strings.Contains(err.Error(), "line break") {
		t.Error(err)
	}
}

// A file that certbot itself could not read, or that MMX cannot read for
// certain, is refused with its line: certbot would treat the certificate as
// new.
func TestRenewalFileThatCannotBeReadIsRefusedWithItsLine(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]string{
		"": "certbot's renewal file is empty",
		"version = 4.0.0\nthis is no setting\n[renewalparams]\n":                          "line 2 (this is no setting): the line is neither a setting (key = value), a section, nor a comment",
		"version = 4.0.0\n[renewalparams\nserver = x\n":                                   "line 2 ([renewalparams): the line starts with [ and is not a section such as [renewalparams]",
		"version = 4.0.0\n[[webroot_map]]\n":                                              "line 2 ([[webroot_map]]): a section in double brackets must be inside another section",
		"[renewalparams]\n[[[deep]]]\n":                                                   "line 2 ([[[deep]]]): the section is nested deeper than certbot writes",
		"[renewalparams]\nserver = a\nserver = b\n":                                       "line 3 (server = b): the key server is in its section twice",
		"[renewalparams]\nserver = a\n[renewalparams]\n":                                  "line 3 ([renewalparams]): the section is in the file twice",
		"[renewalparams]\nrenew_hook = \"/usr/local/bin/open\n":                           "line 2 (renew_hook = \"/usr/local/bin/open): the renewal setting value has an unclosed quote",
		"[renewalparams]\n= value\n":                                                      "line 2 (= value): the line is neither a setting",
		"[renewalparams]\n" + strings.Repeat("x", 80) + "\n":                              "line 2 (" + strings.Repeat("x", 60) + "...): the line is neither a setting",
		"[renewalparams]\nserver = a\n[[webroot_map]]\na = /x\n[[webroot_map]]\nb = /x\n": "line 5 ([[webroot_map]]): the section is in the file twice",
	} {
		if _, err := parseCertbotRenewalFile(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v; want %s", text, err, want)
		}
	}
	// The same key in two sections is no repetition.
	mustParseRenewalFile(t, "version = 1\n[renewalparams]\nversion = 2\n[[webroot_map]]\nversion = /x\n[other]\nversion = 3\n")
}

// A change touches the one line of its key and keeps every other line,
// comment, and section; a new key goes after the section's last own
// setting, above a section inside it.
func TestRenewalFileChangesKeepEveryOtherLine(t *testing.T) {
	t.Parallel()
	file := mustParseRenewalFile(t, renewalFileOfCertbot121)
	if err := file.set("renewalparams", "renew_hook", "/usr/local/bin/other-script"); err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(renewalFileOfCertbot121, "reload-after-certificate-renewal", "other-script", 1); file.text() != want {
		t.Fatalf("a changed key:\n%s", file.text())
	}
	file = mustParseRenewalFile(t, strings.Replace(renewalFileOfCertbot121, "renew_hook = /usr/local/bin/reload-after-certificate-renewal\n", "", 1))
	if err := file.set("renewalparams", "renew_hook", "/usr/local/bin/new-script"); err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(renewalFileOfCertbot121, "renew_hook = /usr/local/bin/reload-after-certificate-renewal\n", "", 1); file.text() != strings.Replace(want, "[[webroot_map]]", "renew_hook = /usr/local/bin/new-script\n[[webroot_map]]", 1) {
		t.Fatalf("a new key:\n%s", file.text())
	}
	if hook, found := mustParseRenewalFile(t, file.text()).one("renewalparams", "renew_hook"); !found || hook != "/usr/local/bin/new-script" {
		t.Fatal("the new key is not read in its section", hook, found)
	}
	file.remove("renewalparams", "renew_hook")
	if strings.Contains(file.text(), "renew_hook") || strings.Count(file.text(), "\n") != strings.Count(renewalFileOfCertbot121, "\n")-1 {
		t.Fatalf("a removed key:\n%s", file.text())
	}

	// An entry of the map changes in its place, and the section below the
	// map stays. A removed map leaves every other line.
	file = mustParseRenewalFile(t, renewalFileOfCertbot58)
	if err := file.set("renewalparams/webroot_map", "www.shop.example.com", "/var/www/other"); err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(renewalFileOfCertbot58, "www.shop.example.com = /var/www/certbot-challenges", "www.shop.example.com = /var/www/other", 1); file.text() != want {
		t.Fatalf("a changed entry of the map:\n%s", file.text())
	}
	file.removeSection("renewalparams/webroot_map")
	if want := strings.Replace(renewalFileOfCertbot58, "[[webroot_map]]\nshop.example.com = /var/www/certbot-challenges\nwww.shop.example.com = /var/www/certbot-challenges\n", "", 1); file.text() != want {
		t.Fatalf("a removed map:\n%s", file.text())
	}
	again := mustParseRenewalFile(t, renewalFileOfCertbot58)
	// A key that is added to a file whose section has a map keeps out of
	// the map, and a file without the section is refused.
	if err := again.set("renewalparams", "dns_rfc2136_credentials", "/etc/letsencrypt/rfc2136.ini"); err != nil {
		t.Fatal(err)
	}
	if got, found := mustParseRenewalFile(t, again.text()).one("renewalparams", "dns_rfc2136_credentials"); !found || got != "/etc/letsencrypt/rfc2136.ini" {
		t.Fatalf("the new key is not in its section:\n%s", again.text())
	}
	if err := mustParseRenewalFile(t, "version = 4.0.0\n").set("renewalparams", "renew_hook", "/x"); err == nil || !strings.Contains(err.Error(), "no section [renewalparams]") {
		t.Fatal(err)
	}
	// An indented key keeps its indentation.
	indented := mustParseRenewalFile(t, "[renewalparams]\n    renew_hook = /a\n")
	if err := indented.set("renewalparams", "renew_hook", "/b"); err != nil || indented.text() != "[renewalparams]\n    renew_hook = /b\n" {
		t.Fatal(err, indented.text())
	}
}
