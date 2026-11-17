package modules

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// certbot_certificate gets one TLS certificate with certbot and leaves its
// renewal to certbot's own timer. MMX links no ACME library: it runs the
// certbot command that is installed on the node.
//
// The module never reads what certbot prints to learn a state. It reads
// certbot's files before and after: the certificate itself for its names,
// and the certificate's renewal file (certbot_renewal_file.go) for its
// settings. Only when these differ from the task does certbot start, because
// certbot asks the certificate authority also when it has nothing to do, and
// the authority limits requests.

// The places of certbot on a node. Tests point them at their own directory.
var (
	certbotConfigurationDirectory = "/etc/letsencrypt"
	certbotWorkDirectory          = "/var/lib/letsencrypt"
	certbotLogDirectory           = "/var/log/letsencrypt"
)

// certbotChallenges are the values of challenge.
var certbotChallenges = []string{"webroot", "standalone", "dns"}

// CertbotCertificate is one TLS certificate that certbot gets and renews.
type CertbotCertificate struct {
	CertificateName            string   `yaml:"certificate_name" json:"certificate_name"`
	State                      string   `yaml:"state,omitempty" json:"state,omitempty"`
	Domains                    []string `yaml:"domains,omitempty" json:"domains,omitempty"`
	AccountEmailWhenRegistered string   `yaml:"account_email_when_registered,omitempty" json:"account_email_when_registered,omitempty"`
	Challenge                  string   `yaml:"challenge,omitempty" json:"challenge,omitempty"`
	WebrootPath                string   `yaml:"webroot_path,omitempty" json:"webroot_path,omitempty"`
	DNSPlugin                  string   `yaml:"dns_plugin,omitempty" json:"dns_plugin,omitempty"`
	DNSCredentialsFile         string   `yaml:"dns_credentials_file,omitempty" json:"dns_credentials_file,omitempty"`
	WaitAfterCreatingDNSRecord string   `yaml:"wait_after_creating_dns_record,omitempty" json:"wait_after_creating_dns_record,omitempty"`
	RunAfterIssueAndRenewal    string   `yaml:"run_after_issue_and_renewal,omitempty" json:"run_after_issue_and_renewal,omitempty"`
	CertificateAuthorityURL    string   `yaml:"certificate_authority_url,omitempty" json:"certificate_authority_url,omitempty"`
	RenewalSettingsToRemove    []string `yaml:"renewal_settings_to_remove,omitempty" json:"renewal_settings_to_remove,omitempty"`
}

