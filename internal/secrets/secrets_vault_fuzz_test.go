package secrets

import (
	"bytes"
	"encoding/base64"
	"io"
	"mmx/internal/modules"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"filippo.io/age"
)

// syntheticSecretValue stands for a secret value in the seeds. No error from
// reading a vault may quote it.
const syntheticSecretValue = "fuzz-secret-value-never-in-errors"

// FuzzSecretsVaultPlaintext feeds untrusted bytes to the reader of a vault's
// decrypted text. Text that reads without error gives a mapping of valid
// secret names; any other text gives no values and an error without a value.
// A second YAML document after the text must be refused.
func FuzzSecretsVaultPlaintext(f *testing.F) {
	f.Add([]byte("{}\n"))
	f.Add([]byte(newVaultText))
	f.Add([]byte("database_password: " + syntheticSecretValue + "\nnumber: 42\ndatabase:\n  user: app\n  password: " + syntheticSecretValue + "\n"))
	f.Add([]byte("# Named secret values\npin: \"0123456789\" # digits in quotes stay text\n"))
	f.Add([]byte("9lives: " + syntheticSecretValue + "\n"))
	f.Add([]byte("token: one\ntoken: " + syntheticSecretValue + "\n"))
	f.Add([]byte("shared: &shared " + syntheticSecretValue + "\ncopy: *shared\ntagged: !!str " + syntheticSecretValue + "\n"))
	f.Add([]byte("token: [" + syntheticSecretValue + "\n"))
	f.Add([]byte("[" + syntheticSecretValue + "]\n"))
	f.Add([]byte("token: " + syntheticSecretValue + "\n---\nother: value\n"))
	f.Fuzz(func(t *testing.T, vaultPlaintext []byte) {
		secretValues, err := decode(vaultPlaintext)
		if err != nil {
			if secretValues != nil {
				t.Fatalf("returned values with the error %v", err)
			}
			if strings.Contains(err.Error(), syntheticSecretValue) {
				t.Fatalf("the error quotes a secret value: %v", err)
			}
			return
		}
		if secretValues == nil {
			t.Fatal("returned no values and no error")
		}
		for secretName := range secretValues {
			if !modules.Name.MatchString(secretName) {
				t.Fatalf("accepted secret name %q", secretName)
			}
		}
		// A text without a YAML document, such as only comments, has no first
		// document for a second one to follow.
		if withoutYAMLDocument(vaultPlaintext) {
			return
		}
		secretValues, err = decode(append(bytes.Clone(vaultPlaintext), "\n---\n{}\n"...))
		if err == nil || secretValues != nil {
			t.Fatal("accepted vault text followed by a second YAML document")
		}
		if strings.Contains(err.Error(), syntheticSecretValue) {
			t.Fatalf("the error quotes a secret value: %v", err)
		}
	})
}

// syntheticVaultPassphrase is the passphrase of encryptedFuzzVaultBase64.
const syntheticVaultPassphrase = "synthetic fuzz vault passphrase"

// encryptedFuzzVaultBase64 is an age file that age.NewScryptRecipient(
// syntheticVaultPassphrase) with SetWorkFactor(1) encrypted once. Its text is
// "token: fuzz-secret-value-never-in-errors\nnumber: 42\n". It is fixed so
// that every fuzzing worker starts from the same seed.
const encryptedFuzzVaultBase64 = "YWdlLWVuY3J5cHRpb24ub3JnL3YxCi0+IHNjcnlwdCA5R0ZPTVNscDR6WTNIM1d0YWdDb0NBIDEK" +
	"MkFkZjc1ZmpnQkZqd1dHZS9jbFk3bFNGOThWRWg3bzJPVllqbjRDQWhxRQotLS0gY1AyQlY4Wjhq" +
	"T1kwaDQwNWNiZkx2WnNOZHVhWGlBUm8ydUhjNHZvZEt2bwqLfXRT3n45TezFnMffPih9DJNGHzCG" +
	"5Oq5njKZy2m6M0sKILGu7UFaXDsaF27D/sxbxjH3UX4jqGkDugyrgl6YPwqVUaP/5Tx0O0mW0mLA" +
	"23fsXEE="

