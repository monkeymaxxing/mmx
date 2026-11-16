# Checks on the test node test_load_balancer_and_data of the example
# examples/subscription-web-service-fleet, after every step of the suite
# tests/integration/subscription_web_service_example.py: the base setup, the
# firewall, the load balancer and its maintenance pages, MySQL, Redis,
# memcached, Solr, ClickHouse, and their backups. Runs as root with /bin/sh;
# prints every check and exits non-zero when one fails. The suite sets
# SUBSCRIPTION_WEB_SERVICE_CLICKHOUSE_REPORTING_PASSWORD to the test value of
# that secret.
reporting_password=${SUBSCRIPTION_WEB_SERVICE_CLICKHOUSE_REPORTING_PASSWORD:?the suite sets the password of the ClickHouse account reporting}
failures=0
# A failed check prints the last lines of what its command printed, also
# what a pipeline's grep looked at: "matches" is grep that first copies its
# input to stderr.
helpers=$(mktemp -d)
trap 'rm -rf "$helpers"' EXIT
cat > "$helpers/matches" <<'MATCHES'
#!/bin/sh
input=$(cat)
printf '%s\n' "$input" >&2
printf '%s\n' "$input" | grep "$@"
MATCHES
chmod 0755 "$helpers/matches"
PATH="$helpers:$PATH"
export PATH
check() {
  description=$1
  shift
  if output=$("$@" 2>&1); then
    echo "ok:     $description"
  else
    echo "FAILED: $description"
    printf '%s\n' "$output" | tail -n 12 | sed 's/^/        /'
    failures=$((failures + 1))
  fi
}
installed() { dpkg-query -W -f='${Status}\n' "$1" | matches -qx 'install ok installed'; }
contains() { printf '%s' "$1" | matches -q -- "$2"; }
# answers_200 NAME PATH CERTIFICATE: an HTTPS request for NAME through the
# load balancer gets HTTP status 200.
answers_200() {
  status=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$3" --resolve "$1:443:127.0.0.1" "https://$1$2")
  echo "HTTP status $status"
  test "$status" = 200
}

echo "== base setup (role base_server_setup, fleet stages data_server_swap and nginx_load_balancers)"
check "locale en_US.UTF-8 is the system default" grep -qx 'LANG=en_US.UTF-8' /etc/default/locale
check "time zone Europe/Berlin" sh -c 'test "$(readlink -f /etc/localtime)" = /usr/share/zoneinfo/Europe/Berlin'
check "systemd-timesyncd keeps the clock" systemctl is-active --quiet systemd-timesyncd
for package in sudo git vim htop tmux fail2ban unattended-upgrades acl rsync nftables; do
  check "package $package installed" installed "$package"
done
check "account alice has uid 2001 and the group sudo" sh -c 'test "$(id -u alice)" = 2001 && id -nG alice | matches -qw sudo'
check "account bob has uid 2002" sh -c 'test "$(id -u bob)" = 2002'
check "alice's key is authorized" grep -q 'alice@example.com' /home/alice/.ssh/authorized_keys
check "account recovery has uid 5001 and a SHA-512 password hash" sh -c 'test "$(id -u recovery)" = 5001 && test "$(getent shadow recovery | cut -d: -f2 | cut -c1-3)" = "\$6\$"'
check "administrators' .profile sources the prompt" grep -qx 'source /etc/bash_prompt' /home/alice/.profile
check "sudo without password for the group sudo" sh -c 'grep -qx "%sudo ALL=(ALL:ALL) NOPASSWD:ALL" /etc/sudoers.d/90-sudo-group-without-password && visudo -c'
check "sshd refuses passwords" sh -c 'sshd -T | matches -qx "passwordauthentication no"'
check "sshd logs verbosely" sh -c 'sshd -T | matches -qx "loglevel VERBOSE"'
check "GitHub's host keys are known" grep -q '^github.com ssh-ed25519 ' /etc/ssh/ssh_known_hosts
check "editor is vim" sh -c 'test "$(readlink -f /usr/bin/editor)" = /usr/bin/vim.basic'
check "automatic security updates are on" grep -q 'Unattended-Upgrade "1"' /etc/apt/apt.conf.d/20auto-upgrades
check "upgrade window starts at 10:00 on the first server of its role" sh -c 'systemctl cat apt-daily-upgrade.timer | matches -qx "OnCalendar=\*-\*-\* 10:00"'
check "the company's internal certificate authority is trusted" test -e /etc/ssl/certs/company-internal-certificate-authority.pem
check "the public names resolve to the load balancer" grep -q ' api.example.com www.example.com # names of the load balancer$' /etc/hosts
check "transparent huge pages stay at the kernel's madvise" grep -q '\[madvise\]' /sys/kernel/mm/transparent_hugepage/enabled
check "vm.swappiness is 1 on a data server" sh -c 'test "$(sysctl -n vm.swappiness)" = 1'
check "the swap file /swapfile is active" sh -c 'swapon --show=NAME --noheadings | matches -qx /swapfile'
check "net.ipv4.ip_nonlocal_bind is 1 on the load balancer" sh -c 'test "$(sysctl -n net.ipv4.ip_nonlocal_bind)" = 1'

