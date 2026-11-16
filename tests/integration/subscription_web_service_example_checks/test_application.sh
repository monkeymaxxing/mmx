# Checks on the test node test_application of the example
# examples/subscription-web-service-fleet, after every step of the suite
# tests/integration/subscription_web_service_example.py: the base setup, the
# firewall, Ruby, nginx with Passenger, the deployed releases, the workers,
# the cron jobs, and the main application's reach to MySQL, Redis,
# memcached, Solr, and ClickHouse on test_load_balancer_and_data. Runs as
# root with /bin/sh; prints every check and exits non-zero when one fails.
# The suite sets SUBSCRIPTION_WEB_SERVICE_CLICKHOUSE_REPORTING_PASSWORD to
# the test value of that secret.
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

main_application=/home/deploy/apps/main_application
database_configuration=$main_application/shared/config/database.yml
services_configuration=$main_application/shared/config/services.yml

echo "== base setup (role base_server_setup)"
check "locale en_US.UTF-8 is the system default" grep -qx 'LANG=en_US.UTF-8' /etc/default/locale
check "time zone Europe/Berlin" sh -c 'test "$(readlink -f /etc/localtime)" = /usr/share/zoneinfo/Europe/Berlin'
check "systemd-timesyncd keeps the clock" systemctl is-active --quiet systemd-timesyncd
for package in sudo git vim htop tmux fail2ban unattended-upgrades nftables ruby ruby-bundler nodejs nginx libnginx-mod-http-passenger imagemagick libvips-dev; do
  check "package $package installed" installed "$package"
done
check "account alice has uid 2001" sh -c 'test "$(id -u alice)" = 2001'
check "upgrade window starts at 08:00 on this server" sh -c 'systemctl cat apt-daily-upgrade.timer | matches -qx "OnCalendar=\*-\*-\* 08:00"'
check "sshd refuses passwords" sh -c 'sshd -T | matches -qx "passwordauthentication no"'

echo "== firewall (role nftables_firewall)"
check "firewall service active" systemctl is-active --quiet fleet-firewall
check "no public web ports on an application server" sh -c '! nft list table inet fleet_firewall | matches -q "dport { 80, 443 }"'

echo "== deploy account and Ruby (roles deploy_account, ruby_and_bundler, nodejs)"
# 1101 is the test inventory's deploy_account_uid: on the test machines'
# cloud images, ubuntu takes 1000 and the harness's login account may take 1001.
check "account deploy has uid 1101 and the group deploy" sh -c 'test "$(id -u deploy)" = 1101 && test "$(id -gn deploy)" = deploy'
check "deploy has exactly the deployment key" sh -c 'test "$(grep -c "^ssh-" /home/deploy/.ssh/authorized_keys)" = 1 && grep -q "ci-deploy@example.com" /home/deploy/.ssh/authorized_keys'
check "Ruby and Bundler work" sh -c 'ruby -e "exit 0" && bundle --version'

echo "== releases (role application_release, playbook deploy)"
check "main application's current release is v2" sh -c "test \"\$(readlink $main_application/current)\" = releases/v2"
check "release v1 was removed" test ! -e "$main_application/releases/v1"
check "release v2 has its gems" sh -c "test -d $main_application/releases/v2/vendor/bundle && test -d $main_application/releases/v2/api/vendor/bundle"
check "release links the shared database configuration" sh -c "test \"\$(readlink $main_application/releases/v2/config/database.yml)\" = $database_configuration"
check "database configuration readable only by deploy" sh -c "test \"\$(stat -c '%U %a' $database_configuration)\" = 'deploy 600'"
for site in gardening cooking; do
  check "brand site $site's current release is v2" sh -c "test \"\$(readlink /home/deploy/apps/$site/current)\" = releases/v2"
done

