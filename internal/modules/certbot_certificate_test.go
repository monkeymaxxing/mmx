package modules

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"mmx/internal/protocol"
)

// A stand-in certbot keeps a node's certificates in $dir/etc as certbot
// does in /etc/letsencrypt, and never uses the network. The test makes each
// certificate that it can "get" beforehand, in $dir/issuable/<names joined
// with +>.pem. These files of $dir steer it:
//
//	authority-refuses   certonly fails as certbot 2.1.0 does, with an error
//	                    of its own, and its log holds this file as the
//	                    certificate authority's answer
//	certbot-fails       certonly fails with this text on standard error
//	saves-elsewhere     certonly puts the certificate into <name>-0001 and
//	                    reports success, as certbot does when it cannot
//	                    read a renewal file
//	delete-fails        delete fails and deletes nothing
//	delete-stops-after-the-renewal-file
//	                    delete removes the renewal file, as certbot does
//	                    first, and then fails: the certificate's two
//	                    directories stay
//
// certonly changes nothing for a certificate that it has for the same
// names, unless --force-renewal is given. Every call is in $dir/calls.
const fakeCertbot = `
etc="$dir/etc"; log="$dir/log/letsencrypt.log"
mkdir -p "$etc/renewal" "$etc/live" "$etc/archive" "$dir/log" "$dir/work"
entry() { echo "2026-10-09 17:09:21,678:DEBUG:$1"; }
command=$1; shift
server=; name=; webroot=; authenticator=; credentials=; wait=; hook=; force=no; names=
while [ $# -gt 0 ]; do
  case "$1" in
  --server) server=$2; shift;;
  --cert-name) name=$2; shift;;
  --email) shift;;
  --webroot) authenticator=webroot;;
  --standalone) authenticator=standalone;;
  --webroot-path) webroot=$2; shift;;
  --authenticator) authenticator=$2; shift;;
  --dns-*-credentials) credentials=$2; shift;;
  --dns-*-propagation-seconds) wait=$2; shift;;
  --deploy-hook) hook=$2; shift;;
  --force-renewal) force=yes;;
  -d) names="$names $2"; shift;;
  --non-interactive|--agree-tos|--no-eff-email|--disable-hook-validation) ;;
  *) echo "unexpected certbot argument $1" >&2; exit 64;;
  esac
  shift
done
names=${names# }
case "$command" in
certonly)
  { entry "certbot._internal.main:certbot version: 4.0.0"; entry "acme.client:Sending GET request to $server."; entry "acme.client:Received response:"; printf 'HTTP 200\nContent-Type: application/json\n\n{}\n'; } >> "$log"
  if [ -f "$dir/authority-refuses" ]; then
    { entry "acme.client:Received response:"; cat "$dir/authority-refuses"; entry "certbot._internal.log:Exiting abnormally:"; echo "Traceback (most recent call last):"; } >> "$log"
    echo "An unexpected error occurred:" >&2; echo "AttributeError: can't set attribute" >&2; exit 1
  fi
  if [ -f "$dir/certbot-fails" ]; then echo "Requesting a certificate for $names"; cat "$dir/certbot-fails" >&2; exit 1; fi
  key=$(echo "$names" | tr ' ' '+')
  if [ -f "$etc/renewal/$name.conf" ] && [ $force = no ] && [ "$(cat "$dir/issued-names-$name" 2>/dev/null)" = "$key" ]; then
    echo "Certificate not yet due for renewal; no action taken."; exit 0
  fi
  [ -f "$dir/issuable/$key.pem" ] || { echo "the test made no certificate for $key" >&2; exit 65; }
  target=$name
  [ ! -f "$dir/saves-elsewhere" ] || target=$name-0001
  mkdir -p "$etc/archive/$target" "$etc/live/$target"
  n=$(( $(ls "$etc/archive/$target" | grep -c '^cert') + 1 ))
  cp "$dir/issuable/$key.pem" "$etc/archive/$target/cert$n.pem"
  for part in privkey chain fullchain; do echo "$part $n" > "$etc/archive/$target/$part$n.pem"; done
  for part in cert privkey chain fullchain; do ln -sf "../../archive/$target/$part$n.pem" "$etc/live/$target/$part.pem"; done
  echo "$key" > "$dir/issued-names-$target"
  {
    echo "# renew_before_expiry = 30 days"; echo "version = 4.0.0"; echo "archive_dir = $etc/archive/$target"
    for part in cert privkey chain fullchain; do echo "$part = $etc/live/$target/$part.pem"; done
    echo; echo "# Options used in the renewal process"; echo "[renewalparams]"; echo "account = 00d1f2dd2e9cfc561d10d8808c07a80a"
    if [ -n "$hook" ]; then case "$hook" in *[\ \',]*) echo "renew_hook = \"$hook\"";; *) echo "renew_hook = $hook";; esac; fi
    echo "server = $server"; echo "authenticator = $authenticator"
    plugin=$(echo "${authenticator#dns-}" | tr - _)
    [ -z "$credentials" ] || echo "dns_${plugin}_credentials = $credentials"
    [ -z "$wait" ] || echo "dns_${plugin}_propagation_seconds = $wait"
    [ "$authenticator" != webroot ] || echo "webroot_path = $webroot,"
    echo "key_type = ecdsa"
    if [ "$authenticator" = webroot ]; then echo "[[webroot_map]]"; for d in $names; do echo "$d = $webroot"; done; fi
  } > "$etc/renewal/$target.conf"
  [ -z "$hook" ] || RENEWED_LINEAGE="$etc/live/$target" RENEWED_DOMAINS="$names" sh -c "$hook"
  echo "Successfully received certificate.";;
delete)
  [ -f "$etc/renewal/$name.conf" ] || { echo "No certificate found with name $name (expected $etc/renewal/$name.conf)." >&2; exit 1; }
  [ ! -f "$dir/delete-fails" ] || { echo "certbot could not delete the certificate" >&2; exit 1; }
  [ ! -f "$dir/delete-stops-after-the-renewal-file" ] || { rm "$etc/renewal/$name.conf"; echo "certbot was stopped" >&2; exit 1; }
  rm -rf "$etc/live/$name" "$etc/archive/$name" "$etc/renewal/$name.conf"
  echo "Deleted all files relating to certificate $name.";;
*) echo "unexpected certbot $command" >&2; exit 64;;
esac`

// certbotFixture is a node with the stand-in certbot.
type certbotFixture struct {
	t   *testing.T
	dir string
	// plugins are the plugins that the node's certbot can load.
	plugins []string
	// script is a script for run_after_issue_and_renewal, which appends the
	// certificate's directory and names to the file ran.
	script, ran string
}

const (
	testCertificate = "shop.example.com"
	testAuthority   = "https://acme.test.example/directory"
)

func newCertbotFixture(t *testing.T) *certbotFixture {
	t.Helper()
	f := &certbotFixture{t: t, dir: standInPrograms(t, map[string]string{"certbot": fakeCertbot}),
		plugins: []string{"dns-rfc2136", "manual", "null", "standalone", "webroot"}}
	old := [3]string{certbotConfigurationDirectory, certbotWorkDirectory, certbotLogDirectory}
	oldPlugins, oldPoll, oldWait := certbotPluginsOf, certbotLockPoll, certbotLockWait
	certbotConfigurationDirectory, certbotWorkDirectory, certbotLogDirectory = filepath.Join(f.dir, "etc"), filepath.Join(f.dir, "work"), filepath.Join(f.dir, "log")
	certbotPluginsOf = func(context.Context, string) ([]string, bool) { return f.plugins, true }
	certbotLockPoll = 5 * time.Millisecond
	t.Cleanup(func() {
		certbotConfigurationDirectory, certbotWorkDirectory, certbotLogDirectory = old[0], old[1], old[2]
		certbotPluginsOf, certbotLockPoll, certbotLockWait = oldPlugins, oldPoll, oldWait
	})
	for _, directory := range []string{certbotConfigurationDirectory, certbotWorkDirectory, certbotLogDirectory} {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	f.ran = filepath.Join(f.dir, "script-ran")
	f.script = f.executable("reload-after-certificate-renewal")
	return f
}

// executable writes a script that notes its run, under name, and returns
// its path.
func (f *certbotFixture) executable(name string) string {
	f.t.Helper()
	path := filepath.Join(f.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		f.t.Fatal(err)
	}
	writeStandInProgram(f.t, path, "#!/bin/sh\necho \"$0 $RENEWED_LINEAGE [$RENEWED_DOMAINS]\" >> "+f.ran+"\n")
	return path
}

// issuable makes a new certificate for names, which the stand-in certbot
// then gets for a request of exactly these names in this order. It returns
// the certificate in PEM form.
func (f *certbotFixture) issuable(names ...string) []byte {
	f.t.Helper()
	certificate, _ := testCertificateFor(f.t, names...)
	writeState(f.t, filepath.Join(f.dir, "issuable", strings.Join(names, "+")+".pem"), string(certificate))
	return certificate
}

// testCertificateFor makes a self-signed certificate with an empty subject,
// as a certificate authority's test server issues it, and its key.
func testCertificateFor(t *testing.T, names ...string) (certificate []byte, key *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key
}

func (f *certbotFixture) apply(c CertbotCertificate) protocol.Result {
	f.t.Helper()
	return f.with(Runtime{}, c)
}

func (f *certbotFixture) dryrun(c CertbotCertificate) protocol.Result {
	f.t.Helper()
	return f.with(Runtime{Dryrun: true}, c)
}

// with runs the task as the node task execution engine does: checked when
// the project loads, its arguments checked, its paths cleaned.
func (f *certbotFixture) with(run Runtime, c CertbotCertificate) protocol.Result {
	f.t.Helper()
	if err := c.Prepare(""); err != nil {
		f.t.Fatal(err)
	}
	var arguments ArgumentList
	c.Arguments(&arguments)
	for _, argument := range arguments.List {
		if err := argument.Rule(argument.Raw); err != nil {
			f.t.Fatalf("%s: %v", argument.Field, err)
		}
	}
	c.CleanPaths()
	return c.Run(testContext(f.t), NodeTask{Runtime: run})
}

// webroot is the task of the design's example, for the test authority.
func (f *certbotFixture) webroot() CertbotCertificate {
	return CertbotCertificate{CertificateName: testCertificate, Domains: []string{"shop.example.com", "www.shop.example.com"},
		AccountEmailWhenRegistered: "admin@example.com", Challenge: "webroot", WebrootPath: "/var/www/certbot-challenges",
		RunAfterIssueAndRenewal: f.script, CertificateAuthorityURL: testAuthority}
}

func (f *certbotFixture) renewalFile() string {
	f.t.Helper()
	return readState(f.t, certbotRenewalFilePath(testCertificate))
}

// fingerprint is the SHA-256 of the certificate that the node has under the
// test's name, "" when it has none.
func (f *certbotFixture) fingerprint() string {
	f.t.Helper()
	certificate, err := readCertbotCertificateFile(certbotCertificateFile(testCertificate))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		f.t.Fatal(err)
	}
	return certificate.fingerprint
}

func certbotDomains(r protocol.Result) string {
	v, _ := r.Value.(map[string]any)
	names, _ := v["domains"].([]any)
	var out []string
	for _, name := range names {
		out = append(out, name.(string))
	}
	return strings.Join(out, " ")
}

func certbotDirectory(r protocol.Result) any {
	v, _ := r.Value.(map[string]any)
	return v["certificate_dir"]
}