echo "== firewall (role nftables_firewall)"
check "firewall service active" systemctl is-active --quiet fleet-firewall
rules=$(nft list table inet fleet_firewall 2>&1)
check "firewall drops what no rule admits" contains "$rules" 'policy drop'
check "firewall admits SSH and MMX" sh -c 'nft list table inet fleet_firewall | matches -q "tcp dport 22 accept" && nft list table inet fleet_firewall | matches -q "tcp dport 7443 accept"'
check "firewall admits HTTP and HTTPS on the load balancer" contains "$rules" 'tcp dport { 80, 443 } accept'
check "ClickHouse's ports are not public" sh -c '! nft list table inet fleet_firewall | matches -q "dport { 8123, 9000 }"'

echo "== load balancer (roles nginx_load_balancer, keepalived_for_load_balancers, test_self_signed_certificates)"
certificates=/etc/ssl/test-self-signed
main_certificate=$certificates/example.com/fullchain.pem
check "nginx active" systemctl is-active --quiet nginx
check "nginx configuration valid" nginx -t
check "unknown names get 404" sh -c 'test "$(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1/)" = 404'
check "API through the load balancer answers with release v2" sh -c 'curl -s -H "Host: api.example.com" http://127.0.0.1/v1/ping | matches -q "^api v2 pong"'
check "API over HTTPS with the load balancer's certificate" sh -c "curl -s --cacert $main_certificate --resolve api.example.com:443:127.0.0.1 https://api.example.com/v1/ping | matches -q '^api v2 pong'"
check "main application over HTTPS answers with release v2" sh -c "curl -s --cacert $main_certificate --resolve www.example.com:443:127.0.0.1 https://www.example.com/ | matches -q '^main_application v2 /'"
check "certificate covers *.example.com" sh -c "openssl x509 -in $main_certificate -noout -ext subjectAltName | matches -q 'DNS:\*.example.com'"
for site in gardening cooking; do
  domain=$site.example.net
  check "brand site $site answers with release v2" sh -c "curl -s -H 'Host: $domain' http://127.0.0.1/ | matches -q '^$site v2 /'"
  check "brand site $site over HTTPS" sh -c "curl -s --cacert $certificates/$domain/fullchain.pem --resolve $domain:443:127.0.0.1 https://$domain/ | matches -q '^$site v2 /'"
done
# The last step hid the maintenance pages again.
for webroot in gardening.example.net cooking.example.net main_application main_application_api; do
  check "maintenance page of /var/www/$webroot is hidden" test ! -e "/var/www/$webroot/maintenance_on.html"
done
check "a request to gardening.example.net gets 200" answers_200 gardening.example.net / "$certificates/gardening.example.net/fullchain.pem"
check "a request to cooking.example.net gets 200" answers_200 cooking.example.net / "$certificates/cooking.example.net/fullchain.pem"
check "a request to www.example.com gets 200" answers_200 www.example.com / "$main_certificate"
check "a request to api.example.com gets 200" answers_200 api.example.com /v1/ping "$main_certificate"
check "nginx status page for keepalived" sh -c 'curl -s http://127.0.0.1:8080/nginx_status | matches -q "Active connections"'
check "keepalived active" systemctl is-active --quiet keepalived
check "keepalived holds the virtual address" sh -c 'ip -brief address | matches -q "192.0.2.10/32"'

echo "== MySQL (roles mysql_server, mysql_backup)"
check "MySQL active" systemctl is-active --quiet mysql
check "server_id is 1" sh -c 'test "$(mysql -N -e "SELECT @@server_id")" = 1'
check "GTID mode on" sh -c 'test "$(mysql -N -e "SELECT @@gtid_mode")" = ON'
check "server listens on every address" sh -c 'test "$(mysql -N -e "SELECT @@bind_address")" = "*"'
check "database main_application_production with the stand-in table" sh -c 'test "$(mysql -N main_application_production -e "SELECT COUNT(*) FROM page_views")" = 3'
# grant_on names a literal database, so MMX writes its _ as \_; --raw prints
# that backslash once.
check "account main_application has every privilege on main_application_production" sh -c 'mysql -N --raw -e "SHOW GRANTS FOR main_application@\"%\"" | matches -qF "GRANT ALL PRIVILEGES ON \`main\\_application\\_production\`.*"'
check "account clickhouse_copy may only read main_application_production" sh -c 'mysql -N --raw -e "SHOW GRANTS FOR clickhouse_copy@\"%\"" | matches -qF "GRANT SELECT ON \`main\\_application\\_production\`.*"'
check "backup timer active" systemctl is-active --quiet mysql-backup.timer
check "a backup run writes a dump with the stand-in table" sh -c 'systemctl start mysql-backup.service && zcat "$(ls -t /var/backups/mysql/all-databases-*.sql.gz | head -n 1)" | matches -q "CREATE TABLE \`page_views\`"'

