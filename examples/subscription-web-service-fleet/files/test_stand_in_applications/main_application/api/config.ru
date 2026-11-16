# Test stand-in for the main application's API (part of the Rails
# application in production). /v1/ping answers "api <release> pong".
release = File.read(File.join(__dir__, "..", "RELEASE")).strip

run lambda { |env|
  path = env["PATH_INFO"]
  body = path == "/v1/ping" ? "api #{release} pong\n" : "api #{release} #{path}\n"
  [200, { "Content-Type" => "text/plain" }, [body]]
}