// The first run gets the certificate with one certbot command, the second
// run starts no certbot at all, and a dryrun changes nothing and says what a
// run would do.
func TestCertbotCertificateIsRequestedOnceAndThenLeftAlone(t *testing.T) {
	f := newCertbotFixture(t)
	f.issuable("shop.example.com", "www.shop.example.com")
	task := f.webroot()

	preview := f.dryrun(task)
	if preview.Status != "succeeded" || preview.Change != "would_change" ||
		preview.Reason != "would ask the certificate authority "+testAuthority+" for a certificate for shop.example.com, www.shop.example.com: the node has no certificate named shop.example.com" ||
		certbotDomains(preview) != "shop.example.com www.shop.example.com" {
		t.Fatalf("%+v", preview)
	}
	if calls := takeCalls(t, f.dir); len(calls) != 0 || f.renewalFile() != "" || readState(t, f.ran) != "" {
		t.Fatal("the dryrun started certbot or changed the node", calls)
	}

	applied := f.apply(task)
	live := filepath.Join(f.dir, "etc/live/shop.example.com")
	if applied.Status != "succeeded" || applied.Change != "changed" || certbotDirectory(applied) != live || certbotDomains(applied) != "shop.example.com www.shop.example.com" ||
		applied.Reason != "got a certificate for shop.example.com, www.shop.example.com from the certificate authority "+testAuthority+": the node has no certificate named shop.example.com." {
		t.Fatalf("%+v", applied)
	}
	want := "certbot certonly --non-interactive --agree-tos --email admin@example.com --no-eff-email --server " + testAuthority +
		" --cert-name shop.example.com --webroot --webroot-path /var/www/certbot-challenges -d shop.example.com -d www.shop.example.com --deploy-hook " + f.script + " --disable-hook-validation"
	if calls := takeCalls(t, f.dir); len(calls) != 1 || calls[0] != want {
		t.Fatalf("%q\nwant %q", calls, want)
	}
	// certbot ran the script after the first certificate.
	if ran := readState(t, f.ran); ran != f.script+" "+live+" [shop.example.com www.shop.example.com]\n" {
		t.Fatalf("the script ran: %q", ran)
	}
	first := f.fingerprint()
	if len(first) != 64 {
		t.Fatalf("the certificate's fingerprint reads %q", first)
	}

	for _, again := range []protocol.Result{f.apply(task), f.dryrun(task)} {
		if again.Status != "succeeded" || again.Change != "unchanged" || again.Reason != "" || certbotDirectory(again) != live || certbotDomains(again) != "shop.example.com www.shop.example.com" {
			t.Fatalf("%+v", again)
		}
	}
	// The same names in another order and another case are the same
	// certificate, and so is another account_email_when_registered: certbot keeps no copy of
	// the address.
	task.Domains, task.AccountEmailWhenRegistered = []string{"WWW.shop.example.com", "shop.example.com"}, "other@example.com"
	if again := f.apply(task); again.Change != "unchanged" {
		t.Fatalf("%+v", again)
	}
	if calls := takeCalls(t, f.dir); len(calls) != 0 || f.fingerprint() != first {
		t.Fatal("a task whose certificate is right started certbot", calls)
	}
}

// Another list of names gets a new certificate under the same name, and a
// certificate from another certificate authority is replaced by one from the
// task's, which certbot does only when it is forced to renew.
func TestCertbotCertificateWithOtherNamesOrAuthorityIsRequestedAgain(t *testing.T) {
	f := newCertbotFixture(t)
	f.issuable("shop.example.com", "www.shop.example.com")
	task := f.webroot()
	f.apply(task)
	first := f.fingerprint()
	takeCalls(t, f.dir)

	task.Domains = []string{"shop.example.com", "api.shop.example.com"}
	f.issuable("shop.example.com", "api.shop.example.com")
	why := "the certificate shop.example.com covers shop.example.com, www.shop.example.com, and the task says shop.example.com, api.shop.example.com"
	if preview := f.dryrun(task); preview.Change != "would_change" || !strings.HasSuffix(preview.Reason, ": "+why) || certbotDomains(preview) != "shop.example.com api.shop.example.com" {
		t.Fatalf("%+v", preview)
	}
	if len(takeCalls(t, f.dir)) != 0 || f.fingerprint() != first {
		t.Fatal("the dryrun started certbot")
	}
	changed := f.apply(task)
	if changed.Change != "changed" || certbotDomains(changed) != "shop.example.com api.shop.example.com" || !strings.HasSuffix(changed.Reason, ": "+why+".") {
		t.Fatalf("%+v", changed)
	}
	calls := takeCalls(t, f.dir)
	if len(calls) != 1 || !strings.Contains(calls[0], "-d shop.example.com -d api.shop.example.com") || strings.Contains(calls[0], "--force-renewal") {
		t.Fatal(calls)
	}
	second := f.fingerprint()
	if second == first || f.apply(task).Change != "unchanged" {
		t.Fatal("the certificate was not replaced once")
	}
	takeCalls(t, f.dir)

	// The other authority. The stand-in, like certbot, would keep the
	// certificate without --force-renewal.
	const other = "https://acme.other.example/directory"
	task.CertificateAuthorityURL = other
	f.issuable("shop.example.com", "api.shop.example.com")
	why = "the certificate shop.example.com comes from the certificate authority " + testAuthority + ", and the task says " + other
	if preview := f.dryrun(task); preview.Change != "would_change" || preview.Reason != "would ask the certificate authority "+other+" for a certificate for shop.example.com, api.shop.example.com: "+why {
		t.Fatalf("%+v", preview)
	}
	if moved := f.apply(task); moved.Change != "changed" || !strings.HasSuffix(moved.Reason, ": "+why+".") {
		t.Fatalf("%+v", moved)
	}
	calls = takeCalls(t, f.dir)
	if len(calls) != 1 || !strings.HasSuffix(calls[0], " --force-renewal") || !strings.Contains(calls[0], "--server "+other+" ") {
		t.Fatal(calls)
	}
	if f.fingerprint() == second || !strings.Contains(f.renewalFile(), "server = "+other+"\n") {
		t.Fatal("the certificate does not come from the other authority")
	}
	if again := f.apply(task); again.Change != "unchanged" || len(takeCalls(t, f.dir)) != 0 {
		t.Fatalf("%+v", again)
	}

	// The task explicitly chooses Let's Encrypt on certbot's command line.
	task.CertificateAuthorityURL = "https://acme-v02.api.letsencrypt.org/directory"
	if preview := f.dryrun(task); !strings.Contains(preview.Reason, "would ask the certificate authority https://acme-v02.api.letsencrypt.org/directory for a certificate") {
		t.Fatalf("%+v", preview)
	}
	f.issuable("shop.example.com", "api.shop.example.com")
	f.apply(task)
	if calls := takeCalls(t, f.dir); len(calls) != 1 || !strings.Contains(calls[0], "--server https://acme-v02.api.letsencrypt.org/directory ") {
		t.Fatal(calls)
	}
}

