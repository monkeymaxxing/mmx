package config

import (
	"strings"
	"testing"

	"mmx/internal/modules"
)

// A certbot_certificate task loads with references in its inputs, later
// tasks may read its certificate_dir and domains, and each rejected
// input names its field and says what to write.
func TestCertbotCertificateLoads(t *testing.T) {
	const required = "    certificate_name: shop.example.com\n    domains: [shop.example.com, www.shop.example.com]\n    account_email_when_registered: admin@example.com\n    certificate_authority_url: https://acme-v02.api.letsencrypt.org/directory\n"
	for name, tasks := range map[string]string{
		"webroot, and a later task that reads the result": "- id: certificate\n  certbot_certificate:\n" + required + "    challenge: webroot\n    webroot_path: /var/www/certbot-challenges\n    run_after_issue_and_renewal: /usr/local/bin/reload-nginx-after-certificate-renewal\n" +
			"- show: \"{{ .node_tasks.certificate.value.certificate_dir }}/fullchain.pem covers {{ .node_tasks.certificate.value.domains }}\"\n",
		"standalone with another authority": "- certbot_certificate:\n" + strings.ReplaceAll(required, "https://acme-v02.api.letsencrypt.org/directory", "https://acme.example.com/directory") + "    challenge: standalone\n",
		"dns with a wildcard name":          "- certbot_certificate:\n    certificate_name: example.com\n    domains: [example.com, \"*.example.com\"]\n    account_email_when_registered: admin@example.com\n    certificate_authority_url: https://acme-v02.api.letsencrypt.org/directory\n    challenge: dns\n    dns_plugin: cloudflare\n    dns_credentials_file: /etc/letsencrypt/cloudflare.ini\n    wait_after_creating_dns_record: 30s\n",
		"dns without a credentials file":    "- certbot_certificate:\n" + required + "    challenge: dns\n    dns_plugin: route53\n",
		"absent":                            "- certbot_certificate:\n    certificate_name: shop.example.com\n    state: absent\n",
		"a retry that the task sets":        "- certbot_certificate:\n" + required + "    challenge: standalone\n  retry: {max_tries: 2, pause_between_tries: 10m}\n",
	} {
		if err := loadExample(t, tasks); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// In the changes of a node task block the task is no tracked input: the
	// block's service does not follow it. certbot's own script is what
	// follows a new certificate.
	if Tracked(Task{Module: "certbot_certificate", ModuleInputs: &modules.CertbotCertificate{CertificateName: "shop.example.com", State: "absent"}}, "changes") {
		t.Error("a certbot_certificate task is a tracked input of its node task block")
	}
	for _, tc := range []struct{ fields, want string }{
		{"    domains: [shop.example.com]\n    account_email_when_registered: admin@example.com\n    certificate_authority_url: https://acme-v02.api.letsencrypt.org/directory\n    challenge: standalone\n", "certbot_certificate.certificate_name is required: certbot's name for the certificate, such as shop.example.com"},
		{"    certificate_name: shop.example.com\n    account_email_when_registered: admin@example.com\n    certificate_authority_url: https://acme-v02.api.letsencrypt.org/directory\n    challenge: standalone\n", "certbot_certificate.domains is required: the names that the certificate covers"},
		{"    certificate_name: shop.example.com\n    domains: [shop.example.com]\n    challenge: standalone\n", "certbot_certificate.account_email_when_registered is required: the contact address of the node's account at the certificate authority"},
		{required, "certbot_certificate.challenge is required: webroot, standalone, dns; challenge has no default"},
		{required + "    challenge: http\n", "certbot_certificate.challenge must be one of webroot, standalone, dns"},
		{required + "    challenge: webroot\n", "challenge: webroot needs webroot_path"},
		{required + "    challenge: standalone\n    webroot_path: /var/www\n", "webroot_path goes only with challenge: webroot"},
		{required + "    challenge: dns\n", "challenge: dns needs dns_plugin"},
		{required + "    challenge: standalone\n    dns_credentials_file: /etc/letsencrypt/cloudflare.ini\n", "dns_credentials_file goes only with challenge: dns"},
		{required + "    challenge: standalone\n    state: removed\n", "certbot_certificate.state must be present or absent"},
		{required + "    state: absent\n", "certbot_certificate with state: absent accepts only certificate_name; remove domains"},
		{"    certificate_name: shop.example.com\n    domains: [shop.example.com, Shop.example.com]\n    account_email_when_registered: admin@example.com\n    certificate_authority_url: https://acme-v02.api.letsencrypt.org/directory\n    challenge: standalone\n", "domains lists Shop.example.com twice (also at domains[0])"},
		{"    certificate_name: shop.example.com\n    domains: [\"*.example.com\"]\n    account_email_when_registered: admin@example.com\n    certificate_authority_url: https://acme-v02.api.letsencrypt.org/directory\n    challenge: webroot\n    webroot_path: /var/www\n", "the wildcard name *.example.com needs challenge: dns, because a certificate authority checks a wildcard name only through DNS"},
		{"    certificate_name: shop.example.com\n    domains: [\"shop example com\"]\n    account_email_when_registered: admin@example.com\n    certificate_authority_url: https://acme-v02.api.letsencrypt.org/directory\n    challenge: standalone\n", "each name in domains must be a DNS name such as shop.example.com or *.example.com"},
		{"    certificate_name: ../shop\n    domains: [shop.example.com]\n    account_email_when_registered: admin@example.com\n    certificate_authority_url: https://acme-v02.api.letsencrypt.org/directory\n    challenge: standalone\n", "certificate_name must use letters, digits, '.', '_', and '-'"},
		{"    certificate_name: shop.example.com\n    domains: [shop.example.com]\n    account_email_when_registered: admin\n    certificate_authority_url: https://acme-v02.api.letsencrypt.org/directory\n    challenge: standalone\n", "account_email_when_registered must be one e-mail address, such as admin@example.com"},
		{required + "    challenge: webroot\n    webroot_path: www/challenges\n", "webroot_path must be an absolute path on one line"},
		{required + "    challenge: dns\n    dns_plugin: dns-cloudflare\n", "dns_plugin is the plugin's name without dns-: write cloudflare"},
		{required + "    challenge: dns\n    dns_plugin: cloudflare\n    dns_credentials_file: cloudflare.ini\n", "dns_credentials_file must be an absolute path on one line"},
		{required + "    challenge: dns\n    dns_plugin: cloudflare\n    wait_after_creating_dns_record: \"30\"\n", "wait_after_creating_dns_record must be a duration of whole seconds, such as 30s or 2m"},
		{required + "    challenge: standalone\n    run_after_issue_and_renewal: systemctl reload nginx\n", "run_after_issue_and_renewal must be an absolute path on one line"},
		{strings.ReplaceAll(required, "https://acme-v02.api.letsencrypt.org/directory", "acme.example.com") + "    challenge: standalone\n", "certificate_authority_url must be the HTTPS or HTTP address of a certificate authority's ACME directory"},
		{required + "    challenge: standalone\n    key_type: rsa\n", "key_type"},
	} {
		err := loadExample(t, "- certbot_certificate:\n"+tc.fields)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v", tc.fields, err)
		}
	}
}
