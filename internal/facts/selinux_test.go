package facts

import (
	"errors"
	"io/fs"
	"testing"
)

// files is a node's files for the SELinux reads; a missing name does not exist.
func files(contents map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		text, ok := contents[name]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(text), nil
	}
}

func TestSELinuxMode(t *testing.T) {
	const enforce, context = "/sys/fs/selinux/enforce", "/proc/self/attr/current"
	running := "system_u:system_r:unconfined_service_t:s0\x00"
	for _, c := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"enforcing", map[string]string{enforce: "1", context: running}, "enforcing"},
		{"permissive", map[string]string{enforce: "0\n", context: running}, "permissive"},
		{"no selinuxfs", map[string]string{context: "unconfined\n"}, "disabled"},
		// selinuxfs mounted without a loaded policy, as on some cloud images.
		{"no policy", map[string]string{enforce: "0", context: "kernel\x00"}, "disabled"},
		{"unexpected state", map[string]string{enforce: "2", context: running}, ""},
	} {
		mode, err := selinuxMode(files(c.files))
		if mode != c.want || (err == nil) != (c.want != "") {
			t.Fatal(c.name, mode, err)
		}
	}
	if mode, err := selinuxMode(func(string) ([]byte, error) { return nil, errors.New("denied") }); mode != "" || err == nil {
		t.Fatal(mode, err)
	}
}