// Changed settings are written into certbot's renewal file, key by key, and
// no certbot starts: a script, the challenge and its fields. Every other
// line of the file stays, and server and account are never written.
func TestCertbotCertificateSettingsAreWrittenIntoTheRenewalFile(t *testing.T) {
	f := newCertbotFixture(t)
	f.issuable("shop.example.com", "www.shop.example.com")
	task := f.webroot()
	f.apply(task)
	takeCalls(t, f.dir)
	certificate := f.fingerprint()
	original := f.renewalFile()
	credentials := filepath.Join(f.dir, "rfc2136.ini")
	writeState(t, credentials, "dns_rfc2136_server = 127.0.0.1\n")
	spaced := f.executable("site scripts/reload, one")

	// What the renewal file holds below [renewalparams] after each change,
	// apart from account, server, and key_type, which never change.
	settings := func() string {
		text := f.renewalFile()
		if !strings.HasPrefix(text, strings.SplitAfter(original, "[renewalparams]\n")[0]) {
			t.Fatalf("the lines above the settings changed:\n%s", text)
		}
		var kept []string
		for _, line := range strings.Split(strings.TrimSpace(strings.SplitAfter(text, "[renewalparams]\n")[1]), "\n") {
			switch line {
			case "account = 00d1f2dd2e9cfc561d10d8808c07a80a", "server = " + testAuthority, "key_type = ecdsa":
			default:
				kept = append(kept, line)
			}
		}
		return strings.Join(kept, "\n")
	}
	for _, step := range []struct {
		name   string
		change func(*CertbotCertificate)
		// differences are what the result's reason lists.
		differences string
		want        string
	}{
		{"another script, whose path needs quotes for certbot's shell",
			func(c *CertbotCertificate) { c.RunAfterIssueAndRenewal = spaced },
			"run_after_issue_and_renewal: certbot has " + f.script + ", and the task says '" + spaced + "'",
			"renew_hook = \"'" + spaced + "'\"\nauthenticator = webroot\nwebroot_path = /var/www/certbot-challenges,\n[[webroot_map]]\nshop.example.com = /var/www/certbot-challenges\nwww.shop.example.com = /var/www/certbot-challenges"},
		{"another webroot directory, also in every entry of the map",
			func(c *CertbotCertificate) { c.WebrootPath = "/var/www/other-challenges/" },
			"webroot_path: certbot has /var/www/certbot-challenges, and the task says /var/www/other-challenges",
			"renew_hook = \"'" + spaced + "'\"\nauthenticator = webroot\nwebroot_path = /var/www/other-challenges,\n[[webroot_map]]\nshop.example.com = /var/www/other-challenges\nwww.shop.example.com = /var/www/other-challenges"},
		{"no script any more",
			func(c *CertbotCertificate) {
				c.RunAfterIssueAndRenewal = ""
				c.RenewalSettingsToRemove = []string{"run_after_issue_and_renewal"}
			},
			"run_after_issue_and_renewal: certbot has '" + spaced + "', and renewal_settings_to_remove deletes it",
			"authenticator = webroot\nwebroot_path = /var/www/other-challenges,\n[[webroot_map]]\nshop.example.com = /var/www/other-challenges\nwww.shop.example.com = /var/www/other-challenges"},
		{"standalone, which takes the webroot settings out",
			func(c *CertbotCertificate) {
				c.Challenge, c.WebrootPath = "standalone", ""
				c.RenewalSettingsToRemove = nil
			},
			"challenge: certbot has webroot, and the task says standalone",
			"authenticator = standalone"},
		{"dns with a credentials file and a wait, and the script again",
			func(c *CertbotCertificate) {
				c.Challenge, c.DNSPlugin, c.DNSCredentialsFile, c.WaitAfterCreatingDNSRecord, c.RunAfterIssueAndRenewal = "dns", "rfc2136", credentials, "1m30s", f.script
			},
			"challenge: certbot has standalone, and the task says dns with dns_plugin rfc2136; dns_credentials_file: certbot has none, and the task says " + credentials +
				"; wait_after_creating_dns_record: certbot has none, and the task says 90; run_after_issue_and_renewal: certbot has none, and the task says " + f.script,
			"authenticator = dns-rfc2136\ndns_rfc2136_credentials = " + credentials + "\ndns_rfc2136_propagation_seconds = 90\nrenew_hook = " + f.script},
		{"the plugin's own wait again, and no credentials file",
			func(c *CertbotCertificate) {
				c.DNSCredentialsFile, c.WaitAfterCreatingDNSRecord = "", ""
				c.RenewalSettingsToRemove = []string{"dns_credentials_file", "wait_after_creating_dns_record"}
			},
			"dns_credentials_file: certbot has " + credentials + ", and renewal_settings_to_remove deletes it; wait_after_creating_dns_record: certbot has 90, and renewal_settings_to_remove deletes it",
			"authenticator = dns-rfc2136\nrenew_hook = " + f.script},
		{"webroot again, which takes the plugin's settings out; certbot writes its map of names at the next renewal",
			func(c *CertbotCertificate) {
				c.Challenge, c.DNSPlugin, c.WebrootPath = "webroot", "", "/var/www/certbot-challenges"
				c.RenewalSettingsToRemove = nil
			},
			"challenge: certbot has dns with dns_plugin rfc2136, and the task says webroot; webroot_path: certbot has none, and the task says /var/www/certbot-challenges",
			"authenticator = webroot\nrenew_hook = " + f.script + "\nwebroot_path = /var/www/certbot-challenges,"},
	} {
		step.change(&task)
		before := f.renewalFile()
		preview := f.dryrun(task)
		if preview.Status != "succeeded" || preview.Change != "would_change" || f.renewalFile() != before ||
			preview.Reason != "would write the task's settings into certbot's renewal settings of the certificate shop.example.com, for its next renewal: "+step.differences+". No new certificate is requested, and nobody tests these settings until certbot renews" {
			t.Fatalf("%s, dryrun: %+v", step.name, preview)
		}
		// The dryrun shows the difference of the file.
		if preview.FileChange == nil || preview.FileChange.Path != certbotRenewalFilePath(testCertificate) || preview.FileChange.Diff == "" {
			t.Fatalf("%s, dryrun: no difference of the renewal file: %+v", step.name, preview.FileChange)
		}
		applied := f.apply(task)
		if applied.Status != "succeeded" || applied.Change != "changed" || !strings.HasPrefix(applied.Reason, "wrote the task's settings into certbot's renewal settings of the certificate shop.example.com, for its next renewal: "+step.differences+".") ||
			certbotDomains(applied) != "shop.example.com www.shop.example.com" {
			t.Fatalf("%s: %+v", step.name, applied)
		}
		if got := settings(); got != step.want {
			t.Fatalf("%s: the renewal file holds\n%s\nwant\n%s", step.name, got, step.want)
		}
		for _, again := range []protocol.Result{f.apply(task), f.dryrun(task)} {
			if again.Change != "unchanged" {
				t.Fatalf("%s, again: %+v", step.name, again)
			}
		}
		if calls := takeCalls(t, f.dir); len(calls) != 0 || f.fingerprint() != certificate {
			t.Fatalf("%s: certbot was started for changed settings: %q", step.name, calls)
		}
	}
	// The file keeps its mode.
	if info, err := os.Stat(certbotRenewalFilePath(testCertificate)); err != nil || info.Mode().Perm() != 0644 {
		t.Fatal(info, err)
	}

	// A section that the task does not name, and a comment, stay through a
	// change; certbot 5 keeps such a section below the map. The map's
	// entries keep their order, which differs between certbot's runs.
	writeState(t, certbotRenewalFilePath(testCertificate), f.renewalFile()+"[[webroot_map]]\nwww.shop.example.com = /var/www/certbot-challenges\nshop.example.com = /var/www/certbot-challenges\n# a note of the administrator\n[acme_renewal_info]\nari_retry_after = 2026-10-09T22:08:58\n")
	task.WebrootPath = "/var/www/third"
	if applied := f.apply(task); applied.Change != "changed" {
		t.Fatalf("%+v", applied)
	}
	if text := f.renewalFile(); !strings.HasSuffix(text, "[[webroot_map]]\nwww.shop.example.com = /var/www/third\nshop.example.com = /var/www/third\n# a note of the administrator\n[acme_renewal_info]\nari_retry_after = 2026-10-09T22:08:58\n") {
		t.Fatalf("the section below the map did not stay:\n%s", text)
	}
	// The map wins over webroot_path at a renewal, so an entry of the map
	// alone is a difference too.
	writeState(t, certbotRenewalFilePath(testCertificate), strings.Replace(f.renewalFile(), "www.shop.example.com = /var/www/third", "www.shop.example.com = /var/www/elsewhere", 1))
	if repaired := f.apply(task); repaired.Change != "changed" || !strings.Contains(repaired.Reason, "webroot_path: certbot has /var/www/third and, for www.shop.example.com, /var/www/elsewhere, and the task says /var/www/third") {
		t.Fatalf("%+v", repaired)
	}
	if strings.Contains(f.renewalFile(), "elsewhere") {
		t.Fatal("the entry of the map was not written")
	}
	// certbot maps only the names whose challenge it answered. When the
	// authority reuses a valid authorization of a name, the map has no entry
	// for that name, and a renewal uses webroot_path for it.
	writeState(t, certbotRenewalFilePath(testCertificate), strings.Replace(f.renewalFile(), "www.shop.example.com = /var/www/third\n", "", 1))
	if again := f.apply(task); again.Change != "unchanged" {
		t.Fatalf("a name without an entry of the map: %+v", again)
	}
	task.WebrootPath = "/var/www/fourth"
	if applied := f.apply(task); applied.Change != "changed" {
		t.Fatalf("%+v", applied)
	}
	if text := f.renewalFile(); !strings.Contains(text, "webroot_path = /var/www/fourth,\n") || !strings.Contains(text, "[[webroot_map]]\nshop.example.com = /var/www/fourth\n") || strings.Contains(text, "www.shop.example.com =") {
		t.Fatalf("the map got an entry that certbot did not write, or lost one:\n%s", text)
	}
	if again := f.apply(task); again.Change != "unchanged" {
		t.Fatalf("%+v", again)
	}
}

// A new certificate also carries the task's settings, whatever certbot
// saved of its own command line: a settings change together with new names
// needs one run.
func TestCertbotCertificateWithNewNamesAlsoGetsTheChangedSettings(t *testing.T) {
	f := newCertbotFixture(t)
	f.issuable("shop.example.com", "www.shop.example.com")
	task := f.webroot()
	f.apply(task)
	task.Domains, task.Challenge, task.WebrootPath, task.RunAfterIssueAndRenewal = []string{"shop.example.com"}, "standalone", "", ""
	task.RenewalSettingsToRemove = []string{"run_after_issue_and_renewal"}
	f.issuable("shop.example.com")
	if applied := f.apply(task); applied.Change != "changed" || certbotDomains(applied) != "shop.example.com" {
		t.Fatalf("%+v", applied)
	}
	if text := f.renewalFile(); !strings.Contains(text, "authenticator = standalone\n") || strings.Contains(text, "webroot") || strings.Contains(text, "renew_hook") {
		t.Fatalf("%s", text)
	}
	for _, again := range []protocol.Result{f.apply(task), f.dryrun(task)} {
		if again.Change != "unchanged" {
			t.Fatalf("%+v", again)
		}
	}
}

