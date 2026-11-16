# Test stand-in for the shared brand site application (a Rails application in
# production). nginx tells it which site it is through BRAND_SITE_NAME.
release = File.read(File.join(__dir__, "RELEASE")).strip
site = ENV.fetch("BRAND_SITE_NAME", "unknown")

run lambda { |env|
  [200, { "Content-Type" => "text/plain" }, ["#{site} #{release} #{env["PATH_INFO"]}\n"]]
}
