package modules

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

func TestParseImageReference(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("ab", 32)
	for text, want := range map[string]imageReference{
		"registry.example.com/shop/web:2.4":       {Registry: "registry.example.com", Repository: "shop/web", Tag: "2.4"},
		"registry.example.com:5000/shop/web":      {Registry: "registry.example.com:5000", Repository: "shop/web"},
		"127.0.0.1:5000/mmx-test/app:1":           {Registry: "127.0.0.1:5000", Repository: "mmx-test/app", Tag: "1"},
		"localhost/app:1":                         {Registry: "localhost", Repository: "app", Tag: "1"},
		"[::1]:5000/app":                          {Registry: "[::1]:5000", Repository: "app"},
		"postgres:17.2":                           {Repository: "postgres", Tag: "17.2"},
		"library/postgres":                        {Repository: "library/postgres"},
		"registry.example.com/shop/web@" + digest: {Registry: "registry.example.com", Repository: "shop/web", Digest: digest},
		"shop/web:2.4@" + digest:                  {Repository: "shop/web", Tag: "2.4", Digest: digest},
	} {
		got, err := parseImageReference(text)
		if err != nil || got != want {
			t.Errorf("parseImageReference(%q) = %+v, %v; want %+v", text, got, err, want)
		}
	}
	for text, message := range map[string]string{
		"":                                 "the image name",
		"Shop/Web:2.4":                     "must be lowercase",
		"registry.example.com/shop/web:":   "is not a tag",
		"registry.example.com/shop//web:1": "the image name",
		"shop/web@sha256:abc":              "is not a digest",
		"bad_host.example.com:x/app:1":     "is not a registry's host name",
		"registry.example.com/shop/web :1": "the image name",
	} {
		if _, err := parseImageReference(text); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("parseImageReference(%q) = %v; want an error with %q", text, err, message)
		}
	}
}

func TestCheckImageReference(t *testing.T) {
	t.Parallel()
	if _, err := checkImageReference("postgres:17.2", false); err != nil {
		t.Errorf("a tag without a registry: %v", err)
	}
	if _, err := checkImageReference("registry.example.com/shop/web:2.4", true); err != nil {
		t.Errorf("a full reference: %v", err)
	}
	_, err := checkImageReference("registry.example.com/shop/web", false)
	if err == nil || err.Error() != `"registry.example.com/shop/web" names no tag and no digest; write the tag, such as registry.example.com/shop/web:2.4, or a digest` {
		t.Errorf("no tag and no digest: %v", err)
	}
	_, err = checkImageReference("nginx:1.27", true)
	if err == nil || err.Error() != `"nginx:1.27" names no registry; write the full reference, such as docker.io/library/nginx:1.27` {
		t.Errorf("no registry: %v", err)
	}
}

func TestRegistryLoginFile(t *testing.T) {
	t.Parallel()
	got := string(registryLoginFile([]registryLogin{{Registry: "registry.example.com", Username: "deploy", Password: "p:w"}}))
	// "deploy:p:w" in base64.
	if want := `{"auths":{"registry.example.com":{"auth":"ZGVwbG95OnA6dw=="}}}`; got != want {
		t.Errorf("registryLoginFile = %s; want %s", got, want)
	}
}

// taskScope is the scope of one node task, with its trace, whose .secrets
// holds these secrets.
func taskScope(secrets map[string]string) (value.Scope, *value.Trace) {
	held := map[string]any{}
	for name, text := range secrets {
		held[name] = &value.Secret{Name: name, Data: text, Available: true}
	}
	return value.Scope{"secrets": held}.Trace()
}

// reportedByTheEngine is a module's result as the node task execution engine
// passes it on: without the logins that the task of scope s handed only to
// its tool.
func reportedByTheEngine(r protocol.Result, s value.Scope) protocol.Result {
	return r.WithoutLoginsHandedOnlyToATool(s.LoginsHandedOnlyToATool())
}

// A registry login is handed only to docker or podman. Reading it marks no
// trace. When the user name or the password holds a secret, the task's trace
// remembers the secret and every form in which the tool gets, keeps, or
// sends the login; a login without a secret is not remembered.
func TestRegistryLoginIsHandedOnlyToATool(t *testing.T) {
	t.Parallel()
	const user, password = "robot$shop+deploy", "sentinel p/w&4711"
	secrets := map[string]string{"registry_username": user, "registry_password": password}
	stored := func(user, password string) string {
		return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	}
	header := func(user, password string) string {
		return base64.URLEncoding.EncodeToString([]byte(`{"password":"` + strings.ReplaceAll(password, "&", `\u0026`) + `","serveraddress":"registry.example.com","username":"` + user + `"}`))
	}
	for name, c := range map[string]struct {
		username, password string
		want               []string
	}{
		"a secret password": {"deploy", "{{ .secrets.registry_password }}",
			[]string{password, "sentinel+p%2Fw%264711", stored("deploy", password), header("deploy", password)}},
		"a secret user name": {"{{ .secrets.registry_username }}", "not-a-secret",
			[]string{user, "robot%24shop%2Bdeploy", stored(user, "not-a-secret"), header(user, "not-a-secret")}},
		"both": {"{{ .secrets.registry_username }}", "{{ .secrets.registry_password }}",
			[]string{user, "robot%24shop%2Bdeploy", password, "sentinel+p%2Fw%264711", stored(user, password), header(user, password)}},
		"no secret": {"deploy", "not-a-secret", nil},
	} {
		s, trace := taskScope(secrets)
		login, err := resolveRegistryLogin(s, "registry.example.com", "docker_image", c.username, c.password)
		if err != nil || trace.Used {
			t.Fatal(name, err, "the login marked the trace:", trace.Used)
		}
		if got := s.LoginsHandedOnlyToATool(); !slices.Equal(got, c.want) {
			t.Errorf("%s: the trace remembers %q, want %q", name, got, c.want)
		}
		// The remembered forms are the ones that the tools get: the text in
		// the login file, and the header for Docker's daemon.
		if file := string(registryLoginFile([]registryLogin{login})); file != `{"auths":{"registry.example.com":{"auth":"`+stored(login.Username, login.Password)+`"}}}` {
			t.Errorf("%s: the login file is %s", name, file)
		}
		if login.headerForDockerDaemon() != header(login.Username, login.Password) {
			t.Errorf("%s: the header is %s", name, login.headerForDockerDaemon())
		}
	}
	// A login that cannot be read names its input.
	s, _ := taskScope(nil)
	if _, err := resolveRegistryLogin(s, "registry.example.com", "docker_compose_project.registry_logins[0]", "deploy", "{{ .secrets.registry_password }}"); err == nil ||
		!strings.HasPrefix(err.Error(), "docker_compose_project.registry_logins[0].registry_password: ") {
		t.Fatal(err)
	}
}