// Files of certbot that would make it treat the certificate as new fail the
// task before certbot starts: certbot would get a certificate and put it
// into another directory, or fail to save it.
func TestCertbotCertificateFailsBeforeAnyRequestWhenCertbotsFilesAreDamaged(t *testing.T) {
	f := newCertbotFixture(t)
	f.issuable("shop.example.com", "www.shop.example.com")
	task := f.webroot()
	f.apply(task)
	takeCalls(t, f.dir)
	good := f.renewalFile()
	path := certbotRenewalFilePath(testCertificate)
	live := certbotLiveDirectory(testCertificate)
	// Another list of names, so that a run would start certbot.
	task.Domains = []string{"shop.example.com"}
	f.issuable("shop.example.com")
	refused := func(what, want string) {
		t.Helper()
		for _, r := range []protocol.Result{f.dryrun(task), f.apply(task)} {
			if r.Status != "failed" || !strings.Contains(r.Reason, want) {
				t.Fatalf("%s: %+v\nwant %s", what, r, want)
			}
		}
		if calls := takeCalls(t, f.dir); len(calls) != 0 {
			t.Fatalf("%s: certbot was started: %q", what, calls)
		}
	}
	writeState(t, path, strings.Replace(good, "[renewalparams]", "this line is no setting\n[renewalparams]", 1))
	refused("a line that is no setting", "certbot_certificate: MMX cannot read the renewal file "+path+" for certificate shop.example.com: line 10 (this line is no setting): the line is neither a setting (key = value), a section, nor a comment. A new certbot request would save a new certificate in "+live+"-0001 and report success. Repair the renewal file, or remove the renewal file together with "+live+" and "+certbotArchiveDirectory(testCertificate))
	writeState(t, path, strings.Split(good, "[renewalparams]")[0])
	refused("no settings section", "certbot's renewal file has no section [renewalparams]")
	writeState(t, path, strings.Replace(good, "chain = ", "#chain = ", 1))
	refused("a missing file setting", "certbot's renewal file has no setting chain")
	writeState(t, path, strings.Replace(good, "cert = "+live, "cert = /srv/certificates", 1))
	refused("a certificate somewhere else", "the renewal setting cert points to /srv/certificates/cert.pem, but MMX requires the certbot certificate path "+live+"/cert.pem")
	writeState(t, path, good)
	target, err := os.Readlink(filepath.Join(live, "privkey.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(live, target)); err != nil {
		t.Fatal(err)
	}
	refused("a link without its file", "the link "+live+"/privkey.pem leads to no file")
	writeState(t, filepath.Join(live, target), "privkey 1\n")
	if err := os.Remove(filepath.Join(live, "chain.pem")); err != nil {
		t.Fatal(err)
	}
	writeState(t, filepath.Join(live, "chain.pem"), "a copy, not a link\n")
	refused("a file where certbot makes a link", live+"/chain.pem is not the link that certbot makes")
	if err := os.Remove(filepath.Join(live, "chain.pem")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../archive/shop.example.com/chain1.pem", filepath.Join(live, "chain.pem")); err != nil {
		t.Fatal(err)
	}
	certificate, _ := os.Readlink(filepath.Join(live, "cert.pem"))
	writeState(t, filepath.Join(live, certificate), "no certificate\n")
	refused("a file that is no certificate", live+"/cert.pem holds no certificate in PEM form")

	// Files of the name without a renewal file: certbot would ask the
	// authority and then refuse to save.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	refused("files without a renewal file", "certbot_certificate: "+live+" holds files, but the renewal file "+path+" is missing after an unfinished removal or certificate request. certbot would get a certificate from the certificate authority and then fail to save the certificate. Remove the remaining files with a certbot_certificate task with state: absent, or restore the missing renewal file")
	// state: absent removes them, and the name is free again.
	if r := f.apply(CertbotCertificate{CertificateName: testCertificate, State: "absent"}); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if r := f.apply(task); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
}

// certbot's exit code does not prove where the certificate is: MMX reads
// the certificate's directory again and fails the task when it does not
// hold the task's names.
func TestCertbotCertificateFailsWhenCertbotSavedTheCertificateSomewhereElse(t *testing.T) {
	f := newCertbotFixture(t)
	f.issuable("shop.example.com", "www.shop.example.com")
	task := f.webroot()
	writeState(t, filepath.Join(f.dir, "saves-elsewhere"), "")
	r := f.apply(task)
	live := filepath.Join(f.dir, "etc/live")
	if r.Status != "failed" || r.Reason != "certbot_certificate: certbot reported success, but the certificate directory "+live+"/shop.example.com does not contain a certificate for the requested names shop.example.com, www.shop.example.com (names covered by the existing certificate: ). certbot may have saved the new certificate in another directory under "+live {
		t.Fatalf("%+v", r)
	}
	// The same with an existing certificate of other names, which stays.
	if err := os.Remove(filepath.Join(f.dir, "saves-elsewhere")); err != nil {
		t.Fatal(err)
	}
	f.apply(task)
	first := f.fingerprint()
	writeState(t, filepath.Join(f.dir, "saves-elsewhere"), "")
	task.Domains = []string{"shop.example.com"}
	f.issuable("shop.example.com")
	r = f.apply(task)
	if r.Status != "failed" || r.Change != "unknown" || !strings.Contains(r.Reason, "(names covered by the existing certificate: shop.example.com, www.shop.example.com)") || f.fingerprint() != first {
		t.Fatalf("%+v", r)
	}
}

// Without certbot a run fails as a missing prerequisite with the design's
// message and a dryrun is not checked. A missing or not executable script, a
// DNS plugin that certbot cannot load, and a missing credentials file are
// the same: an earlier node task may install each. certbot never starts.
func TestCertbotCertificateNamesWhatAnEarlierNodeTaskMustInstall(t *testing.T) {
	f := newCertbotFixture(t)
	missing := func(task CertbotCertificate, want string) {
		t.Helper()
		if r := f.apply(task); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != want {
			t.Fatalf("%+v\nwant %s", r, want)
		}
		if r := f.dryrun(task); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != want {
			t.Fatalf("%+v\nwant %s", r, want)
		}
		if calls := takeCalls(t, f.dir); len(calls) != 0 {
			t.Fatal(calls)
		}
	}
	task := f.webroot()
	task.RunAfterIssueAndRenewal = filepath.Join(f.dir, "no-such-script")
	missing(task, "certbot_certificate: the script "+task.RunAfterIssueAndRenewal+" of run_after_issue_and_renewal does not exist; install it in an earlier node task, for example with file and permissions_mode 0755")
	writeState(t, task.RunAfterIssueAndRenewal, "#!/bin/sh\n")
	missing(task, "certbot_certificate: the script "+task.RunAfterIssueAndRenewal+" of run_after_issue_and_renewal is not an executable file; give it permissions_mode 0755 in an earlier node task")

	dns := CertbotCertificate{CertificateName: "wildcard.example.com", Domains: []string{"example.com", "*.example.com"}, AccountEmailWhenRegistered: "admin@example.com",
		Challenge: "dns", DNSPlugin: "cloudflare", DNSCredentialsFile: filepath.Join(f.dir, "cloudflare.ini"), CertificateAuthorityURL: testAuthority}
	missing(dns, "certbot_certificate: certbot's DNS plugin cloudflare is not installed on this node; install its package in an earlier node task: python3-certbot-dns-cloudflare")
	f.plugins = append(f.plugins, "dns-cloudflare")
	missing(dns, "certbot_certificate: the credentials file "+dns.DNSCredentialsFile+" of dns_credentials_file does not exist; write it in an earlier node task, for example with file and permissions_mode 0600")
	// With the file, and for a plugin that takes none, certbot is asked.
	writeState(t, dns.DNSCredentialsFile, "dns_cloudflare_api_token = x\n")
	f.issuable("example.com", "*.example.com")
	if r := f.apply(dns); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if calls := takeCalls(t, f.dir); len(calls) != 1 || !strings.Contains(calls[0], "--authenticator dns-cloudflare --dns-cloudflare-credentials "+dns.DNSCredentialsFile+" -d example.com -d *.example.com") {
		t.Fatal(calls)
	}
	f.plugins = append(f.plugins, "dns-route53")
	route53 := CertbotCertificate{CertificateName: "route53.example.com", Domains: []string{"route53.example.com"}, AccountEmailWhenRegistered: "admin@example.com",
		Challenge: "dns", DNSPlugin: "route53", WaitAfterCreatingDNSRecord: "45s", CertificateAuthorityURL: testAuthority}
	f.issuable("route53.example.com")
	if r := f.apply(route53); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if calls := takeCalls(t, f.dir); len(calls) != 1 || !strings.Contains(calls[0], "--authenticator dns-route53 --dns-route53-propagation-seconds 45 -d route53.example.com") || strings.Contains(calls[0], "credentials") {
		t.Fatal(calls)
	}
	// When MMX cannot ask which plugins certbot has, certbot itself says.
	certbotPluginsOf = func(context.Context, string) ([]string, bool) { return nil, false }
	unknown := CertbotCertificate{CertificateName: "unknown.example.com", Domains: []string{"unknown.example.com"}, AccountEmailWhenRegistered: "admin@example.com",
		Challenge: "dns", DNSPlugin: "digitalocean", CertificateAuthorityURL: testAuthority}
	f.issuable("unknown.example.com")
	if r := f.apply(unknown); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	takeCalls(t, f.dir)

	// No certbot on the node.
	setWorkerEnvironment(t, "PATH", t.TempDir())
	install := missingProgramError("certbot").Error()
	plain := f.webroot()
	plain.RunAfterIssueAndRenewal = ""
	if r := (Runtime{}).CertbotCertificate(testContext(t), plain); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != install {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{Dryrun: true}).CertbotCertificate(testContext(t), plain); r.Status != "not_checked" || r.Reason != install {
		t.Fatalf("%+v", r)
	}
	// A certificate that the node has cannot be deleted without certbot; a
	// certificate that the node does not have needs no certbot.
	absent := CertbotCertificate{CertificateName: "wildcard.example.com", State: "absent"}
	if r := (Runtime{}).CertbotCertificate(testContext(t), absent); r.FailureKind != "missing_prerequisite" || r.Reason != install {
		t.Fatalf("%+v", r)
	}
	absent.CertificateName = "never.example.com"
	if r := (Runtime{}).CertbotCertificate(testContext(t), absent); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
}

// A dryrun goes on when an earlier node task of the same dryrun would
// install the script or the credentials file that is missing: the usual
// playbook writes the script and then asks for the certificate, and its
// first dryrun says that a certificate would be requested. A file that no
// earlier node task of the dryrun would install leaves the task not checked,
// and a run never counts a file that is not there.
func TestCertbotCertificateDryrunCountsWhatEarlierNodeTasksWouldInstall(t *testing.T) {
	f := newCertbotFixture(t)
	f.plugins = append(f.plugins, "dns-cloudflare")
	task := f.webroot()
	task.RunAfterIssueAndRenewal = filepath.Join(f.dir, "script-of-this-dryrun")
	notInstalled := "certbot_certificate: the script " + task.RunAfterIssueAndRenewal + " of run_after_issue_and_renewal does not exist; install it in an earlier node task, for example with file and permissions_mode 0755"
	with := func(dryrun bool, c CertbotCertificate, previewPaths ...string) protocol.Result {
		t.Helper()
		return c.Run(testContext(t), NodeTask{Runtime: Runtime{Dryrun: dryrun, PreviewPaths: previewPaths}})
	}
	wouldAsk := "would ask the certificate authority " + testAuthority + " for a certificate for shop.example.com, www.shop.example.com: the node has no certificate named shop.example.com"
	if r := with(true, task, "/etc/nginx/conf.d/shop-http.conf", task.RunAfterIssueAndRenewal); r.Status != "succeeded" || r.Change != "would_change" || r.Reason != wouldAsk {
		t.Fatalf("%+v", r)
	}
	// Another file's path, and a path below the script's, are not the script.
	for _, others := range [][]string{nil, {"/etc/nginx/conf.d/shop-http.conf"}, {task.RunAfterIssueAndRenewal + ".d/other"}} {
		if r := with(true, task, others...); r.Status != "not_checked" || r.Reason != notInstalled {
			t.Fatalf("%q: %+v", others, r)
		}
	}
	if r := with(false, task, task.RunAfterIssueAndRenewal); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != notInstalled {
		t.Fatalf("%+v", r)
	}
	// A script that is there and not executable, which an earlier node task
	// of the dryrun would change, counts too.
	writeState(t, task.RunAfterIssueAndRenewal, "#!/bin/sh\n")
	if r := with(true, task); r.Status != "not_checked" || !strings.Contains(r.Reason, "is not an executable file") {
		t.Fatalf("%+v", r)
	}
	if r := with(true, task, task.RunAfterIssueAndRenewal); r.Change != "would_change" || r.Reason != wouldAsk {
		t.Fatalf("%+v", r)
	}

	// The credentials file of a DNS plugin, in the same way.
	dns := CertbotCertificate{CertificateName: "wildcard.example.com", Domains: []string{"example.com", "*.example.com"}, AccountEmailWhenRegistered: "admin@example.com",
		Challenge: "dns", DNSPlugin: "cloudflare", DNSCredentialsFile: filepath.Join(f.dir, "cloudflare.ini"), CertificateAuthorityURL: testAuthority}
	if r := with(true, dns); r.Status != "not_checked" || !strings.Contains(r.Reason, "the credentials file "+dns.DNSCredentialsFile+" of dns_credentials_file does not exist") {
		t.Fatalf("%+v", r)
	}
	if r := with(true, dns, dns.DNSCredentialsFile); r.Change != "would_change" || !strings.HasPrefix(r.Reason, "would ask the certificate authority "+testAuthority+" for a certificate for example.com, *.example.com") {
		t.Fatalf("%+v", r)
	}
	if r := with(false, dns, dns.DNSCredentialsFile); r.FailureKind != "missing_prerequisite" {
		t.Fatalf("%+v", r)
	}
	// The plugin itself is no file of an earlier node task: without it the
	// dryrun stays not checked.
	f.plugins = f.plugins[:len(f.plugins)-1]
	if r := with(true, dns, dns.DNSCredentialsFile); r.Status != "not_checked" || !strings.Contains(r.Reason, "certbot's DNS plugin cloudflare is not installed") {
		t.Fatalf("%+v", r)
	}

	// For a certificate that the node has, the dryrun then shows the
	// settings that the run would write.
	f.issuable("shop.example.com", "www.shop.example.com")
	have := f.webroot()
	if r := f.apply(have); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if err := os.Remove(task.RunAfterIssueAndRenewal); err != nil {
		t.Fatal(err)
	}
	if r := with(true, task, task.RunAfterIssueAndRenewal); r.Change != "would_change" || !strings.Contains(r.Reason, "run_after_issue_and_renewal: certbot has "+f.script+", and the task says "+task.RunAfterIssueAndRenewal) {
		t.Fatalf("%+v", r)
	}
	if calls := takeCalls(t, f.dir); len(calls) != 1 {
		t.Fatal("a dryrun started certbot", calls)
	}
}

// A refused request fails the task with the certificate authority's own
// answer from certbot's log, which says more than certbot prints, and with
// what certbot printed when the log holds no such answer. Nothing is left
// half done, and the next try gets the certificate.
func TestCertbotCertificateFailureHoldsTheAuthoritysAnswerAndTheNextTrySucceeds(t *testing.T) {
	f := newCertbotFixture(t)
	f.issuable("shop.example.com", "www.shop.example.com")
	task := f.webroot()
	// An earlier answer in the log, from before the task, does not count.
	writeState(t, certbotLogFile(), "2026-10-09 17:00:00,000:DEBUG:acme.client:Received response:\nHTTP 400\nContent-Type: application/problem+json\n\n{\"type\": \"urn:ietf:params:acme:error:malformed\", \"detail\": \"an old answer\"}\n")
	writeState(t, filepath.Join(f.dir, "certbot-fails"), "Some challenges have failed.\nAsk for help or search for solutions at https://community.letsencrypt.org.\n")
	r := f.apply(task)
	if r.Status != "failed" || r.Change != "unknown" || r.FailureKind != "" ||
		r.Reason != "certbot_certificate: certbot did not get the certificate shop.example.com from "+testAuthority+":\nSome challenges have failed.\nAsk for help or search for solutions at https://community.letsencrypt.org.\nRequesting a certificate for shop.example.com www.shop.example.com" {
		t.Fatalf("%+v", r)
	}
	if f.renewalFile() != "" || f.fingerprint() != "" {
		t.Fatal("a failed request left a certificate")
	}
	if err := os.Remove(filepath.Join(f.dir, "certbot-fails")); err != nil {
		t.Fatal(err)
	}

	// A rate limit, as the certbot of Debian 12 shows it: an error of its
	// own program on the terminal, the authority's answer only in the log.
	writeState(t, filepath.Join(f.dir, "authority-refuses"), "HTTP 429\nContent-Type: application/problem+json; charset=utf-8\nRetry-After: 600000\nReplay-Nonce: as83J_yjxw8_qCaLzk02bg\n\n{\n   \"type\": \"urn:ietf:params:acme:error:rateLimited\",\n   \"detail\": \"too many certificates (5) already issued for this exact set of identifiers in the last 168h0m0s\",\n   \"status\": 429\n}\n")
	r = f.apply(task)
	if r.Status != "failed" || r.Change != "unknown" ||
		r.Reason != "certbot_certificate: the certificate authority "+testAuthority+" refused the request for the certificate shop.example.com: too many certificates (5) already issued for this exact set of identifiers in the last 168h0m0s (error type urn:ietf:params:acme:error:rateLimited, HTTP status 429; the certificate authority says to ask again after: 600000)" {
		t.Fatalf("%+v", r)
	}
	// The task tries once: one certbot for each of the two runs.
	if calls := takeCalls(t, f.dir); len(calls) != 2 {
		t.Fatal(calls)
	}
	if err := os.Remove(filepath.Join(f.dir, "authority-refuses")); err != nil {
		t.Fatal(err)
	}
	if r := f.apply(task); r.Status != "succeeded" || r.Change != "changed" || f.fingerprint() == "" {
		t.Fatalf("%+v", r)
	}
	if r := f.apply(task); r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
}

// The answer of the certificate authority is read from certbot's log only
// in the one form that certbot writes; for every other form nothing is
// found, and the caller reports certbot's own text.
func TestAuthoritysAnswerIsReadOnlyInItsExpectedForm(t *testing.T) {
	t.Parallel()
	const entry = "2026-10-09 17:09:21,678:DEBUG:acme.client:Received response:\n"
	problem := "HTTP 400\nCache-Control: public, max-age=0, no-cache\nContent-Type: application/problem+json; charset=utf-8\nLink: <https://localhost:14000/dir>;rel=\"index\"\n\n{\n   \"type\": \"urn:ietf:params:acme:error:rejectedIdentifier\",\n   \"detail\": \"Order included an identifier for which issuance is forbidden by policy: \\\"blocked-domain.example\\\"\",\n   \"status\": 400\n}\n"
	after := "2026-10-09 17:09:21,678:DEBUG:certbot._internal.log:Exiting abnormally:\nTraceback (most recent call last):\n  File \"/usr/bin/certbot\", line 33, in <module>\n"
	got, found := lastAuthorityErrorIn("2026-10-09 17:09:21,633:DEBUG:certbot._internal.main:certbot version: 4.0.0\n" + entry + "HTTP 200\nContent-Type: application/json\n\n{}\n" + entry + problem + after)
	want := acmeProblem{Type: "urn:ietf:params:acme:error:rejectedIdentifier", Detail: `Order included an identifier for which issuance is forbidden by policy: "blocked-domain.example"`, HTTPStatus: "400"}
	if !found || got != want {
		t.Fatalf("%+v %v", got, found)
	}
	if got.sentence() != `Order included an identifier for which issuance is forbidden by policy: "blocked-domain.example" (error type urn:ietf:params:acme:error:rejectedIdentifier, HTTP status 400)` {
		t.Fatal(got.sentence())
	}
	for name, log := range map[string]string{
		"an empty log":     "",
		"no answer at all": "2026-10-09 17:09:21,633:DEBUG:certbot._internal.main:certbot version: 4.0.0\n",
		"an error answer that another one followed": entry + problem + entry + "HTTP 200\nContent-Type: application/json\n\n{\"status\": \"invalid\"}\n" + after,
		"an answer of another kind":                 entry + "HTTP 500\nContent-Type: text/html\n\n<html>Internal Server Error</html>\n" + after,
		"an error document without a type":          entry + "HTTP 400\nContent-Type: application/problem+json\n\n{\"detail\": \"something\"}\n",
		"an error document that is no JSON":         entry + "HTTP 400\nContent-Type: application/problem+json\n\nnot JSON\n",
		"an answer without a status line":           entry + "Content-Type: application/problem+json\n\n{\"type\": \"urn:x\"}\n",
		"an answer without a body":                  entry + "HTTP 400\nContent-Type: application/problem+json\n",
		"lines that are no headers":                 entry + "HTTP 400\nsomething else\n\n{\"type\": \"urn:x\"}\n",
		"another line form of the log":              "Oct 09 17:09:21 acme.client Received response:\n" + problem,
	} {
		if got, found := lastAuthorityErrorIn(log); found {
			t.Errorf("%s: found %+v", name, got)
		}
	}
	// A document without a text still names its type.
	if got, found := lastAuthorityErrorIn(entry + "HTTP 403\nContent-Type: application/problem+json\n\n{\"type\": \"urn:ietf:params:acme:error:unauthorized\"}\n"); !found ||
		got.sentence() != "the certificate authority gave no text (error type urn:ietf:params:acme:error:unauthorized, HTTP status 403)" {
		t.Fatalf("%+v %v", got, found)
	}
}

// What certbot wrote into its log since a mark: the rest of the same file,
// the whole of a new file, and never more than the bound.
func TestCertbotLogIsReadFromTheMarkAndBounded(t *testing.T) {
	newCertbotFixture(t)
	if got := markCertbotLog().since(); got != "" {
		t.Fatalf("without a log file: %q", got)
	}
	writeState(t, certbotLogFile(), "before the task\n")
	mark := markCertbotLog()
	log, err := os.OpenFile(certbotLogFile(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(log, "after the mark\n")
	log.Close()
	if got := mark.since(); got != "after the mark\n" {
		t.Fatalf("%q", got)
	}
	// A certbot that rotates its log starts a new log file for each run.
	if err := os.Rename(certbotLogFile(), certbotLogFile()+".1"); err != nil {
		t.Fatal(err)
	}
	writeState(t, certbotLogFile(), "a new file\n")
	if got := mark.since(); got != "a new file\n" {
		t.Fatalf("%q", got)
	}
	writeState(t, certbotLogFile(), strings.Repeat("x", certbotLogLimit)+"the end\n")
	if got := markCertbotLog().since(); got != "" {
		t.Fatalf("nothing was written after the mark: %d bytes", len(got))
	}
	if got := (certbotLogMark{}).since(); len(got) != certbotLogLimit || !strings.HasSuffix(got, "the end\n") {
		t.Fatalf("%d bytes", len(got))
	}
}

// TestHelperHoldsCertbotsLock is no test: the lock tests start it as another
// process, which takes certbot's lock as certbot does and holds it until its
// standard input closes.
func TestHelperHoldsCertbotsLock(t *testing.T) {
	path := os.Getenv("MMX_TEST_HOLD_CERTBOT_LOCK")
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &syscall.Flock_t{Type: syscall.F_WRLCK}); err != nil {
		t.Fatal(err)
	}
	fmt.Println("locked")
	bufio.NewReader(os.Stdin).ReadString('\n')
	// certbot removes its lock file before it lets the lock go.
	os.Remove(path)
}

// anotherCertbot starts a process that holds certbot's lock in one of
// certbot's directories, and returns the function that ends it. Every
// goroutine may call that function, and a call after the first one returns
// when the process has ended.
func anotherCertbot(t *testing.T, directory string) (end func()) {
	t.Helper()
	holder := exec.Command(os.Args[0], "-test.run=^TestHelperHoldsCertbotsLock$")
	holder.Env = append(os.Environ(), "MMX_TEST_HOLD_CERTBOT_LOCK="+filepath.Join(directory, ".certbot.lock"))
	input, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatal("the helper did not take the lock", line, err)
	}
	end = sync.OnceFunc(func() {
		input.Close()
		holder.Wait()
	})
	t.Cleanup(end)
	return end
}

// While another certbot holds certbot's lock, in any of its three
// directories, the task waits and starts no certbot. It goes on when the
// lock is free, says how long it waited, and gives up at its limit or when
// the task's own time limit ends, with the lock file in its message. A lock
// file that nobody holds is no lock.
func TestCertbotCertificateWaitsWhileAnotherCertbotRuns(t *testing.T) {
	f := newCertbotFixture(t)
	// Keep the kernel's real lock query. End or stop the waiting task only
	// after that query has observed the other process's lock.
	originalLockHeld := certbotLockHeld
	var afterLockObserved func()
	certbotLockHeld = func() (string, error) {
		held, err := originalLockHeld()
		if held != "" && err == nil && afterLockObserved != nil {
			action := afterLockObserved
			afterLockObserved = nil
			action()
		}
		return held, err
	}
	t.Cleanup(func() { certbotLockHeld = originalLockHeld })
	f.issuable("shop.example.com", "www.shop.example.com")
	task := f.webroot()
	// A file that an ended certbot left behind.
	writeState(t, filepath.Join(certbotConfigurationDirectory, ".certbot.lock"), "")
	if held, err := certbotLockHeld(); err != nil || held != "" {
		t.Fatal(held, err)
	}
	for _, directory := range []string{certbotConfigurationDirectory, certbotWorkDirectory, certbotLogDirectory} {
		end := anotherCertbot(t, directory)
		if held, err := certbotLockHeld(); err != nil || held != filepath.Join(directory, ".certbot.lock") {
			t.Fatal(held, err)
		}
		end()
		if held, err := certbotLockHeld(); err != nil || held != "" {
			t.Fatal("the lock is still held after its process ended", held, err)
		}
	}

	// The limit of the wait.
	end := anotherCertbot(t, certbotConfigurationDirectory)
	lock := filepath.Join(certbotConfigurationDirectory, ".certbot.lock")
	certbotLockWait = 40 * time.Millisecond
	r := f.apply(task)
	if r.Status != "failed" || r.Change != "unchanged" || r.Reason != "certbot_certificate: another certbot process still holds a lock after 40ms (lock file "+lock+"), so the certbot_certificate node task did not start certbot. On some distributions, the certbot renewal service waits for several minutes before renewing certificates. Run the playbook again when the running certbot process exits" {
		t.Fatalf("%+v", r)
	}
	// The task's own time limit ends the wait earlier: the task waits until
	// the limit itself and says what it waited for. The node task execution
	// engine puts its words about the limit before this reason.
	certbotLockWait = time.Minute
	synctest.Test(t, func(t *testing.T) {
		limited, cancel := context.WithTimeout(testContext(t), 40*time.Millisecond)
		defer cancel()
		r = task.Run(limited, NodeTask{})
		if r.Status != "failed" || r.Change != "unchanged" || !errors.Is(limited.Err(), context.DeadlineExceeded) ||
			r.Reason != "certbot_certificate: the node task reached its time limit after waiting 0s for another certbot process (lock file "+lock+"), so the certbot_certificate node task did not start certbot. On some distributions, the certbot renewal service waits for several minutes before renewing certificates. Run the playbook again when the running certbot process exits" {
			t.Fatalf("%+v %v", r, limited.Err())
		}
	})
	// A task that is stopped while it waits says so.
	stopped, stop := context.WithCancel(testContext(t))
	afterLockObserved = stop
	defer stop()
	r = task.Run(stopped, NodeTask{})
	if r.Status != "failed" || r.Change != "unchanged" || r.Reason != "certbot_certificate: the node task was stopped while waiting for another certbot process holding the lock "+lock+": context canceled" {
		t.Fatalf("%+v", r)
	}
	// A dryrun starts no certbot, so it does not wait.
	if preview := f.dryrun(task); preview.Change != "would_change" {
		t.Fatalf("%+v", preview)
	}
	if calls := takeCalls(t, f.dir); len(calls) != 0 {
		t.Fatal("certbot was started while another certbot ran", calls)
	}

	// The other certbot ends while the task waits.
	afterLockObserved = end
	r = f.apply(task)
	if r.Status != "succeeded" || r.Change != "changed" || afterLockObserved != nil {
		t.Fatalf("%+v", r)
	}
	if calls := takeCalls(t, f.dir); len(calls) != 1 {
		t.Fatal(calls)
	}
	// Changed settings wait in the same way before they are written: a
	// renewal that runs has read the renewal file and writes it again when
	// it ends. A dryrun does not wait.
	endRenewal := anotherCertbot(t, certbotLogDirectory)
	certbotLockWait = 40 * time.Millisecond
	other := f.executable("another-script")
	task.RunAfterIssueAndRenewal = other
	settings := f.renewalFile()
	if r := f.apply(task); r.Status != "failed" || r.Change != "unchanged" || !strings.Contains(r.Reason, "another certbot process still holds a lock after 40ms") || f.renewalFile() != settings {
		t.Fatalf("%+v", r)
	}
	if preview := f.dryrun(task); preview.Change != "would_change" || !strings.Contains(preview.Reason, "would write the task's settings") {
		t.Fatalf("%+v", preview)
	}
	// While the task waits, the other certbot writes the renewal file, as a
	// renewal does. The task then writes its setting into that file, and
	// keeps what the other certbot wrote.
	certbotLockWait = time.Minute
	var renewalWriteError error
	afterLockObserved = func() {
		renewalWriteError = os.WriteFile(certbotRenewalFilePath(testCertificate), []byte(settings+"[acme_renewal_info]\nari_retry_after = 2026-10-09T22:08:58\n"), 0o644)
		endRenewal()
	}
	r = f.apply(task)
	if renewalWriteError != nil {
		t.Fatal(renewalWriteError)
	}
	if r.Status != "succeeded" || r.Change != "changed" || !strings.HasPrefix(r.Reason, "wrote the task's settings into certbot's renewal settings") || !strings.Contains(r.Reason, "until certbot renews") || afterLockObserved != nil {
		t.Fatalf("%+v", r)
	}
	if text := f.renewalFile(); !strings.Contains(text, "renew_hook = "+other+"\n") || !strings.HasSuffix(text, "[acme_renewal_info]\nari_retry_after = 2026-10-09T22:08:58\n") {
		t.Fatalf("the renewal file after the wait:\n%s", text)
	}
	if calls := takeCalls(t, f.dir); len(calls) != 0 {
		t.Fatal("changed settings started certbot", calls)
	}
	// Deleting waits in the same way.
	certbotLockPoll = 5 * time.Millisecond
	endLast := anotherCertbot(t, certbotWorkDirectory)
	certbotLockWait = 40 * time.Millisecond
	absent := CertbotCertificate{CertificateName: testCertificate, State: "absent"}
	if r := f.apply(absent); r.Status != "failed" || r.Change != "unchanged" || !strings.Contains(r.Reason, "another certbot process still holds a lock after 40ms") || f.fingerprint() == "" {
		t.Fatalf("%+v", r)
	}
	endLast()
	if r := f.apply(absent); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
}

// state: absent deletes the certificate with certbot delete; a missing
// certificate is unchanged, and a dryrun deletes nothing.
func TestCertbotCertificateAbsentDeletesTheCertificate(t *testing.T) {
	f := newCertbotFixture(t)
	absent := CertbotCertificate{CertificateName: testCertificate, State: "absent"}
	for _, r := range []protocol.Result{f.apply(absent), f.dryrun(absent)} {
		if r.Status != "succeeded" || r.Change != "unchanged" || r.Value != nil {
			t.Fatalf("%+v", r)
		}
	}
	f.issuable("shop.example.com", "www.shop.example.com")
	f.apply(f.webroot())
	takeCalls(t, f.dir)
	preview := f.dryrun(absent)
	if preview.Change != "would_change" || preview.Reason != "would delete the certificate shop.example.com, its key, and its renewal settings" || f.fingerprint() == "" || len(takeCalls(t, f.dir)) != 0 {
		t.Fatalf("%+v", preview)
	}

	// A failure of certbot delete, and another try after it.
	writeState(t, filepath.Join(f.dir, "delete-fails"), "")
	failed := f.apply(absent)
	if failed.Status != "failed" || failed.Change != "unknown" || !strings.HasPrefix(failed.Reason, "certbot_certificate: certbot delete left "+certbotRenewalFilePath(testCertificate)+", "+certbotLiveDirectory(testCertificate)+", "+certbotArchiveDirectory(testCertificate)+" of the certificate shop.example.com:\ncertbot could not delete the certificate") {
		t.Fatalf("%+v", failed)
	}
	if err := os.Remove(filepath.Join(f.dir, "delete-fails")); err != nil {
		t.Fatal(err)
	}
	takeCalls(t, f.dir)
	deleted := f.apply(absent)
	if deleted.Status != "succeeded" || deleted.Change != "changed" || deleted.Reason != "deleted the certificate shop.example.com, its key, and its renewal settings." {
		t.Fatalf("%+v", deleted)
	}
	if calls := takeCalls(t, f.dir); len(calls) != 1 || calls[0] != "certbot delete --cert-name shop.example.com --non-interactive" {
		t.Fatal(calls)
	}
	for _, path := range []string{certbotRenewalFilePath(testCertificate), certbotLiveDirectory(testCertificate), certbotArchiveDirectory(testCertificate)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("certbot delete left", path)
		}
	}
	if again := f.apply(absent); again.Change != "unchanged" || len(takeCalls(t, f.dir)) != 0 || f.fingerprint() != "" {
		t.Fatalf("%+v", again)
	}
}

