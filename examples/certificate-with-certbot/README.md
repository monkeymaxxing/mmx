# A TLS certificate with certbot

This example gets one TLS certificate for a site from Let's Encrypt with
certbot. certbot's own timer handles renewal.

The node must already run a web server that answers plain HTTP for the
site's names and maps `/.well-known/acme-challenge/<token>` to
`/var/www/certbot-challenges/.well-known/acme-challenge/<token>`. For nginx,
put the following block inside the site's `server` block for port 80:

```nginx
location /.well-known/acme-challenge/ {
    root /var/www/certbot-challenges;
}
```

The nginx `root` adds the request path below `/var/www/certbot-challenges`,
matching where certbot writes its challenge files.

Set the server address, the site's name, and your contact address in
`inventories/example.yml`, then run:

```sh
mmx dryrun configure
mmx run configure
mmx run configure
```

On the node, in order:

1. MMX installs certbot from the distribution's packages. On Rocky Linux and
   AlmaLinux it first installs `epel-release`, which adds the EPEL repository
   that has certbot.
2. MMX makes the directory `/var/www/certbot-challenges`.
3. MMX installs the script
   `/usr/local/bin/reload-web-server-after-certificate-renewal`, which
   reloads the web server.
4. `certbot_certificate` starts certbot, which asks the certificate
   authority for one certificate for `shop.example.com` and
   `www.shop.example.com`. The authority fetches a file that certbot put
   into the directory, through the web server. The certificate and its key
   are then in `/etc/letsencrypt/live/shop.example.com/`, as `fullchain.pem`
   and `privkey.pem`. certbot runs the web server reload script once.
5. MMX enables certbot's timer, which renews every certificate of the node.
   certbot runs the web server reload script again after each renewal.

The second run changes nothing and starts no certbot.

On a node without certbot, the first dryrun reports the certificate task
`not_checked`, with the reason that certbot is not installed: the package
task before it would install certbot, and only then can MMX compare. On a
node that has certbot, the dryrun says that a certificate would be
requested.

**Try it against a test server first.** Let's Encrypt limits how many
certificates it issues for a name in a week. Its test server has much higher
limits and issues certificates that no browser trusts. Set
`certificate_authority_url` in the inventory to
`https://acme-staging-v02.api.letsencrypt.org/directory` while you try the
example. When you write Let's Encrypt's own address back, the next run gets
a new certificate from Let's Encrypt. A certificate from an authority
different from the authority named by the task is replaced.

**The TLS site comes after the certificate.** A web server refuses a
configuration that names certificate files which do not exist. Install the
site's TLS configuration in a node task block after the certificate task;
the `certbot_certificate` page shows
[the order](../../docs/reference/modules/certbot_certificate.md#the-first-run-the-certificate-before-the-configuration-that-names-it).

## Distributions

The example works on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9,
and AlmaLinux 10. The playbook chooses by the node's facts:

| Distribution | certbot comes from | The timer that the playbook enables |
|---|---|---|
| Debian, Ubuntu | the distribution's own packages | `certbot.timer` |
| Rocky Linux, AlmaLinux | the EPEL repository | `certbot-renew.timer` |

On a node with SELinux the example needs no label task: the `directory`
task makes `/var/www/certbot-challenges` with the label that lets a web
server read it. The `certbot_certificate` page says
[what a directory elsewhere needs](../../docs/reference/modules/certbot_certificate.md#on-a-node-with-selinux).

For every field, see the
[`certbot_certificate` page](../../docs/reference/modules/certbot_certificate.md).
