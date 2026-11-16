# Test stand-in for the main application's web pages (a Rails application in
# production). It answers with its name, its release, and the path it was
# asked for.
release = File.read(File.join(__dir__, "RELEASE")).strip

run lambda { |env|
  [200, { "Content-Type" => "text/plain" }, ["main_application #{release} #{env["PATH_INFO"]}\n"]]
}