// state: absent finishes a removal that stopped part of the way. certbot
// removes the renewal file first, so a certbot delete that was stopped
// leaves the certificate's two directories, with its key, and no renewal
// file: MMX removes them itself, in the same run when certbot delete left
// them there, and in the next run when it finds them so.
func TestCertbotCertificateAbsentFinishesARemovalThatStoppedPartOfTheWay(t *testing.T) {
	f := newCertbotFixture(t)
	absent := CertbotCertificate{CertificateName: testCertificate, State: "absent"}
	renewal, live, archive := certbotRenewalFilePath(testCertificate), certbotLiveDirectory(testCertificate), certbotArchiveDirectory(testCertificate)
	gone := func() {
		t.Helper()
		for _, path := range []string{renewal, live, archive} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("still on the node:", path)
			}
		}
	}
	f.issuable("shop.example.com", "www.shop.example.com")
	f.apply(f.webroot())
	takeCalls(t, f.dir)

	// certbot delete stops after the renewal file, in this run.
	writeState(t, filepath.Join(f.dir, "delete-stops-after-the-renewal-file"), "")
	r := f.apply(absent)
	if r.Status != "succeeded" || r.Change != "changed" || r.Reason != "deleted the certificate shop.example.com, its key, and its renewal settings. certbot delete left "+live+", "+archive+", which MMX removed." {
		t.Fatalf("%+v", r)
	}
	gone()
	if calls := takeCalls(t, f.dir); len(calls) != 1 || calls[0] != "certbot delete --cert-name shop.example.com --non-interactive" {
		t.Fatal(calls)
	}
	if again := f.apply(absent); again.Change != "unchanged" || len(takeCalls(t, f.dir)) != 0 {
		t.Fatalf("%+v", again)
	}

	// The directories are there when the task starts, without a renewal
	// file: an earlier removal stopped. A dryrun says what a run would
	// remove and removes nothing; the run needs no certbot for it.
	if err := os.Remove(filepath.Join(f.dir, "delete-stops-after-the-renewal-file")); err != nil {
		t.Fatal(err)
	}
	f.issuable("shop.example.com", "www.shop.example.com")
	f.apply(f.webroot())
	takeCalls(t, f.dir)
	if err := os.Remove(renewal); err != nil {
		t.Fatal(err)
	}
	preview := f.dryrun(absent)
	if preview.Status != "succeeded" || preview.Change != "would_change" || preview.Reason != "would remove what an unfinished removal or request of the certificate shop.example.com left on the node: "+live+", "+archive {
		t.Fatalf("%+v", preview)
	}
	if f.fingerprint() == "" {
		t.Fatal("the dryrun removed the certificate's files")
	}
	// Another certbot, which may be the removal itself, is waited for.
	end := anotherCertbot(t, certbotConfigurationDirectory)
	certbotLockWait = 40 * time.Millisecond
	if r := f.apply(absent); r.Status != "failed" || r.Change != "unchanged" || !strings.Contains(r.Reason, "another certbot process still holds a lock after 40ms") || f.fingerprint() == "" {
		t.Fatalf("%+v", r)
	}
	end()
	path := workerPath(t)
	setWorkerEnvironment(t, "PATH", t.TempDir())
	r = f.apply(absent)
	setWorkerEnvironment(t, "PATH", path)
	if r.Status != "succeeded" || r.Change != "changed" || r.Reason != "removed what an unfinished removal or request of the certificate shop.example.com left on the node: "+live+", "+archive+"." {
		t.Fatalf("%+v", r)
	}
	gone()
	for _, again := range []protocol.Result{f.apply(absent), f.dryrun(absent)} {
		if again.Status != "succeeded" || again.Change != "unchanged" {
			t.Fatalf("%+v", again)
		}
	}
	if calls := takeCalls(t, f.dir); len(calls) != 0 {
		t.Fatal("certbot was started for directories without a renewal file", calls)
	}

	// Only the archive is left, with a key whose copy an older certbot
	// keeps: the copy goes with it.
	_, key := testCertificateFor(t, "unused.example.com")
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	text := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
	copyOfKey := filepath.Join(certbotConfigurationDirectory, "keys", "0000_key-certbot.pem")
	writeState(t, filepath.Join(archive, "privkey1.pem"), text)
	writeState(t, copyOfKey, text)
	r = f.apply(absent)
	if r.Status != "succeeded" || r.Change != "changed" || !strings.HasPrefix(r.Reason, "removed what an unfinished removal or request of the certificate shop.example.com left on the node: "+archive+". Also removed 1 copies of its keys") {
		t.Fatalf("%+v", r)
	}
	gone()
	if _, err := os.Lstat(copyOfKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the copy of the key is still on the node")
	}
}