echo "== Redis and memcached (roles redis_instances, memcached)"
for instance in application-persistent:6379 application-cache:6380; do
  name=${instance%%:*}
  port=${instance##*:}
  check "redis-$name active and answers on port $port" sh -c "systemctl is-active --quiet redis-$name && redis-cli -p $port ping | matches -qx PONG"
done
check "persisted instance keeps an append-only log" sh -c 'redis-cli -p 6379 config get appendonly | matches -qx yes'
check "cache instance keeps nothing on disk" sh -c 'test -z "$(redis-cli -p 6380 config get save | sed -n 2p)"'
check "a full cache drops its least recently used keys" sh -c 'redis-cli -p 6380 config get maxmemory-policy | matches -qx allkeys-lru'
check "the package's own Redis service is off" sh -c '! systemctl is-active --quiet redis-server && ! systemctl is-enabled --quiet redis-server'
check "a run of the persisted log's backup copies it" sh -c '
  cron_file=/etc/cron.d/redis-application-persistent-backup
  command=$(grep -v "^#" $cron_file | grep -v "^[A-Z_]*=" | grep . | tail -n 1 | cut -d" " -f7-)
  variables=$(grep "^[A-Z_]*=" $cron_file | tr "\n" " ")
  sh -c "export $variables && $command" && ls /var/backups/redis/application-persistent-appendonlydir/ | matches -q manifest'
check "memcached active with 64 MiB" sh -c "systemctl is-active --quiet memcached && python3 -c \"import socket; s = socket.create_connection(('127.0.0.1', 11211), 3); s.sendall(b'stats\\r\\nquit\\r\\n'); print(s.recv(65536).decode())\" | matches -q 'STAT limit_maxbytes 67108864'"

echo "== Solr (role solr)"
check "Solr active" systemctl is-active --quiet solr
check "Solr runs with a 256 MiB heap" sh -c 'ps -o args= -u solr | matches -q -- "-Xmx256m"'
check "core production loaded" sh -c 'curl -s "http://127.0.0.1:8983/solr/admin/cores?action=STATUS&core=production&wt=json" | matches -q "\"instanceDir\""'
check "a document is indexed and found" sh -c '
  curl -s -X POST -H "Content-Type: application/json" "http://127.0.0.1:8983/solr/production/update?commit=true" \
    -d "[{\"id\":\"article-1\",\"type\":\"article\",\"name\":\"Growing tomatoes on a balcony\"}]" > /dev/null &&
  curl -s "http://127.0.0.1:8983/solr/production/select?q=name:tomatoes&wt=json" | matches -Eq "\"numFound\": ?1[,}]"'
check "backup timer active" systemctl is-active --quiet solr-backup.timer
# Solr writes the snapshot after the backup request returns.
check "a backup run makes a snapshot" sh -c '
  systemctl start solr-backup.service || { journalctl -u solr-backup.service --no-pager -n 5; exit 1; }
  for second in $(seq 30); do ls /var/backups/solr | matches -q "^snapshot\." && exit 0; sleep 1; done; exit 1'

echo "== ClickHouse (role clickhouse)"
check "ClickHouse active" systemctl is-active --quiet clickhouse-server
check "ClickHouse 26.8 from ClickHouse's repository" sh -c 'clickhouse-client --query "SELECT version()" | matches -q "^26\.8\."'
check "account reporting reads with its password" sh -c "test \"\$(curl -s -u 'reporting:$reporting_password' 'http://127.0.0.1:8123/?query=SELECT%201')\" = 1"
check "account reporting cannot write" sh -c "! curl -s -f -u 'reporting:$reporting_password' --data-binary 'CREATE TABLE reporting.forbidden (x UInt8) ENGINE = Memory' http://127.0.0.1:8123/"
check "hourly copy from MySQL is scheduled" systemctl is-active --quiet clickhouse-copy-page-views.timer
check "a copy run brings the 3 MySQL rows" sh -c 'systemctl start clickhouse-copy-page-views.service && test "$(clickhouse-client --query "SELECT count() FROM reporting.page_views")" = 3'

echo
if [ "$failures" -gt 0 ]; then
  echo "$failures checks failed on test_load_balancer_and_data"
  exit 1
fi
echo "all checks passed on test_load_balancer_and_data"