echo "== nginx with Passenger (roles nginx_with_passenger, main_application_nginx_servers, brand_site_nginx_servers)"
check "nginx active" systemctl is-active --quiet nginx
check "nginx configuration valid" nginx -t
check "API answers on port 8080 with release v2" sh -c 'curl -s http://127.0.0.1:8080/v1/ping | matches -qx "api v2 pong"'
check "main application answers on port 8081 with release v2" sh -c 'curl -s http://127.0.0.1:8081/ | matches -q "^main_application v2 /"'
check "brand site gardening on port 8092" sh -c 'curl -s http://127.0.0.1:8092/ | matches -q "^gardening v2 /"'
check "brand site cooking on port 8093" sh -c 'curl -s http://127.0.0.1:8093/ | matches -q "^cooking v2 /"'
check "Passenger's pool is limited" sh -c 'matches -Eq "^[[:space:]]*passenger_max_pool_size [0-9]+;$" < /etc/nginx/nginx.conf'

echo "== workers (role main_application_workers)"
for group in default long_running notifications; do
  check "worker service of the queue group $group active" systemctl is-active --quiet "main-application-workers-$group.service"
done
check "worker target active and enabled" sh -c 'systemctl is-active --quiet main-application-workers.target && systemctl is-enabled --quiet main-application-workers.target'
# A worker writes its line every 10 seconds after its start.
check "workers run release v2" sh -c '
  for second in $(seq 30); do grep -q "release=v2 queue_group=default" /home/deploy/logs/main-application-workers-default.log && exit 0; sleep 1; done; exit 1'
check "notification workers name their queues" sh -c 'systemctl show --property Environment main-application-workers-notifications.service | matches -q "QUEUES=Notifications::EmailDelivery,Notifications::PushDelivery"'

echo "== cron jobs (role main_application_cron_jobs)"
check "three cron jobs of the main application" sh -c 'test "$(ls /etc/cron.d/main-application-* | wc -l)" = 3'
check "billing:renew_subscriptions runs as deploy at 00:10" grep -q '^10 0 \* \* \* deploy ' /etc/cron.d/main-application-billing-renew-subscriptions
check "a cron job's command passes its rake argument to the deployed release" sh -c '
  cron_file=/etc/cron.d/main-application-reports-send-weekly-summary
  command=$(grep -v "^#" $cron_file | grep -v "^[A-Z_]*=" | grep . | tail -n 1 | cut -d" " -f7-)
  variables=$(grep "^[A-Z_]*=" $cron_file | tr "\n" " ")
  su -s /bin/sh deploy -c "cd /home/deploy && export $variables && $command" &&
  tail -n 1 /home/deploy/logs/reports_send_weekly_summary.log | matches -q "reports:send_summary.*weekly.*release=v2 RAILS_ENV=production"'

echo "== the main application reaches its services on test_load_balancer_and_data"
database_host=$(sed -n 's/^  host: //p' "$database_configuration")
database_password=$(sed -n 's/^  password: "\(.*\)"$/\1/p' "$database_configuration")
cache_host=$(sed -n 's|^  memcached: \(.*\):11211$|\1|p' "$services_configuration")
solr_url=$(sed -n 's/^  solr: //p' "$services_configuration")
check "MySQL with the account main_application" sh -c "test \"\$(MYSQL_PWD='$database_password' mysql -h '$database_host' -u main_application -N main_application_production -e 'SELECT COUNT(*) FROM page_views')\" = 3"
check "Redis instance application-persistent" sh -c "redis-cli -h '$cache_host' -p 6379 ping | matches -qx PONG"
check "memcached" sh -c "python3 -c \"import socket; s = socket.create_connection(('$cache_host', 11211), 3); s.sendall(b'version\\r\\n'); print(s.recv(100).decode())\" | matches -q '^VERSION'"
check "Solr core" sh -c "curl -s '$solr_url/select?q=*:*' | matches -q numFound"
check "ClickHouse with the account reporting" sh -c "test \"\$(curl -s -u 'reporting:$reporting_password' 'http://$database_host:8123/?query=SELECT%201')\" = 1"

echo
if [ "$failures" -gt 0 ]; then
  echo "$failures checks failed on test_application"
  exit 1
fi
echo "all checks passed on test_application"