// certbot older than 2.6 keeps a second copy of each private key, and the
// certificate request made with it, in directories that certbot delete
// leaves alone. state: absent removes the files that carry a key of the
// deleted certificate, and no file of another certificate.
func TestCertbotCertificateAbsentRemovesTheKeyCopiesOfAnOlderCertbot(t *testing.T) {
	f := newCertbotFixture(t)
	f.issuable("shop.example.com", "www.shop.example.com")
	f.apply(f.webroot())
	f.issuable("other.example.com")
	other := f.webroot()
	other.CertificateName, other.Domains = "other.example.com", []string{"other.example.com"}
	f.apply(other)
	takeCalls(t, f.dir)

	keys, requests := filepath.Join(certbotConfigurationDirectory, "keys"), filepath.Join(certbotConfigurationDirectory, "csr")
	// The files as certbot 1.21 and 2.1 number them, across the whole node:
	// the first and the third are the certificate's, from its issue and a
	// renewal; the second is the other certificate's.
	for number, belongsTo := range []string{testCertificate, "other.example.com", testCertificate} {
		_, key := testCertificateFor(t, "unused.example.com")
		encoded, err := x509.MarshalPKCS8PrivateKey(key)
		if number == 2 {
			// certbot wrote this form of a key before it wrote PKCS #8.
			encoded, err = x509.MarshalECPrivateKey(key)
		}
		if err != nil {
			t.Fatal(err)
		}
		kind := map[bool]string{false: "PRIVATE KEY", true: "EC PRIVATE KEY"}[number == 2]
		text := string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: encoded}))
		request, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{belongsTo}}, key)
		if err != nil {
			t.Fatal(err)
		}
		archived := map[int]string{0: "privkey1.pem", 1: "privkey1.pem", 2: "privkey2.pem"}[number]
		writeState(t, filepath.Join(certbotArchiveDirectory(belongsTo), archived), text)
		writeState(t, filepath.Join(keys, fmt.Sprintf("%04d_key-certbot.pem", number)), text)
		writeState(t, filepath.Join(requests, fmt.Sprintf("%04d_csr-certbot.pem", number)), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: request})))
	}
	writeState(t, filepath.Join(keys, "notes.txt"), "not a key\n")
	left := func() string {
		var names []string
		for _, directory := range []string{keys, requests} {
			entries, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
		}
		return strings.Join(names, " ")
	}
	absent := CertbotCertificate{CertificateName: testCertificate, State: "absent"}
	if preview := f.dryrun(absent); preview.Change != "would_change" || left() != "0000_key-certbot.pem 0001_key-certbot.pem 0002_key-certbot.pem notes.txt 0000_csr-certbot.pem 0001_csr-certbot.pem 0002_csr-certbot.pem" {
		t.Fatalf("%+v %s", preview, left())
	}
	// certbot delete fails after the copies are gone: the result says that
	// the node changed, and the next run finishes.
	writeState(t, filepath.Join(f.dir, "delete-fails"), "")
	if failed := f.apply(absent); failed.Status != "failed" || failed.Change != "changed" || left() != "0001_key-certbot.pem notes.txt 0001_csr-certbot.pem" {
		t.Fatalf("%+v %s", failed, left())
	}
	if err := os.Remove(filepath.Join(f.dir, "delete-fails")); err != nil {
		t.Fatal(err)
	}
	deleted := f.apply(absent)
	if deleted.Status != "succeeded" || deleted.Change != "changed" || left() != "0001_key-certbot.pem notes.txt 0001_csr-certbot.pem" {
		t.Fatalf("%+v %s", deleted, left())
	}
	// With the copies still there, the result counts them.
	f.issuable("shop.example.com", "www.shop.example.com")
	f.apply(f.webroot())
	_, key := testCertificateFor(t, "unused.example.com")
	encoded, _ := x509.MarshalPKCS8PrivateKey(key)
	text := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
	writeState(t, filepath.Join(certbotArchiveDirectory(testCertificate), "privkey1.pem"), text)
	writeState(t, filepath.Join(keys, "0003_key-certbot.pem"), text)
	deleted = f.apply(absent)
	if deleted.Change != "changed" || deleted.Reason != "deleted the certificate shop.example.com, its key, and its renewal settings. Also removed 1 copies of its keys and certificate requests that this older certbot keeps in "+keys+" and "+requests+"." {
		t.Fatalf("%+v", deleted)
	}
	// The other certificate and its copies are as they were.
	if again := f.apply(other); again.Change != "unchanged" || left() != "0001_key-certbot.pem notes.txt 0001_csr-certbot.pem" {
		t.Fatalf("%+v %s", again, left())
	}
}