// opensCheaplyWithFuzzPassphrase reports whether MMX's vault reader would read
// encryptedVault within its size bound and find an age header that
// syntheticVaultPassphrase opens at scrypt work factor 1.
//
// MMX's reader accepts a work factor of up to 22, which costs about 15 seconds
// and 4 GiB of memory for each attempt. A mutated header could ask for that on
// the shared machine that runs the fuzzer. This gate refuses every header
// that needs more than work factor 1 or that the known passphrase does not
// authenticate, so MMX's reader only ever repeats the cheap scrypt work.
func opensCheaplyWithFuzzPassphrase(t testing.TB, encryptedVault []byte) bool {
	if len(encryptedVault) > MaxVaultBytes+64*1024 {
		return false
	}
	header, err := age.ExtractHeader(io.LimitReader(bytes.NewReader(encryptedVault), MaxVaultBytes+64*1024))
	if err != nil {
		return false
	}
	identity, err := age.NewScryptIdentity(syntheticVaultPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	identity.SetMaxWorkFactor(1)
	_, err = age.DecryptHeader(header, identity)
	return err == nil
}

// FuzzEncryptedSecretsVaultFile feeds untrusted age file bytes to the two
// readers of an encrypted vault: from its bytes, as for the version in the
// last Git commit, and from the vault file. Both must agree: the same values
// on success, no values on failure, and no error that quotes a secret value.
func FuzzEncryptedSecretsVaultFile(f *testing.F) {
	encryptedVault, err := base64.StdEncoding.DecodeString(encryptedFuzzVaultBase64)
	if err != nil {
		f.Fatal(err)
	}
	// The only seed that reads must pass the gate and give the known values,
	// or the fuzzer would quietly skip every input.
	if !opensCheaplyWithFuzzPassphrase(f, encryptedVault) {
		f.Fatal("the fixed encrypted vault does not pass the work factor gate")
	}
	secretValues, err := ReadEncrypted(encryptedVault, syntheticVaultPassphrase)
	if err != nil || secretValues["token"] != syntheticSecretValue || secretValues["number"] != 42 {
		f.Fatalf("the fixed encrypted vault does not read as expected: %v", err)
	}
	corruptedPayload := bytes.Clone(encryptedVault)
	corruptedPayload[len(corruptedPayload)-1] ^= 0xff
	f.Add(encryptedVault)
	f.Add([]byte{})
	f.Add([]byte("token: " + syntheticSecretValue + "\nnumber: 42\n"))
	f.Add([]byte("age-encryption.org/v1\n-> scrypt\n--- \n"))
	f.Add(encryptedVault[:len(encryptedVault)-10])
	f.Add(corruptedPayload)
	f.Add(bytes.Replace(encryptedVault, []byte(" 1\n"), []byte(" 20\n"), 1))
	f.Fuzz(func(t *testing.T, encryptedVault []byte) {
		if !opensCheaplyWithFuzzPassphrase(t, encryptedVault) {
			return
		}
		valuesFromBytes, errFromBytes := ReadEncrypted(encryptedVault, syntheticVaultPassphrase)
		vaultPath := filepath.Join(t.TempDir(), "secrets.age")
		if err := os.WriteFile(vaultPath, encryptedVault, 0600); err != nil {
			t.Fatal(err)
		}
		valuesFromFile, errFromFile := Read(vaultPath, syntheticVaultPassphrase)
		for _, err := range []error{errFromBytes, errFromFile} {
			if err != nil && strings.Contains(err.Error(), syntheticSecretValue) {
				t.Fatalf("the error quotes a secret value: %v", err)
			}
		}
		if (errFromBytes == nil) != (errFromFile == nil) {
			t.Fatalf("the vault bytes gave the error %v, the vault file gave the error %v", errFromBytes, errFromFile)
		}
		if errFromBytes != nil {
			if valuesFromBytes != nil || valuesFromFile != nil {
				t.Fatalf("returned values with the error %v", errFromBytes)
			}
			return
		}
		if valuesFromBytes == nil {
			t.Fatal("returned no values and no error")
		}
		if !reflect.DeepEqual(valuesFromBytes, valuesFromFile) {
			t.Fatal("the vault bytes and the vault file gave different values")
		}
	})
}