// certbotCertificateDescription is what mmx describe-module shows for
// certbot_certificate.
var certbotCertificateDescription = Description{
	Name: "certbot_certificate", Topic: TopicCertificates, Schema: reflect.TypeOf(CertbotCertificate{}),
	Summary: "one TLS certificate that certbot gets from an ACME certificate authority such as Let's Encrypt; certbot's own timer renews it, MMX never does; install certbot with a package task first; using this module accepts the terms of service of the certificate authority, as certbot's --agree-tos does",
	Fields: []Field{
		{Name: "certificate_name", Required: true, Notes: "certbot's name for the certificate; its files are under /etc/letsencrypt/live/<certificate_name>/"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent deletes the certificate, its key, and its renewal settings from the node with certbot delete, and accepts only certificate_name; it does not revoke the certificate at the certificate authority"},
		{Name: "domains", Required: true, Notes: "required with state: present; the names that the certificate covers, such as shop.example.com or *.example.com; the certificate covers exactly this list, and another list gets a new certificate"},
		{Name: "account_email_when_registered", Required: true, Notes: "required with state: present; the contact address of the node's account at the certificate authority; certbot sends it when it registers that account, with the node's first certificate from that authority; MMX does not compare it afterwards, and certbot update_account changes it at the authority"},
		{Name: "challenge", Values: certbotChallenges, Required: true, Notes: "required with state: present, no default; how the certificate authority checks that the server controls the names: webroot through a file that the running web server serves, standalone through certbot's own server on port 80, where nothing else may listen, or dns through a DNS record, which a wildcard name needs"},
		{Name: "webroot_path", Notes: "required with challenge: webroot; the web root; certbot writes challenge files below <webroot_path>/.well-known/acme-challenge/, which the web server must serve at /.well-known/acme-challenge/"},
		{Name: "dns_plugin", Notes: "required with challenge: dns; the name of certbot's DNS plugin without dns-, such as cloudflare or rfc2136; install the plugin's package first"},
		{Name: "dns_credentials_file", Notes: "only with challenge: dns; the path, on the node, of the plugin's credentials file; write it with a file task from secrets, permissions_mode 0600; a plugin that reads its login from a file needs it, and route53, which gets its login another way, takes none; omit to keep the saved setting"},
		{Name: "wait_after_creating_dns_record", Notes: "only with challenge: dns; how long certbot waits after it created the DNS record before the certificate authority looks for it: a duration of whole seconds, such as 30s; omit to keep the saved setting, or use the plugin's default for a new certificate"},
		{Name: "run_after_issue_and_renewal", Notes: "the path of a script on the node, which must exist and be executable; certbot runs it after the first certificate is issued and after every renewal, also between MMX runs; omit to keep the saved setting"},
		{Name: "certificate_authority_url", Required: true, Notes: "required with state: present; the HTTP or HTTPS ACME directory URL of the certificate authority, such as https://acme-v02.api.letsencrypt.org/directory; a certificate from another authority is replaced"},
		{Name: "renewal_settings_to_remove", Notes: "List of saved settings to delete: run_after_issue_and_renewal, dns_credentials_file, or wait_after_creating_dns_record. Do not also give a setting that this list removes. An omitted setting stays unchanged."},
	},
	Example: "- certbot_certificate:\n    certificate_name: shop.example.com\n    certificate_authority_url: https://acme-v02.api.letsencrypt.org/directory\n    domains: [shop.example.com, www.shop.example.com]\n    account_email_when_registered: admin@example.com\n    challenge: webroot\n    webroot_path: /var/www/certbot-challenges\n    run_after_issue_and_renewal: /usr/local/bin/reload-nginx-after-certificate-renewal\n",
	Results: []string{"status", "change", "reason", "value.certificate_dir", "value.domains"},
	Dryrun:  "requests nothing, starts no certbot, and writes nothing; reports would_change with the names when the certificate is missing, with both lists when the names differ, with both addresses when the certificate comes from another certificate authority, and with each setting that differs when only certbot's renewal settings would change, then also with the difference of certbot's renewal file; not_checked while certbot or the DNS plugin is missing, because an earlier node task may install it, and while the script or the credentials file is missing, unless an earlier node task of the same dryrun would install that file",
}

var (
	certbotCertificateNameRule            = rule("certificate_name must use letters, digits, '.', '_', and '-', and start with a letter or a digit: certbot names the certificate's directory and its renewal file after it", regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`).MatchString)
	certbotDomainName                     = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)
	certbotAccountEmailWhenRegisteredRule = rule("account_email_when_registered must be one e-mail address, such as admin@example.com", func(s string) bool {
		name, domain, found := strings.Cut(s, "@")
		return found && name != "" && domain != "" && !strings.HasPrefix(s, "-") && !strings.ContainsAny(s, " \t\r\n,\x00") && !strings.Contains(domain, "@")
	})
	certbotDNSPluginName = regexp.MustCompile(`^[a-z0-9]+([_-][a-z0-9]+)*$`)
	certbotDNSPluginRule = parsed(func(s string) error {
		if after, found := strings.CutPrefix(strings.ReplaceAll(s, "_", "-"), "dns-"); found && after != "" {
			return fmt.Errorf("dns_plugin is the plugin's name without dns-: write %s", after)
		}
		if !certbotDNSPluginName.MatchString(s) {
			return errors.New("dns_plugin must be the name of certbot's DNS plugin in lower-case letters, digits, '-', and '_', such as cloudflare or rfc2136")
		}
		return nil
	})
	certbotWaitRule = parsed(func(s string) error {
		_, err := certbotWaitSeconds(s)
		return err
	})
	certbotDirectoryURLRule = rule("certificate_authority_url must be the HTTPS or HTTP address of a certificate authority's ACME directory, such as https://acme.example.com/directory", func(s string) bool {
		u, err := url.Parse(s)
		return err == nil && u.Hostname() != "" && (u.Scheme == "http" || u.Scheme == "https") && !strings.ContainsAny(s, " \t\r\n\x00")
	})
)

// certbotPathRule is the rule for a path that goes into certbot's renewal
// file, which holds one value on one line.
func certbotPathRule(field string) func(any) error {
	return rule(field+" must be an absolute path on one line without .. parts", func(s string) bool {
		return filepath.IsAbs(s) && !strings.ContainsAny(s, "\x00\r\n") && withoutParentPart(s)
	})
}

// certbotDomainRule is the rule for one name in domains. A certificate
// authority checks a wildcard name only through DNS, so the task's challenge
// chooses the rule.
func certbotDomainRule(challenge string) func(any) error {
	return parsed(func(s string) error {
		if !certbotDomainName.MatchString(s) {
			return errors.New("each name in domains must be a DNS name such as shop.example.com or *.example.com, in letters, digits, '-', and '.'; write a name with other letters in its xn-- form")
		}
		if strings.HasPrefix(s, "*.") && (challenge == "webroot" || challenge == "standalone") {
			return fmt.Errorf("the wildcard name %s needs challenge: dns, because a certificate authority checks a wildcard name only through DNS", s)
		}
		return nil
	})
}

// certbotWaitSeconds reads wait_after_creating_dns_record as the whole
// seconds that certbot's option takes.
func certbotWaitSeconds(written string) (int, error) {
	duration, err := time.ParseDuration(written)
	if err != nil || duration < 0 || duration%time.Second != 0 {
		return 0, errors.New("wait_after_creating_dns_record must be a duration of whole seconds, such as 30s or 2m")
	}
	return int(duration / time.Second), nil
}

// Prepare checks the inputs when the project loads.
func (c *CertbotCertificate) Prepare(base string) error {
	fields := []struct {
		name string
		set  bool
	}{
		{"domains", len(c.Domains) > 0}, {"account_email_when_registered", c.AccountEmailWhenRegistered != ""}, {"challenge", c.Challenge != ""},
		{"webroot_path", c.WebrootPath != ""}, {"dns_plugin", c.DNSPlugin != ""}, {"dns_credentials_file", c.DNSCredentialsFile != ""},
		{"wait_after_creating_dns_record", c.WaitAfterCreatingDNSRecord != ""}, {"run_after_issue_and_renewal", c.RunAfterIssueAndRenewal != ""},
		{"certificate_authority_url", c.CertificateAuthorityURL != ""}, {"renewal_settings_to_remove", c.RenewalSettingsToRemove != nil},
	}
	switch {
	case c.CertificateName == "":
		return invalid("certificate_name", "certbot_certificate.certificate_name is required: certbot's name for the certificate, such as shop.example.com")
	case c.State != "" && !slices.Contains(presenceStates, c.State):
		return invalid("state", "certbot_certificate.state must be "+strings.Join(presenceStates, " or "))
	}
	if c.State == "absent" {
		for _, field := range fields {
			if field.set {
				return invalid(field.name, "certbot_certificate with state: absent accepts only certificate_name; remove "+field.name)
			}
		}
		return nil
	}
	switch {
	case len(c.Domains) == 0:
		return invalid("domains", "certbot_certificate.domains is required: the names that the certificate covers")
	case c.AccountEmailWhenRegistered == "":
		return invalid("account_email_when_registered", "certbot_certificate.account_email_when_registered is required: the contact address of the node's account at the certificate authority")
	case c.CertificateAuthorityURL == "":
		return invalid("certificate_authority_url", "certbot_certificate.certificate_authority_url is required: write the HTTP or HTTPS ACME directory URL of the certificate authority")
	case c.Challenge == "":
		return invalid("challenge", "certbot_certificate.challenge is required: "+strings.Join(certbotChallenges, ", ")+"; challenge has no default because challenge selects how the certificate authority checks control of the domain names")
	case !slices.Contains(certbotChallenges, c.Challenge):
		return invalid("challenge", "certbot_certificate.challenge must be one of "+strings.Join(certbotChallenges, ", "))
	case c.Challenge == "webroot" && c.WebrootPath == "":
		return invalid("webroot_path", "challenge: webroot needs webroot_path: the web root; certbot writes challenge files below <webroot_path>/.well-known/acme-challenge/, which the web server must serve at /.well-known/acme-challenge/")
	case c.Challenge != "webroot" && c.WebrootPath != "":
		return invalid("webroot_path", "webroot_path goes only with challenge: webroot")
	case c.Challenge == "dns" && c.DNSPlugin == "":
		return invalid("dns_plugin", "challenge: dns needs dns_plugin: the name of certbot's DNS plugin, such as cloudflare or rfc2136")
	}
	if c.Challenge != "dns" {
		for _, field := range fields[4:7] {
			if field.set {
				return invalid(field.name, field.name+" goes only with challenge: dns")
			}
		}
	}
	for _, name := range c.RenewalSettingsToRemove {
		var given string
		switch name {
		case "run_after_issue_and_renewal":
			given = c.RunAfterIssueAndRenewal
		case "dns_credentials_file":
			given = c.DNSCredentialsFile
		case "wait_after_creating_dns_record":
			given = c.WaitAfterCreatingDNSRecord
		default:
			return invalid("renewal_settings_to_remove", "renewal_settings_to_remove accepts only run_after_issue_and_renewal, dns_credentials_file, or wait_after_creating_dns_record")
		}
		if given != "" {
			return invalid("renewal_settings_to_remove", name+" is set as a task input and listed in renewal_settings_to_remove; remove the task input or the renewal_settings_to_remove entry")
		}
	}
	return certbotRepeatedDomain(c.Domains)
}

// certbotRepeatedDomain refuses a name that domains lists twice. A name
// that is still a reference is compared once it is resolved.
func certbotRepeatedDomain(domains []string) error {
	seen := map[string]int{}
	for i, name := range domains {
		if Dynamic(name) {
			continue
		}
		if first, repeated := seen[strings.ToLower(name)]; repeated {
			return invalid(fmt.Sprintf("domains[%d]", i), fmt.Sprintf("domains lists %s twice (also at domains[%d])", name, first))
		}
		seen[strings.ToLower(name)] = i
	}
	return nil
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (c *CertbotCertificate) Arguments(arguments *ArgumentList) {
	arguments.Public("certificate_name", &c.CertificateName, certbotCertificateNameRule)
	for i := range c.Domains {
		arguments.Public(fmt.Sprintf("domains[%d]", i), &c.Domains[i], certbotDomainRule(c.Challenge))
	}
	for _, argument := range []struct {
		field  string
		target *string
		rule   func(any) error
	}{
		{"account_email_when_registered", &c.AccountEmailWhenRegistered, certbotAccountEmailWhenRegisteredRule},
		{"webroot_path", &c.WebrootPath, certbotPathRule("webroot_path")},
		{"dns_plugin", &c.DNSPlugin, certbotDNSPluginRule},
		{"dns_credentials_file", &c.DNSCredentialsFile, certbotPathRule("dns_credentials_file")},
		{"wait_after_creating_dns_record", &c.WaitAfterCreatingDNSRecord, certbotWaitRule},
		{"run_after_issue_and_renewal", &c.RunAfterIssueAndRenewal, certbotPathRule("run_after_issue_and_renewal")},
		{"certificate_authority_url", &c.CertificateAuthorityURL, certbotDirectoryURLRule},
	} {
		if *argument.target != "" {
			arguments.Public(argument.field, argument.target, argument.rule)
		}
	}
}

// CleanPaths cleans the resolved paths, which MMX compares with what
// certbot's renewal file holds.
func (c *CertbotCertificate) CleanPaths() {
	for _, path := range []*string{&c.WebrootPath, &c.DNSCredentialsFile, &c.RunAfterIssueAndRenewal} {
		if *path != "" {
			*path = filepath.Clean(*path)
		}
	}
}

// Run does the module's work on the node.
func (c *CertbotCertificate) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.CertbotCertificate(ctx, *c)
}

// certbotLiveDirectory is where certbot keeps the links to a certificate's
// current files.
func certbotLiveDirectory(name string) string {
	return filepath.Join(certbotConfigurationDirectory, "live", name)
}

func certbotCertificateFile(name string) string {
	return filepath.Join(certbotLiveDirectory(name), "cert.pem")
}

func certbotArchiveDirectory(name string) string {
	return filepath.Join(certbotConfigurationDirectory, "archive", name)
}

func certbotRenewalFilePath(name string) string {
	return filepath.Join(certbotConfigurationDirectory, "renewal", name+".conf")
}

// certbotFileLimit bounds a file of certbot that MMX reads whole.
const certbotFileLimit = 1 << 20

func readCertbotFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, certbotFileLimit+1))
	if err == nil && len(data) > certbotFileLimit {
		err = fmt.Errorf("%s holds more than 1 MiB", path)
	}
	return data, err
}

// certbotCertificateFileState is what one certificate file holds.
type certbotCertificateFileState struct {
	// names are the names that the certificate covers, from its Subject
	// Alternative Name extension. The subject's common name does not count:
	// a certificate authority may leave the subject empty.
	names []string
	// fingerprint is the SHA-256 of the certificate, in hexadecimal.
	fingerprint string
}

func readCertbotCertificateFile(path string) (certbotCertificateFileState, error) {
	data, err := readCertbotFile(path)
	if err != nil {
		return certbotCertificateFileState{}, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return certbotCertificateFileState{}, fmt.Errorf("%s holds no certificate in PEM form", path)
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return certbotCertificateFileState{}, fmt.Errorf("%s: %w", path, err)
	}
	names := slices.Clone(certificate.DNSNames)
	for _, address := range certificate.IPAddresses {
		names = append(names, address.String())
	}
	sum := sha256.Sum256(certificate.Raw)
	return certbotCertificateFileState{names: names, fingerprint: hex.EncodeToString(sum[:])}, nil
}

// certbotCertificateState is what the node has of one certificate, read
// from certbot's files.
type certbotCertificateState struct {
	// exists: certbot has a renewal file for the name.
	exists  bool
	renewal *certbotRenewalFile
	certbotCertificateFileState
}

// readCertbotCertificate reads the certificate and its renewal file. It
// fails for everything that would make certbot treat the name as new while
// files of that name exist: certbot would then get a certificate from the
// certificate authority and put it into another directory, or fail to save
// it at all.
func readCertbotCertificate(name string) (certbotCertificateState, error) {
	path := certbotRenewalFilePath(name)
	data, err := readCertbotFile(path)
	if errors.Is(err, os.ErrNotExist) {
		for _, directory := range []string{certbotLiveDirectory(name), certbotArchiveDirectory(name)} {
			if entries, err := os.ReadDir(directory); err == nil && len(entries) > 0 {
				return certbotCertificateState{}, fmt.Errorf("certbot_certificate: %s holds files, but the renewal file %s is missing after an unfinished removal or certificate request. certbot would get a certificate from the certificate authority and then fail to save the certificate. Remove the remaining files with a certbot_certificate task with state: absent, or restore the missing renewal file", directory, path)
			}
		}
		return certbotCertificateState{}, nil
	}
	damaged := func(err error) (certbotCertificateState, error) {
		return certbotCertificateState{}, fmt.Errorf("certbot_certificate: MMX cannot read the renewal file %s for certificate %s: %w. A new certbot request would save a new certificate in %s-0001 and report success. Repair the renewal file, or remove the renewal file together with %s and %s", path, name, err, certbotLiveDirectory(name), certbotLiveDirectory(name), certbotArchiveDirectory(name))
	}
	if err != nil {
		return damaged(err)
	}
	renewal, err := parseCertbotRenewalFile(string(data))
	if err != nil {
		return damaged(err)
	}
	if !renewal.hasSection("renewalparams") {
		return damaged(errors.New("certbot's renewal file has no section [renewalparams]"))
	}
	// certbot takes a renewal file only with these four links, each to a
	// file that exists.
	for _, key := range []string{"cert", "privkey", "chain", "fullchain"} {
		link, found := renewal.one("", key)
		if !found {
			return damaged(fmt.Errorf("certbot's renewal file has no setting %s", key))
		}
		if want := filepath.Join(certbotLiveDirectory(name), key+".pem"); link != want {
			return damaged(fmt.Errorf("the renewal setting %s points to %s, but MMX requires the certbot certificate path %s", key, link, want))
		}
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			return damaged(fmt.Errorf("%s is not the link that certbot makes", link))
		}
		if _, err := os.Stat(link); err != nil {
			return damaged(fmt.Errorf("the link %s leads to no file", link))
		}
	}
	certificate, err := readCertbotCertificateFile(certbotCertificateFile(name))
	if err != nil {
		return damaged(err)
	}
	return certbotCertificateState{exists: true, renewal: renewal, certbotCertificateFileState: certificate}, nil
}

// sameNames reports whether two lists hold the same names, in any order and
// in any case.
func sameNames(a, b []string) bool {
	normal := func(names []string) []string {
		out := make([]string, len(names))
		for i, name := range names {
			out[i] = strings.ToLower(name)
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	return slices.Equal(normal(a), normal(b))
}

// certbotSettings are the settings of a certificate's next renewal that a
// task declares, in the words of certbot's renewal file.
type certbotSettings struct {
	// authenticator is webroot, standalone, or dns-<plugin>.
	authenticator string
	webrootPath   string
	// plugin is the DNS plugin's name, "" for another challenge.
	plugin string
	// credentials and waitSeconds are "" when the task does not give them.
	credentials, waitSeconds string
	// hook is the script as certbot's shell reads it, "" without one.
	hook      string
	remove    []string
	preserved []certbotRenewalLine
}

func (c CertbotCertificate) settings() certbotSettings {
	s := certbotSettings{authenticator: c.Challenge, webrootPath: c.WebrootPath, hook: shellWord(c.RunAfterIssueAndRenewal), remove: c.RenewalSettingsToRemove}
	if c.Challenge == "dns" {
		s.plugin, s.authenticator, s.credentials = c.DNSPlugin, "dns-"+c.DNSPlugin, c.DNSCredentialsFile
		if c.WaitAfterCreatingDNSRecord != "" {
			seconds, _ := certbotWaitSeconds(c.WaitAfterCreatingDNSRecord)
			s.waitSeconds = strconv.Itoa(seconds)
		}
	}
	return s
}

// shellSafeWord is text that a shell reads as one word, as it stands.
var shellSafeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellWord writes a path as one word for the shell. certbot keeps a script
// as a shell command and runs it with a shell, which would split a path at
// a space.
func shellWord(path string) string {
	if path == "" || shellSafeWord.MatchString(path) {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// certbotPluginKeys are the two keys under which certbot's renewal file
// keeps a DNS plugin's settings. certbot writes the plugin's name with
// underscores there.
func certbotPluginKeys(plugin string) (credentials, wait string) {
	prefix := "dns_" + strings.ReplaceAll(plugin, "-", "_")
	return prefix + "_credentials", prefix + "_propagation_seconds"
}

const certbotRenewalParameters = "renewalparams"

// differences lists the task's fields whose value certbot's renewal file
// does not hold, each as a sentence.
func (s certbotSettings) differences(file *certbotRenewalFile) []string {
	var found []string
	compare := func(field, key, want string) {
		if want == "" {
			return
		}
		have, has := file.one(certbotRenewalParameters, key)
		switch {
		case want == "" && has:
			found = append(found, fmt.Sprintf("%s: certbot has %s, and the task gives none", field, have))
		case want != "" && !has:
			found = append(found, fmt.Sprintf("%s: certbot has none, and the task says %s", field, want))
		case want != have:
			found = append(found, fmt.Sprintf("%s: certbot has %s, and the task says %s", field, have, want))
		}
	}
	old, _ := file.one(certbotRenewalParameters, "authenticator")
	if old != s.authenticator {
		found = append(found, fmt.Sprintf("challenge: certbot has %s, and the task says %s", certbotChallengeOf(old), certbotChallengeOf(s.authenticator)))
	}
	if s.authenticator == "webroot" {
		paths, _ := file.value(certbotRenewalParameters, "webroot_path")
		have := strings.Join(paths, ", ")
		if len(paths) == 0 {
			have = "none"
		}
		differs := len(paths) != 1 || paths[0] != s.webrootPath
		// certbot uses a name's entry in the map before webroot_path, so
		// every entry counts.
		for _, entry := range file.settings(certbotRenewalParameters + "/webroot_map") {
			if path := strings.Join(entry.values, ", "); len(entry.values) != 1 || path != s.webrootPath {
				differs = true
				if len(paths) != 1 || path != paths[0] {
					have += fmt.Sprintf(" and, for %s, %s", entry.key, path)
				}
			}
		}
		if differs {
			found = append(found, fmt.Sprintf("webroot_path: certbot has %s, and the task says %s", have, s.webrootPath))
		}
	}
	if s.plugin != "" {
		credentials, wait := certbotPluginKeys(s.plugin)
		compare("dns_credentials_file", credentials, s.credentials)
		compare("wait_after_creating_dns_record", wait, s.waitSeconds)
	}
	compare("run_after_issue_and_renewal", "renew_hook", s.hook)
	for _, entry := range file.settings(certbotRenewalParameters) {
		if field := certbotOptionalInput(entry.key); field != "" && slices.Contains(s.remove, field) {
			found = append(found, fmt.Sprintf("%s: certbot has %s, and renewal_settings_to_remove deletes it", field, strings.Join(entry.values, ", ")))
		}
	}
	for _, entry := range s.preserved {
		compare(certbotOptionalInput(entry.key), entry.key, entry.values[0])
	}
	return found
}

// certbotChallengeOf names an authenticator of certbot's renewal file in
// the task's words.
func certbotChallengeOf(authenticator string) string {
	if plugin, found := strings.CutPrefix(authenticator, "dns-"); found {
		return "dns with dns_plugin " + plugin
	}
	if authenticator == "" {
		return "none"
	}
	return authenticator
}

// write puts the settings into the renewal file. It changes only the
// lines of the keys whose values differ from the task's, so every other
// line stays as certbot or a person wrote it, also the order of the lines.
// It never writes server or account: with only the address of the
// certificate authority changed, the certificate could never renew.
func (s certbotSettings) write(file *certbotRenewalFile) error {
	// ensure makes key in section hold text, or no value for "". list: the
	// file writes the value as a list of one.
	ensure := func(section, key, text string, list bool) error {
		values, has := file.value(section, key)
		switch {
		case text == "":
			file.remove(section, key)
			return nil
		case has && len(values) == 1 && values[0] == text:
			return nil
		}
		written, err := certbotRenewalWritten(text, list)
		if err != nil {
			return err
		}
		if list {
			written += ","
		}
		return file.set(section, key, written)
	}
	if err := ensure(certbotRenewalParameters, "authenticator", s.authenticator, false); err != nil {
		return err
	}
	webrootMap := certbotRenewalParameters + "/webroot_map"
	if s.authenticator == "webroot" {
		if err := ensure(certbotRenewalParameters, "webroot_path", s.webrootPath, true); err != nil {
			return err
		}
		// certbot uses a name's entry in the map before webroot_path, so each
		// entry gets the directory. A file without the map needs none:
		// certbot writes it at the next renewal.
		for _, entry := range file.settings(webrootMap) {
			if err := ensure(webrootMap, entry.key, s.webrootPath, false); err != nil {
				return err
			}
		}
	} else {
		file.remove(certbotRenewalParameters, "webroot_path")
		file.removeSection(webrootMap)
	}
	if s.plugin != "" {
		credentials, wait := certbotPluginKeys(s.plugin)
		if s.credentials != "" {
			if err := ensure(certbotRenewalParameters, credentials, s.credentials, false); err != nil {
				return err
			}
		}
		if s.waitSeconds != "" {
			if err := ensure(certbotRenewalParameters, wait, s.waitSeconds, false); err != nil {
				return err
			}
		}
	}
	if s.hook != "" {
		if err := ensure(certbotRenewalParameters, "renew_hook", s.hook, false); err != nil {
			return err
		}
	}
	for _, entry := range s.preserved {
		if err := ensure(certbotRenewalParameters, entry.key, entry.values[0], false); err != nil {
			return err
		}
	}
	for _, entry := range file.settings(certbotRenewalParameters) {
		if slices.Contains(s.remove, certbotOptionalInput(entry.key)) {
			file.remove(certbotRenewalParameters, entry.key)
		}
	}
	return nil
}

// certbotOptionalInput maps native renewal keys to their public input.
func certbotOptionalInput(key string) string {
	if key == "renew_hook" {
		return "run_after_issue_and_renewal"
	}
	if strings.HasPrefix(key, "dns_") {
		if strings.HasSuffix(key, "_credentials") {
			return "dns_credentials_file"
		}
		if strings.HasSuffix(key, "_propagation_seconds") {
			return "wait_after_creating_dns_record"
		}
	}
	return ""
}

// A certificate request lets certbot rewrite its renewal file. Carry omitted
// settings through that rewrite, including settings of a previous DNS plugin.
func (s certbotSettings) preserveOmitted(file *certbotRenewalFile) certbotSettings {
	if file == nil {
		return s
	}
	for _, entry := range file.settings(certbotRenewalParameters) {
		field := certbotOptionalInput(entry.key)
		if field == "" || slices.Contains(s.remove, field) {
			continue
		}
		given := map[string]string{"dns_credentials_file": s.credentials, "wait_after_creating_dns_record": s.waitSeconds, "run_after_issue_and_renewal": s.hook}[field]
		if given == "" && len(entry.values) == 1 {
			s.preserved = append(s.preserved, entry)
		}
	}
	return s
}

// certbotValue is the value of a certbot_certificate result.
func certbotValue(name string, names []string) map[string]any {
	list := make([]any, len(names))
	for i, domain := range names {
		list[i] = domain
	}
	return map[string]any{"certificate_dir": certbotLiveDirectory(name), "domains": list}
}

// CertbotCertificate compares the certificate that certbot has under the
// task's name, and its renewal settings, with the task, and starts certbot
// only for a certificate that the node does not have, that covers other
// names, or that comes from another certificate authority.
func (run Runtime) CertbotCertificate(ctx context.Context, c CertbotCertificate) protocol.Result {
	if c.State == "absent" {
		return run.certbotCertificateAbsent(ctx, c)
	}
	if err := certbotRepeatedDomain(c.Domains); err != nil {
		return protocol.ConfigurationError(err)
	}
	program, err := lookPath(ctx, "certbot")
	if err != nil {
		return run.notYet(missingProgramError("certbot"))
	}
	have, err := readCertbotCertificate(c.CertificateName)
	if err != nil {
		return protocol.Failed(err)
	}
	if stop := run.certbotPrerequisites(ctx, c, program); stop != nil {
		return *stop
	}
	authority := c.CertificateAuthorityURL
	settings := c.settings()
	request, otherAuthority := c.newCertificateNeeded(have, authority)
	if request == "" && (run.Dryrun || len(settings.differences(have.renewal)) == 0) {
		// Nothing to do, or a dryrun's account of the settings that differ.
		return run.writeCertbotSettings(ctx, c, have, settings)
	}
	if run.Dryrun {
		r := protocol.Success("would_change")
		r.Reason = fmt.Sprintf("would ask the certificate authority %s for a certificate for %s: %s", authority, strings.Join(c.Domains, ", "), request)
		r.Value = certbotValue(c.CertificateName, c.Domains)
		return r
	}
	// A run that changes something waits for another certbot first, also
	// when it only writes settings: a renewal that runs now has read the
	// renewal file and writes it again when it ends, and MMX's change would
	// be lost. After a wait the task compares again with what the other
	// certbot left.
	waited, err := waitUntilNoOtherCertbotRuns(ctx)
	if err != nil {
		r := protocol.Failed(err)
		r.Change = "unchanged"
		return r
	}
	if waited > 0 {
		if have, err = readCertbotCertificate(c.CertificateName); err != nil {
			return protocol.Failed(err)
		}
		request, otherAuthority = c.newCertificateNeeded(have, authority)
	}
	if request == "" {
		r := run.writeCertbotSettings(ctx, c, have, settings)
		if sentence := waitedSentence(waited); r.Status == "succeeded" && sentence != "" {
			if r.Reason != "" {
				r.Reason += "."
			}
			r.Reason = strings.TrimSpace(r.Reason + sentence)
		}
		return r
	}
	if err := run.Mutation("certbot_certificate " + c.CertificateName); err != nil {
		return protocol.Failed(err)
	}
	mark := markCertbotLog()
	settings = settings.preserveOmitted(have.renewal)
	printed := runCertbot(ctx, program, c.requestArguments(authority, settings, otherAuthority)...)
	after, err := readCertbotCertificate(c.CertificateName)
	failed := func(err error) protocol.Result {
		r := protocol.Failed(err)
		if after.fingerprint != have.fingerprint {
			r.Change = "changed"
		}
		return r
	}
	switch {
	case ctx.Err() != nil:
		return failed(fmt.Errorf("certbot_certificate: certbot was stopped while it asked for the certificate %s: %w", c.CertificateName, ctx.Err()))
	case printed.Status != "succeeded":
		// The authority's own answer says more than certbot's exit code and
		// text, and the same in every version of certbot.
		if problem, found := lastAuthorityErrorIn(mark.since()); found {
			return failed(fmt.Errorf("certbot_certificate: the certificate authority %s refused the request for the certificate %s: %s", authority, c.CertificateName, problem.sentence()))
		}
		return failed(fmt.Errorf("certbot_certificate: certbot did not get the certificate %s from %s:\n%s", c.CertificateName, authority, certbotOutput(printed)))
	case err != nil:
		return failed(err)
	case !after.exists || !sameNames(after.names, c.Domains):
		// certbot's exit code does not prove where the certificate is.
		return failed(fmt.Errorf("certbot_certificate: certbot reported success, but the certificate directory %s does not contain a certificate for the requested names %s (names covered by the existing certificate: %s). certbot may have saved the new certificate in another directory under %s", certbotLiveDirectory(c.CertificateName), strings.Join(c.Domains, ", "), strings.Join(after.names, ", "), filepath.Join(certbotConfigurationDirectory, "live")))
	}
	if server, _ := after.renewal.one(certbotRenewalParameters, "server"); server != authority {
		return failed(fmt.Errorf("certbot_certificate: certbot reported success, and its renewal settings of the certificate %s still name the certificate authority %s, not %s", c.CertificateName, server, authority))
	}
	// certbot saved the settings of its own command line. Whatever still
	// differs, MMX writes.
	written := run.writeCertbotSettings(ctx, c, after, settings)
	if written.Status != "succeeded" {
		written.Change = "changed"
		return written
	}
	r := protocol.Success("changed")
	r.Reason = fmt.Sprintf("got a certificate for %s from the certificate authority %s: %s.%s", strings.Join(after.names, ", "), authority, request, waitedSentence(waited))
	r.Value = certbotValue(c.CertificateName, after.names)
	return r
}

// newCertificateNeeded says why the node needs a new certificate, "" when
// it has a certificate for the task's names from the task's certificate
// authority. otherAuthority: the certificate that the node has comes from
// another authority.
func (c CertbotCertificate) newCertificateNeeded(have certbotCertificateState, authority string) (why string, otherAuthority bool) {
	if !have.exists {
		return fmt.Sprintf("the node has no certificate named %s", c.CertificateName), false
	}
	if !sameNames(have.names, c.Domains) {
		why = fmt.Sprintf("the certificate %s covers %s, and the task says %s", c.CertificateName, strings.Join(have.names, ", "), strings.Join(c.Domains, ", "))
	}
	if server, _ := have.renewal.one(certbotRenewalParameters, "server"); server != authority {
		otherAuthority = true
		if why != "" {
			why += "; "
		}
		why += fmt.Sprintf("the certificate %s comes from the certificate authority %s, and the task says %s", c.CertificateName, server, authority)
	}
	return why, otherAuthority
}

// certbotPrerequisites checks what an earlier node task may still install:
// the script, the DNS plugin, and the plugin's credentials file. certbot
// itself finds a missing credentials file only after it asked the
// certificate authority, names no plugin in its message for a missing
// plugin when the task gives a credentials file, and stops for a missing
// script only at the first certificate: at a renewal it reports success
// without it.
//
// A dryrun cannot check a task whose script or credentials file is missing,
// unless an earlier node task of the same dryrun would create or change
// that file, or a directory that holds it: then the file counts as there, and the dryrun
// goes on to say what a run would do.
func (run Runtime) certbotPrerequisites(ctx context.Context, c CertbotCertificate, program string) *protocol.Result {
	stop := func(format string, values ...any) *protocol.Result {
		r := run.notYet(fmt.Errorf("certbot_certificate: "+format, values...))
		return &r
	}
	comesInThisDryrun := func(path string) bool {
		return run.EarlierNodeTaskWouldChange(path)
	}
	if script := c.RunAfterIssueAndRenewal; script != "" && !comesInThisDryrun(script) {
		info, err := os.Stat(script)
		switch {
		case err != nil:
			return stop("the script %s of run_after_issue_and_renewal does not exist; install it in an earlier node task, for example with file and permissions_mode 0755", script)
		case !info.Mode().IsRegular() || info.Mode()&0111 == 0:
			return stop("the script %s of run_after_issue_and_renewal is not an executable file; give it permissions_mode 0755 in an earlier node task", script)
		}
	}
	if c.Challenge != "dns" {
		return nil
	}
	if plugins, known := certbotPluginsOf(ctx, program); known && !slices.Contains(plugins, "dns-"+c.DNSPlugin) {
		return stop("certbot's DNS plugin %s is not installed on this node; install its package in an earlier node task: python3-certbot-dns-%s", c.DNSPlugin, c.DNSPlugin)
	}
	if file := c.DNSCredentialsFile; file != "" && !comesInThisDryrun(file) {
		if info, err := os.Stat(file); err != nil || !info.Mode().IsRegular() {
			return stop("the credentials file %s of dns_credentials_file does not exist; write it in an earlier node task, for example with file and permissions_mode 0600", file)
		}
	}
	return nil
}

// requestArguments are certbot's arguments for a new certificate.
func (c CertbotCertificate) requestArguments(authority string, settings certbotSettings, otherAuthority bool) []string {
	for _, entry := range settings.preserved {
		switch entry.key {
		case "renew_hook":
			settings.hook = entry.values[0]
		default:
			credentials, wait := certbotPluginKeys(settings.plugin)
			if entry.key == credentials {
				settings.credentials = entry.values[0]
			}
			if entry.key == wait {
				settings.waitSeconds = entry.values[0]
			}
		}
	}
	args := []string{"certonly", "--non-interactive", "--agree-tos", "--email", c.AccountEmailWhenRegistered, "--no-eff-email",
		"--server", authority, "--cert-name", c.CertificateName}
	switch c.Challenge {
	case "webroot":
		args = append(args, "--webroot", "--webroot-path", c.WebrootPath)
	case "standalone":
		args = append(args, "--standalone")
	case "dns":
		args = append(args, "--authenticator", settings.authenticator)
		if settings.credentials != "" {
			args = append(args, "--dns-"+c.DNSPlugin+"-credentials", settings.credentials)
		}
		if settings.waitSeconds != "" {
			args = append(args, "--dns-"+c.DNSPlugin+"-propagation-seconds", settings.waitSeconds)
		}
	}
	for _, name := range c.Domains {
		args = append(args, "-d", name)
	}
	if settings.hook != "" {
		// certbot's own check of a script takes the text up to the first
		// space for the program, so it refuses a path that needed quotes.
		// MMX has checked the script itself.
		args = append(args, "--deploy-hook", settings.hook, "--disable-hook-validation")
	}
	if otherAuthority {
		// Without this, certbot keeps the certificate of the other authority
		// and reports success.
		args = append(args, "--force-renewal")
	}
	return args
}

// writeCertbotSettings writes the task's settings into the renewal file of
// a certificate that has the task's names, when they differ. Nobody tests
// them until certbot's next renewal.
func (run Runtime) writeCertbotSettings(ctx context.Context, c CertbotCertificate, have certbotCertificateState, settings certbotSettings) protocol.Result {
	unchanged := protocol.Success("unchanged")
	unchanged.Value = certbotValue(c.CertificateName, have.names)
	differences := settings.differences(have.renewal)
	if len(differences) == 0 {
		return unchanged
	}
	path := certbotRenewalFilePath(c.CertificateName)
	cannot := func(err error) protocol.Result {
		return protocol.Failed(fmt.Errorf("certbot_certificate: MMX cannot write the task's settings into %s: %w", path, err))
	}
	if err := settings.write(have.renewal); err != nil {
		return cannot(err)
	}
	text := have.renewal.text()
	// What MMX is about to write must read back as what the task says.
	if again, err := parseCertbotRenewalFile(text); err != nil || len(settings.differences(again)) > 0 {
		return cannot(errors.New("the changed certbot renewal file does not decode to the requested task settings; the mismatch is a fault in MMX"))
	}
	r := run.File(ctx, File{Path: path, Literal: &text}, value.Scope{})
	if r.Status != "succeeded" {
		r.Reason = cannot(errors.New(r.Reason)).Reason
		return r
	}
	r.Value = unchanged.Value
	would := "wrote"
	if run.Dryrun {
		would = "would write"
	}
	r.Reason = fmt.Sprintf("%s the task's settings into certbot's renewal settings of the certificate %s, for its next renewal: %s. No new certificate is requested, and nobody tests these settings until certbot renews", would, c.CertificateName, strings.Join(differences, "; "))
	return r
}

// certbotCertificateAbsent deletes a certificate with certbot delete, which
// asks no certificate authority, and finishes a removal that stopped part of
// the way. certbot knows a certificate by its renewal file and removes that
// file first. So a certbot delete that was stopped, like a first request
// that was stopped while certbot saved the certificate, leaves the
// certificate's two directories, with its private key, and no renewal file.
// MMX then removes the directories itself.
func (run Runtime) certbotCertificateAbsent(ctx context.Context, c CertbotCertificate) protocol.Result {
	name := c.CertificateName
	renewal := certbotRenewalFilePath(name)
	// look reads what the node has of the certificate: whether certbot knows
	// it, and which of its two directories are there. A directory under its
	// staging name is one whose removal by MMX stopped half way.
	look := func() (known bool, directories []string, err error) {
		there := func(path string) bool {
			_, statErr := os.Lstat(path)
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) && err == nil {
				err = statErr
			}
			return statErr == nil
		}
		known = there(renewal)
		for _, directory := range []string{certbotLiveDirectory(name), certbotArchiveDirectory(name)} {
			if there(directory) || there(filepath.Join(filepath.Dir(directory), stagingName(name))) {
				directories = append(directories, directory)
			}
		}
		return known, directories, err
	}
	known, directories, err := look()
	if err != nil {
		return protocol.Failed(err)
	}
	if !known && len(directories) == 0 {
		return protocol.Success("unchanged")
	}
	// certbot delete needs certbot; the directories that an unfinished
	// removal left need none.
	program := ""
	findCertbot := func() *protocol.Result {
		if program, err = lookPath(ctx, "certbot"); err != nil {
			r := run.notYet(missingProgramError("certbot"))
			return &r
		}
		return nil
	}
	if known {
		if stop := findCertbot(); stop != nil {
			return *stop
		}
	}
	if run.Dryrun {
		r := protocol.Success("would_change")
		r.Reason = fmt.Sprintf("would delete the certificate %s, its key, and its renewal settings", name)
		if !known {
			r.Reason = fmt.Sprintf("would remove what an unfinished removal or request of the certificate %s left on the node: %s", name, strings.Join(directories, ", "))
		}
		return r
	}
	waited, err := waitUntilNoOtherCertbotRuns(ctx)
	if err != nil {
		r := protocol.Failed(err)
		r.Change = "unchanged"
		return r
	}
	if waited > 0 {
		// The other certbot may have been the removal itself.
		if known, directories, err = look(); err != nil {
			return protocol.Failed(err)
		}
		if !known && len(directories) == 0 {
			r := protocol.Success("unchanged")
			r.Reason = strings.TrimSpace(waitedSentence(waited))
			return r
		}
		if known && program == "" {
			if stop := findCertbot(); stop != nil {
				return *stop
			}
		}
	}
	if err := run.Mutation("certbot_certificate " + name); err != nil {
		return protocol.Failed(err)
	}
	// The copies go first: after the archive is gone nothing would say
	// which keys were this certificate's, and a run that stopped in between
	// would leave them for good.
	copies, err := removeCopiesOfCertbotKeys(name)
	changed := copies > 0
	failed := func(err error) protocol.Result {
		r := protocol.Failed(err)
		if changed {
			r.Change = "changed"
		}
		return r
	}
	if err != nil {
		return failed(fmt.Errorf("certbot_certificate: removing the copies of the keys of the certificate %s: %w", name, err))
	}
	reason := fmt.Sprintf("removed what an unfinished removal or request of the certificate %s left on the node: %s.", name, strings.Join(directories, ", "))
	if known {
		before := len(directories)
		printed := runCertbot(ctx, program, "delete", "--cert-name", name, "--non-interactive")
		if known, directories, err = look(); err != nil {
			return failed(err)
		}
		changed = changed || !known || len(directories) < before
		switch {
		case ctx.Err() != nil:
			return failed(fmt.Errorf("certbot_certificate: certbot was stopped while it deleted the certificate %s: %w", name, ctx.Err()))
		case known:
			// Without its renewal file gone, the certificate is not deleted.
			return failed(fmt.Errorf("certbot_certificate: certbot delete left %s of the certificate %s:\n%s", strings.Join(append([]string{renewal}, directories...), ", "), name, certbotOutput(printed)))
		}
		reason = fmt.Sprintf("deleted the certificate %s, its key, and its renewal settings.", name)
		if len(directories) > 0 {
			reason += fmt.Sprintf(" certbot delete left %s, which MMX removed.", strings.Join(directories, ", "))
		}
	}
	// What certbot delete left in this run, or a removal or request before
	// it: the directory module deletes each tree, and finishes a deletion of
	// its own that stopped half way.
	for _, directory := range directories {
		removed := run.Directory(ctx, Directory{Path: directory, State: "absent", AlsoDeleteEverythingInside: true}, value.Scope{})
		changed = changed || removed.Change == "changed"
		if removed.Status != "succeeded" {
			return failed(fmt.Errorf("certbot_certificate: removing %s of the certificate %s: %s", directory, name, removed.Reason))
		}
	}
	r := protocol.Success("changed")
	r.Reason = reason + waitedSentence(waited)
	if copies > 0 {
		r.Reason += fmt.Sprintf(" Also removed %d copies of its keys and certificate requests that this older certbot keeps in %s and %s.", copies, filepath.Join(certbotConfigurationDirectory, "keys"), filepath.Join(certbotConfigurationDirectory, "csr"))
	}
	return r
}