// Each rejected input names its field and says what to write.
func TestCertbotCertificateRejectedInputs(t *testing.T) {
	t.Parallel()
	good := CertbotCertificate{CertificateName: "shop.example.com", Domains: []string{"shop.example.com"}, AccountEmailWhenRegistered: "admin@example.com", CertificateAuthorityURL: testAuthority, Challenge: "standalone"}
	if err := good.Prepare(""); err != nil {
		t.Fatal(err)
	}
	with := func(change func(*CertbotCertificate)) CertbotCertificate {
		c := good
		c.Domains = append([]string{}, good.Domains...)
		change(&c)
		return c
	}
	for message, c := range map[string]CertbotCertificate{
		"certificate_name: certbot_certificate.certificate_name is required: certbot's name for the certificate, such as shop.example.com":                                                                                    with(func(c *CertbotCertificate) { c.CertificateName = "" }),
		"state: certbot_certificate.state must be present or absent":                                                                                                                                                          with(func(c *CertbotCertificate) { c.State = "removed" }),
		"domains: certbot_certificate.domains is required: the names that the certificate covers":                                                                                                                             with(func(c *CertbotCertificate) { c.Domains = nil }),
		"account_email_when_registered: certbot_certificate.account_email_when_registered is required: the contact address of the node's account at the certificate authority":                                                with(func(c *CertbotCertificate) { c.AccountEmailWhenRegistered = "" }),
		"challenge: certbot_certificate.challenge is required: webroot, standalone, dns; challenge has no default because challenge selects how the certificate authority checks control of the domain names":                 with(func(c *CertbotCertificate) { c.Challenge = "" }),
		"challenge: certbot_certificate.challenge must be one of webroot, standalone, dns":                                                                                                                                    with(func(c *CertbotCertificate) { c.Challenge = "http" }),
		"webroot_path: challenge: webroot needs webroot_path: the web root; certbot writes challenge files below <webroot_path>/.well-known/acme-challenge/, which the web server must serve at /.well-known/acme-challenge/": with(func(c *CertbotCertificate) { c.Challenge = "webroot" }),
		"webroot_path: webroot_path goes only with challenge: webroot":                                                                                                                                                        with(func(c *CertbotCertificate) { c.WebrootPath = "/var/www" }),
		"dns_plugin: challenge: dns needs dns_plugin: the name of certbot's DNS plugin, such as cloudflare or rfc2136":                                                                                                        with(func(c *CertbotCertificate) { c.Challenge = "dns" }),
		"dns_plugin: dns_plugin goes only with challenge: dns":                                                                                                                                                                with(func(c *CertbotCertificate) { c.DNSPlugin = "cloudflare" }),
		"dns_credentials_file: dns_credentials_file goes only with challenge: dns":                                                                                                                                            with(func(c *CertbotCertificate) { c.DNSCredentialsFile = "/etc/letsencrypt/cloudflare.ini" }),
		"wait_after_creating_dns_record: wait_after_creating_dns_record goes only with challenge: dns":                                                                                                                        with(func(c *CertbotCertificate) { c.WaitAfterCreatingDNSRecord = "30s" }),
		"domains[1]: domains lists SHOP.example.com twice (also at domains[0])":                                                                                                                                               with(func(c *CertbotCertificate) { c.Domains = append(c.Domains, "SHOP.example.com") }),
		"domains: certbot_certificate with state: absent accepts only certificate_name; remove domains":                                                                                                                       with(func(c *CertbotCertificate) { c.State = "absent" }),
		"certificate_authority_url: certbot_certificate with state: absent accepts only certificate_name; remove certificate_authority_url":                                                                                   {CertificateName: "shop.example.com", State: "absent", CertificateAuthorityURL: testAuthority},
	} {
		if err := c.Prepare(""); err == nil || err.Error() != message {
			t.Errorf("%+v: %v\nwant %s", c, err, message)
		}
	}
	if err := (&CertbotCertificate{CertificateName: "shop.example.com", State: "absent"}).Prepare(""); err != nil {
		t.Error(err)
	}
	// A name that is still a reference is compared when the task runs.
	twice := with(func(c *CertbotCertificate) { c.Domains = []string{"{{ .vars.name }}", "shop.example.com"} })
	if err := twice.Prepare(""); err != nil {
		t.Error(err)
	}
	twice.Domains[0] = "shop.example.com"
	if r := (Runtime{}).CertbotCertificate(testContext(t), twice); r.FailureKind != "config" || !strings.Contains(r.Reason, "domains lists shop.example.com twice") {
		t.Errorf("%+v", r)
	}

	// The rules of the inputs that may hold a reference.
	rules := func(c CertbotCertificate) map[string]func(any) error {
		var arguments ArgumentList
		c.Arguments(&arguments)
		found := map[string]func(any) error{}
		for _, argument := range arguments.List {
			found[argument.Field] = argument.Rule
		}
		return found
	}
	everything := CertbotCertificate{CertificateName: "x", Domains: []string{"x"}, AccountEmailWhenRegistered: "x", Challenge: "dns", DNSPlugin: "x", DNSCredentialsFile: "x",
		WaitAfterCreatingDNSRecord: "x", RunAfterIssueAndRenewal: "x", CertificateAuthorityURL: "x"}
	webroot := CertbotCertificate{CertificateName: "x", Domains: []string{"x"}, Challenge: "webroot", WebrootPath: "x"}
	for _, check := range []struct {
		rule         func(any) error
		value, error string
	}{
		{rules(everything)["certificate_name"], "shop.example.com", ""},
		{rules(everything)["certificate_name"], "shop_certificate-2", ""},
		{rules(everything)["certificate_name"], "../shop", "certificate_name must use letters, digits, '.', '_', and '-', and start with a letter or a digit: certbot names the certificate's directory and its renewal file after it"},
		{rules(everything)["certificate_name"], "-shop", "certificate_name must use"},
		{rules(everything)["domains[0]"], "shop.example.com", ""},
		{rules(everything)["domains[0]"], "*.example.com", ""},
		{rules(everything)["domains[0]"], "xn--mnchen-3ya.example", ""},
		{rules(everything)["domains[0]"], "münchen.example", "each name in domains must be a DNS name such as shop.example.com or *.example.com, in letters, digits, '-', and '.'; write a name with other letters in its xn-- form"},
		{rules(everything)["domains[0]"], "shop.example.com,www.shop.example.com", "each name in domains must be a DNS name"},
		{rules(everything)["domains[0]"], "-d", "each name in domains must be a DNS name"},
		{rules(everything)["domains[0]"], "*.*.example.com", "each name in domains must be a DNS name"},
		{rules(webroot)["domains[0]"], "*.example.com", "the wildcard name *.example.com needs challenge: dns, because a certificate authority checks a wildcard name only through DNS"},
		{rules(everything)["account_email_when_registered"], "admin@example.com", ""},
		{rules(everything)["account_email_when_registered"], "not-an-address", "account_email_when_registered must be one e-mail address, such as admin@example.com"},
		{rules(everything)["account_email_when_registered"], "one@example.com, two@example.com", "account_email_when_registered must be one e-mail address"},
		{rules(webroot)["webroot_path"], "/var/www/certbot challenges", ""},
		{rules(webroot)["webroot_path"], "var/www", "webroot_path must be an absolute path on one line"},
		{rules(everything)["dns_plugin"], "rfc2136", ""},
		{rules(everything)["dns_plugin"], "multi-provider", ""},
		{rules(everything)["dns_plugin"], "dns-cloudflare", "dns_plugin is the plugin's name without dns-: write cloudflare"},
		{rules(everything)["dns_plugin"], "Cloud Flare", "dns_plugin must be the name of certbot's DNS plugin in lower-case letters, digits, '-', and '_', such as cloudflare or rfc2136"},
		{rules(everything)["dns_credentials_file"], "/etc/letsencrypt/cloudflare.ini", ""},
		{rules(everything)["dns_credentials_file"], "cloudflare.ini", "dns_credentials_file must be an absolute path on one line"},
		{rules(everything)["wait_after_creating_dns_record"], "30s", ""},
		{rules(everything)["wait_after_creating_dns_record"], "2m", ""},
		{rules(everything)["wait_after_creating_dns_record"], "0s", ""},
		{rules(everything)["wait_after_creating_dns_record"], "30", "wait_after_creating_dns_record must be a duration of whole seconds, such as 30s or 2m"},
		{rules(everything)["wait_after_creating_dns_record"], "1500ms", "wait_after_creating_dns_record must be a duration of whole seconds"},
		{rules(everything)["wait_after_creating_dns_record"], "-5s", "wait_after_creating_dns_record must be a duration of whole seconds"},
		{rules(everything)["run_after_issue_and_renewal"], "/usr/local/lib/shop site/reload", ""},
		{rules(everything)["run_after_issue_and_renewal"], "systemctl reload nginx", "run_after_issue_and_renewal must be an absolute path on one line"},
		{rules(everything)["run_after_issue_and_renewal"], "/usr/local/bin/one\n/usr/local/bin/two", "run_after_issue_and_renewal must be an absolute path on one line"},
		{rules(everything)["certificate_authority_url"], "https://acme.example.com/directory", ""},
		{rules(everything)["certificate_authority_url"], "acme.example.com", "certificate_authority_url must be the HTTPS or HTTP address of a certificate authority's ACME directory, such as https://acme.example.com/directory"},
	} {
		if check.rule == nil {
			t.Fatalf("no rule for the value %q", check.value)
		}
		err := check.rule(check.value)
		if (check.error == "") != (err == nil) || (err != nil && !strings.HasPrefix(err.Error(), check.error)) {
			t.Errorf("%q: %v; want %q", check.value, err, check.error)
		}
	}
}

