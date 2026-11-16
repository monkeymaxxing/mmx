package config

import (
	"strings"
	"testing"
)

// Later tasks and outputs may read public_key; each rejected input names its
// field and what to write.
func TestSSHKeyPairLoads(t *testing.T) {
	key := "- id: backup_key\n  ssh_key_pair:\n    for_user_account: backup\n    private_key_path: /home/backup/.ssh/id_ed25519\n    key_type: ed25519\n"
	if err := loadExample(t, key+"- show: \"{{ .node_tasks.backup_key.public_key }}\"\n"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ fields, want string }{
		{"    private_key_path: /home/backup/.ssh/id_ed25519\n    key_type: ed25519\n", "for_user_account is required"},
		{"    for_user_account: backup\n    key_type: ed25519\n", "private_key_path is required"},
		{"    for_user_account: backup\n    private_key_path: /home/backup/.ssh/id_ed25519\n", "key_type is required: ed25519 or rsa"},
		{"    for_user_account: backup\n    private_key_path: /home/backup/.ssh/id_ed25519\n    key_type: dsa\n", "key_type is required: ed25519 or rsa"},
		{"    for_user_account: backup\n    private_key_path: .ssh/id_ed25519\n    key_type: ed25519\n", "path must be absolute"},
		{"    for_user_account: \"bad name\"\n    private_key_path: /home/backup/.ssh/id_ed25519\n    key_type: ed25519\n", "invalid account name"},
		{"    for_user_account: backup\n    private_key_path: /home/backup/.ssh/id_ed25519\n    key_type: ed25519\n    key_comment_when_created: \"two\\nlines\"\n", "key_comment_when_created must be one line"},
		{"    for_user_account: backup\n    private_key_path: /home/backup/.ssh/id_ed25519\n    key_type: ed25519\n    type: rsa\n", "type"},
	} {
		err := loadExample(t, "- ssh_key_pair:\n"+tc.fields)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v", tc.fields, err)
		}
	}
	if err := loadExample(t, key+"- show: \"{{ .node_tasks.backup_key.private_key }}\"\n"); err == nil || !strings.Contains(err.Error(), "unknown node task result field") {
		t.Fatal(err)
	}
}