// The script's path reaches certbot as one word of a shell, and certbot's
// plugin keys carry the plugin's name with underscores.
func TestCertbotWordsForTheShellAndThePluginKeys(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"/usr/local/bin/reload-nginx-after-certificate-renewal": "/usr/local/bin/reload-nginx-after-certificate-renewal",
		"/usr/local/lib/shop site/reload":                       "'/usr/local/lib/shop site/reload'",
		"/usr/local/lib/it's/reload":                            `'/usr/local/lib/it'\''s/reload'`,
		"/usr/local/bin/reload;rm":                              "'/usr/local/bin/reload;rm'",
		"/usr/local/bin/$(reload)":                              "'/usr/local/bin/$(reload)'",
		"":                                                      "",
	} {
		if got := shellWord(path); got != want {
			t.Errorf("%q is %s; want %s", path, got, want)
		}
		if path == "" {
			continue
		}
		// A shell reads the word back as the path.
		if out, err := exec.Command("/bin/sh", "-c", "printf %s "+shellWord(path)).Output(); err != nil || string(out) != path {
			t.Errorf("the shell reads %s as %q, %v", shellWord(path), out, err)
		}
	}
	if credentials, wait := certbotPluginKeys("multi-provider"); credentials != "dns_multi_provider_credentials" || wait != "dns_multi_provider_propagation_seconds" {
		t.Error(credentials, wait)
	}
}

// MMX asks the interpreter that the certbot program names in its first line
// which plugins certbot can load, and knows nothing for a program that is no
// script or an answer that is not certbot's.
func TestCertbotPluginsAreListedByTheInterpreterOfTheCertbotProgram(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	interpreter := filepath.Join(dir, "python3")
	answer := filepath.Join(dir, "answer")
	// The stand-in interpreter prints the answer and notes its arguments.
	writeStandInProgram(t, interpreter, "#!/bin/sh\necho \"$1 $2 $3\" > "+dir+"/arguments\nprintf '%s' \"$4\" > "+dir+"/code\ncat "+answer+"\n")
	program := filepath.Join(dir, "certbot")
	writeState(t, program, "#! "+interpreter+" -s\nimport sys\n")
	if got := certbotInterpreter(program); !reflect.DeepEqual(got, []string{interpreter, "-s"}) {
		t.Fatal(got)
	}
	writeState(t, answer, `["dns-rfc2136", "manual", "null", "standalone", "webroot"]`+"\n")
	plugins, known := certbotPluginsOf(testContext(t), program)
	if !known || !reflect.DeepEqual(plugins, []string{"dns-rfc2136", "manual", "null", "standalone", "webroot"}) {
		t.Fatal(plugins, known)
	}
	// -B: Python writes no byte-code file, so a dryrun writes nothing.
	if arguments := readState(t, filepath.Join(dir, "arguments")); arguments != "-s -B -c\n" || !strings.Contains(readState(t, filepath.Join(dir, "code")), `group="certbot.plugins"`) {
		t.Fatal(arguments)
	}
	for name, text := range map[string]string{
		"an answer without certbot's own plugins": `["dns-rfc2136"]`,
		"an answer that is no list":               `{"webroot": true}`,
		"no answer":                               "",
	} {
		writeState(t, answer, text)
		if plugins, known := certbotPluginsOf(testContext(t), program); known {
			t.Errorf("%s: %q", name, plugins)
		}
	}
	for name, first := range map[string]string{
		"a program that is no script":         "\x7fELF\x02\x01\x01",
		"an interpreter that is no full path": "#!python3",
		"an empty first line":                 "#!",
	} {
		writeState(t, program, first+"\n")
		if got := certbotInterpreter(program); got != nil {
			t.Errorf("%s: %q", name, got)
		}
		if _, known := certbotPluginsOf(testContext(t), program); known {
			t.Errorf("%s: plugins are known", name)
		}
	}
	if got := certbotInterpreter(filepath.Join(dir, "missing")); got != nil {
		t.Error(got)
	}
}

func TestCertbotWaitedSentence(t *testing.T) {
	for _, example := range []struct {
		waited   time.Duration
		sentence string
	}{
		{0, ""}, {999 * time.Millisecond, ""},
		{time.Second, " The task waited 1s until another certbot had ended."},
		{2900 * time.Millisecond, " The task waited 2s until another certbot had ended."},
	} {
		if got := waitedSentence(example.waited); got != example.sentence {
			t.Fatalf("%s: %q, want %q", example.waited, got, example.sentence)
		}
	}
}

func TestCertbotOmittedRenewalSettingsStayAcrossChangesAndRequests(t *testing.T) {
	f := newCertbotFixture(t)
	f.issuable("shop.example.com", "www.shop.example.com")
	input := f.webroot()
	input.Challenge, input.WebrootPath, input.DNSPlugin = "dns", "", "rfc2136"
	input.DNSCredentialsFile = filepath.Join(f.dir, "rfc2136.ini")
	input.WaitAfterCreatingDNSRecord = "30s"
	writeState(t, input.DNSCredentialsFile, "dns_rfc2136_server = 127.0.0.1\n")
	if result := f.apply(input); result.Status != "succeeded" {
		t.Fatal(result)
	}
	before := f.renewalFile()
	takeCalls(t, f.dir)
	input.RunAfterIssueAndRenewal, input.DNSCredentialsFile, input.WaitAfterCreatingDNSRecord = "", "", ""
	for _, result := range []protocol.Result{f.dryrun(input), f.apply(input)} {
		if result.Status != "succeeded" || result.Change != "unchanged" {
			t.Fatal(result)
		}
	}
	if f.renewalFile() != before || len(takeCalls(t, f.dir)) != 0 {
		t.Fatal("omission changed saved settings")
	}
	input.Domains = []string{"shop.example.com", "api.shop.example.com"}
	f.issuable(input.Domains...)
	if result := f.apply(input); result.Status != "succeeded" {
		t.Fatal(result)
	}
	for _, setting := range []string{"renew_hook = " + f.script, "dns_rfc2136_credentials = " + filepath.Join(f.dir, "rfc2136.ini"), "dns_rfc2136_propagation_seconds = 30"} {
		if !strings.Contains(f.renewalFile(), setting) {
			t.Fatal(f.renewalFile())
		}
	}
	input.Challenge, input.DNSPlugin = "standalone", ""
	if result := f.apply(input); result.Status != "succeeded" {
		t.Fatal(result)
	}
	if !strings.Contains(f.renewalFile(), "dns_rfc2136_credentials") {
		t.Fatal("challenge change removed omitted DNS settings")
	}
	input.RenewalSettingsToRemove = []string{"run_after_issue_and_renewal", "dns_credentials_file", "wait_after_creating_dns_record"}
	if result := f.dryrun(input); result.Change != "would_change" {
		t.Fatal(result)
	}
	if result := f.apply(input); result.Status != "succeeded" || result.Change != "changed" {
		t.Fatal(result)
	}
	if text := f.renewalFile(); strings.Contains(text, "renew_hook") || strings.Contains(text, "dns_rfc2136_") {
		t.Fatal(text)
	}
	if result := f.apply(input); result.Change != "unchanged" {
		t.Fatal(result)
	}
}

func TestCertbotRequiredAuthorityAndExplicitRemovalRefusals(t *testing.T) {
	t.Parallel()
	good := CertbotCertificate{CertificateName: "shop.example.com", Domains: []string{"shop.example.com"}, AccountEmailWhenRegistered: "admin@example.com", Challenge: "dns", DNSPlugin: "rfc2136", CertificateAuthorityURL: testAuthority}
	for _, test := range []struct {
		change func(*CertbotCertificate)
		reason string
	}{
		{func(c *CertbotCertificate) { c.CertificateAuthorityURL = "" }, "certificate_authority_url is required"},
		{func(c *CertbotCertificate) { c.RenewalSettingsToRemove = []string{"challenge"} }, "accepts only"},
		{func(c *CertbotCertificate) {
			c.RenewalSettingsToRemove = []string{"run_after_issue_and_renewal"}
			c.RunAfterIssueAndRenewal = "/usr/local/bin/reload"
		}, "set as a task input and listed in renewal_settings_to_remove"},
		{func(c *CertbotCertificate) {
			c.RenewalSettingsToRemove = []string{"dns_credentials_file"}
			c.DNSCredentialsFile = "/etc/secret"
		}, "set as a task input and listed in renewal_settings_to_remove"},
		{func(c *CertbotCertificate) {
			c.RenewalSettingsToRemove = []string{"wait_after_creating_dns_record"}
			c.WaitAfterCreatingDNSRecord = "30s"
		}, "set as a task input and listed in renewal_settings_to_remove"},
		{func(c *CertbotCertificate) {
			*c = CertbotCertificate{CertificateName: "shop.example.com", State: "absent", RenewalSettingsToRemove: []string{"run_after_issue_and_renewal"}}
		}, "accepts only certificate_name"},
	} {
		input := good
		test.change(&input)
		if err := input.Prepare(""); err == nil || !strings.Contains(err.Error(), test.reason) {
			t.Fatal(err)
		}
	}
}
